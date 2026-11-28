package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testAddress = "192.168.64.5"
	testHome    = "/Users/lume"
	testKey     = "/etc/distribution/id_ed25519"
)

type started struct {
	Process
	stdin  string
	handle *recordedHandle
}

type behaviour struct {
	code   int
	stdout string
	block  bool
}

// recorder is a Runner that records every process and answers from respond instead of running anything.
type recorder struct {
	t       *testing.T
	respond func(process Process, stdin string) behaviour

	mutex     sync.Mutex
	processes []*started
}

func (r *recorder) Start(process Process) (Handle, error) {
	var stdin string
	if process.Stdin != nil {
		content, err := io.ReadAll(process.Stdin)
		if err != nil {
			r.t.Errorf("read stdin: %v", err)
		}
		stdin = string(content)
	}
	answer := r.respond(process, stdin)
	if answer.stdout != "" && process.Stdout != nil {
		_, _ = io.WriteString(process.Stdout, answer.stdout)
	}
	handle := &recordedHandle{done: make(chan struct{}), code: answer.code}
	if !answer.block {
		handle.finish()
	}
	r.mutex.Lock()
	r.processes = append(r.processes, &started{Process: process, stdin: stdin, handle: handle})
	r.mutex.Unlock()
	return handle, nil
}

func (r *recorder) all() []*started {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return slices.Clone(r.processes)
}

func (r *recorder) find(match func(*started) bool) []*started {
	var found []*started
	for _, process := range r.all() {
		if match(process) {
			found = append(found, process)
		}
	}
	return found
}

func lumeCall(subcommand string) func(*started) bool {
	return func(process *started) bool {
		return process.Path == "lume" && len(process.Arguments) > 0 && process.Arguments[0] == subcommand
	}
}

type recordedHandle struct {
	once   sync.Once
	done   chan struct{}
	code   int
	mutex  sync.Mutex
	killed bool
}

func (h *recordedHandle) finish() { h.once.Do(func() { close(h.done) }) }

func (h *recordedHandle) Wait() (int, error) {
	<-h.done
	h.mutex.Lock()
	defer h.mutex.Unlock()
	if h.killed {
		return -1, nil
	}
	return h.code, nil
}

func (h *recordedHandle) Kill() error {
	h.mutex.Lock()
	h.killed = true
	h.mutex.Unlock()
	h.finish()
	return nil
}

func (h *recordedHandle) wasKilled() bool {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	return h.killed
}

func detailsJSON(name string, address string) string {
	return fmt.Sprintf(`[{"name":%q,"os":"macOS","status":"running","ipAddress":%q,"sshAvailable":true,"locationName":"home"}]`, name, address)
}

// guest answers like a healthy lume host whose machines boot at testAddress.
func guest(overrides func(process Process, stdin string) (behaviour, bool)) func(Process, string) behaviour {
	return func(process Process, stdin string) behaviour {
		if overrides != nil {
			if answer, ok := overrides(process, stdin); ok {
				return answer
			}
		}
		switch process.Path {
		case "lume":
			switch process.Arguments[0] {
			case "run":
				return behaviour{block: true}
			case "get":
				return behaviour{stdout: detailsJSON(process.Arguments[1], testAddress)}
			case "ls":
				return behaviour{stdout: "[]"}
			}
			return behaviour{}
		case "/usr/bin/ssh":
			if stdin == homeScript {
				return behaviour{stdout: testHome}
			}
		}
		return behaviour{}
	}
}

func newLume(t *testing.T, respond func(Process, string) behaviour) (*Lume, *recorder) {
	t.Helper()
	runner := &recorder{t: t, respond: respond}
	return &Lume{
		Golden:    "golden",
		Key:       testKey,
		Directory: t.TempDir(),
		Poll:      5 * time.Millisecond,
		Boot:      2 * time.Second,
		Grace:     time.Second,
		Runner:    runner,
	}, runner
}

func createLume(t *testing.T, lume *Lume, name string) Machine {
	t.Helper()
	machine, err := lume.Create(context.Background(), name)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return machine
}

