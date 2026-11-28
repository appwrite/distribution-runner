// Package lease runs one claimed iOS build lease: it fetches the source on the host, builds inside a disposable VM,
// verifies the IPA against the lease, uploads it and reports every step, destroying the VM whatever happens.
package lease

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"syscall"
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

	// MachinePrefix starts the name of every machine a lease runs in; Runner.Prefix scopes it to one runner.
	MachinePrefix = "distribution-"

	ReasonBuild        = "build"
	ReasonTimeout      = "timeout"
	ReasonCanceled     = "canceled"
	ReasonLost         = "lost"
	ReasonStopped      = "stopped"
	ReasonSource       = "source"
	ReasonVM           = "vm"
	ReasonOutput       = "output"
	ReasonVerification = "verification"
	ReasonUpload       = "upload"
	ReasonLease        = "lease"
	ReasonAgent        = "agent"
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
	// artifactBytes is RFC §14's default _APP_DISTRIBUTION_SIZE_LIMIT, the largest IPA the server accepts. It also
	// bounds the source tarball, since both land on the host disk the VMs share.
	artifactBytes = 4_000_000_000
	scratchPrefix = "lease-"
	// scopeBytes is how much of the runner id's SHA-256 names the runner in its machines and scratch directories.
	scopeBytes = 6
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

// failure is a run that ended without a build: it becomes a non-zero exit and an optional failed artifact event, or,
// when requeue is set, the lease handed back so another attempt runs it. message is what the customer sees; cause,
// which can carry host details, is only logged.
type failure struct {
	exit     int
	reason   string
	code     protocol.Code
	message  string
	artifact string
	kind     string
	cause    error
	requeue  bool
}

func (f *failure) Error() string {
	return fmt.Sprintf("lease: %s: %s", f.reason, f.message)
}

func (f *failure) Unwrap() error {
	return f.cause
}

// Infrastructure reports whether a Run error lies with the host, the VM layer or the agent, rather than with the
// build, the lease or the server; a host that keeps failing this way would fail every lease it claims.
func Infrastructure(err error) bool {
	var failed *failure
	return errors.As(err, &failed) && (failed.reason == ReasonVM || failed.reason == ReasonAgent)
}

type execution struct {
	runner   *Runner
	lease    protocol.Lease
	session  *protocol.Session
	reporter *protocol.Reporter
	logger   *slog.Logger
	elapsed  time.Duration
	occupied time.Duration
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
			failed = &failure{exit: ExitFailed, reason: ReasonAgent, code: protocol.CodeJobFailed, message: messageAgent, cause: err}
		}
		if failed.requeue && execution.requeue(ctx) {
			logger.Warn("lease handed back", "reason", failed.reason, "error", failed.message, "cause", failed.cause)
			return failed
		}
		execution.report(failed)
		logger.Warn("lease failed", "reason", failed.reason, "exitCode", failed.exit, "error", failed.message, "cause", failed.cause)
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
			err = &failure{exit: ExitFailed, reason: ReasonAgent, code: protocol.CodeJobFailed, message: messageAgent, cause: fmt.Errorf("panic: %v", recovered)}
		}
	}()

	expected, err := e.expected()
	if err != nil {
		return outcome{}, &failure{exit: ExitFailed, reason: ReasonLease, code: protocol.CodeJobFailed, message: err.Error(), cause: err}
	}
	variables, err := Environment(e.lease)
	if err != nil {
		return outcome{}, &failure{exit: ExitFailed, reason: ReasonLease, code: protocol.CodeJobFailed, message: err.Error(), cause: err}
	}

	scratch, err := e.runner.scratch()
	if err != nil {
		return outcome{}, e.unavailable(ctx, err, ReasonAgent, messageAgent)
	}
	defer os.RemoveAll(scratch)
	input := filepath.Join(scratch, DirectoryInput)

	e.stage(StageSource)
	if err := writeInput(input, e.lease, variables); err != nil {
		return outcome{}, e.unavailable(ctx, err, ReasonAgent, messageAgent)
	}
	if err := e.fetch(ctx, filepath.Join(input, FileSource)); err != nil {
		return outcome{}, err
	}

	e.stage(StageBuild)
	created := time.Now()
	machine, err := e.runner.Layer.Create(ctx, machineName(e.runner.Prefix(), e.lease))
	defer e.destroy(ctx, machine, created)
	if err != nil {
		return outcome{}, e.unavailable(ctx, err, ReasonVM, messageMachine)
	}
	work := path.Join(machine.Home, Work)
	err = e.runner.Layer.Copy(ctx, machine, vm.Transfer{Direction: vm.In, Source: input, Destination: path.Join(work, DirectoryInput)})
	if removeErr := os.RemoveAll(input); removeErr != nil {
		e.logger.Warn("removing the host copy of the lease input failed", "error", removeErr)
	}
	if err != nil {
		return outcome{}, e.unavailable(ctx, err, ReasonVM, messageMachine)
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
		return outcome{}, e.failed(ctx, err, ReasonUpload, protocol.CodeUploadFailed, outputArtifact, uploadMessage(err))
	}
	return outcome{content: content, verify: verified, upload: time.Since(started)}, nil
}

