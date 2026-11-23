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
	"sync"
)

type Operation string

const (
	OperationCreate  Operation = "create"
	OperationExec    Operation = "exec"
	OperationCopy    Operation = "copy"
	OperationDestroy Operation = "destroy"
)

// Call is one recorded Layer call. Args and Env are set for exec, Transfer for copy.
type Call struct {
	Operation Operation
	Machine   string
	Args      []string
	Env       map[string]string
	Transfer  Transfer
}

// Fake is a Layer whose machines are temporary directories on the host. Guest paths map under the machine's
// directory (Machine.Address), and relative ones resolve against Home, so files copied in land where Build finds
// them. It records every call. The zero value is ready to use.
type Fake struct {
	// Build stands in for the build script. work is the host directory DISTRIBUTION_WORK names (Home when unset)
	// and env the command's environment. An *ExitError chooses the exit code; any other error exits 1 with the
	// message on Stderr. A panic propagates out of Exec.
	Build func(work string, env map[string]string) error
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
	root, err := os.MkdirTemp(f.Directory, "fake-vm-"+name+"-")
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
	if err := validateArgs(command.Args); err != nil {
		return -1, err
	}
	if err := validateEnvironment(command.Env); err != nil {
		return -1, err
	}
	call := Call{Operation: OperationExec, Machine: machine.Name, Args: slices.Clone(command.Args), Env: maps.Clone(command.Env)}
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
	if value, ok := command.Env["DISTRIBUTION_WORK"]; ok {
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
		done <- result{err: build(work, maps.Clone(command.Env))}
	}()

	select {
	case <-ctx.Done():
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
	source, destination := transfer.Source, transfer.Destination
	if transfer.Direction == In {
		destination = host(machine, guestPath(machine.Home, destination))
	} else {
		source = host(machine, guestPath(machine.Home, source))
	}
	if information, err := os.Stat(destination); err == nil && information.IsDir() {
		return fmt.Errorf("%w: %s", ErrDestination, transfer.Destination)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	return copyTree(source, destination)
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
