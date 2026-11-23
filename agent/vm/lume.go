package vm

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Lume drives lume 0.7.2 (github.com/trycua/cua, libs/lume, MIT licence) through its command line, and the guest
// through the system ssh and scp with a key. Every host process is an argv; nothing runs through a host shell, and
// command arguments and environment travel on ssh's standard input, never in an argv.
//
// Every machine gets its own ssh host key: the agent mints it, shares it read-only into that machine alone with
// `lume run --shared-dir`, and pins it, so one guest cannot answer for another with a key the golden image shares.
// The golden image installs the key before sshd answers with it (vm/README.md, host keys).
//
// Unproven pending spike S7: no build has run through this adapter on a real Mac yet. The commands and flags are
// taken from the lume-v0.7.2 sources (clone, run --display none --vnc disabled --shared-dir, get --format json,
// stop --force, delete --force, ls --format json); the boot timing, address discovery on vmnet NAT, the guest
// picking up its host key and two concurrent clones per host are not yet measured.
type Lume struct {
	// Golden is the stopped VM every machine is cloned from.
	Golden string
	// Binary is the lume executable, "lume" from PATH by default.
	Binary string
	// SSH and SCP default to /usr/bin/ssh and /usr/bin/scp.
	SSH string
	SCP string
	// User is the guest account, "lume" by default (the user lume's unattended setup creates).
	User string
	// Key is the private key the guest account authorises.
	Key string
	// Boot bounds waiting for an address and a working ssh login, 5 minutes by default; Poll is the interval
	// between attempts, 2 seconds by default.
	Boot time.Duration
	Poll time.Duration
	// Grace bounds stopping a cancelled command and a killed VM process, 20 seconds by default.
	Grace time.Duration
	// Directory holds per-machine host files, the system temporary directory by default.
	Directory string
	Runner    Runner

	mutex    sync.Mutex
	machines map[string]*instance
}

var _ Layer = (*Lume)(nil)

const (
	defaultBoot    = 5 * time.Minute
	defaultPoll    = 2 * time.Second
	defaultGrace   = 20 * time.Second
	destroyTimeout = 2 * time.Minute
	logBytes       = 8 << 10
	shellCommand   = "/bin/sh -s"
	telemetry      = "LUME_TELEMETRY_ENABLED=false"
	statusRunning  = "running"
	prepareExists  = 3
	notFile        = 4
	// machinePrefix names a machine's host directory, followed by the machine name and a random suffix.
	machinePrefix = "vm-"
	// Share is the directory lume shares read-only into the guest, which sees it as
	// /Volumes/My Shared Files/host-key. HostKeyFile is the private key in it.
	Share       = "host-key"
	HostKeyFile = "ssh_host_ed25519_key"
	knownHosts  = "known_hosts"
)

var errAddress = errors.New("vm: lume reported an invalid address")

type instance struct {
	name      string
	directory string
	address   string
	run       Handle
	exited    chan struct{}
	code      int
	err       error
	log       *tail
}

type details struct {
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	IPAddress    *string `json:"ipAddress"`
	SSHAvailable *bool   `json:"sshAvailable"`
}

func (l *Lume) Create(ctx context.Context, name string) (Machine, error) {
	if err := l.validate(); err != nil {
		return Machine{}, err
	}
	if err := validateName(name); err != nil {
		return Machine{}, err
	}
	created := &instance{name: name, exited: make(chan struct{}), log: &tail{limit: logBytes}}
	if err := l.reserve(created); err != nil {
		return Machine{}, err
	}

	directory, err := os.MkdirTemp(l.Directory, machinePrefix+name+"-")
	if err == nil && (unsafeOption(directory) || strings.Contains(directory, ":")) {
		err = fmt.Errorf("vm: temporary directory %q is not usable in an ssh option or a lume share", directory)
	}
	if err == nil {
		err = writeHostKey(directory, name)
	}
	if err != nil {
		l.release(name)
		if directory != "" {
			_ = os.RemoveAll(directory)
		}
		return Machine{}, err
	}
	created.directory = directory

	present, err := l.present(ctx, name)
	if err == nil && present {
		err = fmt.Errorf("%w: lume already has %s", ErrExists, name)
	}
	if err != nil {
		l.release(name)
		_ = os.RemoveAll(directory)
		return Machine{}, err
	}

	machine, err := l.clone(ctx, created)
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), destroyTimeout)
		defer cancel()
		if destroyErr := l.Destroy(cleanup, Machine{Name: name}); destroyErr != nil {
			err = errors.Join(err, destroyErr)
		}
		return Machine{}, err
	}
	return machine, nil
}

