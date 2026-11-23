// Package lease runs one claimed iOS build lease: it fetches the source on the host, builds inside a disposable VM,
// verifies the IPA against the lease, uploads it and reports every step, destroying the VM whatever happens.
package lease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/appwrite/distribution-runner/agent/protocol"
	"github.com/appwrite/distribution-runner/agent/verify"
	"github.com/appwrite/distribution-runner/agent/vm"
)

const (
	Script = "/usr/local/distribution/build.sh"
	// Work is the directory under the guest home that DISTRIBUTION_WORK names.
	Work = "distribution"

	DirectoryInput   = "input"
	DirectoryOutput  = "output"
	DirectorySigning = "signing"
	DirectoryProfile = "profiles"

	FileEnvironment  = "environment"
	FileSource       = "source.tar.gz"
	FileCertificate  = "certificate.p12"
	FilePassword     = "password"
	FileIPA          = "app.ipa"
	FileResult       = "result.json"
	ExtensionProfile = ".mobileprovision"

	StageSource = "source"
	StageBuild  = "build"

	ExitTimeout = 137
	ExitFailed  = -1
)

const (
	sourceArtifact = "source"
	resultArtifact = "result"
	outputArtifact = "output"

	artifactDownload = "download"
	artifactRead     = "read"
	artifactUpload   = "upload"
	formatJSON       = "json"

	defaultInterval = time.Second
	defaultGrace    = 2 * time.Minute
	defaultTimeout  = time.Hour
	defaultTTL      = 5 * time.Minute
	minimumExtend   = 10 * time.Millisecond
	reportTimeout   = 30 * time.Second
	messageBytes    = 4096
	resultBytes     = 5 << 20
	toolchainBytes  = 1024
)

var (
	errTimeout  = errors.New("lease: timeout")
	errCanceled = errors.New("lease: canceled by the server")
	errLost     = errors.New("lease: lost")
)

// Runner runs leases for one enrolled runner. It is safe for concurrent use; each Run owns its own VM.
type Runner struct {
	Client      *protocol.Client
	Credentials protocol.Credentials
	Layer       vm.Layer
	// Image names the golden image, reported as the exit event's image and the result's toolchain image.
	Image string
	// Directory holds per-lease host scratch directories, the system temporary directory by default.
	Directory string
	// Download fetches the source tarball; redirects are restricted to HTTPS unless AllowHTTP is set.
	Download  *http.Client
	AllowHTTP bool
	Logger    *slog.Logger
	// Interval is how often logs are sent, a second by default. Grace bounds destroying the VM, 2 minutes by default.
	Interval time.Duration
	Grace    time.Duration
}

// failure is a run that ended without a build: it becomes an optional failed artifact event and a non-zero exit.
type failure struct {
	exit     int
	reason   string
	code     protocol.Code
	message  string
	artifact string
	kind     string
	cause    error
}

func (f *failure) Error() string {
	return fmt.Sprintf("lease: %s: %s", f.reason, f.message)
}

func (f *failure) Unwrap() error {
	return f.cause
}

type execution struct {
	runner   *Runner
	lease    protocol.Lease
	session  *protocol.Session
	reporter *protocol.Reporter
	logger   *slog.Logger
	elapsed  time.Duration
}

type result struct {
	Artifact     string         `json:"artifact"`
	Type         string         `json:"type"`
	Digest       string         `json:"digest"`
	Size         int64          `json:"size"`
	Identifier   string         `json:"identifier"`
	VersionName  string         `json:"versionName"`
	VersionCode  int            `json:"versionCode"`
	BuildNumber  string         `json:"buildNumber"`
	TeamID       string         `json:"teamId"`
	Signed       bool           `json:"signed"`
	Entitlements map[string]any `json:"entitlements"`
	Toolchain    toolchain      `json:"toolchain"`
}

type toolchain struct {
	Image    string  `json:"image"`
	Fastlane *string `json:"fastlane"`
	Xcode    *string `json:"xcode"`
	Flutter  *string `json:"flutter"`
}

