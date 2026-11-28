package vm

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func program(t *testing.T, name string) string {
	t.Helper()
	found, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not found: %v", name, err)
	}
	return found
}

func TestSystemPassesArgumentsLiterally(t *testing.T) {
	var output bytes.Buffer
	literal := "$HOME `id` $(id) ; && 'quoted' \"double\""
	handle, err := System{}.Start(Process{Path: program(t, "printf"), Arguments: []string{"%s", literal}, Stdout: &output})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := handle.Wait(); code != 0 || err != nil {
		t.Fatalf("wait = %d, %v", code, err)
	}
	if output.String() != literal {
		t.Fatalf("output %q, want %q", output.String(), literal)
	}
}

func TestSystemPassesAMinimalEnvironment(t *testing.T) {
	t.Setenv("DISTRIBUTION_AGENT_SECRET", "enrolment-secret-sentinel")
	t.Setenv("HOME", "/home/agent")
	var output bytes.Buffer
	handle, err := System{}.Start(Process{Path: program(t, "env"), Environment: []string{"VM_TEST_VALUE=x y"}, Stdout: &output})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Wait(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	for _, want := range []string{"VM_TEST_VALUE=x y", "HOME=/home/agent", "LUME_TELEMETRY_ENABLED=false", "PATH=" + os.Getenv("PATH")} {
		if !slices.Contains(lines, want) {
			t.Errorf("environment %q lacks %q", lines, want)
		}
	}
	if strings.Contains(output.String(), "enrolment-secret-sentinel") {
		t.Errorf("the agent's own environment reached a child: %q", lines)
	}
}

func TestSystemReportsExitCodes(t *testing.T) {
	handle, err := System{}.Start(Process{Path: program(t, "false")})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := handle.Wait(); code != 1 || err != nil {
		t.Fatalf("wait = %d, %v; want 1, nil", code, err)
	}
	if err := handle.Kill(); err != nil {
		t.Fatalf("kill after exit: %v", err)
	}
}

func TestSystemKill(t *testing.T) {
	handle, err := System{}.Start(Process{Path: program(t, "sleep"), Arguments: []string{"60"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Kill(); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := handle.Wait()
		done <- code
	}()
	select {
	case code := <-done:
		if code != -1 {
			t.Fatalf("killed process exited %d, want -1", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kill did not stop the process")
	}
}

func TestSystemStartFailure(t *testing.T) {
	if _, err := (System{}).Start(Process{Path: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("start of a missing program succeeded")
	}
}
