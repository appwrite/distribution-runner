package vm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func createFake(t *testing.T, fake *Fake, name string) Machine {
	t.Helper()
	machine, err := fake.Create(context.Background(), name)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = fake.Destroy(context.Background(), machine) })
	return machine
}

func writeFile(t *testing.T, name string, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestFakeRunsTheVMContract(t *testing.T) {
	ctx := context.Background()
	host := t.TempDir()
	writeFile(t, filepath.Join(host, "source.tar.gz"), "source", 0o644)
	writeFile(t, filepath.Join(host, "signing", "password"), "secret", 0o600)
	writeFile(t, filepath.Join(host, "signing", "profiles", "com.example.app.mobileprovision"), "profile", 0o600)

	var seen map[string]string
	fake := &Fake{Directory: t.TempDir(), Build: func(_ context.Context, work string, environment map[string]string) error {
		seen = environment
		if got := readFile(t, filepath.Join(work, "input", "source.tar.gz")); got != "source" {
			t.Errorf("source = %q", got)
		}
		if got := readFile(t, filepath.Join(work, "input", "signing", "profiles", "com.example.app.mobileprovision")); got != "profile" {
			t.Errorf("profile = %q", got)
		}
		writeFile(t, filepath.Join(work, "output", "app.ipa"), "ipa", 0o644)
		writeFile(t, filepath.Join(work, "output", "result.json"), "{}", 0o644)
		return nil
	}}
	machine := createFake(t, fake, "lease-1")
	if machine.Home != "/Users/lume" || machine.Address == "" {
		t.Fatalf("machine = %+v", machine)
	}
	work := machine.Home + "/distribution"

	transfers := []Transfer{
		{Direction: In, Source: filepath.Join(host, "source.tar.gz"), Destination: work + "/input/source.tar.gz"},
		{Direction: In, Source: filepath.Join(host, "signing"), Destination: "distribution/input/signing"},
	}
	for _, transfer := range transfers {
		if err := fake.Copy(ctx, machine, transfer); err != nil {
			t.Fatalf("copy %+v: %v", transfer, err)
		}
	}
	information, err := os.Stat(filepath.Join(machine.Address, "Users", "lume", "distribution", "input", "signing", "password"))
	if err != nil || information.Mode().Perm() != 0o600 {
		t.Fatalf("password mode = %v, %v; want 0600", information, err)
	}

	env := map[string]string{"DISTRIBUTION_WORK": work}
	code, err := fake.Exec(ctx, machine, Command{Arguments: []string{"/usr/local/distribution/build.sh"}, Environment: env})
	if err != nil || code != 0 {
		t.Fatalf("exec = %d, %v", code, err)
	}
	if seen["DISTRIBUTION_WORK"] != work {
		t.Fatalf("build env = %v", seen)
	}

	out := t.TempDir()
	for _, name := range []string{"app.ipa", "result.json"} {
		transfer := Transfer{Direction: Out, Source: work + "/output/" + name, Destination: filepath.Join(out, "nested", name), Limit: 1 << 20}
		if err := fake.Copy(ctx, machine, transfer); err != nil {
			t.Fatalf("copy out %s: %v", name, err)
		}
	}
	if got := readFile(t, filepath.Join(out, "nested", "app.ipa")); got != "ipa" {
		t.Fatalf("app.ipa = %q", got)
	}
	missing := Transfer{Direction: Out, Source: work + "/output/dsyms.zip", Destination: filepath.Join(out, "dsyms.zip"), Limit: 1 << 20}
	if err := fake.Copy(ctx, machine, missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copy of a missing file = %v, want not exist", err)
	}

	if err := fake.Destroy(ctx, machine); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, err := os.Stat(machine.Address); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("machine directory survived destroy: %v", err)
	}

	var operations []Operation
	for _, call := range fake.Calls() {
		operations = append(operations, call.Operation)
	}
	want := []Operation{OperationCreate, OperationCopy, OperationCopy, OperationExec, OperationCopy, OperationCopy, OperationCopy, OperationDestroy}
	if !slices.Equal(operations, want) {
		t.Fatalf("operations = %v, want %v", operations, want)
	}
	exec := fake.Calls()[3]
	if !slices.Equal(exec.Arguments, []string{"/usr/local/distribution/build.sh"}) || exec.Environment["DISTRIBUTION_WORK"] != work {
		t.Fatalf("recorded exec = %+v", exec)
	}
}