// Run executes the lease and reports it. It returns nil after a build that was verified, uploaded and completed, and
// the failure otherwise. The run stops at lease.timeout seconds from the call, and as soon as the server cancels the
// lease or it cannot be extended before it expires.
func (r *Runner) Run(ctx context.Context, lease protocol.Lease) error {
	logger := r.logger().With("lease", lease)
	session, err := r.Client.Session(r.Credentials, lease)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var extending sync.WaitGroup
	extendContext, stopExtending := context.WithCancel(ctx)
	defer extending.Wait()
	defer stopExtending()

	reporter := protocol.NewReporter(session, positive(r.Interval, defaultInterval))
	defer func() {
		closing, done := context.WithTimeout(context.WithoutCancel(ctx), reportTimeout)
		defer done()
		if err := reporter.Close(closing); err != nil {
			logger.Warn("reporting lease events failed", "error", err)
		}
	}()

	execution := &execution{runner: r, lease: lease, session: session, reporter: reporter, logger: logger}
	extending.Go(func() { execution.extend(extendContext, cancel, deadline(lease.ExpiresAt)) })

	ctx, stopTimeout := context.WithTimeoutCause(ctx, timeout(lease), errTimeout)
	defer stopTimeout()

	reporter.Emit(protocol.Event{Type: protocol.TypeStart})
	outcome, err := execution.run(ctx)
	if err != nil {
		var failed *failure
		if !errors.As(err, &failed) {
			failed = &failure{exit: ExitFailed, reason: "agent", code: protocol.CodeJobFailed, message: err.Error(), cause: err}
		}
		execution.report(failed)
		logger.Warn("lease failed", "reason", failed.reason, "exitCode", failed.exit, "error", failed.message)
		return failed
	}
	execution.succeed(outcome)
	logger.Info("lease succeeded", "digest", outcome.content.Digest, "size", outcome.content.Size)
	return nil
}

type outcome struct {
	content result
	verify  time.Duration
	upload  time.Duration
}

// run does the work and returns what success reports; the VM is destroyed before it returns, a panic included.
func (e *execution) run(ctx context.Context) (done outcome, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			e.logger.Error("lease run panicked", "panic", fmt.Sprint(recovered), "stack", string(debug.Stack()))
			err = &failure{exit: ExitFailed, reason: "agent", code: protocol.CodeJobFailed, message: "the agent failed unexpectedly"}
		}
	}()

	expected, err := e.expected()
	if err != nil {
		return outcome{}, &failure{exit: ExitFailed, reason: "lease", code: protocol.CodeJobFailed, message: err.Error(), cause: err}
	}
	variables, err := Environment(e.lease)
	if err != nil {
		return outcome{}, &failure{exit: ExitFailed, reason: "lease", code: protocol.CodeJobFailed, message: err.Error(), cause: err}
	}

	scratch, err := e.runner.scratch()
	if err != nil {
		return outcome{}, e.failed(ctx, err, "agent", protocol.CodeJobFailed, "")
	}
	defer os.RemoveAll(scratch)
	input := filepath.Join(scratch, DirectoryInput)

	e.stage(StageSource)
	if err := writeInput(input, e.lease, variables); err != nil {
		return outcome{}, e.failed(ctx, err, "agent", protocol.CodeJobFailed, "")
	}
	if err := e.fetch(ctx, filepath.Join(input, FileSource)); err != nil {
		return outcome{}, err
	}

	e.stage(StageBuild)
	machine, err := e.runner.Layer.Create(ctx, machineName(e.lease))
	defer e.destroy(ctx, machine)
	if err != nil {
		return outcome{}, e.failed(ctx, err, "vm", protocol.CodeJobFailed, "")
	}
	work := path.Join(machine.Home, Work)
	err = e.runner.Layer.Copy(ctx, machine, vm.Transfer{Direction: vm.In, Source: input, Destination: path.Join(work, DirectoryInput)})
	if removeErr := os.RemoveAll(input); removeErr != nil {
		e.logger.Warn("removing the host copy of the lease input failed", "error", removeErr)
	}
	if err != nil {
		return outcome{}, e.failed(ctx, err, "vm", protocol.CodeJobFailed, "")
	}

	if err := e.build(ctx, machine, work); err != nil {
		return outcome{}, err
	}

	output := filepath.Join(scratch, DirectoryOutput)
	started := time.Now()
	content, err := e.collect(ctx, machine, work, output, expected)
	if err != nil {
		return outcome{}, err
	}
	verified := time.Since(started)

	started = time.Now()
	if _, err := e.session.Upload(ctx, filepath.Join(output, FileIPA)); err != nil {
		return outcome{}, e.failed(ctx, err, "upload", protocol.CodeUploadFailed, outputArtifact)
	}
	return outcome{content: content, verify: verified, upload: time.Since(started)}, nil
}

