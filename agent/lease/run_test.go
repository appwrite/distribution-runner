package lease

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/appwrite/distribution-runner/agent/protocol"
	"github.com/appwrite/distribution-runner/agent/verify"
	"github.com/appwrite/distribution-runner/agent/vm"
)

const (
	testRunnerID    = "runner1"
	testRunnerToken = "runner-token"
	testLeaseID     = "lease1"
	testLeaseToken  = "lease-token"
	testIdentifier  = "com.example.app"
	testTeam        = "ABCDE12345"
	testImage       = "golden-test"
	leasePath       = "/v1/distribution/runners/" + testRunnerID + "/leases/" + testLeaseID
	tarballPath     = "/tarball"
	urlSentinel     = "url-sentinel-7f3a"
	headerSentinel  = "header-sentinel-91c2"
)

type recorded struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

type event struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type server struct {
	t      *testing.T
	server *httptest.Server

	mutex    sync.Mutex
	requests []recorded
	events   []event
	uploaded bytes.Buffer
	sources  int
	// extend answers PATCH on the lease; source returns the descriptor for the nth request, from 1.
	extend func() protocol.Extension
	source func(count int) protocol.Source
}

func newServer(t *testing.T) *server {
	t.Helper()
	fake := &server{t: t}
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH "+leasePath, fake.serveExtend)
	mux.HandleFunc("POST "+leasePath+"/events", fake.serveEvents)
	mux.HandleFunc("POST "+leasePath+"/artifacts", fake.serveUpload)
	mux.HandleFunc("GET "+leasePath+"/source", fake.serveSource)
	mux.HandleFunc("GET "+tarballPath, fake.serveTarball)
	fake.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return
		}
		fake.mutex.Lock()
		fake.requests = append(fake.requests, recorded{Method: request.Method, URL: request.URL.String(), Header: request.Header.Clone(), Body: body})
		fake.mutex.Unlock()
		request.Body = io.NopCloser(bytes.NewReader(body))
		mux.ServeHTTP(writer, request)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (s *server) serveExtend(writer http.ResponseWriter, _ *http.Request) {
	extension := protocol.Extension{ExpiresAt: time.Now().Add(time.Hour)}
	if s.extend != nil {
		extension = s.extend()
	}
	respond(writer, http.StatusOK, extension)
}

func (s *server) serveEvents(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	var batch struct {
		Events []event `json:"events"`
	}
	if err := json.Unmarshal(body, &batch); err != nil {
		s.t.Errorf("decode events: %v", err)
	}
	s.mutex.Lock()
	s.events = append(s.events, batch.Events...)
	s.mutex.Unlock()
	writer.WriteHeader(http.StatusNoContent)
}

