// Package vm runs one build in a throwaway virtual machine: create it from a golden image, copy files in, execute
// the fixed build script, copy the results out and destroy it.
package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Layer is the VM backend the lease runner drives. Implementations are safe for concurrent use.
type Layer interface {
	// Create clones and boots a machine. On failure it removes whatever it made and returns a zero Machine.
	Create(ctx context.Context, name string) (Machine, error)
	// Exec runs Args inside the machine and returns its exit code. A non-zero exit is not an error; the error reports
	// a failure to run the command at all, or ctx ending, which also stops the command.
	Exec(ctx context.Context, machine Machine, command Command) (int, error)
	// Copy moves a file or directory between the host and the machine.
	Copy(ctx context.Context, machine Machine, transfer Transfer) error
	// Destroy stops and deletes the machine. It succeeds when the machine is already gone, and on a zero Machine.
	Destroy(ctx context.Context, machine Machine) error
}

// Machine identifies a created VM.
type Machine struct {
	Name string
	// Address is where the host reaches the machine: the guest IP for Lume, the root directory for Fake.
	Address string
	// Home is the absolute home directory inside the machine. Relative machine paths resolve against it.
	Home string
}

// Command is run inside the machine. Env is added to the guest login environment.
type Command struct {
	Args   []string
	Env    map[string]string
	Stdout io.Writer
	Stderr io.Writer
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

// Transfer copies Source to Destination. In copies from the host to the machine, Out the reverse. Destination names
// the copy itself, its parent directories are created, and it must not be an existing directory.
type Transfer struct {
	Direction   Direction
	Source      string
	Destination string
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
	ErrDestination = errors.New("vm: destination is an existing directory")
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
	return nil
}

// validateEnvironment accepts POSIX shell identifiers as keys, so every key can be exported by the guest shell.
func validateEnvironment(env map[string]string) error {
	for key, value := range env {
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

func validateArgs(args []string) error {
	if len(args) == 0 {
		return errors.New("vm: command has no arguments")
	}
	for _, argument := range args {
		if strings.ContainsRune(argument, 0) {
			return errors.New("vm: command argument contains NUL")
		}
	}
	return nil
}
