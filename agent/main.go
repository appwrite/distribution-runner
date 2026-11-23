// Command distribution-agent is the trusted macOS host agent for Appwrite Distribution: it enrols the host, sends
// heartbeats, claims iOS build leases and runs each in a disposable VM. It never executes customer code itself.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/appwrite/distribution-runner/agent/lease"
	"github.com/appwrite/distribution-runner/agent/protocol"
	"github.com/appwrite/distribution-runner/agent/vm"
)

var version = "dev"

const (
	backendLume = "lume"
	backendFake = "fake"
	fakeImage   = "fake"
	// fakeLabel is added to a fake runner's labels, so only leases that ask for it, drills, land on it.
	fakeLabel = "fake"

	secretVariable    = "DISTRIBUTION_AGENT_SECRET"
	maximumCapacity   = 2
	heartbeatInterval = 30 * time.Second
	requestTimeout    = 30 * time.Second
	claimInterval     = 2 * time.Second
	claimCeiling      = 30 * time.Second
	stateMode         = 0o600
	// failureLimit is how many leases in a row may fail on the host before the agent stops claiming.
	failureLimit = 3
)

var (
	errRevoked   = errors.New("the server refused the runner token: remove the state file and enrol again")
	errUnhealthy = fmt.Errorf("%d leases in a row failed on this host: draining until the host is fixed and the agent restarted", failureLimit)
)

type config struct {
	endpoint   string
	secretFile string
	state      string
	name       string
	labels     []string
	capacity   int
	backend    string
	golden     string
	key        string
	user       string
	lume       string
	directory  string
	allowHTTP  bool
	version    bool
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	secret := os.Getenv(secretVariable)
	if err := os.Unsetenv(secretVariable); err != nil {
		logger.Error("agent stopped", "error", err)
		os.Exit(1)
	}
	lookup := func(name string) string {
		if name == secretVariable {
			return secret
		}
		return os.Getenv(name)
	}
	if err := run(os.Args[1:], lookup, logger); err != nil {
		logger.Error("agent stopped", "error", err)
		os.Exit(1)
	}
}

func run(arguments []string, lookup func(string) string, logger *slog.Logger) error {
	settings, err := parse(arguments, lookup, os.Stderr)
	if err != nil {
		return err
	}
	if settings.version {
		fmt.Println(version)
		return nil
	}
	client, err := protocol.New(settings.endpoint, nil, settings.allowHTTP)
	if err != nil {
		return err
	}

	drain, stopDrain := context.WithCancel(context.Background())
	work, stopWork := context.WithCancel(context.Background())
	defer stopDrain()
	defer stopWork()
	go watchSignals(logger, stopDrain, stopWork)

	lock, err := lockState(settings.state)
	if err != nil {
		return err
	}
	defer lock.Close()
	credentials, err := enrol(drain, client, settings, lookup, logger)
	if err != nil {
		return err
	}
	layer, image := backend(settings)
	agent := &agent{
		client:      client,
		credentials: credentials,
		labels:      settings.labels,
		capacity:    settings.capacity,
		logger:      logger,
		runner: &lease.Runner{
			Client:      client,
			Credentials: credentials,
			Layer:       layer,
			Image:       image,
			Directory:   settings.directory,
			AllowHTTP:   settings.allowHTTP,
			Logger:      logger,
		},
	}
	if err := agent.runner.Sweep(drain); err != nil {
		logger.Warn("removing what an earlier run left behind failed", "error", err)
	}
	logger.Info("agent started", "version", version, "runner", credentials, "capacity", settings.capacity, "vm", settings.backend, "machines", agent.runner.Prefix()+"*")
	if err := agent.serve(drain, work); err != nil {
		return err
	}
	logger.Info("agent drained")
	return nil
}

// watchSignals drains on the first SIGTERM or SIGINT and stops the builds in flight on the second. A third signal
// gets the default behaviour and ends the process.
func watchSignals(logger *slog.Logger, drain context.CancelFunc, stop context.CancelFunc) {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	<-signals
	logger.Info("draining: finishing the leases in flight")
	drain()
	<-signals
	signal.Stop(signals)
	logger.Warn("stopping the leases in flight")
	stop()
}