func (s *server) serveUpload(writer http.ResponseWriter, request *http.Request) {
	file, _, err := request.FormFile(protocol.UploadField)
	if err != nil {
		s.t.Errorf("upload has no %s part: %v", protocol.UploadField, err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	defer file.Close()
	var start, end, total int64
	if _, err := fmt.Sscanf(request.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil {
		s.t.Errorf("content-range %q: %v", request.Header.Get("Content-Range"), err)
	}
	chunk, _ := io.ReadAll(file)
	s.mutex.Lock()
	if int64(s.uploaded.Len()) == start {
		s.uploaded.Write(chunk)
	}
	uploaded := (end + protocol.ChunkSize) / protocol.ChunkSize
	s.mutex.Unlock()
	respond(writer, http.StatusCreated, map[string]any{
		"$id":            request.Header.Get("X-Appwrite-Id"),
		"chunksUploaded": uploaded,
		"chunksTotal":    (total + protocol.ChunkSize - 1) / protocol.ChunkSize,
	})
}

func (s *server) serveSource(writer http.ResponseWriter, _ *http.Request) {
	s.mutex.Lock()
	s.sources++
	count := s.sources
	s.mutex.Unlock()
	descriptor := s.tarball()
	if s.source != nil {
		descriptor = s.source(count)
	}
	respond(writer, http.StatusOK, descriptor)
}

func (s *server) tarball() protocol.Source {
	return protocol.Source{
		URL:     s.server.URL + tarballPath + "?token=" + urlSentinel,
		Headers: protocol.Map[string]{"Authorization": "Bearer " + headerSentinel},
		Format:  sourceFormat,
	}
}

func (s *server) serveTarball(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer "+headerSentinel || request.URL.Query().Get("token") != urlSentinel {
		writer.WriteHeader(http.StatusForbidden)
		return
	}
	var archive bytes.Buffer
	compressor := gzip.NewWriter(&archive)
	_, _ = compressor.Write([]byte("repository"))
	_ = compressor.Close()
	_, _ = writer.Write(archive.Bytes())
}

func respond(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

func (s *server) recorded() []recorded {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return slices.Clone(s.requests)
}

func (s *server) sourceCount() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.sources
}

func (s *server) upload() []byte {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return bytes.Clone(s.uploaded.Bytes())
}

func (s *server) received() []event {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return slices.Clone(s.events)
}

func (s *server) uploads() []recorded {
	var uploads []recorded
	for _, request := range s.recorded() {
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL, "/artifacts") {
			uploads = append(uploads, request)
		}
	}
	return uploads
}

func (s *server) types() []string {
	var types []string
	for _, received := range s.received() {
		types = append(types, received.Type)
	}
	return types
}

func (s *server) find(t *testing.T, eventType string, artifact string) event {
	t.Helper()
	for _, received := range s.received() {
		if received.Type != eventType {
			continue
		}
		if artifact != "" {
			var data protocol.ArtifactData
			if err := json.Unmarshal(received.Data, &data); err != nil || data.ArtifactID != artifact {
				continue
			}
		}
		return received
	}
	t.Fatalf("no %s %s event in %v", eventType, artifact, s.types())
	return event{}
}

func (s *server) lease() protocol.Lease {
	return protocol.Lease{
		LeaseID:   testLeaseID,
		Token:     testLeaseToken,
		ExpiresAt: time.Now().Add(time.Hour),
		Endpoint:  s.server.URL + "/v1",
		Query:     protocol.Map[string]{"project": "project1"},
		BuildID:   "build1",
		Attempt:   1,
		Timeout:   60,
		Platform:  "ios",
		Framework: "flutter",
		Source:    protocol.Checkout{RootDirectory: "apps/mobile", Commit: "abc123"},
		Environment: protocol.Map[string]{
			VariableIdentifier:     testIdentifier,
			"DISTRIBUTION_SCHEME":  "Runner",
			"CUSTOMER_FLAG":        "it's \"quoted\" $HOME `x`\nnext",
			VariableVersionName:    "ignored",
			"DISTRIBUTION_TEAM_ID": "ignored",
		},
		Commands: protocol.Commands{Install: "flutter pub get", Build: "flutter build ipa"},
		Signing: protocol.Signing{
			Certificate: []byte("certificate-sentinel-3b8e"),
			Password:    "password-sentinel-d41c",
			Profiles:    protocol.Map[[]byte]{testIdentifier: []byte("profile-sentinel-a9f0")},
			TeamID:      testTeam,
		},
		Version: protocol.Version{Name: "1.4.0", Code: 57},
	}
}

type logs struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (l *logs) Write(data []byte) (int, error) {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	return l.buffer.Write(data)
}

func (l *logs) String() string {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	return l.buffer.String()
}

type harness struct {
	server *server
	fake   *vm.Fake
	runner *Runner
	logs   *logs
}

func newHarness(t *testing.T, build func(_ context.Context, work string, environment map[string]string) error) *harness {
	t.Helper()
	fake := newServer(t)
	client, err := protocol.New(fake.server.URL+"/v1", fake.server.Client(), true)
	if err != nil {
		t.Fatalf("protocol.New: %v", err)
	}
	machines := &vm.Fake{Directory: t.TempDir(), Build: build}
	output := &logs{}
	scratch := privateDirectory(t)
	return &harness{
		server: fake,
		fake:   machines,
		logs:   output,
		runner: &Runner{
			Client:      client,
			Credentials: protocol.Credentials{RunnerID: testRunnerID, Token: testRunnerToken},
			Layer:       machines,
			Image:       testImage,
			Directory:   scratch,
			Download:    fake.server.Client(),
			AllowHTTP:   true,
			Logger:      slog.New(slog.NewTextHandler(output, &slog.HandlerOptions{Level: slog.LevelDebug})),
			Interval:    10 * time.Millisecond,
			Grace:       5 * time.Second,
		},
	}
}

// privateDirectory is a temporary directory only its owner may enter, as Runner.Directory must be.
func privateDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func (h *harness) operations() []vm.Operation {
	var operations []vm.Operation
	for _, call := range h.fake.Calls() {
		operations = append(operations, call.Operation)
	}
	return operations
}

func (h *harness) assertDestroyed(t *testing.T) {
	t.Helper()
	if !slices.Contains(h.operations(), vm.OperationDestroy) {
		t.Errorf("destroy was not called: %v", h.operations())
	}
	if live := h.fake.Live(); len(live) != 0 {
		t.Errorf("machines left running: %v", live)
	}
	entries, err := os.ReadDir(h.runner.Directory)
	if err != nil || len(entries) != 0 {
		t.Errorf("host scratch directory not removed: %v %v", entries, err)
	}
}

func (h *harness) assertNoUpload(t *testing.T) {
	t.Helper()
	if uploads := h.server.uploads(); len(uploads) != 0 {
		t.Errorf("uploaded %d chunks, want none", len(uploads))
	}
	if slices.Contains(h.server.types(), protocol.TypeComplete) {
		t.Errorf("complete sent after a failure: %v", h.server.types())
	}
}

func exitData(t *testing.T, received event) protocol.ExitData {
	t.Helper()
	var data protocol.ExitData
	if err := json.Unmarshal(received.Data, &data); err != nil {
		t.Fatalf("decode exit: %v", err)
	}
	return data
}

func assertFailure(t *testing.T, err error, reason string) *failure {
	t.Helper()
	var failed *failure
	if !errors.As(err, &failed) {
		t.Fatalf("Run returned %v, want a failure", err)
	}
	if failed.reason != reason {
		t.Errorf("reason = %q, want %q (%s)", failed.reason, reason, failed.message)
	}
	return failed
}

// padded builds the sample IPA and adds an incompressible resource so the upload needs two chunks.
func padded(ctx context.Context, work string) error {
	if err := Sample(ctx, work); err != nil {
		return err
	}
	name := filepath.Join(work, DirectoryOutput, FileIPA)
	original, err := zip.OpenReader(name)
	if err != nil {
		return err
	}
	defer original.Close()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range original.File {
		if err := writer.Copy(entry); err != nil {
			return err
		}
	}
	resource, err := writer.CreateHeader(&zip.FileHeader{Name: "Payload/App.app/Assets.car", Method: zip.Store})
	if err != nil {
		return err
	}
	if _, err := io.CopyN(resource, rand.Reader, protocol.ChunkSize+1024); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return os.WriteFile(name, buffer.Bytes(), 0o600)
}

// blocking is a build that runs until its context ends.
func blocking(*testing.T) func(context.Context, string, map[string]string) error {
	return func(ctx context.Context, _ string, _ map[string]string) error {
		<-ctx.Done()
		return ctx.Err()
	}
}

func TestRunSuccess(t *testing.T) {
	var environment []byte
	var modes = map[string]os.FileMode{}
	h := newHarness(t, func(ctx context.Context, work string, _ map[string]string) error {
		var err error
		environment, err = os.ReadFile(filepath.Join(work, DirectoryInput, FileEnvironment))
		if err != nil {
			return err
		}
		_ = filepath.WalkDir(filepath.Join(work, DirectoryInput), func(name string, entry os.DirEntry, err error) error {
			if err == nil && !entry.IsDir() {
				information, _ := entry.Info()
				relative, _ := filepath.Rel(work, name)
				modes[filepath.ToSlash(relative)] = information.Mode().Perm()
			}
			return nil
		})
		return padded(ctx, work)
	})
	lease := h.server.lease()

	if err := h.runner.Run(context.Background(), lease); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{
		protocol.TypeStart, protocol.TypeStage, protocol.TypeStage,
		protocol.TypeExit, protocol.TypeArtifact, protocol.TypeArtifact, protocol.TypeComplete,
	}
	if got := h.server.types(); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	received := h.server.received()
	for index, stage := range []string{StageSource, StageBuild} {
		var data protocol.StageData
		_ = json.Unmarshal(received[index+1].Data, &data)
		if data.Stage != stage {
			t.Errorf("stage %d = %q, want %q", index, data.Stage, stage)
		}
	}
	exit := exitData(t, received[3])
	if exit.ExitCode != 0 || exit.Image != testImage || exit.Error != nil {
		t.Errorf("exit = %+v", exit)
	}

	var resultEvent struct {
		protocol.ArtifactData
		Content result `json:"content"`
	}
	if err := json.Unmarshal(received[4].Data, &resultEvent); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	content := resultEvent.Content
	uploaded := h.server.upload()
	sum := sha256.Sum256(uploaded)
	if resultEvent.ArtifactID != resultArtifact || resultEvent.Status != protocol.ArtifactSuccess || resultEvent.Format != formatJSON {
		t.Errorf("result event = %+v", resultEvent.ArtifactData)
	}
	if content.Digest != hex.EncodeToString(sum[:]) || content.Size != int64(len(uploaded)) {
		t.Errorf("result digest %s size %d do not match the upload", content.Digest, content.Size)
	}
	if content.Identifier != testIdentifier || content.VersionName != "1.4.0" || content.VersionCode != 57 ||
		content.BuildNumber != "57" || content.TeamID != testTeam || !content.Signed || content.Type != "ipa" {
		t.Errorf("result content = %+v", content)
	}
	if content.Toolchain.Image != testImage || content.Toolchain.Xcode != nil {
		t.Errorf("toolchain = %+v, want the agent's image and the vm's versions", content.Toolchain)
	}
	var output protocol.ArtifactData
	_ = json.Unmarshal(received[5].Data, &output)
	if output.ArtifactID != outputArtifact || output.Status != protocol.ArtifactSuccess {
		t.Errorf("output event = %+v", output)
	}

	uploads := h.server.uploads()
	if len(uploads) != 2 {
		t.Fatalf("uploaded %d chunks, want 2", len(uploads))
	}
	total := len(uploaded)
	if got := uploads[0].Header.Get("Content-Range"); got != fmt.Sprintf("bytes 0-%d/%d", protocol.ChunkSize-1, total) {
		t.Errorf("first content-range = %q", got)
	}
	if id := uploads[0].Header.Get("X-Appwrite-Id"); id == "" || uploads[1].Header.Get("X-Appwrite-Id") != id {
		t.Errorf("x-appwrite-id = %q then %q, want one minted id on every chunk", id, uploads[1].Header.Get("X-Appwrite-Id"))
	}
	for _, request := range h.server.recorded() {
		if strings.HasPrefix(request.URL, "/v1/") && request.Header.Get("Authorization") != "Bearer "+testLeaseToken {
			t.Errorf("%s %s: Authorization = %q", request.Method, request.URL, request.Header.Get("Authorization"))
		}
	}

	wantOperations := []vm.Operation{vm.OperationCreate, vm.OperationCopy, vm.OperationExec, vm.OperationCopy, vm.OperationCopy, vm.OperationDestroy}
	if got := h.operations(); !slices.Equal(got, wantOperations) {
		t.Errorf("vm calls = %v, want %v", got, wantOperations)
	}
	calls := h.fake.Calls()
	if copied := calls[1].Transfer; copied.Direction != vm.In || copied.Destination != "/Users/lume/distribution/input" {
		t.Errorf("copy in = %+v", copied)
	}
	if exec := calls[2]; !slices.Equal(exec.Arguments, []string{Script}) || len(exec.Environment) != 1 || exec.Environment[VariableWork] != "/Users/lume/distribution" {
		t.Errorf("exec = %+v", exec)
	}
	h.assertDestroyed(t)

	variables, err := ParseEnvironment(environment)
	if err != nil {
		t.Fatalf("parse input/environment: %v", err)
	}
	for name, value := range map[string]string{
		VariableIdentifier:     testIdentifier,
		"DISTRIBUTION_SCHEME":  "Runner",
		"CUSTOMER_FLAG":        lease.Environment["CUSTOMER_FLAG"],
		VariablePlatform:       "ios",
		VariableFramework:      "flutter",
		VariableRootDirectory:  "apps/mobile",
		VariableInstallCommand: "flutter pub get",
		VariableBuildCommand:   "flutter build ipa",
		VariableVersionName:    "1.4.0",
		VariableVersionCode:    "57",
		VariableTeamID:         testTeam,
		VariableBuildID:        "build1",
		VariableCI:             "true",
	} {
		if variables[name] != value {
			t.Errorf("input/environment %s = %q, want %q", name, variables[name], value)
		}
	}
	for _, name := range []string{
		"input/environment", "input/source.tar.gz", "input/signing/certificate.p12", "input/signing/password",
		"input/signing/profiles/" + testIdentifier + ExtensionProfile,
	} {
		if mode, ok := modes[name]; !ok || mode != 0o600 {
			t.Errorf("%s mode = %v (present %v), want 0600", name, mode, ok)
		}
	}
}

func TestRunBuildFailure(t *testing.T) {
	h := newHarness(t, func(context.Context, string, map[string]string) error { return &vm.ExitError{Code: 65} })

	err := h.runner.Run(context.Background(), h.server.lease())

	assertFailure(t, err, "build")
	exit := exitData(t, h.server.find(t, protocol.TypeExit, ""))
	if exit.ExitCode != 65 || exit.Reason != "build" || exit.Error == nil || exit.Error.Code != protocol.CodeJobExitNonzero {
		t.Errorf("exit = %+v", exit)
	}
	if types := h.server.types(); types[len(types)-1] != protocol.TypeExit || slices.Contains(types, protocol.TypeArtifact) {
		t.Errorf("events = %v, want the exit last and no artifact", types)
	}
	h.assertNoUpload(t)
	h.assertDestroyed(t)
}

func TestRunVerificationFailure(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, work string, _ map[string]string) error { return Sample(ctx, work) })
	lease := h.server.lease()
	lease.Environment[FixtureIdentifier] = "com.example.other"

	err := h.runner.Run(context.Background(), lease)

	failed := assertFailure(t, err, "verification")
	if !errors.Is(failed, verify.ErrIdentifier) {
		t.Errorf("failure %v does not match verify.ErrIdentifier", failed)
	}
	var artifact protocol.ArtifactData
	_ = json.Unmarshal(h.server.find(t, protocol.TypeArtifact, resultArtifact).Data, &artifact)
	if artifact.Status != protocol.ArtifactFailed || artifact.Error == nil || artifact.Error.Code != protocol.CodeArtifactFailed {
		t.Errorf("result artifact = %+v", artifact)
	}
	exit := exitData(t, h.server.find(t, protocol.TypeExit, ""))
	if exit.ExitCode != ExitFailed || exit.Reason != "verification" || exit.Error == nil || exit.DurationSeconds <= 0 {
		t.Errorf("exit = %+v, want the build script's run time", exit)
	}
	if types := h.server.types(); !slices.Equal(types[len(types)-2:], []string{protocol.TypeExit, protocol.TypeArtifact}) {
		t.Errorf("events = %v, want the exit, then the failed artifact", types)
	}
	if !strings.Contains(artifact.Error.Message, "com.example.other") {
		t.Errorf("message %q does not name the mismatch", artifact.Error.Message)
	}
	h.assertNoUpload(t)
	h.assertDestroyed(t)
}