func TestLumeCreate(t *testing.T) {
	lume, runner := newLume(t, guest(nil))
	machine := createLume(t, lume, "lease-1")

	if machine != (Machine{Name: "lease-1", Address: testAddress, Home: testHome}) {
		t.Fatalf("machine = %+v", machine)
	}
	processes := runner.all()
	argv := func(index int) []string {
		return append([]string{processes[index].Path}, processes[index].Arguments...)
	}
	if len(processes) != 5 {
		t.Fatalf("started %d processes, want 5", len(processes))
	}
	directory := machineDirectory(t, lume)
	expected := [][]string{
		{"lume", "ls", "--format", "json"},
		{"lume", "clone", "golden", "lease-1"},
		{"lume", "run", "lease-1", "--display", "none", "--vnc", "disabled", "--shared-dir", filepath.Join(directory, Share) + ":ro"},
		{"lume", "get", "lease-1", "--format", "json"},
	}
	for index, want := range expected {
		if got := argv(index); !slices.Equal(got, want) {
			t.Fatalf("process %d = %q, want %q", index, got, want)
		}
		if !slices.Equal(processes[index].Environment, []string{"LUME_TELEMETRY_ENABLED=false"}) {
			t.Fatalf("process %d env = %q, want telemetry disabled", index, processes[index].Environment)
		}
	}
	probe := processes[4]
	if probe.Path != "/usr/bin/ssh" || probe.stdin != homeScript {
		t.Fatalf("probe = %q with stdin %q", argv(4), probe.stdin)
	}
	if tail := probe.Arguments[len(probe.Arguments)-4:]; !slices.Equal(tail, []string{"-T", "--", testAddress, "/bin/sh -s"}) {
		t.Fatalf("ssh destination = %q", tail)
	}
	if processes[2].handle.wasKilled() {
		t.Fatal("lume run was killed by a successful create")
	}
	if _, err := os.Stat(filepath.Join(directory, Share)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the host key share outlived the boot: %v", err)
	}
}

// machineDirectory returns the one per-machine directory under lume.Directory.
func machineDirectory(t *testing.T, lume *Lume) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(lume.Directory, machinePrefix+"*"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("per-machine directories = %v, %v", matches, err)
	}
	return matches[0]
}

func TestLumePinsAHostKeyPerMachine(t *testing.T) {
	var shared []string
	lume, runner := newLume(t, guest(func(process Process, _ string) (behaviour, bool) {
		if process.Path == "lume" && process.Arguments[0] == "run" {
			share := strings.TrimSuffix(process.Arguments[len(process.Arguments)-1], ":ro")
			private, _ := os.ReadFile(filepath.Join(share, HostKeyFile))
			public, _ := os.ReadFile(filepath.Join(share, HostKeyFile+".pub"))
			shared = append(shared, string(private), string(public))
			return behaviour{block: true}, true
		}
		return behaviour{}, false
	}))
	lume.User = "builder"
	createLume(t, lume, "first")
	directory := machineDirectory(t, lume)
	createLume(t, lume, "second")

	if len(shared) != 4 || !strings.Contains(shared[0], "BEGIN OPENSSH PRIVATE KEY") || shared[0] == shared[2] {
		t.Fatalf("shared keys = %d, want a distinct private key per machine", len(shared))
	}
	pinned, err := os.ReadFile(filepath.Join(directory, knownHosts))
	if err != nil {
		t.Fatal(err)
	}
	public := strings.Fields(shared[1])
	if string(pinned) != "first "+public[0]+" "+public[1]+"\n" {
		t.Fatalf("known_hosts = %q, want the shared public key under the machine name", pinned)
	}

	common := []string{
		"-F", "none",
		"-i", testKey,
		"-o", "User=builder",
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-o", "BatchMode=yes",
		"-o", "PasswordAuthentication=no",
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "ControlPath=none",
		"-o", "ForwardAgent=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "HostKeyAlgorithms=ssh-ed25519",
		"-o", "UserKnownHostsFile=" + filepath.Join(directory, knownHosts),
		"-o", "HostKeyAlias=first",
	}
	argv := runner.all()[4].Arguments
	for index := 0; index < len(common); index += 2 {
		pair := common[index : index+2]
		found := false
		for position := 0; position+1 < len(argv); position++ {
			found = found || (argv[position] == pair[0] && argv[position+1] == pair[1])
		}
		if !found {
			t.Fatalf("ssh argv %q lacks %q", argv, pair)
		}
	}
}

func TestHostKeyIsReadableByOpenSSH(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	key, err := newHostKey("machine")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), HostKeyFile)
	writeFile(t, path, string(key.Private), 0o600)

	output, err := exec.Command(keygen, "-y", "-f", path).Output()
	if err != nil {
		t.Fatalf("ssh-keygen -y: %v", err)
	}
	if derived := strings.Fields(string(output)); len(derived) < 2 || derived[0]+" "+derived[1] != key.Public {
		t.Fatalf("ssh-keygen derived %q, want %q", output, key.Public)
	}
}