func parse(arguments []string, lookup func(string) string, output io.Writer) (config, error) {
	flags := flag.NewFlagSet("distribution-agent", flag.ContinueOnError)
	flags.SetOutput(output)
	setting := func(name string, fallback string) string {
		if value := lookup(name); value != "" {
			return value
		}
		return fallback
	}
	var settings config
	var labels, capacity, allowHTTP string
	flags.StringVar(&settings.endpoint, "endpoint", setting("DISTRIBUTION_AGENT_ENDPOINT", ""), "Appwrite endpoint including /v1 (DISTRIBUTION_AGENT_ENDPOINT)")
	flags.StringVar(&settings.secretFile, "secret-file", setting("DISTRIBUTION_AGENT_SECRET_FILE", ""), "file holding the enrolment secret; "+secretVariable+" also works (DISTRIBUTION_AGENT_SECRET_FILE)")
	flags.StringVar(&settings.state, "state", setting("DISTRIBUTION_AGENT_STATE", defaultState()), "file holding the runner credentials, mode 0600 (DISTRIBUTION_AGENT_STATE)")
	flags.StringVar(&settings.name, "name", setting("DISTRIBUTION_AGENT_NAME", hostname()), "runner name (DISTRIBUTION_AGENT_NAME)")
	flags.StringVar(&labels, "labels", setting("DISTRIBUTION_AGENT_LABELS", ""), "comma-separated labels, such as xcode-27.2 (DISTRIBUTION_AGENT_LABELS)")
	flags.StringVar(&capacity, "capacity", setting("DISTRIBUTION_AGENT_CAPACITY", "1"), "concurrent leases, 1 or 2; lume runs 1 until spike S7 proves guest isolation (DISTRIBUTION_AGENT_CAPACITY)")
	flags.StringVar(&settings.backend, "vm", setting("DISTRIBUTION_AGENT_VM", backendLume), "vm backend, lume, or fake for local drills with allow-http (DISTRIBUTION_AGENT_VM)")
	flags.StringVar(&settings.golden, "golden", setting("DISTRIBUTION_AGENT_GOLDEN", ""), "golden lume image every build is cloned from (DISTRIBUTION_AGENT_GOLDEN)")
	flags.StringVar(&settings.key, "ssh-key", setting("DISTRIBUTION_AGENT_SSH_KEY", ""), "private key the guest account authorises (DISTRIBUTION_AGENT_SSH_KEY)")
	flags.StringVar(&settings.user, "ssh-user", setting("DISTRIBUTION_AGENT_SSH_USER", ""), "guest account, lume by default (DISTRIBUTION_AGENT_SSH_USER)")
	flags.StringVar(&settings.lume, "lume", setting("DISTRIBUTION_AGENT_LUME", ""), "lume executable, lume from PATH by default (DISTRIBUTION_AGENT_LUME)")
	flags.StringVar(&settings.directory, "directory", setting("DISTRIBUTION_AGENT_DIRECTORY", filepath.Join(os.TempDir(), "distribution-agent")), "host scratch directory (DISTRIBUTION_AGENT_DIRECTORY)")
	flags.StringVar(&allowHTTP, "allow-http", setting("DISTRIBUTION_AGENT_ALLOW_HTTP", "false"), "allow a plain http endpoint, for local development only (DISTRIBUTION_AGENT_ALLOW_HTTP)")
	flags.BoolVar(&settings.version, "version", false, "print the version and exit")
	if err := flags.Parse(arguments); err != nil {
		return config{}, err
	}
	if flags.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if settings.version {
		return settings, nil
	}

	var err error
	if settings.allowHTTP, err = strconv.ParseBool(allowHTTP); err != nil {
		return config{}, fmt.Errorf("allow-http: %w", err)
	}
	if settings.capacity, err = strconv.Atoi(capacity); err != nil || settings.capacity < 1 || settings.capacity > maximumCapacity {
		return config{}, fmt.Errorf("capacity must be 1 or %d", maximumCapacity)
	}
	for label := range strings.SplitSeq(labels, ",") {
		if label = strings.TrimSpace(label); label != "" {
			settings.labels = append(settings.labels, label)
		}
	}
	switch {
	case settings.endpoint == "":
		return config{}, errors.New("endpoint is required")
	case settings.state == "":
		return config{}, errors.New("state is required")
	case settings.name == "":
		return config{}, errors.New("name is required")
	case settings.backend != backendLume && settings.backend != backendFake:
		return config{}, fmt.Errorf("vm must be %s or %s", backendLume, backendFake)
	case settings.backend == backendLume && (settings.golden == "" || settings.key == ""):
		return config{}, errors.New("the lume backend needs golden and ssh-key")
	case settings.backend == backendLume && settings.capacity > 1:
		return config{}, errors.New("the lume backend runs one lease per host until spike S7 proves that concurrent guests cannot reach each other (vm/README.md, host setup)")
	case settings.backend == backendFake && !settings.allowHTTP:
		return config{}, errors.New("the fake vm backend is for local drills only: it needs allow-http")
	}
	if settings.backend == backendFake && !slices.Contains(settings.labels, fakeLabel) {
		settings.labels = append(settings.labels, fakeLabel)
	}
	return settings, nil
}

