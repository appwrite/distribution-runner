// Package vm runs one build in a throwaway virtual machine: create it from a golden image, copy files in, execute
// the fixed build script, copy the results out and destroy it.
package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Layer is the VM backend the lease runner drives. Implementations are safe for concurrent use.
type Layer interface {
	// Create clones and boots a machine. On failure it removes whatever it made and returns a zero Machine.
	Create(ctx context.Context, name string) (Machine, error)
	// Exec runs Arguments inside the machine and returns its exit code. A non-zero exit is not an error; the error
	// reports a failure to run the command at all, or ctx ending, which also stops the command.
	Exec(ctx context.Context, machine Machine, command Command) (int, error)
	// Copy moves a file or directory into the machine, or one regular file out of it.
	Copy(ctx context.Context, machine Machine, transfer Transfer) error
	// Destroy stops and deletes the machine. It succeeds when the machine is already gone, and on a zero Machine.
	Destroy(ctx context.Context, machine Machine) error
	// Sweep destroys every machine whose name starts with prefix that this Layer did not create, with its host
	// files: what an earlier agent process left behind when it crashed or was killed.
	Sweep(ctx context.Context, prefix string) error
}

// Machine identifies a created VM.
type Machine struct {
	Name string
	// Address is where the host reaches the machine: the guest IP for Lume, the root directory for Fake.
	Address string
	// Home is the absolute home directory inside the machine. Relative machine paths resolve against it.
	Home string
}

// Command is run inside the machine. Environment is added to the guest login environment.
type Command struct {
	Arguments   []string
	Environment map[string]string
	Stdout      io.Writer
	Stderr      io.Writer
}

type Direction int

const (
	In Direction = iota + 1
	Out
)

func (d Direction) String() string {
	switch d {
	case In:
		return "in"
	case Out:
		return "out"
	}
	return fmt.Sprintf("Direction(%d)", int(d))
}

// Transfer copies Source to Destination. In copies a file or directory from the host to the machine, Out copies one
// regular file of at most Limit bytes from the machine into a new host file of mode 0600. Destination names the copy
// itself and its parent directories are created; an In destination must not be an existing directory, and an Out
// destination must not exist at all.
type Transfer struct {
	Direction   Direction
	Source      string
	Destination string
	Limit       int64
}

// ExitError carries a command's exit code. Fake's Build returns it to choose the code Exec reports.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("exit status %d", e.Code)
}

var (
	ErrNotFound    = errors.New("vm: machine not found")
	ErrExists      = errors.New("vm: machine already exists")
	ErrDestination = errors.New("vm: destination already exists")
	ErrNotFile     = errors.New("vm: source is not a regular file")
	ErrLimit       = errors.New("vm: copy exceeds its limit")
)

// validateName accepts names that cannot be mistaken for a command-line option: a letter or digit, then letters,
// digits, '.', '_' or '-'.
func validateName(name string) error {
	if name == "" {
		return errors.New("vm: empty machine name")
	}
	for index, character := range name {
		switch {
		case 'a' <= character && character <= 'z', 'A' <= character && character <= 'Z', '0' <= character && character <= '9':
		case index > 0 && (character == '.' || character == '_' || character == '-'):
		default:
			return fmt.Errorf("vm: invalid machine name %q", name)
		}
	}
	return nil
}

func validateTransfer(transfer Transfer) error {
	if transfer.Direction != In && transfer.Direction != Out {
		return fmt.Errorf("vm: invalid transfer direction %s", transfer.Direction)
	}
	if transfer.Source == "" || transfer.Destination == "" {
		return errors.New("vm: transfer needs a source and a destination")
	}
	if strings.ContainsRune(transfer.Source, 0) || strings.ContainsRune(transfer.Destination, 0) {
		return errors.New("vm: transfer path contains NUL")
	}
	if transfer.Direction == Out && transfer.Limit <= 0 {
		return errors.New("vm: a copy out needs a positive limit")
	}
	return nil
}

// validateEnvironment accepts POSIX shell identifiers as keys, so every key can be exported by the guest shell.
func validateEnvironment(environment map[string]string) error {
	for key, value := range environment {
		if !identifier(key) {
			return fmt.Errorf("vm: invalid environment name %q", key)
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("vm: environment value for %s contains NUL", key)
		}
	}
	return nil
}

func identifier(name string) bool {
	if name == "" {
		return false
	}
	for index, character := range name {
		switch {
		case character == '_', 'a' <= character && character <= 'z', 'A' <= character && character <= 'Z':
		case index > 0 && '0' <= character && character <= '9':
		default:
			return false
		}
	}
	return true
}

func validateArguments(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("vm: command has no arguments")
	}
	for _, argument := range arguments {
		if strings.ContainsRune(argument, 0) {
			return errors.New("vm: command argument contains NUL")
		}
	}
	return nil
}

// createExclusive creates destination, which must not exist, as a private file, with its parent directories.
func createExclusive(destination string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("%w: %s", ErrDestination, destination)
	}
	return file, err
}

// limitWriter writes at most limit bytes to writer and fails with ErrLimit, writing nothing, once a write would
// pass it.
type limitWriter struct {
	writer  io.Writer
	limit   int64
	written int64
	over    bool
}

func (w *limitWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.limit-w.written {
		w.over = true
		return 0, ErrLimit
	}
	count, err := w.writer.Write(data)
	w.written += int64(count)
	return count, err
}