func TestRunExtendCanceled(t *testing.T) {
	h := newHarness(t, blocking(t))
	h.server.extend = func() protocol.Extension {
		return protocol.Extension{ExpiresAt: time.Now().Add(time.Hour), Canceled: true}
	}
	lease := h.server.lease()
	lease.ExpiresAt = time.Now().Add(300 * time.Millisecond)

	err := h.runner.Run(context.Background(), lease)

	assertFailure(t, err, "canceled")
	exit := exitData(t, h.server.find(t, protocol.TypeExit, ""))
	if exit.ExitCode != ExitFailed || exit.Reason != "canceled" {
		t.Errorf("exit = %+v", exit)
	}
	h.assertNoUpload(t)
	h.assertDestroyed(t)
}

func TestRunTimeout(t *testing.T) {
	h := newHarness(t, blocking(t))
	lease := h.server.lease()
	lease.Timeout = 1
	started := time.Now()

	err := h.runner.Run(context.Background(), lease)

	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("Run took %v, want under 2s", elapsed)
	}
	assertFailure(t, err, "timeout")
	exit := exitData(t, h.server.find(t, protocol.TypeExit, ""))
	if exit.ExitCode != ExitTimeout || exit.Reason != "timeout" || exit.DurationSeconds <= 0 {
		t.Errorf("exit = %+v", exit)
	}
	h.assertNoUpload(t)
	h.assertDestroyed(t)
}