func TestLumeExecKeepsArgumentsAndSecretsOutOfArgv(t *testing.T) {
	const (
		secret   = "sentinel-secret-value"
		argument = "sentinel-argument-value"
	)
	lume, runner := newLume(t, guest(func(process Process, stdin string) (behaviour, bool) {
		if process.Path == "/usr/bin/ssh" && strings.Contains(stdin, "exec ") {
			_, _ = io.WriteString(process.Stdout, "built\n")
			_, _ = io.WriteString(process.Stderr, "warning\n")
			return behaviour{code: 7}, true
		}
		return behaviour{}, false
	}))
	machine := createLume(t, lume, "secrets")

	var stdout, stderr strings.Builder
	env := map[string]string{"DISTRIBUTION_WORK": testHome + "/distribution", "TOKEN": secret}
	args := append([]string{"/usr/local/distribution/build.sh", argument}, awkward...)
	code, err := lume.Exec(context.Background(), machine, Command{Arguments: args, Environment: env, Stdout: &stdout, Stderr: &stderr})
	if err != nil || code != 7 {
		t.Fatalf("exec = %d, %v; want 7, nil", code, err)
	}
	if stdout.String() != "built\n" || stderr.String() != "warning\n" {
		t.Fatalf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}

	for _, process := range runner.all() {
		for _, value := range append(append([]string{process.Path}, process.Arguments...), process.Environment...) {
			if strings.Contains(value, secret) || strings.Contains(value, argument) {
				t.Fatalf("argv or env of %s carries a secret: %q", process.Path, value)
			}
		}
	}
	execs := runner.find(func(process *started) bool { return strings.Contains(process.stdin, "exec ") })
	if len(execs) != 1 {
		t.Fatalf("found %d exec scripts", len(execs))
	}
	if !strings.Contains(execs[0].stdin, quote("TOKEN="+secret)) || !strings.Contains(execs[0].stdin, quote(argument)) {
		t.Fatalf("exec script does not carry the quoted values:\n%s", execs[0].stdin)
	}
}

// TestLumeExecScriptDecodesBack runs the script the adapter sent to ssh through a local sh, standing in for the
// guest, and checks it reproduces the argv and environment exactly.
func TestLumeExecScriptDecodesBack(t *testing.T) {
	lume, runner := newLume(t, guest(nil))
	machine := createLume(t, lume, "decode")

	env := map[string]string{"VALUE": "it's $HOME `id`\n\"quoted\"", "DISTRIBUTION_WORK": "/Users/lume/distribution"}
	args := append([]string{"/bin/sh", "-c", `printf '%s\0' "$VALUE" "$DISTRIBUTION_WORK" "$@"`, "_"}, awkward...)
	if _, err := lume.Exec(context.Background(), machine, Command{Arguments: args, Environment: env}); err != nil {
		t.Fatal(err)
	}
	execs := runner.find(func(process *started) bool { return strings.Contains(process.stdin, "exec ") })
	if len(execs) != 1 {
		t.Fatalf("found %d exec scripts", len(execs))
	}
	script := strings.Replace(execs[0].stdin, "'/tmp/distribution-", "'"+t.TempDir()+"/distribution-", 1)

	command := exec.Command("/bin/sh", "-s")
	command.Stdin = strings.NewReader(script)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, script)
	}
	want := append([]string{env["VALUE"], env["DISTRIBUTION_WORK"]}, awkward...)
	if got := decode(t, output); !slices.Equal(got, want) {
		t.Fatalf("decoded %q, want %q", got, want)
	}
}