func (e *execution) build(ctx context.Context, machine vm.Machine, work string) error {
	stdout := &lines{writer: e.reporter.Writer(protocol.StreamStdout)}
	stderr := &lines{writer: e.reporter.Writer(protocol.StreamStderr)}
	started := time.Now()
	code, err := e.runner.Layer.Exec(ctx, machine, vm.Command{
		Arguments:   []string{Script},
		Environment: map[string]string{VariableWork: work},
		Stdout:      stdout,
		Stderr:      stderr,
	})
	e.elapsed = time.Since(started)
	stdout.finish()
	stderr.finish()
	if err != nil {
		return e.failed(ctx, err, ReasonVM, protocol.CodeJobFailed, "", messageMachine)
	}
	if code != 0 {
		return &failure{
			exit:    code,
			reason:  ReasonBuild,
			code:    protocol.CodeJobExitNonzero,
			message: fmt.Sprintf("the build script exited with status %d", code),
		}
	}
	return nil
}

// collect copies the IPA and result.json out of the VM, each a regular file under its size limit, and verifies the
// IPA against the lease. Only the toolchain is taken from the VM's result.json; every identity field comes from the
// verified IPA, and the result must fit in the event that reports it.
func (e *execution) collect(ctx context.Context, machine vm.Machine, work string, output string, expected verify.Expected) (result, error) {
	for _, copied := range []struct {
		name  string
		limit int64
	}{{FileIPA, artifactBytes}, {FileResult, resultBytes}} {
		err := e.runner.Layer.Copy(ctx, machine, vm.Transfer{
			Direction:   vm.Out,
			Source:      path.Join(work, DirectoryOutput, copied.name),
			Destination: filepath.Join(output, copied.name),
			Limit:       copied.limit,
		})
		if err != nil {
			code, message := copyFailure(copied.name, copied.limit, err)
			return result{}, e.failed(ctx, err, ReasonOutput, code, resultArtifact, message)
		}
	}
	reported, err := readToolchain(filepath.Join(output, FileResult))
	if err != nil {
		return result{}, e.failed(ctx, err, ReasonOutput, protocol.CodeArtifactJSONInvalid, resultArtifact, err.Error())
	}
	verified, err := verify.IPA(filepath.Join(output, FileIPA), expected, output)
	if err != nil {
		message := "the ipa could not be read for verification"
		if verify.Rejected(err) {
			message = err.Error()
		}
		return result{}, e.failed(ctx, err, ReasonVerification, protocol.CodeArtifactFailed, resultArtifact, message)
	}
	reported.Image = e.runner.Image
	content := result{
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
	}
	if err := e.session.Check(resultEvent(content, 0)); err != nil {
		return result{}, e.failed(ctx, err, ReasonVerification, protocol.CodeArtifactFailed, resultArtifact,
			"the verified result is too large to report: "+err.Error())
	}
	return content, nil
}

// copyFailure is the code and customer message for an output file that could not be copied out.
func copyFailure(name string, limit int64, err error) (protocol.Code, string) {
	output := DirectoryOutput + "/" + name
	switch {
	case errors.Is(err, vm.ErrLimit):
		return protocol.CodeArtifactReadFailed, fmt.Sprintf("%s exceeds %d bytes", output, limit)
	case errors.Is(err, vm.ErrNotFile), errors.Is(err, os.ErrNotExist):
		return protocol.CodeArtifactNotFound, output + " is missing or not a regular file"
	}
	return protocol.CodeArtifactReadFailed, output + " could not be copied out of the build machine"
}

// expected checks the lease carries what verification needs, the identifier cross-checked against the profile keys.
// An empty version name keeps the project's version, which the IPA then reports.
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
	case e.lease.Version.Code <= 0:
		return verify.Expected{}, errors.New("lease has no version code")
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

// failed turns a step's error into a failure carrying message, unless the run was stopped, in which case the stop is
// the failure.
func (e *execution) failed(ctx context.Context, err error, reason string, code protocol.Code, artifact string, message string) error {
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
	return &failure{exit: ExitFailed, reason: reason, code: code, message: message, artifact: artifact, kind: kind, cause: err}
}