func (e *execution) build(ctx context.Context, machine vm.Machine, work string) error {
	stdout := &lines{writer: e.reporter.Writer(protocol.StreamStdout)}
	stderr := &lines{writer: e.reporter.Writer(protocol.StreamStderr)}
	started := time.Now()
	code, err := e.runner.Layer.Exec(ctx, machine, vm.Command{
		Args:   []string{Script},
		Env:    map[string]string{VariableWork: work},
		Stdout: stdout,
		Stderr: stderr,
	})
	e.elapsed = time.Since(started)
	stdout.finish()
	stderr.finish()
	if err != nil {
		return e.failed(ctx, err, "vm", protocol.CodeJobFailed, "")
	}
	if code != 0 {
		return &failure{
			exit:    code,
			reason:  "build",
			code:    protocol.CodeJobExitNonzero,
			message: fmt.Sprintf("the build script exited with status %d", code),
		}
	}
	return nil
}

// collect copies the IPA and result.json out of the VM and verifies the IPA against the lease. Only the toolchain is
// taken from the VM's result.json; every identity field comes from the verified IPA.
func (e *execution) collect(ctx context.Context, machine vm.Machine, work string, output string, expected verify.Expected) (result, error) {
	for _, name := range []string{FileIPA, FileResult} {
		err := e.runner.Layer.Copy(ctx, machine, vm.Transfer{
			Direction:   vm.Out,
			Source:      path.Join(work, DirectoryOutput, name),
			Destination: filepath.Join(output, name),
		})
		if err != nil {
			return result{}, e.failed(ctx, fmt.Errorf("copy output/%s: %w", name, err), "output", protocol.CodeArtifactNotFound, resultArtifact)
		}
	}
	reported, err := readToolchain(filepath.Join(output, FileResult))
	if err != nil {
		return result{}, e.failed(ctx, err, "output", protocol.CodeArtifactJSONInvalid, resultArtifact)
	}
	verified, err := verify.IPA(filepath.Join(output, FileIPA), expected)
	if err != nil {
		return result{}, e.failed(ctx, err, "verification", protocol.CodeArtifactFailed, resultArtifact)
	}
	reported.Image = e.runner.Image
	return result{
		Artifact:     FileIPA,
		Type:         "ipa",
		Digest:       verified.Digest,
		Size:         verified.Size,
		Identifier:   verified.Identifier,
		VersionName:  verified.VersionName,
		VersionCode:  e.lease.Version.Code,
		BuildNumber:  verified.VersionCode,
		TeamID:       verified.TeamID,
		Signed:       verified.Signed,
		Entitlements: verified.Entitlements,
		Toolchain:    reported,
	}, nil
}

// expected checks the lease carries what verification needs, the identifier cross-checked against the profile keys.
func (e *execution) expected() (verify.Expected, error) {
	identifier := e.lease.Environment[VariableIdentifier]
	signing := e.lease.Signing
	switch {
	case identifier == "":
		return verify.Expected{}, fmt.Errorf("lease environment has no %s", VariableIdentifier)
	case signing.TeamID == "":
		return verify.Expected{}, errors.New("lease has no signing team")
	case len(signing.Certificate) == 0:
		return verify.Expected{}, errors.New("lease has no signing certificate")
	case e.lease.Version.Name == "" || e.lease.Version.Code <= 0:
		return verify.Expected{}, errors.New("lease has no version name or code")
	}
	if _, ok := signing.Profiles[identifier]; !ok {
		return verify.Expected{}, fmt.Errorf("lease has no provisioning profile for %s", identifier)
	}
	for key := range signing.Profiles {
		if !bundleName(key) {
			return verify.Expected{}, fmt.Errorf("lease profile key %q is not a bundle identifier", key)
		}
	}
	return verify.Expected{
		Identifier:  identifier,
		VersionName: e.lease.Version.Name,
		VersionCode: strconv.Itoa(e.lease.Version.Code),
		TeamID:      signing.TeamID,
		Profiles:    signing.Profiles,
	}, nil
}