func TestRunPanicDestroysMachine(t *testing.T) {
	h := newHarness(t, func(context.Context, string, map[string]string) error { panic("build exploded") })

	err := h.runner.Run(context.Background(), h.server.lease())

	assertFailure(t, err, "agent")
	exit := exitData(t, h.server.find(t, protocol.TypeExit, ""))
	if exit.ExitCode != ExitFailed || exit.Error == nil || exit.Error.Code != protocol.CodeJobFailed {
		t.Errorf("exit = %+v", exit)
	}
	h.assertNoUpload(t)
	h.assertDestroyed(t)
}

func TestRunSourceFailure(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, work string, _ map[string]string) error { return Sample(ctx, work) })
	h.server.source = func(int) protocol.Source {
		return protocol.Source{URL: h.server.server.URL + tarballPath, Format: sourceFormat}
	}

	err := h.runner.Run(context.Background(), h.server.lease())

	assertFailure(t, err, "source")
	var artifact protocol.ArtifactData
	_ = json.Unmarshal(h.server.find(t, protocol.TypeArtifact, sourceArtifact).Data, &artifact)
	if artifact.Status != protocol.ArtifactFailed || artifact.Error == nil || artifact.Error.Code != protocol.CodeDownloadHTTPError {
		t.Errorf("source artifact = %+v", artifact)
	}
	if h.server.sourceCount() != sourceAttempts {
		t.Errorf("source descriptor requested %d times, want %d", h.server.sourceCount(), sourceAttempts)
	}
	if slices.Contains(h.operations(), vm.OperationCreate) {
		t.Errorf("a vm was created without source: %v", h.operations())
	}
	h.assertNoUpload(t)
}