// unavailable is a failure of this host before the build started, which another attempt need not share: the lease
// is handed back rather than failed, unless the run was stopped, in which case the stop is the failure.
func (e *execution) unavailable(ctx context.Context, err error, reason string, message string) error {
	if stopped := stopped(ctx); stopped != nil {
		stopped.cause = err
		return stopped
	}
	return &failure{exit: ExitFailed, reason: reason, code: protocol.CodeJobFailed, message: message, cause: err, requeue: true}
}

// requeue delivers the events reported so far, then hands the lease back. It reports whether the server took the lease
// back; when it did not, the caller reports the failure as an exit instead, so the build does not wait for the lease
// to expire.
func (e *execution) requeue(ctx context.Context) bool {
	requeuing, cancel := context.WithTimeout(context.WithoutCancel(ctx), reportTimeout)
	defer cancel()
	if err := e.reporter.Flush(requeuing); err != nil {
		e.logger.Warn("reporting lease events before handing the lease back failed", "error", err)
	}
	if err := e.session.Requeue(requeuing); err != nil {
		e.logger.Warn("handing the lease back failed: reporting the failure instead", "error", err)
		return false
	}
	return true
}

const (
	messageAgent   = "the agent failed"
	messageMachine = "the build machine failed"
)

func uploadMessage(err error) string {
	var response *protocol.ResponseError
	if errors.As(err, &response) {
		return "the upload failed: " + response.Error()
	}
	return "the upload failed"
}

func stopped(ctx context.Context) *failure {
	if ctx.Err() == nil {
		return nil
	}
	cause := context.Cause(ctx)
	switch {
	case errors.Is(cause, errTimeout):
		return &failure{exit: ExitTimeout, reason: ReasonTimeout, code: protocol.CodeJobFailed, message: "the build exceeded the lease timeout"}
	case errors.Is(cause, errCanceled):
		return &failure{exit: ExitFailed, reason: ReasonCanceled, code: protocol.CodeJobFailed, message: "the lease was canceled"}
	case errors.Is(cause, errLost):
		return &failure{exit: ExitFailed, reason: ReasonLost, code: protocol.CodeJobFailed, message: "the lease could not be extended before it expired"}
	}
	return &failure{exit: ExitFailed, reason: ReasonStopped, code: protocol.CodeJobFailed, message: "the agent stopped the build"}
}

// report sends the exit, as success does, then the failed artifact. durationSeconds is the build script's run time
// whenever it ran, and 0 when the run failed before it; occupancySeconds is the VM's, 0 when none was asked for.
func (e *execution) report(failed *failure) {
	reported := protocol.Failure{Code: failed.code, Message: truncate(failed.message, messageBytes)}
	e.reporter.Emit(protocol.Event{Type: protocol.TypeExit, Data: protocol.ExitData{
		ExitCode:         failed.exit,
		Reason:           failed.reason,
		Image:            e.runner.Image,
		DurationSeconds:  e.elapsed.Seconds(),
		OccupancySeconds: e.occupied.Seconds(),
		Error:            &reported,
	}})
	if failed.artifact != "" {
		e.reporter.Emit(protocol.Event{Type: protocol.TypeArtifact, Data: protocol.ArtifactData{
			ArtifactID:   failed.artifact,
			ArtifactType: failed.kind,
			Status:       protocol.ArtifactFailed,
			Error:        &reported,
		}})
	}
}

func (e *execution) succeed(done outcome) {
	uploaded := done.upload.Seconds()
	e.reporter.Emit(protocol.Event{Type: protocol.TypeExit, Data: protocol.ExitData{
		ExitCode:         0,
		Image:            e.runner.Image,
		DurationSeconds:  e.elapsed.Seconds(),
		OccupancySeconds: e.occupied.Seconds(),
	}})
	e.reporter.Emit(resultEvent(done.content, done.verify))
	e.reporter.Emit(protocol.Event{Type: protocol.TypeArtifact, Data: protocol.ArtifactData{
		ArtifactID:      outputArtifact,
		ArtifactType:    artifactUpload,
		Status:          protocol.ArtifactSuccess,
		DurationSeconds: &uploaded,
	}})
	e.reporter.Emit(protocol.Event{Type: protocol.TypeComplete})
}

func resultEvent(content result, verification time.Duration) protocol.Event {
	seconds := verification.Seconds()
	return protocol.Event{Type: protocol.TypeArtifact, Data: protocol.ArtifactData{
		ArtifactID:      resultArtifact,
		ArtifactType:    artifactRead,
		Status:          protocol.ArtifactSuccess,
		Content:         content,
		Format:          formatJSON,
		DurationSeconds: &seconds,
	}}
}