// failed turns a step's error into a failure, unless the run was stopped, in which case the stop is the failure.
func (e *execution) failed(ctx context.Context, err error, reason string, code protocol.Code, artifact string) error {
	if stopped := stopped(ctx); stopped != nil {
		stopped.cause = err
		return stopped
	}
	kind := ""
	switch artifact {
	case sourceArtifact:
		kind = artifactDownload
	case resultArtifact:
		kind = artifactRead
	case outputArtifact:
		kind = artifactUpload
	}
	return &failure{exit: ExitFailed, reason: reason, code: code, message: err.Error(), artifact: artifact, kind: kind, cause: err}
}

func stopped(ctx context.Context) *failure {
	if ctx.Err() == nil {
		return nil
	}
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, errTimeout):
		return &failure{exit: ExitTimeout, reason: "timeout", code: protocol.CodeJobFailed, message: "the build exceeded the lease timeout"}
	case errors.Is(cause, errCanceled):
		return &failure{exit: ExitFailed, reason: "canceled", code: protocol.CodeJobFailed, message: "the lease was canceled"}
	case errors.Is(cause, errLost):
		return &failure{exit: ExitFailed, reason: "lost", code: protocol.CodeJobFailed, message: "the lease could not be extended before it expired"}
	}
	return &failure{exit: ExitFailed, reason: "stopped", code: protocol.CodeJobFailed, message: "the agent stopped the build"}
}

func (e *execution) report(failed *failure) {
	reported := protocol.Failure{Code: failed.code, Message: truncate(failed.message, messageBytes)}
	if failed.artifact != "" {
		e.reporter.Emit(protocol.Event{Type: protocol.TypeArtifact, Data: protocol.ArtifactData{
			ArtifactID:   failed.artifact,
			ArtifactType: failed.kind,
			Status:       protocol.ArtifactFailed,
			Error:        &reported,
		}})
	}
	duration := 0.0
	if failed.exit != ExitFailed {
		duration = e.elapsed.Seconds()
	}
	e.reporter.Emit(protocol.Event{Type: protocol.TypeExit, Data: protocol.ExitData{
		ExitCode:        failed.exit,
		Reason:          failed.reason,
		Image:           e.runner.Image,
		DurationSeconds: duration,
		Error:           &reported,
	}})
}

func (e *execution) succeed(done outcome) {
	verified := done.verify.Seconds()
	uploaded := done.upload.Seconds()
	e.reporter.Emit(protocol.Event{Type: protocol.TypeExit, Data: protocol.ExitData{
		ExitCode:        0,
		Image:           e.runner.Image,
		DurationSeconds: e.elapsed.Seconds(),
	}})
	e.reporter.Emit(protocol.Event{Type: protocol.TypeArtifact, Data: protocol.ArtifactData{
		ArtifactID:      resultArtifact,
		ArtifactType:    artifactRead,
		Status:          protocol.ArtifactSuccess,
		Content:         done.content,
		Format:          formatJSON,
		DurationSeconds: &verified,
	}})
	e.reporter.Emit(protocol.Event{Type: protocol.TypeArtifact, Data: protocol.ArtifactData{
		ArtifactID:      outputArtifact,
		ArtifactType:    artifactUpload,
		Status:          protocol.ArtifactSuccess,
		DurationSeconds: &uploaded,
	}})
	e.reporter.Emit(protocol.Event{Type: protocol.TypeComplete})
}

func (e *execution) stage(stage string) {
	e.reporter.Emit(protocol.Event{Type: protocol.TypeStage, Data: protocol.StageData{Stage: stage}})
}

// destroy runs with its own bounded context, so a cancelled, timed out or panicking run still removes the VM.
func (e *execution) destroy(ctx context.Context, machine vm.Machine) {
	destroying, cancel := context.WithTimeout(context.WithoutCancel(ctx), positive(e.runner.Grace, defaultGrace))
	defer cancel()
	if err := e.runner.Layer.Destroy(destroying, machine); err != nil {
		e.logger.Error("destroying the vm failed", "machine", machine.Name, "error", err)
	}
}

// extend renews the lease every third of its remaining time. It cancels the run when the server cancels the lease,
// refuses the renewal, or the lease expires before a renewal succeeds.
func (e *execution) extend(ctx context.Context, cancel context.CancelCauseFunc, expiresAt time.Time) {
	for {
		if sleep(ctx, max(time.Until(expiresAt)/3, minimumExtend)) != nil {
			return
		}
		extension, err := e.session.Extend(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err == nil && extension.Canceled:
			e.logger.Info("lease canceled by the server")
			cancel(errCanceled)
			return
		case err == nil:
			expiresAt = deadline(extension.ExpiresAt)
		case refused(err), !time.Now().Before(expiresAt):
			e.logger.Warn("lease lost", "error", err)
			cancel(errLost)
			return
		default:
			e.logger.Warn("extending the lease failed", "error", err)
		}
	}
}