func TestRunNeverLogsSourceURL(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, work string, _ map[string]string) error { return Sample(ctx, work) })
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	h.server.source = func(count int) protocol.Source {
		if count == 1 {
			return protocol.Source{
				URL:     unreachable.URL + tarballPath + "?token=" + urlSentinel,
				Headers: protocol.Map[string]{"Authorization": "Bearer " + headerSentinel},
				Format:  sourceFormat,
			}
		}
		return h.server.tarball()
	}

	if err := h.runner.Run(context.Background(), h.server.lease()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if h.server.sourceCount() != 2 {
		t.Errorf("source descriptor requested %d times, want 2 (one retry)", h.server.sourceCount())
	}
	if !strings.Contains(h.logs.String(), "source download failed") {
		t.Errorf("the failed attempt was not logged:\n%s", h.logs.String())
	}
	events, _ := json.Marshal(h.server.received())
	for _, secret := range []string{urlSentinel, headerSentinel, tarballPath + "?"} {
		if strings.Contains(h.logs.String(), secret) {
			t.Errorf("logs contain %q:\n%s", secret, h.logs.String())
		}
		if bytes.Contains(events, []byte(secret)) {
			t.Errorf("events contain %q", secret)
		}
	}
}

func TestRunNeverLeaksSigningSecrets(t *testing.T) {
	var environment []byte
	signing := map[string][]byte{}
	h := newHarness(t, func(ctx context.Context, work string, _ map[string]string) error {
		input := filepath.Join(work, DirectoryInput)
		environment, _ = os.ReadFile(filepath.Join(input, FileEnvironment))
		_ = filepath.WalkDir(filepath.Join(input, DirectorySigning), func(name string, entry os.DirEntry, err error) error {
			if err == nil && !entry.IsDir() {
				signing[name], _ = os.ReadFile(name)
			}
			return nil
		})
		return Sample(ctx, work)
	})
	lease := h.server.lease()

	if err := h.runner.Run(context.Background(), lease); err != nil {
		t.Fatalf("Run: %v", err)
	}

	secrets := [][]byte{lease.Signing.Certificate, []byte(lease.Signing.Password), lease.Signing.Profiles[testIdentifier]}
	var forms [][]byte
	for _, secret := range secrets {
		forms = append(forms, secret, []byte(base64.StdEncoding.EncodeToString(secret)), []byte(base64.RawURLEncoding.EncodeToString(secret)))
	}
	var places = map[string][]byte{
		"logs":              []byte(h.logs.String()),
		"input/environment": environment,
	}
	for index, received := range h.server.received() {
		places["event "+strconv.Itoa(index)] = received.Data
	}
	for index, request := range h.server.recorded() {
		headers, _ := json.Marshal(request.Header)
		places["request url "+strconv.Itoa(index)] = []byte(request.URL)
		places["request headers "+strconv.Itoa(index)] = headers
	}
	for index, call := range h.fake.Calls() {
		recorded, _ := json.Marshal(struct {
			Arguments   []string
			Environment map[string]string
		}{call.Arguments, call.Environment})
		places["vm call "+strconv.Itoa(index)] = recorded
	}
	for place, data := range places {
		for _, form := range forms {
			if bytes.Contains(data, form) {
				t.Errorf("%s contains secret %q", place, form)
			}
		}
	}
	for index, secret := range secrets {
		found := false
		for _, data := range signing {
			found = found || bytes.Equal(data, secret)
		}
		if !found {
			t.Errorf("secret %d did not reach input/signing", index)
		}
	}
}