func backend(settings config) (vm.Layer, string) {
	if settings.backend == backendFake {
		return &vm.Fake{
			Directory: settings.directory,
			Build: func(ctx context.Context, work string, _ map[string]string) error {
				return lease.Sample(ctx, work)
			},
		}, fakeImage
	}
	return &vm.Lume{
		Golden:    settings.golden,
		Binary:    settings.lume,
		User:      settings.user,
		Key:       settings.key,
		Directory: settings.directory,
	}, settings.golden
}

// enrol returns the credentials in the state file, enrolling with the secret and saving them when there are none.
func enrol(ctx context.Context, client *protocol.Client, settings config, lookup func(string) string, logger *slog.Logger) (protocol.Credentials, error) {
	credentials, err := loadCredentials(settings.state)
	if err == nil {
		return credentials, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return protocol.Credentials{}, err
	}
	secret, err := enrolmentSecret(settings.secretFile, lookup)
	if err != nil {
		return protocol.Credentials{}, err
	}
	enrolling, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	credentials, err = client.Enrol(enrolling, secret, protocol.Registration{
		Name:     settings.name,
		Platform: protocol.PlatformMacOS,
		Labels:   settings.labels,
		Capacity: settings.capacity,
		Version:  version,
	})
	if err != nil {
		return protocol.Credentials{}, fmt.Errorf("enrol: %w", err)
	}
	if err := saveCredentials(settings.state, credentials); err != nil {
		return protocol.Credentials{}, fmt.Errorf("save credentials: %w", err)
	}
	logger.Info("enrolled", "runner", credentials)
	return credentials, nil
}

func enrolmentSecret(file string, lookup func(string) string) (string, error) {
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read enrolment secret: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	if secret := lookup(secretVariable); secret != "" {
		return secret, nil
	}
	return "", errors.New("not enrolled: set " + secretVariable + " or secret-file")
}

// lockState holds an exclusive lock beside the state file until the returned file is closed, so two agents never run
// as one runner: they would share its machine prefix and sweep each other's machines at start.
func lockState(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, stateMode)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another agent is running with the state file %s", path)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return file, nil
}

// loadCredentials refuses a state file that is a symlink, that another user owns or that other users can read, since
// it holds the runner token.
func loadCredentials(path string) (protocol.Credentials, error) {
	information, err := os.Lstat(path)
	if err != nil {
		return protocol.Credentials{}, err
	}
	if !information.Mode().IsRegular() {
		return protocol.Credentials{}, fmt.Errorf("state file %s is not a regular file", path)
	}
	if information.Mode().Perm()&0o077 != 0 {
		return protocol.Credentials{}, fmt.Errorf("state file %s must not be readable by other users (mode %v)", path, information.Mode().Perm())
	}
	if status, ok := information.Sys().(*syscall.Stat_t); ok && int(status.Uid) != os.Getuid() {
		return protocol.Credentials{}, fmt.Errorf("state file %s belongs to user %d, not the agent", path, status.Uid)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return protocol.Credentials{}, err
	}
	var credentials protocol.Credentials
	if err := json.Unmarshal(data, &credentials); err != nil || credentials.RunnerID == "" || credentials.Token == "" {
		return protocol.Credentials{}, fmt.Errorf("state file %s holds no valid credentials", path)
	}
	return credentials, nil
}

