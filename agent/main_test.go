package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
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
)

func lookup(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestParse(t *testing.T) {
	settings, err := parse([]string{"--labels", "xcode-27.2, arm64,,", "--capacity", "2"}, lookup(map[string]string{
		"DISTRIBUTION_AGENT_ENDPOINT": "https://cloud.example/v1",
		"DISTRIBUTION_AGENT_VM":       backendFake,
		"DISTRIBUTION_AGENT_STATE":    "/tmp/state.json",
		"DISTRIBUTION_AGENT_NAME":     "mac-1",
	}), io.Discard)

	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if settings.endpoint != "https://cloud.example/v1" || settings.capacity != 2 || settings.backend != backendFake ||
		!slices.Equal(settings.labels, []string{"xcode-27.2", "arm64"}) || settings.allowHTTP {
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
}

type controlPlane struct {
	t      *testing.T
	server *httptest.Server

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
		first := plane.claims == 1
		plane.mutex.Unlock()
		if !first {
			writer.WriteHeader(http.StatusNoContent)
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
			if received.Type == protocol.TypeComplete {
				close(plane.done)
			}
		}
		plane.mutex.Unlock()
		writer.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+leasePath+"/artifacts", func(writer http.ResponseWriter, _ *http.Request) {
		reply(writer, http.StatusCreated, map[string]any{"$id": "upload1", "chunksUploaded": 1, "chunksTotal": 1})
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
	go func() {
		select {
		case <-plane.done:
		case <-time.After(20 * time.Second):
			t.Error("no complete event within 20s")
		}
		stopDrain()
	}()

	agent.serve(drain, context.Background())

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
