package vm

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"time"
)

// Process is one host program started from an argv, never through a shell.
type Process struct {
	Path string
	Args []string
	// Env is appended to the agent's own environment.
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Runner starts host processes. Lume uses System; tests inject a recorder.
type Runner interface {
	Start(process Process) (Handle, error)
}

// Handle is a started process. Wait is called once; Kill may be called at any time, also after the process exited.
type Handle interface {
	Wait() (int, error)
	Kill() error
}

// System runs processes with os/exec.
type System struct{}

const pipeDelay = 5 * time.Second

func (System) Start(process Process) (Handle, error) {
	command := exec.Command(process.Path, process.Args...)
	command.Env = append(os.Environ(), process.Env...)
	command.Stdin = process.Stdin
	command.Stdout = process.Stdout
	command.Stderr = process.Stderr
	command.WaitDelay = pipeDelay
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &systemHandle{command: command}, nil
}

type systemHandle struct {
	command *exec.Cmd
}

// Wait returns the exit code. A process ended by a signal reports -1.
func (h *systemHandle) Wait() (int, error) {
	err := h.command.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return h.command.ProcessState.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

func (h *systemHandle) Kill() error {
	err := h.command.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