func TestLumeExecCancellationKillsTheRemoteCommand(t *testing.T) {
	lume, runner := newLume(t, guest(func(process Process, stdin string) (behaviour, bool) {
		if process.Path == "/usr/bin/ssh" && strings.Contains(stdin, "exec ") {
			return behaviour{block: true}, true
		}
		return behaviour{}, false
	}))
	machine := createLume(t, lume, "cancel")

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := lume.Exec(ctx, machine, Command{Arguments: []string{"sleep", "3600"}})
		result <- err
	}()
	var execution *started
	deadline := time.Now().Add(2 * time.Second)
	for execution == nil {
		if found := runner.find(func(process *started) bool { return strings.Contains(process.stdin, "exec ") }); len(found) == 1 {
			execution = found[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("exec never started ssh")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("exec = %v, want canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("exec did not return after cancellation")
	}
	if !execution.handle.wasKilled() {
		t.Fatal("the local ssh was not killed")
	}
	start := strings.Index(execution.stdin, ">'") + 2
	group := execution.stdin[start : start+strings.Index(execution.stdin[start:], "'")]
	if !strings.HasPrefix(group, "/tmp/distribution-") {
		t.Fatalf("group file = %q", group)
	}
	kills := runner.find(func(process *started) bool { return process.stdin == killScript(group) })
	if len(kills) != 1 || kills[0].Path != "/usr/bin/ssh" {
		t.Fatalf("found %d kill scripts for %s", len(kills), group)
	}
}

func TestLumeCopy(t *testing.T) {
	const content = "ipa bytes"
	source := testHome + "/distribution/output/app.ipa"
	lume, runner := newLume(t, guest(func(process Process, stdin string) (behaviour, bool) {
		if process.Path == "/usr/bin/ssh" && stdin == readScript(source) {
			return behaviour{stdout: content}, true
		}
		return behaviour{}, false
	}))
	machine := createLume(t, lume, "copy")
	ctx := context.Background()
	host := t.TempDir()
	writeFile(t, filepath.Join(host, "signing", "password"), "secret", 0o600)

	in := Transfer{Direction: In, Source: filepath.Join(host, "signing"), Destination: "distribution/input/signing"}
	if err := lume.Copy(ctx, machine, in); err != nil {
		t.Fatalf("copy in: %v", err)
	}
	destination := filepath.Join(host, "result", "app.ipa")
	if err := lume.Copy(ctx, machine, Transfer{Direction: Out, Source: source, Destination: destination, Limit: 1 << 20}); err != nil {
		t.Fatalf("copy out: %v", err)
	}
	information, err := os.Lstat(destination)
	if err != nil || !information.Mode().IsRegular() || information.Mode().Perm() != 0o600 || readFile(t, destination) != content {
		t.Fatalf("copied file = %v, %v", information, err)
	}

	prepare := runner.find(func(process *started) bool { return strings.HasPrefix(process.stdin, "set -e\nmkdir") })
	if len(prepare) != 1 || prepare[0].stdin != prepareScript(testHome+"/distribution/input", testHome+"/distribution/input/signing") {
		t.Fatalf("prepare scripts = %d", len(prepare))
	}
	copies := runner.find(func(process *started) bool { return process.Path == "/usr/bin/scp" })
	if len(copies) != 1 {
		t.Fatalf("found %d scp processes, want only the copy in", len(copies))
	}
	argv := copies[0].Arguments
	want := []string{"-q", "-p", "-r", "--", filepath.Join(host, "signing"), testAddress + ":" + testHome + "/distribution/input/signing"}
	if got := argv[len(argv)-len(want):]; !slices.Equal(got, want) {
		t.Fatalf("scp = %q, want suffix %q", argv, want)
	}
	if slices.Contains(argv, "-T") || slices.Contains(argv, "-O") || !slices.Contains(argv, "IdentitiesOnly=yes") {
		t.Fatalf("scp does not share the ssh options or weakens filename checking: %q", argv)
	}
	reads := runner.find(func(process *started) bool { return process.stdin == readScript(source) })
	if len(reads) != 1 || reads[0].Path != "/usr/bin/ssh" {
		t.Fatalf("found %d reads of %s over ssh", len(reads), source)
	}
}

func TestLumeCopyOutRefusesWhatIsNotABoundedFile(t *testing.T) {
	lume, _ := newLume(t, guest(func(process Process, stdin string) (behaviour, bool) {
		switch {
		case stdin == readScript(testHome+"/huge"):
			return behaviour{stdout: strings.Repeat("x", 2048)}, true
		case stdin == readScript(testHome+"/directory"):
			return behaviour{code: notFile}, true
		case stdin == readScript(testHome+"/broken"):
			_, _ = io.WriteString(process.Stderr, "cat: read error\n")
			return behaviour{code: 1}, true
		}
		return behaviour{}, false
	}))
	machine := createLume(t, lume, "bounded")
	host := t.TempDir()
	existing := filepath.Join(host, "existing")
	writeFile(t, existing, "kept", 0o600)

	cases := map[string]struct {
		source      string
		destination string
		want        error
		message     string
	}{
		"over the limit":       {source: "huge", destination: filepath.Join(host, "huge"), want: ErrLimit},
		"not a regular file":   {source: "directory", destination: filepath.Join(host, "directory"), want: ErrNotFile},
		"existing destination": {source: "file", destination: existing, want: ErrDestination},
		"existing directory":   {source: "file", destination: t.TempDir(), want: ErrDestination},
		"failing read":         {source: "broken", destination: filepath.Join(host, "broken"), message: "cat: read error"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			err := lume.Copy(context.Background(), machine, Transfer{Direction: Out, Source: test.source, Destination: test.destination, Limit: 1024})
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("copy = %v, want %v", err, test.want)
			}
			if test.message != "" && (err == nil || !strings.Contains(err.Error(), test.message)) {
				t.Fatalf("copy = %v, want %q", err, test.message)
			}
			if test.want != ErrDestination {
				if _, statErr := os.Lstat(test.destination); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("a refused copy left %s behind: %v", test.destination, statErr)
				}
			}
		})
	}
	if readFile(t, existing) != "kept" {
		t.Fatal("a refused copy overwrote an existing file")
	}
	if err := lume.Copy(context.Background(), machine, Transfer{Direction: Out, Source: "file", Destination: filepath.Join(host, "unbounded")}); err == nil {
		t.Fatal("a copy out without a limit was accepted")
	}
}