// clone copies the golden image and boots the copy. Its caller destroys whatever a failure leaves, a partial clone
// included, since lume names the machine before the copy finishes.
func (l *Lume) clone(ctx context.Context, created *instance) (Machine, error) {
	if _, err := l.lume(ctx, "clone", l.Golden, created.name); err != nil {
		return Machine{}, err
	}
	machine, err := l.start(ctx, created)
	if err != nil {
		return Machine{}, err
	}
	if err := os.RemoveAll(filepath.Join(created.directory, Share)); err != nil {
		return Machine{}, err
	}
	return machine, nil
}

// writeHostKey mints the machine's host key into its share and pins it, under the machine's name, in known_hosts.
func writeHostKey(directory string, name string) error {
	key, err := newHostKey(name)
	if err != nil {
		return err
	}
	share := filepath.Join(directory, Share)
	if err := os.Mkdir(share, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(share, HostKeyFile), key.Private, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(share, HostKeyFile+".pub"), []byte(key.Public+" "+name+"\n"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, knownHosts), []byte(name+" "+key.Public+"\n"), 0o600)
}

func (l *Lume) Exec(ctx context.Context, machine Machine, command Command) (int, error) {
	if err := validateArguments(command.Arguments); err != nil {
		return -1, err
	}
	if err := validateEnvironment(command.Environment); err != nil {
		return -1, err
	}
	running, err := l.lookup(machine)
	if err != nil {
		return -1, err
	}
	group := "/tmp/distribution-" + rand.Text() + ".pgid"
	handle, err := l.runner().Start(Process{
		Path:      l.sshBinary(),
		Arguments: l.sshArguments(running),
		Stdin:     strings.NewReader(execScript(group, command.Environment, command.Arguments)),
		Stdout:    command.Stdout,
		Stderr:    command.Stderr,
	})
	if err != nil {
		return -1, err
	}
	result := wait(handle)
	select {
	case outcome := <-result:
		return outcome.code, outcome.err
	case <-ctx.Done():
	}
	stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), l.grace())
	defer cancel()
	_, _ = l.shell(stop, running, killScript(group), io.Discard)
	_ = handle.Kill()
	<-result
	return -1, fmt.Errorf("vm: exec in %s stopped: %w", machine.Name, context.Cause(ctx))
}

func (l *Lume) Copy(ctx context.Context, machine Machine, transfer Transfer) error {
	if err := validateTransfer(transfer); err != nil {
		return err
	}
	running, err := l.lookup(machine)
	if err != nil {
		return err
	}
	switch transfer.Direction {
	case In:
		return l.copyIn(ctx, running, machine.Home, transfer)
	default:
		return l.copyOut(ctx, running, machine.Home, transfer)
	}
}

func (l *Lume) Destroy(ctx context.Context, machine Machine) error {
	if machine.Name == "" {
		return nil
	}
	if err := validateName(machine.Name); err != nil {
		return err
	}
	l.mutex.Lock()
	running := l.machines[machine.Name]
	delete(l.machines, machine.Name)
	l.mutex.Unlock()

	if running != nil {
		running.stop(l.grace())
		_ = os.RemoveAll(running.directory)
	} else {
		_, _, _ = l.lumeResult(ctx, "stop", machine.Name, "--force")
	}

	code, stderr, err := l.lumeResult(ctx, "delete", machine.Name, "--force")
	if err != nil {
		return err
	}
	if running == nil {
		if err := l.removeDirectories(machine.Name); err != nil {
			return err
		}
	}
	if code == 0 {
		return nil
	}
	present, listErr := l.present(ctx, machine.Name)
	if listErr != nil {
		return fmt.Errorf("vm: lume delete %s exited %d: %s; %w", machine.Name, code, stderr, listErr)
	}
	if !present {
		return nil
	}
	return fmt.Errorf("vm: lume delete %s exited %d: %s", machine.Name, code, stderr)
}