func (e *execution) stage(stage string) {
	e.reporter.Emit(protocol.Event{Type: protocol.TypeStage, Data: protocol.StageData{Stage: stage}})
}

// destroy runs with its own bounded context, so a cancelled, timed out or panicking run still removes the VM. It
// records the VM's occupancy: from created, when its creation started, to the end of its teardown.
func (e *execution) destroy(ctx context.Context, machine vm.Machine, created time.Time) {
	destroying, cancel := context.WithTimeout(context.WithoutCancel(ctx), positive(e.runner.Grace, defaultGrace))
	defer cancel()
	if err := e.runner.Layer.Destroy(destroying, machine); err != nil {
		e.logger.Error("destroying the vm failed", "machine", machine.Name, "error", err)
	}
	e.occupied = time.Since(created)
}

// extend renews the lease every third of its remaining time. It cancels the run when the server cancels the lease,
// refuses the renewal, or the lease expires before a renewal succeeds; no renewal outlives the lease it renews.
func (e *execution) extend(ctx context.Context, cancel context.CancelCauseFunc, expiresAt time.Time) {
	for {
		if sleep(ctx, max(time.Until(expiresAt)/3, minimumExtend)) != nil {
			return
		}
		renewing, stop := context.WithDeadline(ctx, expiresAt)
		extension, err := e.session.Extend(renewing)
		stop()
		switch {
		case ctx.Err() != nil:
			return
		case err == nil && extension.Canceled:
			e.logger.Info("lease canceled by the server")
			cancel(errCanceled)
			return
		case err == nil:
			expiresAt = deadline(extension.ExpiresAt)
		case permanent(err), !time.Now().Before(expiresAt):
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

func permanent(err error) bool {
	var response *protocol.ResponseError
	return errors.As(err, &response) && response.Permanent()
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

// readToolchain reads the toolchain versions from result.json. Its errors describe the file, never the host.
func readToolchain(name string) (toolchain, error) {
	file, err := os.Open(name)
	if err != nil {
		return toolchain{}, fmt.Errorf("%s could not be read", FileResult)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, resultBytes+1))
	if err != nil {
		return toolchain{}, fmt.Errorf("%s could not be read", FileResult)
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

func machineName(prefix string, lease protocol.Lease) string {
	var builder strings.Builder
	builder.WriteString(prefix)
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
		if err := private(r.Directory); err != nil {
			return "", err
		}
	}
	return os.MkdirTemp(r.Directory, scratchPrefix+r.Prefix())
}

// private refuses a directory that is a symlink, that another user owns, or that anyone else may enter.
func private(directory string) error {
	information, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !information.IsDir() {
		return fmt.Errorf("lease: %s is not a directory", directory)
	}
	if information.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("lease: %s must not be accessible to other users (mode %v)", directory, information.Mode().Perm())
	}
	if status, ok := information.Sys().(*syscall.Stat_t); ok && int(status.Uid) != os.Getuid() {
		return fmt.Errorf("lease: %s belongs to user %d, not the agent", directory, status.Uid)
	}
	return nil
}

// Prefix starts the name of every machine and scratch directory this runner creates, and is all Sweep removes. It
// carries a fixed-length digest of the runner id, so two agents on one host, each with its own enrolment, never sweep
// each other's machines, and no runner's prefix starts another's.
func (r *Runner) Prefix() string {
	digest := sha256.Sum256([]byte(r.Credentials.RunnerID))
	return MachinePrefix + hex.EncodeToString(digest[:scopeBytes]) + "-"
}

// Sweep removes what an earlier process of this runner left behind when it crashed or was killed: its lease scratch
// directories under Directory, which can hold signing material, and its machines. It refuses to run when the golden
// image carries the runner's prefix, since that would destroy it. Run it before the first lease, never while one runs.
func (r *Runner) Sweep(ctx context.Context) error {
	prefix := r.Prefix()
	if strings.HasPrefix(r.Image, prefix) {
		return fmt.Errorf("lease: the golden image %q is named like this runner's machines (%s*): rename it", r.Image, prefix)
	}
	var failures []error
	if r.Directory != "" {
		entries, err := os.ReadDir(r.Directory)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), scratchPrefix+prefix) {
				failures = append(failures, os.RemoveAll(filepath.Join(r.Directory, entry.Name())))
			}
		}
	}
	failures = append(failures, r.Layer.Sweep(ctx, prefix))
	return errors.Join(failures...)
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