func TestLumeCopyRefusesDirectories(t *testing.T) {
	lume, _ := newLume(t, guest(func(process Process, stdin string) (behaviour, bool) {
		if strings.HasPrefix(stdin, "set -e\nmkdir") {
			return behaviour{code: prepareExists}, true
		}
		return behaviour{}, false
	}))
	machine := createLume(t, lume, "refuse")
	source := filepath.Join(t.TempDir(), "file")
	writeFile(t, source, "x", 0o600)

	if err := lume.Copy(context.Background(), machine, Transfer{Direction: In, Source: source, Destination: "exists"}); !errors.Is(err, ErrDestination) {
		t.Fatalf("copy in onto a directory = %v", err)
	}
	if err := lume.Copy(context.Background(), machine, Transfer{Direction: In, Source: filepath.Join(t.TempDir(), "missing"), Destination: "x"}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copy of a missing source = %v", err)
	}
}

func TestLumeCopyFailureReportsStderr(t *testing.T) {
	lume, _ := newLume(t, guest(func(process Process, _ string) (behaviour, bool) {
		if process.Path == "/usr/bin/scp" {
			_, _ = io.WriteString(process.Stderr, "scp: no such file\n")
			return behaviour{code: 1}, true
		}
		return behaviour{}, false
	}))
	machine := createLume(t, lume, "scp")
	source := filepath.Join(t.TempDir(), "file")
	writeFile(t, source, "x", 0o600)
	err := lume.Copy(context.Background(), machine, Transfer{Direction: In, Source: source, Destination: "file"})
	if err == nil || !strings.Contains(err.Error(), "scp: no such file") {
		t.Fatalf("copy = %v", err)
	}
}