func TestRunRejectsLeaseWithoutProfile(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, work string, _ map[string]string) error { return Sample(ctx, work) })
	lease := h.server.lease()
	lease.Signing.Profiles = protocol.Map[[]byte]{"com.example.other": []byte("profile")}

	err := h.runner.Run(context.Background(), lease)

	assertFailure(t, err, "lease")
	if len(h.fake.Calls()) != 0 {
		t.Errorf("vm calls for an invalid lease: %v", h.operations())
	}
	h.assertNoUpload(t)
}

func TestRunHidesHostDetailsFromInfrastructureFailures(t *testing.T) {
	h := newHarness(t, blocking(t))
	h.fake.Fail = func(call vm.Call) error {
		if call.Operation == vm.OperationCreate {
			return errors.New("lume clone: /Users/agent/.lume/golden: No space left on device")
		}
		return nil
	}

	err := h.runner.Run(context.Background(), h.server.lease())

	failed := assertFailure(t, err, ReasonVM)
	exit := exitData(t, h.server.find(t, protocol.TypeExit, ""))
	if exit.Error == nil || exit.Error.Message != messageMachine || exit.DurationSeconds != 0 {
		t.Errorf("exit = %+v, want the generic message and no run time", exit)
	}
	if events, _ := json.Marshal(h.server.received()); bytes.Contains(events, []byte("/Users/agent")) {
		t.Errorf("events carry a host path: %s", events)
	}
	if !strings.Contains(h.logs.String(), "No space left on device") || failed.cause == nil {
		t.Errorf("the cause was not logged:\n%s", h.logs.String())
	}
}