// Sweep deletes every lume machine named with prefix that this Lume does not own, and every per-machine directory
// of such a machine.
func (l *Lume) Sweep(ctx context.Context, prefix string) error {
	if err := validateName(prefix); err != nil {
		return err
	}
	listed, err := l.list(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range listed {
		if !strings.HasPrefix(entry.Name, prefix) || l.owned(entry.Name) {
			continue
		}
		if err := l.Destroy(ctx, Machine{Name: entry.Name}); err != nil {
			failures = append(failures, err)
		}
	}
	entries, err := os.ReadDir(l.directory())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		failures = append(failures, err)
	}
	for _, entry := range entries {
		name, ok := strings.CutPrefix(entry.Name(), machinePrefix)
		if !ok || !strings.HasPrefix(name, prefix) || l.ownedDirectory(entry.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(l.directory(), entry.Name())); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (l *Lume) owned(name string) bool {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	_, ok := l.machines[name]
	return ok
}

func (l *Lume) ownedDirectory(base string) bool {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	for _, running := range l.machines {
		if filepath.Base(running.directory) == base {
			return true
		}
	}
	return false
}

// removeDirectories removes the per-machine directories of a machine this Lume no longer tracks.
func (l *Lume) removeDirectories(name string) error {
	matches, err := filepath.Glob(filepath.Join(l.directory(), machinePrefix+name+"-*"))
	if err != nil {
		return err
	}
	var failures []error
	for _, match := range matches {
		if suffix := strings.TrimPrefix(filepath.Base(match), machinePrefix+name+"-"); strings.Contains(suffix, "-") {
			continue
		}
		failures = append(failures, os.RemoveAll(match))
	}
	return errors.Join(failures...)
}

func (l *Lume) directory() string {
	return fallback(l.Directory, os.TempDir())
}

func (l *Lume) start(ctx context.Context, created *instance) (Machine, error) {
	handle, err := l.runner().Start(Process{
		Path:        l.lumeBinary(),
		Arguments:   []string{"run", created.name, "--display", "none", "--vnc", "disabled", "--shared-dir", filepath.Join(created.directory, Share) + ":ro"},
		Environment: []string{telemetry},
		Stdout:      created.log,
		Stderr:      created.log,
	})
	if err != nil {
		close(created.exited)
		return Machine{}, err
	}
	created.run = handle
	go func() {
		created.code, created.err = handle.Wait()
		close(created.exited)
	}()

	ctx, cancel := context.WithTimeout(ctx, l.boot())
	defer cancel()
	ticker := time.NewTicker(l.poll())
	defer ticker.Stop()
	last := errors.New("no address yet")
	for {
		if err := created.exitError(); err != nil {
			return Machine{}, err
		}
		address, err := l.address(ctx, created.name)
		if errors.Is(err, errAddress) {
			return Machine{}, err
		}
		if err != nil && ctx.Err() == nil {
			last = err
		}
		if address != "" {
			l.mutex.Lock()
			created.address = address
			l.mutex.Unlock()
			home, err := l.home(ctx, created)
			if err == nil {
				machine := Machine{Name: created.name, Address: address, Home: home}
				return machine, nil
			}
			last = err
		}
		select {
		case <-ctx.Done():
			return Machine{}, fmt.Errorf("vm: %s did not boot: %w (last: %v)", created.name, context.Cause(ctx), last)
		case <-created.exited:
		case <-ticker.C:
		}
	}
}

// address returns the guest IP once lume reports the machine running with ssh reachable, and "" before that. A failed
// listing is worth retrying; an invalid address is not.
func (l *Lume) address(ctx context.Context, name string) (string, error) {
	output, err := l.lume(ctx, "get", name, "--format", "json")
	if err != nil {
		return "", err
	}
	var listed []details
	if err := json.Unmarshal(output, &listed); err != nil {
		return "", fmt.Errorf("vm: lume get %s: %w", name, err)
	}
	for _, entry := range listed {
		if entry.Name != name {
			continue
		}
		if entry.Status != statusRunning || entry.IPAddress == nil || *entry.IPAddress == "" ||
			entry.SSHAvailable == nil || !*entry.SSHAvailable {
			return "", nil
		}
		address, err := netip.ParseAddr(*entry.IPAddress)
		if err != nil || address.Zone() != "" || address.IsUnspecified() || address.IsMulticast() {
			return "", fmt.Errorf("%w %q for %s", errAddress, *entry.IPAddress, name)
		}
		return address.String(), nil
	}
	return "", nil
}

func (l *Lume) home(ctx context.Context, created *instance) (string, error) {
	var output bytes.Buffer
	code, err := l.shell(ctx, created, homeScript, &output)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("ssh exited %d", code)
	}
	home := output.String()
	if !path.IsAbs(home) || strings.ContainsAny(home, "\n\x00") {
		return "", fmt.Errorf("vm: guest reported an invalid home %q", home)
	}
	return path.Clean(home), nil
}

func (l *Lume) copyIn(ctx context.Context, running *instance, home string, transfer Transfer) error {
	source, err := filepath.Abs(transfer.Source)
	if err != nil {
		return err
	}
	if _, err := os.Stat(source); err != nil {
		return err
	}
	destination := guestPath(home, transfer.Destination)
	var output bytes.Buffer
	code, err := l.shell(ctx, running, prepareScript(path.Dir(destination), destination), &output)
	if err != nil {
		return err
	}
	if code == prepareExists {
		return fmt.Errorf("%w: %s", ErrDestination, destination)
	}
	if code != 0 {
		return fmt.Errorf("vm: preparing %s exited %d", destination, code)
	}
	return l.scp(ctx, running, source, remote(running.address, destination))
}

// copyOut streams one regular guest file through "cat" into a new private host file, stopping at the limit, so a
// guest cannot fill the host with a huge or endless file, a device or a directory tree.
func (l *Lume) copyOut(ctx context.Context, running *instance, home string, transfer Transfer) (err error) {
	source := guestPath(home, transfer.Source)
	file, err := createExclusive(transfer.Destination)
	if err != nil {
		return err
	}
	limited := &limitWriter{writer: file, limit: transfer.Limit}
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(transfer.Destination)
		}
	}()
	stderr := &tail{limit: logBytes}
	code, err := l.run(ctx, Process{
		Path:      l.sshBinary(),
		Arguments: l.sshArguments(running),
		Stdin:     strings.NewReader(readScript(source)),
		Stdout:    limited,
		Stderr:    stderr,
	})
	switch {
	case limited.over:
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrLimit, source, transfer.Limit)
	case err != nil:
		return err
	case code == notFile:
		return fmt.Errorf("%w: %s", ErrNotFile, source)
	case code != 0:
		return fmt.Errorf("vm: reading %s exited %d: %s", source, code, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (l *Lume) scp(ctx context.Context, running *instance, source string, destination string) error {
	stderr := &tail{limit: logBytes}
	arguments := append(l.options(running), "-q", "-p", "-r", "--", source, destination)
	code, err := l.run(ctx, Process{Path: l.scpBinary(), Arguments: arguments, Stderr: stderr})
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("vm: scp exited %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// shell runs a fixed guest script through ssh, with the script on standard input.
func (l *Lume) shell(ctx context.Context, running *instance, script string, stdout io.Writer) (int, error) {
	return l.run(ctx, Process{
		Path:      l.sshBinary(),
		Arguments: l.sshArguments(running),
		Stdin:     strings.NewReader(script),
		Stdout:    stdout,
		Stderr:    io.Discard,
	})
}

func (l *Lume) sshArguments(running *instance) []string {
	return append(l.options(running), "-T", "--", running.address, shellCommand)
}

// options are shared by ssh and scp. "-F none" keeps the agent user's ssh configuration (ProxyCommand, Match exec,
// control sockets) out of the connection, and only the configured key is offered. The machine's own host key is the
// only one accepted.
func (l *Lume) options(running *instance) []string {
	options := []string{
		"-F", "none",
		"-i", l.Key,
		"-o", "User=" + l.user(),
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-o", "BatchMode=yes",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "UpdateHostKeys=no",
		"-o", "ControlMaster=no",
		"-o", "ControlPath=none",
		"-o", "ForwardAgent=no",
		"-o", "ForwardX11=no",
		"-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-o", "LogLevel=ERROR",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "HostKeyAlgorithms=" + hostKeyType,
		"-o", "UserKnownHostsFile=" + filepath.Join(running.directory, knownHosts),
		"-o", "HostKeyAlias=" + running.name,
	}
	return options
}

func (l *Lume) lume(ctx context.Context, arguments ...string) ([]byte, error) {
	var output bytes.Buffer
	code, stderr, err := l.lumeCapture(ctx, &output, arguments...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("vm: lume %s exited %d: %s", arguments[0], code, stderr)
	}
	return output.Bytes(), nil
}

func (l *Lume) lumeResult(ctx context.Context, arguments ...string) (int, string, error) {
	return l.lumeCapture(ctx, io.Discard, arguments...)
}

func (l *Lume) lumeCapture(ctx context.Context, stdout io.Writer, arguments ...string) (int, string, error) {
	stderr := &tail{limit: logBytes}
	code, err := l.run(ctx, Process{
		Path:        l.lumeBinary(),
		Arguments:   arguments,
		Environment: []string{telemetry},
		Stdout:      stdout,
		Stderr:      stderr,
	})
	return code, strings.TrimSpace(stderr.String()), err
}

func (l *Lume) present(ctx context.Context, name string) (bool, error) {
	listed, err := l.list(ctx)
	if err != nil {
		return false, err
	}
	for _, entry := range listed {
		if entry.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (l *Lume) list(ctx context.Context) ([]details, error) {
	output, err := l.lume(ctx, "ls", "--format", "json")
	if err != nil {
		return nil, err
	}
	var listed []details
	if err := json.Unmarshal(output, &listed); err != nil {
		return nil, fmt.Errorf("vm: lume ls: %w", err)
	}
	return listed, nil
}

// run starts a process and waits for it, killing it when ctx ends.
func (l *Lume) run(ctx context.Context, process Process) (int, error) {
	if err := ctx.Err(); err != nil {
		return -1, context.Cause(ctx)
	}
	handle, err := l.runner().Start(process)
	if err != nil {
		return -1, err
	}
	result := wait(handle)
	select {
	case outcome := <-result:
		return outcome.code, outcome.err
	case <-ctx.Done():
		_ = handle.Kill()
		<-result
		return -1, context.Cause(ctx)
	}
}

func (l *Lume) reserve(created *instance) error {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if l.machines == nil {
		l.machines = map[string]*instance{}
	}
	if _, exists := l.machines[created.name]; exists {
		return fmt.Errorf("%w: %s", ErrExists, created.name)
	}
	l.machines[created.name] = created
	return nil
}

func (l *Lume) release(name string) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	delete(l.machines, name)
}

func (l *Lume) lookup(machine Machine) (*instance, error) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	running := l.machines[machine.Name]
	if running == nil || running.address == "" || running.address != machine.Address {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, machine.Name)
	}
	return running, nil
}

func (l *Lume) validate() error {
	if err := validateName(l.Golden); err != nil {
		return fmt.Errorf("vm: golden image: %w", err)
	}
	if err := validateName(l.user()); err != nil {
		return fmt.Errorf("vm: user: %w", err)
	}
	if l.Key == "" {
		return errors.New("vm: an ssh key is required")
	}
	if unsafeOption(l.Key) {
		return fmt.Errorf("vm: %q is not usable in an ssh option", l.Key)
	}
	return nil
}

// unsafeOption reports values ssh would split on whitespace or expand as % tokens.
func unsafeOption(value string) bool {
	return strings.ContainsAny(value, " \t\n\r\"'%\x00")
}

func (l *Lume) lumeBinary() string { return fallback(l.Binary, "lume") }
func (l *Lume) sshBinary() string  { return fallback(l.SSH, "/usr/bin/ssh") }
func (l *Lume) scpBinary() string  { return fallback(l.SCP, "/usr/bin/scp") }
func (l *Lume) user() string       { return fallback(l.User, "lume") }

func (l *Lume) boot() time.Duration  { return positive(l.Boot, defaultBoot) }
func (l *Lume) poll() time.Duration  { return positive(l.Poll, defaultPoll) }
func (l *Lume) grace() time.Duration { return positive(l.Grace, defaultGrace) }

func (l *Lume) runner() Runner {
	if l.Runner == nil {
		return System{}
	}
	return l.Runner
}

func fallback(value string, otherwise string) string {
	if value == "" {
		return otherwise
	}
	return value
}

func positive(value time.Duration, otherwise time.Duration) time.Duration {
	if value <= 0 {
		return otherwise
	}
	return value
}

func (i *instance) exitError() error {
	select {
	case <-i.exited:
		if i.err != nil {
			return fmt.Errorf("vm: lume run %s failed: %w", i.name, i.err)
		}
		return fmt.Errorf("vm: lume run %s exited %d: %s", i.name, i.code, strings.TrimSpace(i.log.String()))
	default:
		return nil
	}
}

func (i *instance) stop(grace time.Duration) {
	if i.run == nil {
		return
	}
	_ = i.run.Kill()
	select {
	case <-i.exited:
	case <-time.After(grace):
	}
}

// guestPath resolves a machine path against the guest home.
func guestPath(home string, name string) string {
	if path.IsAbs(name) {
		return path.Clean(name)
	}
	return path.Join(home, name)
}

// remote is scp's host:path form. The path is absolute, and the address an IP literal, bracketed for IPv6.
func remote(address string, name string) string {
	if strings.Contains(address, ":") {
		return "[" + address + "]:" + name
	}
	return address + ":" + name
}

type outcome struct {
	code int
	err  error
}

func wait(handle Handle) <-chan outcome {
	result := make(chan outcome, 1)
	go func() {
		code, err := handle.Wait()
		result <- outcome{code: code, err: err}
	}()
	return result
}

// tail keeps the last limit bytes written, for error messages.
type tail struct {
	mutex  sync.Mutex
	limit  int
	buffer []byte
}

func (t *tail) Write(data []byte) (int, error) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.buffer = append(t.buffer, data...)
	if excess := len(t.buffer) - t.limit; excess > 0 {
		t.buffer = append(t.buffer[:0], t.buffer[excess:]...)
	}
	return len(data), nil
}

func (t *tail) String() string {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	return string(t.buffer)
}