func TestLumeDestroyIsIdempotent(t *testing.T) {
	present := false
	deleteCode := 0
	var mutex sync.Mutex
	lume, runner := newLume(t, guest(func(process Process, _ string) (behaviour, bool) {
		mutex.Lock()
		defer mutex.Unlock()
		if process.Path == "lume" && process.Arguments[0] == "delete" {
			return behaviour{code: deleteCode}, true
		}
		if process.Path == "lume" && process.Arguments[0] == "ls" && present {
			return behaviour{stdout: detailsJSON("gone", testAddress)}, true
		}
		return behaviour{}, false
	}))
	ctx := context.Background()
	machine := createLume(t, lume, "gone")
	run := runner.find(lumeCall("run"))[0]

	if err := lume.Destroy(ctx, machine); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if !run.handle.wasKilled() {
		t.Fatal("lume run was not killed")
	}
	deletes := runner.find(lumeCall("delete"))
	if len(deletes) != 1 || !slices.Equal(deletes[0].Arguments, []string{"delete", "gone", "--force"}) {
		t.Fatalf("deletes = %d", len(deletes))
	}
	if entries, _ := os.ReadDir(lume.Directory); len(entries) != 0 {
		t.Fatalf("per-machine directory survived: %v", entries)
	}
	if _, err := lume.Exec(ctx, machine, Command{Arguments: []string{"true"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("exec after destroy = %v", err)
	}

	mutex.Lock()
	deleteCode = 1
	mutex.Unlock()
	if err := lume.Destroy(ctx, machine); err != nil {
		t.Fatalf("second destroy: %v", err)
	}
	if stops := runner.find(lumeCall("stop")); len(stops) != 1 || !slices.Equal(stops[0].Arguments, []string{"stop", "gone", "--force"}) {
		t.Fatalf("stops = %d", len(stops))
	}

	mutex.Lock()
	present = true
	mutex.Unlock()
	if err := lume.Destroy(ctx, machine); err == nil {
		t.Fatal("destroy succeeded while lume still lists the machine")
	}

	before := len(runner.all())
	if err := lume.Destroy(ctx, Machine{}); err != nil || len(runner.all()) != before {
		t.Fatalf("destroy of a zero machine = %v and started processes", err)
	}
}

func TestLumeCreateCleansUpWhenBootTimesOut(t *testing.T) {
	lume, runner := newLume(t, guest(func(process Process, _ string) (behaviour, bool) {
		if process.Path == "lume" && process.Arguments[0] == "get" {
			return behaviour{stdout: `[{"name":"slow","status":"running","ipAddress":null,"sshAvailable":null}]`}, true
		}
		return behaviour{}, false
	}))
	lume.Boot = 50 * time.Millisecond

	machine, err := lume.Create(context.Background(), "slow")
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || machine != (Machine{}) {
		t.Fatalf("create = %+v, %v; want a boot timeout", machine, err)
	}
	if !runner.find(lumeCall("run"))[0].handle.wasKilled() {
		t.Fatal("lume run survived a failed create")
	}
	if len(runner.find(lumeCall("delete"))) != 1 {
		t.Fatal("the clone was not deleted")
	}
	if _, err := lume.Create(context.Background(), "slow"); err == nil || errors.Is(err, ErrExists) {
		t.Fatalf("the failed name stayed reserved: %v", err)
	}
}

func TestLumeCreateRetriesSSHUntilReady(t *testing.T) {
	attempts := 0
	lume, runner := newLume(t, guest(func(process Process, stdin string) (behaviour, bool) {
		if stdin == homeScript {
			attempts++
			if attempts < 3 {
				return behaviour{code: 255}, true
			}
		}
		return behaviour{}, false
	}))
	machine := createLume(t, lume, "retry")
	if machine.Home != testHome || len(runner.find(func(process *started) bool { return process.stdin == homeScript })) != 3 {
		t.Fatalf("machine = %+v after %d probes", machine, attempts)
	}
}

func TestLumeCreateRejectsInvalidAddress(t *testing.T) {
	for index, address := range []string{"-oProxyCommand=touch /tmp/owned", "0.0.0.0", "fe80::1%en0", "example.com"} {
		t.Run(fmt.Sprintf("address %d", index), func(t *testing.T) {
			lume, runner := newLume(t, guest(func(process Process, _ string) (behaviour, bool) {
				if process.Path == "lume" && process.Arguments[0] == "get" {
					return behaviour{stdout: detailsJSON("bad", address)}, true
				}
				return behaviour{}, false
			}))
			if _, err := lume.Create(context.Background(), "bad"); err == nil || !strings.Contains(err.Error(), "invalid address") {
				t.Fatalf("create = %v, want an invalid address error", err)
			}
			if ssh := runner.find(func(process *started) bool { return process.Path == "/usr/bin/ssh" }); len(ssh) != 0 {
				t.Fatalf("ssh ran against an invalid address: %q", ssh[0].Arguments)
			}
			if len(runner.find(lumeCall("delete"))) != 1 {
				t.Fatal("the clone was not deleted")
			}
		})
	}
}

func TestLumeCreateAcceptsIPv6(t *testing.T) {
	lume, runner := newLume(t, guest(func(process Process, _ string) (behaviour, bool) {
		if process.Path == "lume" && process.Arguments[0] == "get" {
			return behaviour{stdout: detailsJSON("six", "fd00::5")}, true
		}
		return behaviour{}, false
	}))
	machine := createLume(t, lume, "six")
	file := filepath.Join(t.TempDir(), "file")
	writeFile(t, file, "x", 0o600)
	if err := lume.Copy(context.Background(), machine, Transfer{Direction: In, Source: file, Destination: "file"}); err != nil {
		t.Fatal(err)
	}
	scp := runner.find(func(process *started) bool { return process.Path == "/usr/bin/scp" })[0]
	if target := scp.Arguments[len(scp.Arguments)-1]; target != "[fd00::5]:"+testHome+"/file" {
		t.Fatalf("scp target = %q", target)
	}
}

func TestLumeCreateFailsWhenRunExits(t *testing.T) {
	lume, runner := newLume(t, guest(func(process Process, _ string) (behaviour, bool) {
		if process.Path == "lume" && process.Arguments[0] == "run" {
			_, _ = io.WriteString(process.Stderr, "Virtualization error\n")
			return behaviour{code: 1}, true
		}
		if process.Path == "lume" && process.Arguments[0] == "get" {
			return behaviour{stdout: `[{"name":"dead","status":"stopped"}]`}, true
		}
		return behaviour{}, false
	}))
	_, err := lume.Create(context.Background(), "dead")
	if err == nil || !strings.Contains(err.Error(), "Virtualization error") {
		t.Fatalf("create = %v", err)
	}
	if len(runner.find(lumeCall("delete"))) != 1 {
		t.Fatal("the clone was not deleted")
	}
}

func TestLumeCreateLeavesAnExistingMachineAlone(t *testing.T) {
	lume, runner := newLume(t, guest(func(process Process, _ string) (behaviour, bool) {
		if process.Path == "lume" && process.Arguments[0] == "ls" {
			return behaviour{stdout: detailsJSON("taken", testAddress)}, true
		}
		return behaviour{}, false
	}))
	_, err := lume.Create(context.Background(), "taken")
	if !errors.Is(err, ErrExists) {
		t.Fatalf("create = %v, want exists", err)
	}
	if len(runner.all()) != 1 {
		t.Fatalf("started %d processes for an existing machine, want only the listing", len(runner.all()))
	}
	if entries, _ := os.ReadDir(lume.Directory); len(entries) != 0 {
		t.Fatalf("per-machine directory survived: %v", entries)
	}
	if _, err := lume.Create(context.Background(), "taken"); !errors.Is(err, ErrExists) {
		t.Fatalf("the refused name stayed reserved: %v", err)
	}
}

func TestLumeCloneFailureDeletesThePartialClone(t *testing.T) {
	lume, runner := newLume(t, guest(func(process Process, _ string) (behaviour, bool) {
		if process.Path == "lume" && process.Arguments[0] == "clone" {
			_, _ = io.WriteString(process.Stderr, "No space left on device\n")
			return behaviour{code: 1}, true
		}
		return behaviour{}, false
	}))
	_, err := lume.Create(context.Background(), "partial")
	if err == nil || !strings.Contains(err.Error(), "No space left") {
		t.Fatalf("create = %v", err)
	}
	if deletes := runner.find(lumeCall("delete")); len(deletes) != 1 || !slices.Equal(deletes[0].Arguments, []string{"delete", "partial", "--force"}) {
		t.Fatalf("deletes = %d, want the partial clone deleted", len(deletes))
	}
	if entries, _ := os.ReadDir(lume.Directory); len(entries) != 0 {
		t.Fatalf("per-machine directory survived: %v", entries)
	}
}

func TestLumeCreateRetriesATransientListing(t *testing.T) {
	gets := 0
	lume, _ := newLume(t, guest(func(process Process, _ string) (behaviour, bool) {
		if process.Path == "lume" && process.Arguments[0] == "get" {
			gets++
			if gets == 1 {
				return behaviour{code: 1}, true
			}
		}
		return behaviour{}, false
	}))
	if machine := createLume(t, lume, "flaky"); machine.Address != testAddress || gets < 2 {
		t.Fatalf("machine = %+v after %d listings", machine, gets)
	}
}

func TestLumeSweep(t *testing.T) {
	const (
		prefix  = "distribution-0123456789ab-"
		foreign = "distribution-ba9876543210-"
		golden  = "distribution-macos-27.0.1-xcode-27.2-r1"
	)
	var listing atomic.Value
	listing.Store("[]")
	lume, runner := newLume(t, guest(func(process Process, _ string) (behaviour, bool) {
		if process.Path == "lume" && process.Arguments[0] == "ls" {
			return behaviour{stdout: listing.Load().(string)}, true
		}
		return behaviour{}, false
	}))
	lume.Golden = golden
	live := createLume(t, lume, prefix+"live")
	listing.Store(`[{"name":"` + prefix + `old"},{"name":"` + golden + `"},{"name":"distribution-macos-27.0.1-xcode-27.2-r0"},` +
		`{"name":"` + foreign + `busy"},{"name":"` + prefix + `live"}]`)
	stale := filepath.Join(lume.Directory, machinePrefix+prefix+"old-123")
	kept := []string{
		filepath.Join(lume.Directory, machinePrefix+golden+"-456"),
		filepath.Join(lume.Directory, machinePrefix+foreign+"busy-789"),
	}
	for _, directory := range append([]string{stale}, kept...) {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := lume.Sweep(context.Background(), prefix); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	deletes := runner.find(lumeCall("delete"))
	if len(deletes) != 1 || !slices.Equal(deletes[0].Arguments, []string{"delete", prefix + "old", "--force"}) {
		t.Fatalf("deletes = %d, want only the stale machine with this agent's prefix", len(deletes))
	}
	if stops := runner.find(lumeCall("stop")); len(stops) != 1 || stops[0].Arguments[1] != prefix+"old" {
		t.Fatalf("stops = %d, want only the stale machine", len(stops))
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale directory survived: %v", err)
	}
	for _, directory := range kept {
		if _, err := os.Stat(directory); err != nil {
			t.Fatalf("%s was removed: %v", directory, err)
		}
	}
	if _, err := lume.Exec(context.Background(), live, Command{Arguments: []string{"true"}}); err != nil {
		t.Fatalf("the live machine did not survive the sweep: %v", err)
	}
	if err := lume.Sweep(context.Background(), "-x"); err == nil {
		t.Fatal("sweep accepted an option-like prefix")
	}
}

func TestLumeSweepNeverTouchesTheGoldenImage(t *testing.T) {
	lume, runner := newLume(t, guest(func(process Process, _ string) (behaviour, bool) {
		if process.Path == "lume" && process.Arguments[0] == "ls" {
			return behaviour{stdout: `[{"name":"distribution-macos-27.0.1-xcode-27.2-r1"},{"name":"distribution-old"}]`}, true
		}
		return behaviour{}, false
	}))
	lume.Golden = "distribution-macos-27.0.1-xcode-27.2-r1"

	if err := lume.Sweep(context.Background(), "distribution-"); err == nil {
		t.Fatal("sweep accepted a prefix the golden image's name starts with")
	}
	if err := lume.Destroy(context.Background(), Machine{Name: lume.Golden}); err == nil {
		t.Fatal("destroy accepted the golden image")
	}
	if processes := runner.all(); len(processes) != 0 {
		t.Fatalf("started %d processes, want none: %+v", len(processes), processes[0].Arguments)
	}
}

func TestLumeCreateHonoursCancellation(t *testing.T) {
	lume, runner := newLume(t, guest(func(process Process, _ string) (behaviour, bool) {
		if process.Path == "lume" && process.Arguments[0] == "get" {
			return behaviour{stdout: "[]"}, true
		}
		return behaviour{}, false
	}))
	lume.Boot = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := lume.Create(ctx, "cancelled"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("create = %v", err)
	}
	if len(runner.find(lumeCall("delete"))) != 1 {
		t.Fatal("cleanup did not run on a fresh context")
	}
}

func TestLumeCreateRejectsDuplicates(t *testing.T) {
	lume, _ := newLume(t, guest(nil))
	createLume(t, lume, "once")
	if _, err := lume.Create(context.Background(), "once"); !errors.Is(err, ErrExists) {
		t.Fatalf("create = %v, want exists", err)
	}
}

func TestLumeValidatesConfiguration(t *testing.T) {
	tests := map[string]func(*Lume){
		"no golden":           func(l *Lume) { l.Golden = "" },
		"option golden":       func(l *Lume) { l.Golden = "-x" },
		"no key":              func(l *Lume) { l.Key = "" },
		"key with a space":    func(l *Lume) { l.Key = "/keys/my key" },
		"key with a token":    func(l *Lume) { l.Key = "/keys/%u" },
		"user with an option": func(l *Lume) { l.User = "-oProxyCommand=x" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			lume, runner := newLume(t, guest(nil))
			mutate(lume)
			if _, err := lume.Create(context.Background(), "machine"); err == nil {
				t.Fatal("create succeeded")
			}
			if len(runner.all()) != 0 {
				t.Fatal("processes started with an invalid configuration")
			}
		})
	}
	lume, _ := newLume(t, guest(nil))
	if _, err := lume.Create(context.Background(), "--force"); err == nil {
		t.Fatal("create accepted an option-like name")
	}
	machine := createLume(t, lume, "valid")
	if _, err := lume.Exec(context.Background(), machine, Command{Arguments: []string{"true"}, Environment: map[string]string{"A=B": "x"}}); err == nil {
		t.Fatal("exec accepted an invalid environment name")
	}
	if _, err := lume.Exec(context.Background(), machine, Command{Arguments: []string{"a\x00b"}}); err == nil {
		t.Fatal("exec accepted a NUL byte")
	}
	if _, err := lume.Exec(context.Background(), Machine{Name: "valid", Address: "10.0.0.1"}, Command{Arguments: []string{"true"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("exec on a stale machine = %v", err)
	}
}