func TestRunRefusesAnOversizedResult(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, work string, _ map[string]string) error {
		if err := Sample(ctx, work); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(work, DirectoryOutput, FileResult), bytes.Repeat([]byte(" "), resultBytes+1), 0o600)
	})

	err := h.runner.Run(context.Background(), h.server.lease())

	assertFailure(t, err, ReasonOutput)
	var artifact protocol.ArtifactData
	_ = json.Unmarshal(h.server.find(t, protocol.TypeArtifact, resultArtifact).Data, &artifact)
	if artifact.Error == nil || artifact.Error.Code != protocol.CodeArtifactReadFailed || !strings.Contains(artifact.Error.Message, "output/result.json exceeds") {
		t.Errorf("result artifact = %+v", artifact.Error)
	}
	h.assertNoUpload(t)
	h.assertDestroyed(t)
}

func TestRunRefusesAResultTooLargeToReport(t *testing.T) {
	h := newHarness(t, func(ctx context.Context, work string, _ map[string]string) error {
		if err := Sample(ctx, work); err != nil {
			return err
		}
		profile, err := os.ReadFile(filepath.Join(work, DirectoryInput, DirectorySigning, DirectoryProfile, testIdentifier+ExtensionProfile))
		if err != nil {
			return err
		}
		return verify.Sample(filepath.Join(work, DirectoryOutput, FileIPA), verify.Spec{
			Identifier:   testIdentifier,
			VersionName:  "1.4.0",
			VersionCode:  "57",
			TeamID:       testTeam,
			Profile:      profile,
			Entitlements: map[string]any{"padding": strings.Repeat("é", 200_000)},
		})
	})

	err := h.runner.Run(context.Background(), h.server.lease())

	assertFailure(t, err, ReasonVerification)
	var artifact protocol.ArtifactData
	_ = json.Unmarshal(h.server.find(t, protocol.TypeArtifact, resultArtifact).Data, &artifact)
	if artifact.Error == nil || !strings.Contains(artifact.Error.Message, "too large to report") {
		t.Errorf("result artifact = %+v", artifact.Error)
	}
	if slices.Contains(h.server.types(), protocol.TypeComplete) {
		t.Error("a build whose result could not be reported completed")
	}
	h.assertNoUpload(t)
}

