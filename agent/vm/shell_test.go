package vm

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var awkward = []string{
	"plain",
	"with space",
	"single'quote",
	"''",
	"'",
	`double"quote`,
	"$HOME",
	"${PATH}",
	"$(id)",
	"`id`",
	"a;b",
	"a && b",
	"a || b",
	"a | b",
	"line\nbreak",
	"trailing\n",
	"",
	"tab\there",
	`back\slash`,
	`\'`,
	"*",
	"~",
	"!",
	"-n",
	"--",
	"%s",
	"> out",
	"é ünïcode ✓",
}

func decode(t *testing.T, output []byte) []string {
	t.Helper()
	if len(output) == 0 || output[len(output)-1] != 0 {
		t.Fatalf("output %q does not end with NUL", output)
	}
	return strings.Split(string(output[:len(output)-1]), "\x00")
}

func TestQuoteDecodesBack(t *testing.T) {
	for _, word := range awkward {
		t.Run(strconv.Quote(word), func(t *testing.T) {
			line := `sh -c 'printf "%s\0" "$@"' _ ` + quote(word)
			output, err := exec.Command("/bin/sh", "-c", line).Output()
			if err != nil {
				t.Fatalf("sh: %v", err)
			}
			if got := decode(t, output); !slices.Equal(got, []string{word}) {
				t.Fatalf("decoded %q, want %q", got, word)
			}
		})
	}
}

func TestQuoteAllDecodesBackAsOneArgv(t *testing.T) {
	line := `sh -c 'printf "%s\0" "$@"' _ ` + quoteAll(awkward)
	output, err := exec.Command("/bin/sh", "-c", line).Output()
	if err != nil {
		t.Fatalf("sh: %v", err)
	}
	if got := decode(t, output); !slices.Equal(got, awkward) {
		t.Fatalf("decoded %q, want %q", got, awkward)
	}
}

func TestExecScriptRunsArgvWithEnvironment(t *testing.T) {
	group := filepath.Join(t.TempDir(), "group")
	env := map[string]string{
		"VALUE": "it's $HOME `id` \"quoted\"\nnext line",
		"EMPTY": "",
	}
	args := append([]string{"/bin/sh", "-c", `printf '%s\0' "$VALUE" "${EMPTY-unset}" "$@"`, "_"}, awkward...)

	command := exec.Command("/bin/sh", "-s")
	command.Stdin = strings.NewReader(execScript(group, env, args))
	output, err := command.Output()
	if err != nil {
		t.Fatalf("script: %v", err)
	}

	want := append([]string{env["VALUE"], ""}, awkward...)
	if got := decode(t, output); !slices.Equal(got, want) {
		t.Fatalf("decoded %q, want %q", got, want)
	}
	recorded, err := os.ReadFile(group)
	if err != nil {
		t.Fatalf("group file: %v", err)
	}
	if _, err := strconv.Atoi(strings.TrimSpace(string(recorded))); err != nil {
		t.Fatalf("group file holds %q, want a process group id", recorded)
	}
}

func TestExecScriptReportsCommandExitCode(t *testing.T) {
	command := exec.Command("/bin/sh", "-s")
	command.Stdin = strings.NewReader(execScript(filepath.Join(t.TempDir(), "group"), nil, []string{"/bin/sh", "-c", "exit 42"}))
	err := command.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 42 {
		t.Fatalf("got %v, want exit status 42", err)
	}
}

// TestKillScriptStopsProcessGroup reproduces sshd's session (a new session per command) and checks that the kill
// script ends the command and the children it started.
func TestKillScriptStopsProcessGroup(t *testing.T) {
	group := filepath.Join(t.TempDir(), "group")
	command := exec.Command("/bin/sh", "-s")
	command.Stdin = strings.NewReader(execScript(group, nil, []string{"/bin/sh", "-c", "sleep 60 & sleep 60; wait"}))
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })

	var identifier int
	deadline := time.Now().Add(5 * time.Second)
	for {
		recorded, _ := os.ReadFile(group)
		if value, err := strconv.Atoi(strings.TrimSpace(string(recorded))); err == nil {
			identifier = value
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command never recorded its process group")
		}
		time.Sleep(10 * time.Millisecond)
	}

	kill := exec.Command("/bin/sh", "-s")
	kill.Stdin = strings.NewReader(killScript(group))
	if output, err := kill.CombinedOutput(); err != nil {
		t.Fatalf("kill script: %v: %s", err, output)
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the command survived the kill script")
	}
	deadline = time.Now().Add(5 * time.Second)
	for syscall.Kill(-identifier, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatal("a child in the process group survived the kill script")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestKillScriptToleratesMissingGroupFile(t *testing.T) {
	kill := exec.Command("/bin/sh", "-s")
	kill.Stdin = strings.NewReader(killScript(filepath.Join(t.TempDir(), "missing")))
	var output bytes.Buffer
	kill.Stdout, kill.Stderr = &output, &output
	if err := kill.Run(); err != nil {
		t.Fatalf("kill script: %v: %s", err, output.String())
	}
}

func TestPrepareScript(t *testing.T) {
	home := t.TempDir()
	parent := filepath.Join(home, "it's", "input")
	destination := filepath.Join(parent, "signing")

	run := func() int {
		command := exec.Command("/bin/sh", "-s")
		command.Stdin = strings.NewReader(prepareScript(parent, destination))
		err := command.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		return 0
	}

	if code := run(); code != 0 {
		t.Fatalf("first prepare exited %d", code)
	}
	if information, err := os.Stat(parent); err != nil || !information.IsDir() {
		t.Fatalf("parent not created: %v", err)
	}
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	if code := run(); code != prepareExists {
		t.Fatalf("prepare over a directory exited %d, want %d", code, prepareExists)
	}
}
