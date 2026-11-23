package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/appwrite/distribution-runner/agent/lease"
	"github.com/appwrite/distribution-runner/agent/protocol"
	"github.com/appwrite/distribution-runner/agent/vm"
)

func lookup(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestParse(t *testing.T) {
	settings, err := parse([]string{"--labels", "xcode-27.2, arm64,,", "--capacity", "2", "--allow-http", "true"}, lookup(map[string]string{
		"DISTRIBUTION_AGENT_ENDPOINT": "http://localhost/v1",
		"DISTRIBUTION_AGENT_VM":       backendFake,
		"DISTRIBUTION_AGENT_STATE":    "/tmp/state.json",
		"DISTRIBUTION_AGENT_NAME":     "mac-1",
	}), io.Discard)

	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if settings.endpoint != "http://localhost/v1" || settings.capacity != 2 || settings.backend != backendFake ||
		!slices.Equal(settings.labels, []string{"xcode-27.2", "arm64", fakeLabel}) || !settings.allowHTTP {
		t.Errorf("settings = %+v", settings)
	}
}

func TestParseLume(t *testing.T) {
	settings, err := parse([]string{"--golden", "golden", "--ssh-key", "/keys/id", "--labels", "xcode-27.2"}, lookup(map[string]string{
		"DISTRIBUTION_AGENT_ENDPOINT": "https://cloud.example/v1",
		"DISTRIBUTION_AGENT_STATE":    "/tmp/state.json",
		"DISTRIBUTION_AGENT_NAME":     "mac-1",
	}), io.Discard)

	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if settings.backend != backendLume || settings.capacity != 1 || !slices.Equal(settings.labels, []string{"xcode-27.2"}) {
		t.Errorf("settings = %+v", settings)
	}
}

func TestParseRejects(t *testing.T) {
	base := map[string]string{
		"DISTRIBUTION_AGENT_ENDPOINT": "https://cloud.example/v1",
		"DISTRIBUTION_AGENT_STATE":    "/tmp/state.json",
		"DISTRIBUTION_AGENT_NAME":     "mac-1",
		"DISTRIBUTION_AGENT_GOLDEN":   "golden",
		"DISTRIBUTION_AGENT_SSH_KEY":  "/keys/id",
	}
	for name, arguments := range map[string][]string{
		"capacity above two":    {"--capacity", "3"},
		"capacity zero":         {"--capacity", "0"},
		"unknown backend":       {"--vm", "tart"},
		"lume without a golden": {"--golden", ""},
		"lume without a key":    {"--ssh-key", ""},
		"lume with two slots":   {"--capacity", "2"},
		"fake over https":       {"--vm", backendFake},
		"known hosts flag":      {"--known-hosts", "/etc/known_hosts"},
		"no endpoint":           {"--endpoint", ""},
		"bad allow-http":        {"--allow-http", "maybe"},
		"stray argument":        {"extra"},
	} {
		if _, err := parse(arguments, lookup(base), io.Discard); err == nil {
			t.Errorf("%s: parse accepted %v", name, arguments)
		}
	}
}

func TestCredentialsStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "agent.json")
	credentials := protocol.Credentials{RunnerID: "runner1", Token: "token1"}

	if err := saveCredentials(path, credentials); err != nil {
		t.Fatalf("saveCredentials: %v", err)
	}
	information, err := os.Stat(path)
	if err != nil || information.Mode().Perm() != stateMode {
		t.Fatalf("state mode = %v, %v; want %v", information.Mode().Perm(), err, os.FileMode(stateMode))
	}
	loaded, err := loadCredentials(path)
	if err != nil || loaded != credentials {
		t.Errorf("loadCredentials = %+v, %v", loaded, err)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCredentials(path); err == nil {
		t.Error("loadCredentials accepted a world-readable state file")
	}

	if err := os.Chmod(path, stateMode); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCredentials(link); err == nil {
		t.Error("loadCredentials followed a symlink")
	}
}

func TestStateLockAdmitsOneAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "agent.json")

	first, err := lockState(path)
	if err != nil {
		t.Fatalf("lockState: %v", err)
	}
	if _, err := lockState(path); err == nil || !strings.Contains(err.Error(), "another agent") {
		t.Fatalf("a second lock = %v, want another agent", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := lockState(path)
	if err != nil {
		t.Fatalf("lockState after the first agent stopped: %v", err)
	}
	second.Close()
}

type controlPlane struct {
	t      *testing.T
	server *httptest.Server

	// claim chooses the status of the nth claim, from 1: 201 hands out the lease. By default only the first does.
	claim func(count int) int

	mutex      sync.Mutex
	enrolments int
	statuses   []string
	claims     int
	events     []string
	done       chan struct{}
}

func newControlPlane(t *testing.T) *controlPlane {
	t.Helper()
	plane := &controlPlane{t: t, done: make(chan struct{})}
	mux := http.NewServeMux()
	runner := "/v1/distribution/runners/runner1"
	leasePath := runner + "/leases/lease1"
	mux.HandleFunc("POST /v1/distribution/runners", func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer enrol-secret" {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		plane.mutex.Lock()
		plane.enrolments++
		plane.mutex.Unlock()
		reply(writer, http.StatusCreated, protocol.Credentials{RunnerID: "runner1", Token: "runner-token"})
	})
	mux.HandleFunc("PATCH "+runner, func(writer http.ResponseWriter, request *http.Request) {
		var status protocol.Status
		_ = json.NewDecoder(request.Body).Decode(&status)
		plane.mutex.Lock()
		plane.statuses = append(plane.statuses, status.Status)
		plane.mutex.Unlock()
		reply(writer, http.StatusOK, map[string]any{})
	})
	mux.HandleFunc("POST "+runner+"/leases", func(writer http.ResponseWriter, _ *http.Request) {
		plane.mutex.Lock()
		plane.claims++
		count := plane.claims
		plane.mutex.Unlock()
		status := http.StatusNoContent
		if count == 1 {
			status = http.StatusCreated
		}
		if plane.claim != nil {
			status = plane.claim(count)
		}
		if status != http.StatusCreated {
			writer.WriteHeader(status)
			return
		}
		reply(writer, http.StatusCreated, plane.lease())
	})
	mux.HandleFunc("PATCH "+leasePath, func(writer http.ResponseWriter, _ *http.Request) {
		reply(writer, http.StatusOK, protocol.Extension{ExpiresAt: time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("POST "+leasePath+"/events", func(writer http.ResponseWriter, request *http.Request) {
		var batch struct {
			Events []struct {
				Type string `json:"type"`
			} `json:"events"`
		}
		_ = json.NewDecoder(request.Body).Decode(&batch)
		plane.mutex.Lock()
		for _, received := range batch.Events {
			plane.events = append(plane.events, received.Type)
			if received.Type == protocol.TypeComplete && plane.done != nil {
				close(plane.done)
				plane.done = nil
			}
		}
		plane.mutex.Unlock()
		writer.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+leasePath+"/artifacts", func(writer http.ResponseWriter, request *http.Request) {
		reply(writer, http.StatusCreated, map[string]any{"$id": request.Header.Get("X-Appwrite-Id"), "chunksUploaded": 1, "chunksTotal": 1})
	})
	mux.HandleFunc("GET "+leasePath+"/source", func(writer http.ResponseWriter, _ *http.Request) {
		reply(writer, http.StatusOK, protocol.Source{URL: plane.server.URL + "/tarball", Format: "tarball"})
	})
	mux.HandleFunc("GET /tarball", func(writer http.ResponseWriter, _ *http.Request) {
		compressor := gzip.NewWriter(writer)
		_, _ = compressor.Write([]byte("repository"))
		_ = compressor.Close()
	})
	plane.server = httptest.NewServer(mux)
	t.Cleanup(plane.server.Close)
	return plane
}

func (p *controlPlane) lease() map[string]any {
	return map[string]any{
		"leaseId": "lease1", "token": "lease-token", "expiresAt": time.Now().Add(time.Hour),
		"endpoint": p.server.URL + "/v1", "query": map[string]string{"project": "project1"},
		"buildId": "build1", "attempt": 1, "timeout": 60, "platform": "ios", "framework": "flutter",
		"source":      map[string]string{"rootDirectory": "", "commit": "abc123"},
		"environment": map[string]string{lease.VariableIdentifier: "com.example.app"},
		"commands":    map[string]string{"install": "", "build": ""},
		"signing": map[string]any{
			"certificate": base64.StdEncoding.EncodeToString([]byte("certificate")),
			"password":    "password",
			"profiles":    map[string]string{"com.example.app": base64.StdEncoding.EncodeToString([]byte("profile"))},
			"teamId":      "ABCDE12345",
		},
		"version": map[string]any{"name": "1.0.0", "code": 7},
	}
}

func reply(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

func TestAgentRunsLeaseWithFakeVM(t *testing.T) {
	plane := newControlPlane(t)
	directory := t.TempDir()
	settings, err := parse([]string{
		"--endpoint", plane.server.URL + "/v1", "--allow-http", "true", "--vm", backendFake,
		"--state", filepath.Join(directory, "agent.json"), "--directory", filepath.Join(directory, "work"),
		"--name", "mac-1",
	}, lookup(map[string]string{"DISTRIBUTION_AGENT_SECRET": "enrol-secret"}), io.Discard)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	client, err := protocol.New(settings.endpoint, nil, settings.allowHTTP)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := enrol(context.Background(), client, settings, lookup(map[string]string{"DISTRIBUTION_AGENT_SECRET": "enrol-secret"}), logger)
	if err != nil {
		t.Fatalf("enrol: %v", err)
	}
	again, err := enrol(context.Background(), client, settings, lookup(nil), logger)
	if err != nil || again != credentials {
		t.Fatalf("second start did not reuse the state file: %+v, %v", again, err)
	}
	layer, image := backend(settings)
	agent := &agent{
		client: client, credentials: credentials, capacity: 1, logger: logger,
		runner: &lease.Runner{Client: client, Credentials: credentials, Layer: layer, Image: image,
			Directory: settings.directory, AllowHTTP: true, Logger: logger, Interval: 10 * time.Millisecond},
	}
	drain, stopDrain := context.WithCancel(context.Background())
	done := plane.done
	go func() {
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("no complete event within 20s")
		}
		stopDrain()
	}()

	if err := agent.serve(drain, context.Background()); err != nil {
		t.Fatalf("serve: %v", err)
	}

	plane.mutex.Lock()
	defer plane.mutex.Unlock()
	if plane.enrolments != 1 {
		t.Errorf("enrolled %d times, want 1", plane.enrolments)
	}
	want := []string{protocol.TypeStart, protocol.TypeStage, protocol.TypeStage, protocol.TypeExit, protocol.TypeArtifact, protocol.TypeArtifact, protocol.TypeComplete}
	if !slices.Equal(plane.events, want) {
		t.Errorf("events = %v, want %v", plane.events, want)
	}
	if len(plane.statuses) < 2 || plane.statuses[0] != protocol.StatusOnline || plane.statuses[len(plane.statuses)-1] != protocol.StatusOffline {
		t.Errorf("heartbeats = %v, want online first and offline last", plane.statuses)
	}
	if strings.Contains(logs.String(), "runner-token") || strings.Contains(logs.String(), "lease-token") {
		t.Errorf("logs contain a token:\n%s", logs.String())
	}
}

// newAgent is an agent for plane whose leases run on layer.
func newAgent(t *testing.T, plane *controlPlane, layer *vm.Fake) (*agent, *synchronized) {
	t.Helper()
	client, err := protocol.New(plane.server.URL+"/v1", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	credentials := protocol.Credentials{RunnerID: "runner1", Token: "runner-token"}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	logs := &synchronized{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	return &agent{
		client: client, credentials: credentials, capacity: 1, logger: logger,
		runner: &lease.Runner{Client: client, Credentials: credentials, Layer: layer, Image: fakeImage,
			Directory: directory, AllowHTTP: true, Logger: logger, Interval: 10 * time.Millisecond},
	}, logs
}

// synchronized is a log sink safe to write from the agent's goroutines and read from the test's.
type synchronized struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (s *synchronized) Write(data []byte) (int, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.buffer.Write(data)
}

func (s *synchronized) String() string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.buffer.String()
}

func TestAgentStopsOnARevokedToken(t *testing.T) {
	plane := newControlPlane(t)
	plane.claim = func(int) int { return http.StatusUnauthorized }
	agent, _ := newAgent(t, plane, &vm.Fake{Directory: t.TempDir()})

	result := make(chan error, 1)
	go func() { result <- agent.serve(context.Background(), context.Background()) }()

	select {
	case err := <-result:
		if !errors.Is(err, errRevoked) {
			t.Fatalf("serve = %v, want the revoked token", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve kept claiming with a revoked token")
	}
	plane.mutex.Lock()
	defer plane.mutex.Unlock()
	if plane.claims != 1 {
		t.Errorf("claimed %d times with a revoked token, want 1", plane.claims)
	}
}

func TestAgentDrainsAfterRepeatedHostFailures(t *testing.T) {
	plane := newControlPlane(t)
	plane.claim = func(int) int { return http.StatusCreated }
	layer := &vm.Fake{Directory: t.TempDir(), Fail: func(call vm.Call) error {
		if call.Operation == vm.OperationCreate {
			return errors.New("lume is not installed")
		}
		return nil
	}}
	agent, logs := newAgent(t, plane, layer)
	drain, stopDrain := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- agent.serve(drain, context.Background()) }()

	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(logs.String(), "draining") {
		if time.Now().After(deadline) {
			t.Fatal("the agent kept claiming after repeated host failures")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	plane.mutex.Lock()
	claims := plane.claims
	plane.mutex.Unlock()
	select {
	case err := <-result:
		t.Fatalf("serve returned %v before the drain signal; it should keep reporting draining", err)
	default:
	}
	stopDrain()
	if err := <-result; err != nil {
		t.Fatalf("serve: %v", err)
	}

	plane.mutex.Lock()
	defer plane.mutex.Unlock()
	if claims != failureLimit || plane.claims != failureLimit {
		t.Errorf("claimed %d then %d times, want %d", claims, plane.claims, failureLimit)
	}
	if plane.statuses[len(plane.statuses)-1] != protocol.StatusOffline {
		t.Errorf("heartbeats = %v, want offline last", plane.statuses)
	}
}
