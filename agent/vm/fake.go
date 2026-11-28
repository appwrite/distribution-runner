package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

const fakePrefix = "fake-vm-"

type Operation string

const (
	OperationCreate  Operation = "create"
	OperationExec    Operation = "exec"
	OperationCopy    Operation = "copy"
	OperationDestroy Operation = "destroy"
)

// Call is one recorded Layer call. Arguments and Environment are set for exec, Transfer for copy.
type Call struct {
	Operation   Operation
	Machine     string
	Arguments   []string
	Environment map[string]string
	Transfer    Transfer
}

// Fake is a Layer whose machines are temporary directories on the host. Guest paths map under the machine's
// directory (Machine.Address), and relative ones resolve against Home, so files copied in land where Build finds
// them. It records every call. The zero value is ready to use.
type Fake struct {
	// Build stands in for the build script. work is the host directory DISTRIBUTION_WORK names (Home when unset)
	// and environment the command's environment; ctx ends when Exec's does, and Exec waits for Build to return. An
	// *ExitError chooses the exit code; any other error exits 1 with the message on Stderr. A panic propagates out
	// of Exec.
	Build func(ctx context.Context, work string, environment map[string]string) error
	// Fail, when it returns an error for a call, makes that call fail with it before it acts.
	Fail func(call Call) error
	// Directory holds the machine directories, the system temporary directory by default.
	Directory string
	// Home is the guest home directory, "/Users/lume" by default.
	Home string

	mutex    sync.Mutex
	machines map[string]string
	calls    []Call
}

var _ Layer = (*Fake)(nil)

func (f *Fake) Create(_ context.Context, name string) (Machine, error) {
	if err := validateName(name); err != nil {
		return Machine{}, err
	}
	if err := f.record(Call{Operation: OperationCreate, Machine: name}); err != nil {
		return Machine{}, err
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if _, exists := f.machines[name]; exists {
		return Machine{}, fmt.Errorf("%w: %s", ErrExists, name)
	}
	root, err := os.MkdirTemp(f.Directory, fakePrefix+name+"-")
	if err != nil {
		return Machine{}, err
	}
	machine := Machine{Name: name, Address: root, Home: f.home()}
	if err := os.MkdirAll(host(machine, machine.Home), 0o700); err != nil {
		_ = os.RemoveAll(root)
		return Machine{}, err
	}
	if f.machines == nil {
		f.machines = map[string]string{}
	}
	f.machines[name] = root
	return machine, nil
}

func (f *Fake) Exec(ctx context.Context, machine Machine, command Command) (int, error) {
	if err := validateArguments(command.Arguments); err != nil {
		return -1, err
	}
	if err := validateEnvironment(command.Environment); err != nil {
		return -1, err
	}
	call := Call{Operation: OperationExec, Machine: machine.Name, Arguments: slices.Clone(command.Arguments), Environment: maps.Clone(command.Environment)}
	if err := f.record(call); err != nil {
		return -1, err
	}
	if err := f.lookup(machine); err != nil {
		return -1, err
	}
	if err := ctx.Err(); err != nil {
		return -1, fmt.Errorf("vm: exec in %s stopped: %w", machine.Name, context.Cause(ctx))
	}
	build := f.Build
	if build == nil {
		return 0, nil
	}
	work := machine.Home
	if value, ok := command.Environment["DISTRIBUTION_WORK"]; ok {
		work = value
	}
	work = host(machine, work)

	type result struct {
		err       error
		recovered any
		panicked  bool
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- result{recovered: recovered, panicked: true}
			}
		}()
		done <- result{err: build(ctx, work, maps.Clone(command.Environment))}
	}()

	select {
	case <-ctx.Done():
		<-done
		return -1, fmt.Errorf("vm: exec in %s stopped: %w", machine.Name, context.Cause(ctx))
	case outcome := <-done:
		if outcome.panicked {
			panic(outcome.recovered)
		}
		if outcome.err == nil {
			return 0, nil
		}
		code := 1
		var exit *ExitError
		if errors.As(outcome.err, &exit) {
			code = exit.Code
		}
		if command.Stderr != nil {
			_, _ = io.WriteString(command.Stderr, outcome.err.Error()+"\n")
		}
		return code, nil
	}
}

