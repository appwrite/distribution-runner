package vm

import (
	"bytes"
	"os/exec"
	"path/filepath"
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
	handle, err := System{}.Start(Process{Path: program(t, "printf"), Args: []string{"%s", literal}, Stdout: &output})
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

func TestSystemAppendsEnvironment(t *testing.T) {
	var output bytes.Buffer
	handle, err := System{}.Start(Process{Path: program(t, "env"), Env: []string{"VM_TEST_VALUE=x y"}, Stdout: &output})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Wait(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "\nVM_TEST_VALUE=x y\n") || !strings.Contains(output.String(), "PATH=") {
		t.Fatalf("environment %q lacks the added value or the inherited one", output.String())
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
	handle, err := System{}.Start(Process{Path: program(t, "sleep"), Args: []string{"60"}})
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