// deadline treats a missing expiry, or one already past because the host clock runs ahead, as the default TTL.
func deadline(expiresAt time.Time) time.Time {
	if time.Until(expiresAt) <= 0 {
		return time.Now().Add(defaultTTL)
	}
	return expiresAt
}

func refused(err error) bool {
	var response *protocol.ResponseError
	if !errors.As(err, &response) {
		return false
	}
	return response.Status >= 400 && response.Status < 500 &&
		response.Status != http.StatusTooManyRequests && response.Status != http.StatusRequestTimeout
}

func writeInput(input string, lease protocol.Lease, variables map[string]string) error {
	profiles := filepath.Join(input, DirectorySigning, DirectoryProfile)
	if err := os.MkdirAll(profiles, 0o700); err != nil {
		return err
	}
	files := map[string][]byte{
		filepath.Join(input, FileEnvironment):                   EncodeEnvironment(variables),
		filepath.Join(input, DirectorySigning, FileCertificate): lease.Signing.Certificate,
		filepath.Join(input, DirectorySigning, FilePassword):    []byte(lease.Signing.Password),
	}
	for identifier, profile := range lease.Signing.Profiles {
		files[filepath.Join(profiles, identifier+ExtensionProfile)] = profile
	}
	for name, data := range files {
		if err := os.WriteFile(name, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func readToolchain(name string) (toolchain, error) {
	file, err := os.Open(name)
	if err != nil {
		return toolchain{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, resultBytes+1))
	if err != nil {
		return toolchain{}, err
	}
	if len(data) > resultBytes {
		return toolchain{}, fmt.Errorf("%s exceeds %d bytes", FileResult, resultBytes)
	}
	var reported struct {
		Toolchain map[string]any `json:"toolchain"`
	}
	if err := json.Unmarshal(data, &reported); err != nil {
		return toolchain{}, fmt.Errorf("%s is not valid json", FileResult)
	}
	return toolchain{
		Fastlane: version(reported.Toolchain["fastlane"]),
		Xcode:    version(reported.Toolchain["xcode"]),
		Flutter:  version(reported.Toolchain["flutter"]),
	}, nil
}

func version(value any) *string {
	text, ok := value.(string)
	if !ok || text == "" {
		return nil
	}
	text = truncate(strings.ToValidUTF8(text, ""), toolchainBytes)
	return &text
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

func machineName(lease protocol.Lease) string {
	var builder strings.Builder
	builder.WriteString("distribution-")
	for _, character := range lease.LeaseID {
		switch {
		case 'a' <= character && character <= 'z', 'A' <= character && character <= 'Z', '0' <= character && character <= '9':
			builder.WriteRune(character)
		default:
			builder.WriteByte('-')
		}
	}
	return builder.String()
}

func timeout(lease protocol.Lease) time.Duration {
	if lease.Timeout <= 0 {
		return defaultTimeout
	}
	return time.Duration(lease.Timeout) * time.Second
}

func positive(value time.Duration, otherwise time.Duration) time.Duration {
	if value <= 0 {
		return otherwise
	}
	return value
}

// scratch creates the lease's private host directory; it holds signing material until the copy into the VM.
func (r *Runner) scratch() (string, error) {
	if r.Directory != "" {
		if err := os.MkdirAll(r.Directory, 0o700); err != nil {
			return "", err
		}
	}
	return os.MkdirTemp(r.Directory, "lease-")
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.Default()
	}
	return r.Logger
}

// lines completes a trailing partial line once the command has ended, so it is reported before the exit event rather
// than when the reporter closes.
type lines struct {
	writer  io.Writer
	mutex   sync.Mutex
	partial bool
}

func (l *lines) Write(data []byte) (int, error) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if len(data) > 0 {
		l.partial = data[len(data)-1] != '\n'
	}
	return l.writer.Write(data)
}

func (l *lines) finish() {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	if l.partial {
		_, _ = l.writer.Write([]byte{'\n'})
		l.partial = false
	}
}