func (f *Fake) Copy(_ context.Context, machine Machine, transfer Transfer) error {
	if err := validateTransfer(transfer); err != nil {
		return err
	}
	if err := f.record(Call{Operation: OperationCopy, Machine: machine.Name, Transfer: transfer}); err != nil {
		return err
	}
	if err := f.lookup(machine); err != nil {
		return err
	}
	if transfer.Direction == Out {
		return copyOut(host(machine, guestPath(machine.Home, transfer.Source)), transfer.Destination, transfer.Limit)
	}
	destination := host(machine, guestPath(machine.Home, transfer.Destination))
	if information, err := os.Stat(destination); err == nil && information.IsDir() {
		return fmt.Errorf("%w: %s", ErrDestination, transfer.Destination)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	return copyTree(transfer.Source, destination)
}

// Sweep removes the machine directories under Directory whose names start with prefix and that this Fake did not
// create.
func (f *Fake) Sweep(_ context.Context, prefix string) error {
	if err := validateName(prefix); err != nil {
		return err
	}
	directory := f.Directory
	if directory == "" {
		directory = os.TempDir()
	}
	entries, err := os.ReadDir(directory)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	f.mutex.Lock()
	owned := map[string]bool{}
	for _, root := range f.machines {
		owned[filepath.Base(root)] = true
	}
	f.mutex.Unlock()
	var failures []error
	for _, entry := range entries {
		name, ok := strings.CutPrefix(entry.Name(), fakePrefix)
		if !ok || !strings.HasPrefix(name, prefix) || owned[entry.Name()] {
			continue
		}
		failures = append(failures, os.RemoveAll(filepath.Join(directory, entry.Name())))
	}
	return errors.Join(failures...)
}

// copyOut copies one regular file of at most limit bytes into a new private file, as Lume does.
func copyOut(source string, destination string, limit int64) (err error) {
	information, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !information.Mode().IsRegular() {
		return fmt.Errorf("%w: %s", ErrNotFile, source)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := createExclusive(destination)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := output.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(&limitWriter{writer: output, limit: limit}, input); err != nil {
		if errors.Is(err, ErrLimit) {
			return fmt.Errorf("%w: %s exceeds %d bytes", ErrLimit, source, limit)
		}
		return err
	}
	return nil
}

func (f *Fake) Destroy(_ context.Context, machine Machine) error {
	if machine.Name == "" {
		return nil
	}
	if err := f.record(Call{Operation: OperationDestroy, Machine: machine.Name}); err != nil {
		return err
	}
	f.mutex.Lock()
	root, exists := f.machines[machine.Name]
	delete(f.machines, machine.Name)
	f.mutex.Unlock()
	if !exists {
		return nil
	}
	return os.RemoveAll(root)
}

// Calls returns every recorded call in order.
func (f *Fake) Calls() []Call {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return slices.Clone(f.calls)
}

// Live returns the names of machines created and not yet destroyed.
func (f *Fake) Live() []string {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return slices.Sorted(maps.Keys(f.machines))
}

func (f *Fake) record(call Call) error {
	f.mutex.Lock()
	f.calls = append(f.calls, call)
	fail := f.Fail
	f.mutex.Unlock()
	if fail == nil {
		return nil
	}
	return fail(call)
}

func (f *Fake) lookup(machine Machine) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if root, exists := f.machines[machine.Name]; !exists || root != machine.Address {
		return fmt.Errorf("%w: %s", ErrNotFound, machine.Name)
	}
	return nil
}

func (f *Fake) home() string {
	if f.Home == "" {
		return "/Users/lume"
	}
	return path.Clean("/" + f.Home)
}

// host maps a guest path to its place under the machine directory; cleaning it as an absolute path keeps ".." from
// leaving that directory.
func host(machine Machine, guest string) string {
	return filepath.Join(machine.Address, filepath.FromSlash(path.Clean("/"+guest)))
}

func copyTree(source string, destination string) error {
	information, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !information.IsDir() {
		return copyFile(source, destination, information.Mode().Perm())
	}
	if err := os.MkdirAll(destination, information.Mode().Perm()|0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyTree(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(source string, destination string, mode fs.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	return os.Chmod(destination, mode)
}