func TestFakeBuildDefaultsToHome(t *testing.T) {
	var got string
	fake := &Fake{Directory: t.TempDir(), Home: "/home/builder", Build: func(_ context.Context, work string, _ map[string]string) error {
		got = work
		return nil
	}}
	machine := createFake(t, fake, "home")
	if _, err := fake.Exec(context.Background(), machine, Command{Arguments: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(machine.Address, "home", "builder"); got != want {
		t.Fatalf("work = %q, want %q", got, want)
	}
}

func TestFakeExitCodes(t *testing.T) {
	tests := []struct {
		name   string
		build  func(context.Context, string, map[string]string) error
		code   int
		stderr string
	}{
		{name: "no build", code: 0},
		{name: "success", build: func(context.Context, string, map[string]string) error { return nil }, code: 0},
		{name: "exit error", build: func(context.Context, string, map[string]string) error { return &ExitError{Code: 3} }, code: 3, stderr: "exit status 3\n"},
		{name: "failure", build: func(context.Context, string, map[string]string) error { return errors.New("archive failed") }, code: 1, stderr: "archive failed\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &Fake{Directory: t.TempDir(), Build: test.build}
			machine := createFake(t, fake, "exit")
			var stderr bytes.Buffer
			code, err := fake.Exec(context.Background(), machine, Command{Arguments: []string{"build"}, Stderr: &stderr})
			if err != nil || code != test.code || stderr.String() != test.stderr {
				t.Fatalf("exec = %d, %v, stderr %q; want %d, %q", code, err, stderr.String(), test.code, test.stderr)
			}
		})
	}
}

func TestFakeBuildPanicPropagates(t *testing.T) {
	fake := &Fake{Directory: t.TempDir(), Build: func(context.Context, string, map[string]string) error { panic("boom") }}
	machine := createFake(t, fake, "panic")
	defer func() {
		if recovered := recover(); recovered != "boom" {
			t.Fatalf("recovered %v, want boom", recovered)
		}
	}()
	_, _ = fake.Exec(context.Background(), machine, Command{Arguments: []string{"build"}})
	t.Fatal("exec returned after a panic")
}

func TestFakeExecHonoursCancellation(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	fake := &Fake{Directory: t.TempDir(), Build: func(ctx context.Context, _ string, _ map[string]string) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}
	machine := createFake(t, fake, "cancel")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	code, err := fake.Exec(ctx, machine, Command{Arguments: []string{"build"}})
	if code != -1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exec = %d, %v; want -1, deadline exceeded", code, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("exec took %v after cancellation", elapsed)
	}

	cancelled, stop := context.WithCancel(context.Background())
	stop()
	ran := false
	fake.Build = func(context.Context, string, map[string]string) error { ran = true; return nil }
	if _, err := fake.Exec(cancelled, machine, Command{Arguments: []string{"build"}}); !errors.Is(err, context.Canceled) || ran {
		t.Fatalf("exec on a cancelled context = %v, ran %v", err, ran)
	}
}

func TestFakeFailInjection(t *testing.T) {
	injected := errors.New("injected")
	fake := &Fake{Directory: t.TempDir(), Fail: func(call Call) error {
		if call.Operation == OperationCopy && call.Transfer.Direction == Out {
			return injected
		}
		return nil
	}}
	machine := createFake(t, fake, "fail")
	source := filepath.Join(t.TempDir(), "file")
	writeFile(t, source, "x", 0o600)

	if err := fake.Copy(context.Background(), machine, Transfer{Direction: In, Source: source, Destination: "file"}); err != nil {
		t.Fatalf("copy in: %v", err)
	}
	err := fake.Copy(context.Background(), machine, Transfer{Direction: Out, Source: "file", Destination: filepath.Join(t.TempDir(), "file"), Limit: 1 << 20})
	if !errors.Is(err, injected) {
		t.Fatalf("copy out = %v, want injected", err)
	}
	calls := fake.Calls()
	if last := calls[len(calls)-1]; last.Operation != OperationCopy || last.Transfer.Direction != Out {
		t.Fatalf("failed call not recorded: %+v", last)
	}

	fake.Fail = func(call Call) error {
		if call.Operation == OperationCreate {
			return injected
		}
		return nil
	}
	if _, err := fake.Create(context.Background(), "refused"); !errors.Is(err, injected) {
		t.Fatalf("create = %v, want injected", err)
	}
	if live := fake.Live(); !slices.Equal(live, []string{"fail"}) {
		t.Fatalf("live = %v", live)
	}
}

func TestFakeDestroyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	fake := &Fake{Directory: t.TempDir()}
	machine := createFake(t, fake, "twice")
	for attempt := range 3 {
		if err := fake.Destroy(ctx, machine); err != nil {
			t.Fatalf("destroy %d: %v", attempt, err)
		}
	}
	if err := fake.Destroy(ctx, Machine{}); err != nil {
		t.Fatalf("destroy of a zero machine: %v", err)
	}
	if len(fake.Live()) != 0 {
		t.Fatalf("live = %v", fake.Live())
	}
	if _, err := fake.Exec(ctx, machine, Command{Arguments: []string{"true"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("exec after destroy = %v, want not found", err)
	}
	if err := fake.Copy(ctx, machine, Transfer{Direction: Out, Source: "a", Destination: filepath.Join(t.TempDir(), "a"), Limit: 1 << 20}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("copy after destroy = %v, want not found", err)
	}
	again, err := fake.Create(ctx, "twice")
	if err != nil {
		t.Fatalf("create after destroy: %v", err)
	}
	if err := fake.Destroy(ctx, again); err != nil {
		t.Fatal(err)
	}
}

func TestFakeRejects(t *testing.T) {
	ctx := context.Background()
	fake := &Fake{Directory: t.TempDir()}
	machine := createFake(t, fake, "rejects")

	if _, err := fake.Create(ctx, "rejects"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate create = %v, want exists", err)
	}
	for _, name := range []string{"", "-oProxyCommand=x", "a b", "a/b", ".hidden"} {
		if _, err := fake.Create(ctx, name); err == nil {
			t.Fatalf("create %q succeeded", name)
		}
	}
	if _, err := fake.Exec(ctx, machine, Command{}); err == nil {
		t.Fatal("exec without arguments succeeded")
	}
	if _, err := fake.Exec(ctx, machine, Command{Arguments: []string{"x"}, Environment: map[string]string{"A-B": "x"}}); err == nil {
		t.Fatal("exec with an invalid environment name succeeded")
	}
	if err := fake.Copy(ctx, machine, Transfer{Source: "a", Destination: "b"}); err == nil {
		t.Fatal("copy without a direction succeeded")
	}

	source := filepath.Join(t.TempDir(), "directory")
	writeFile(t, filepath.Join(source, "file"), "x", 0o600)
	if err := fake.Copy(ctx, machine, Transfer{Direction: In, Source: source, Destination: "directory"}); err != nil {
		t.Fatal(err)
	}
	if err := fake.Copy(ctx, machine, Transfer{Direction: In, Source: source, Destination: "directory"}); !errors.Is(err, ErrDestination) {
		t.Fatalf("copy onto a directory = %v, want destination error", err)
	}
	if err := fake.Copy(ctx, machine, Transfer{Direction: Out, Source: "directory", Destination: filepath.Join(t.TempDir(), "out"), Limit: 1 << 20}); !errors.Is(err, ErrNotFile) {
		t.Fatalf("copy out of a directory = %v, want not a file", err)
	}
	if err := fake.Copy(ctx, machine, Transfer{Direction: Out, Source: "directory/file", Destination: t.TempDir(), Limit: 1 << 20}); !errors.Is(err, ErrDestination) {
		t.Fatalf("copy out onto a directory = %v, want destination error", err)
	}
	if err := fake.Copy(ctx, machine, Transfer{Direction: Out, Source: "directory/file", Destination: filepath.Join(t.TempDir(), "small"), Limit: 0}); err == nil {
		t.Fatal("copy out without a limit succeeded")
	}
	small := filepath.Join(t.TempDir(), "small")
	if err := fake.Copy(ctx, machine, Transfer{Direction: Out, Source: "directory/file", Destination: small, Limit: 1}); err != nil {
		t.Fatalf("copy out at the limit: %v", err)
	}
	link := filepath.Join(host(machine, machine.Home), "link")
	if err := os.Symlink("directory/file", link); err != nil {
		t.Fatal(err)
	}
	if err := fake.Copy(ctx, machine, Transfer{Direction: Out, Source: "link", Destination: filepath.Join(t.TempDir(), "link"), Limit: 1 << 20}); !errors.Is(err, ErrNotFile) {
		t.Fatalf("copy out of a symlink = %v, want not a file", err)
	}
	writeFile(t, filepath.Join(host(machine, machine.Home), "large"), "too large", 0o600)
	large := filepath.Join(t.TempDir(), "large")
	if err := fake.Copy(ctx, machine, Transfer{Direction: Out, Source: "large", Destination: large, Limit: 4}); !errors.Is(err, ErrLimit) {
		t.Fatalf("copy out past the limit = %v, want the limit", err)
	}
	if _, err := os.Lstat(large); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused copy left its destination: %v", err)
	}
}

func TestFakeSweep(t *testing.T) {
	fake := &Fake{Directory: t.TempDir()}
	live := createFake(t, fake, "distribution-live")
	stale := filepath.Join(fake.Directory, fakePrefix+"distribution-old-1")
	other := filepath.Join(fake.Directory, fakePrefix+"other-1")
	for _, directory := range []string{stale, other} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := fake.Sweep(context.Background(), "distribution-"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale machine survived: %v", err)
	}
	for _, directory := range []string{other, live.Address} {
		if _, err := os.Stat(directory); err != nil {
			t.Fatalf("%s was swept: %v", directory, err)
		}
	}
}

func TestFakePathsStayInsideTheMachine(t *testing.T) {
	fake := &Fake{Directory: t.TempDir()}
	machine := createFake(t, fake, "escape")
	source := filepath.Join(t.TempDir(), "file")
	writeFile(t, source, "x", 0o600)

	transfer := Transfer{Direction: In, Source: source, Destination: "../../../../../../escaped"}
	if err := fake.Copy(context.Background(), machine, transfer); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(machine.Address, "escaped")); got != "x" {
		t.Fatalf("escaped = %q", got)
	}
	if !strings.HasPrefix(host(machine, "/../../etc/passwd"), machine.Address+string(filepath.Separator)) {
		t.Fatalf("host path left the machine: %s", host(machine, "/../../etc/passwd"))
	}
}