func TestRunRefusesAnOversizedSource(t *testing.T) {
	h := newHarness(t, blocking(t))
	huge := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Length", strconv.FormatInt(artifactBytes+1, 10))
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(huge.Close)
	h.server.source = func(int) protocol.Source {
		return protocol.Source{URL: huge.URL + tarballPath, Format: sourceFormat}
	}

	err := h.runner.Run(context.Background(), h.server.lease())

	assertFailure(t, err, ReasonSource)
	if h.server.sourceCount() != 1 {
		t.Errorf("an oversized source was requested %d times, want once", h.server.sourceCount())
	}
	h.assertNoUpload(t)
}

func TestDownloaderStripsDescriptorHeadersOnAnotherHost(t *testing.T) {
	received := make(chan http.Header, 2)
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- request.Header.Clone()
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/same" {
			http.Redirect(writer, request, "/final", http.StatusFound)
			return
		}
		if request.URL.Path == "/final" {
			received <- request.Header.Clone()
			return
		}
		http.Redirect(writer, request, target.URL+"/final", http.StatusFound)
	}))
	t.Cleanup(origin.Close)
	runner := &Runner{AllowHTTP: true}
	headers := map[string]string{"Private-Token": headerSentinel}

	for _, test := range []struct {
		path string
		kept bool
	}{{path: "/other", kept: false}, {path: "/same", kept: true}} {
		request, err := http.NewRequest(http.MethodGet, origin.URL+test.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Private-Token", headerSentinel)
		response, err := runner.downloader(headers).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if got := (<-received).Get("Private-Token") == headerSentinel; got != test.kept {
			t.Errorf("%s: token forwarded %v, want %v", test.path, got, test.kept)
		}
	}
}

func TestRunLosesALeaseWhoseExtensionHangs(t *testing.T) {
	h := newHarness(t, blocking(t))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.server.extend = func() protocol.Extension {
		<-release
		return protocol.Extension{}
	}
	lease := h.server.lease()
	lease.ExpiresAt = time.Now().Add(300 * time.Millisecond)
	started := time.Now()

	err := h.runner.Run(context.Background(), lease)

	assertFailure(t, err, ReasonLost)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("Run took %v after the lease expired", elapsed)
	}
	h.assertNoUpload(t)
	h.assertDestroyed(t)
}

func TestRunnerSweep(t *testing.T) {
	h := newHarness(t, blocking(t))
	stale := filepath.Join(h.runner.Directory, scratchPrefix+"123")
	kept := filepath.Join(h.runner.Directory, "state.json")
	machine := filepath.Join(h.fake.Directory, "fake-vm-"+MachinePrefix+"old-1")
	for _, directory := range []string{stale, machine} {
		if err := os.MkdirAll(filepath.Join(directory, DirectorySigning), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(kept, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := h.runner.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	for _, gone := range []string{stale, machine} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the sweep: %v", gone, err)
		}
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("the sweep removed %s: %v", kept, err)
	}
}

func TestRunRefusesAScratchDirectoryOthersCanEnter(t *testing.T) {
	h := newHarness(t, blocking(t))
	if err := os.Chmod(h.runner.Directory, 0o755); err != nil {
		t.Fatal(err)
	}

	err := h.runner.Run(context.Background(), h.server.lease())

	assertFailure(t, err, ReasonAgent)
	if len(h.fake.Calls()) != 0 {
		t.Errorf("vm calls with a shared scratch directory: %v", h.operations())
	}
}

func TestSampleStopsWhenTheContextEnds(t *testing.T) {
	work := t.TempDir()
	input := filepath.Join(work, DirectoryInput)
	variables := EncodeEnvironment(map[string]string{VariableIdentifier: testIdentifier, FixtureSleep: "3600"})
	files := map[string][]byte{
		FileEnvironment: variables,
		FileSource:      []byte("source"),
		filepath.Join(DirectorySigning, FileCertificate):                                   []byte("certificate"),
		filepath.Join(DirectorySigning, FilePassword):                                      []byte("password"),
		filepath.Join(DirectorySigning, DirectoryProfile, testIdentifier+ExtensionProfile): []byte("profile"),
	}
	for name, data := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(input, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(input, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := Sample(ctx, work); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sample = %v, want the deadline", err)
	}
	if _, err := os.Stat(filepath.Join(work, DirectoryOutput)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Sample wrote output after its context ended: %v", err)
	}
}