// saveCredentials writes the state file atomically with mode 0600.
func saveCredentials(path string, credentials protocol.Credentials) error {
	data, err := json.Marshal(credentials)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(stateMode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

// syncDirectory makes a rename in directory durable.
func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

type agent struct {
	client      *protocol.Client
	credentials protocol.Credentials
	runner      *lease.Runner
	labels      []string
	capacity    int
	logger      *slog.Logger

	failures atomic.Int32
	halt     context.CancelCauseFunc
}

// serve heartbeats and runs one claim loop per slot until drain ends, then waits for the leases in flight and
// reports the runner offline. work stops those leases early. When failureLimit leases in a row fail on the host it
// stops claiming and reports draining until drain ends; when the server refuses the runner token it returns that.
func (a *agent) serve(drain context.Context, work context.Context) error {
	claiming, halt := context.WithCancelCause(drain)
	defer halt(nil)
	a.halt = halt
	beating, stopBeating := context.WithCancel(context.Background())
	var heartbeats sync.WaitGroup
	heartbeats.Go(func() { a.heartbeat(beating, claiming) })

	var slots sync.WaitGroup
	for slot := range a.capacity {
		slots.Go(func() { a.slot(claiming, work, slot) })
	}
	slots.Wait()
	cause := context.Cause(claiming)
	if errors.Is(cause, errUnhealthy) {
		<-drain.Done()
	}
	stopBeating()
	heartbeats.Wait()
	a.beat(protocol.StatusOffline)
	if errors.Is(cause, errRevoked) {
		return cause
	}
	return nil
}

func (a *agent) heartbeat(ctx context.Context, drain context.Context) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	draining := drain.Done()
	a.beat(protocol.StatusOnline)
	for {
		select {
		case <-ctx.Done():
			return
		case <-draining:
			draining = nil
		case <-ticker.C:
		}
		status := protocol.StatusOnline
		if drain.Err() != nil {
			status = protocol.StatusDraining
		}
		a.beat(status)
	}
}

func (a *agent) beat(status string) {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	err := a.client.Heartbeat(ctx, a.credentials, protocol.Status{Status: status, Version: version, Labels: a.labels})
	if err != nil {
		a.logger.Warn("heartbeat failed", "status", status, "error", err)
	}
}

// slot claims and runs one lease at a time until claiming ends. An empty queue or a failed claim backs off up to
// claimCeiling, and so does a lease that failed on the host. Claims use the work context, so a drain never abandons a
// claim whose lease the server already handed out.
func (a *agent) slot(claiming context.Context, work context.Context, slot int) {
	logger := a.logger.With("slot", slot)
	wait := claimInterval
	for claiming.Err() == nil && work.Err() == nil {
		claimed, err := a.client.Claim(work, a.credentials, a.labels)
		if revoked(err) {
			logger.Error("claim refused", "error", err)
			a.halt(errRevoked)
			return
		}
		if err != nil || claimed == nil {
			if err != nil {
				logger.Warn("claim failed", "error", err)
			}
			if pause(claiming, wait) != nil {
				return
			}
			wait = min(wait*2, claimCeiling)
			continue
		}
		wait = claimInterval
		logger.Info("lease claimed", "lease", *claimed)
		err = a.runner.Run(work, *claimed)
		if err != nil {
			logger.Warn("lease ended without a build", "lease", *claimed, "error", err)
		}
		if !lease.Infrastructure(err) {
			a.failures.Store(0)
			continue
		}
		failures := a.failures.Add(1)
		if failures >= failureLimit {
			logger.Error("draining", "error", errUnhealthy)
			a.halt(errUnhealthy)
			return
		}
		if pause(claiming, min(claimInterval<<failures, claimCeiling)) != nil {
			return
		}
	}
}

func revoked(err error) bool {
	var response *protocol.ResponseError
	return errors.As(err, &response) && response.Status == http.StatusUnauthorized
}

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func defaultState() string {
	directory, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(directory, "appwrite-distribution", "agent.json")
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}
