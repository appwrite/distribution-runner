package protocol

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionSendsLeaseTokenAndQueryHeaders(t *testing.T) {
	server := newFake(t)
	server.handle("PATCH "+leasePath, func(writer http.ResponseWriter, _ *http.Request) {
		respond(t, writer, http.StatusOK, map[string]any{"expiresAt": "2026-11-23T10:05:00.000+00:00", "canceled": false})
	})
	server.handle("GET "+leasePath+"/source", func(writer http.ResponseWriter, _ *http.Request) {
		respond(t, writer, http.StatusOK, map[string]any{"url": "https://codeload.example/a.tar.gz", "headers": []string{}, "format": "tarball"})
	})
	server.handle("POST "+leasePath+"/events", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
	server.handle("POST "+leasePath+"/artifacts", func(writer http.ResponseWriter, request *http.Request) {
		respond(t, writer, http.StatusAccepted, map[string]any{"$id": "artifact1", "chunksTotal": 1, "chunksUploaded": 1})
	})
	path := filepath.Join(t.TempDir(), "app.ipa")
	if err := os.WriteFile(path, []byte("ipa"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := server.session(t)
	ctx := context.Background()

	if _, err := session.Extend(ctx); err != nil {
		t.Fatalf("Extend: %v", err)
	}
	if _, err := session.Source(ctx); err != nil {
		t.Fatalf("Source: %v", err)
	}
	if err := session.Report(ctx, []Event{{Type: TypeStart}}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	if _, err := session.Upload(ctx, path); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	requests := server.recorded()
	if len(requests) != 4 {
		t.Fatalf("requests = %d, want 4", len(requests))
	}
	for _, request := range requests {
		if !strings.HasPrefix(request.Path, leasePath) {
			t.Errorf("%s %s is not under the lease path", request.Method, request.Path)
		}
		bearer(t, request, testLeaseToken)
		if got := request.Header.Get("X-Appwrite-Project"); got != testProject {
			t.Errorf("%s %s: X-Appwrite-Project = %q", request.Method, request.Path, got)
		}
		if got := request.Header.Get("X-Appwrite-Environment"); got != testRegion {
			t.Errorf("%s %s: X-Appwrite-Environment = %q", request.Method, request.Path, got)
		}
	}
}

func TestSessionRejectsUnsafeQueryKeys(t *testing.T) {
	server := newFake(t)
	client := server.client(t)
	for _, key := range []string{"key", "jwt", "mode", "Mode", "response-format", "Response-Format", "", "project id", "project\r\nOrigin", "projé", "id", "Id"} {
		lease := server.lease()
		lease.Query = Map[string]{key: "value"}
		if _, err := client.Session(Credentials{RunnerID: testRunnerID}, lease); err == nil {
			t.Errorf("Session accepted query key %q", key)
		}
	}
}

func TestSessionValidatesLease(t *testing.T) {
	client, err := New("https://cloud.example/v1", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	credentials := Credentials{RunnerID: testRunnerID, Token: testRunnerKey}
	valid := Lease{LeaseID: testLeaseID, Token: testLeaseToken, Endpoint: "https://fra.cloud.example/v1"}

	if _, err := client.Session(credentials, valid); err != nil {
		t.Errorf("Session(valid) = %v", err)
	}
	insecure := valid
	insecure.Endpoint = "http://fra.cloud.example/v1"
	if _, err := client.Session(credentials, insecure); err == nil {
		t.Error("Session accepted an http lease endpoint without allowHTTP")
	}
	tokenless := valid
	tokenless.Token = ""
	if _, err := client.Session(credentials, tokenless); err == nil {
		t.Error("Session accepted a lease without a token")
	}
	if _, err := client.Session(Credentials{}, valid); err == nil {
		t.Error("Session accepted credentials without a runner id")
	}
}

func TestExtendReportsCancel(t *testing.T) {
	server := newFake(t)
	server.handle("PATCH "+leasePath, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Date", "Mon, 23 Nov 2026 10:00:00 GMT")
		respond(t, writer, http.StatusOK, map[string]any{"expiresAt": "2026-11-23T10:05:00.000+00:00", "canceled": true})
	})

	extension, err := server.session(t).Extend(context.Background())
	if err != nil {
		t.Fatalf("Extend: %v", err)
	}

	if !extension.Canceled {
		t.Error("Canceled = false, want true")
	}
	if remaining := time.Until(extension.ExpiresAt); remaining < 4*time.Minute || remaining > 5*time.Minute {
		t.Errorf("ExpiresAt is %v away, want the 5 minutes the server's own clock gives", remaining)
	}
}

func TestSourceDescriptor(t *testing.T) {
	cases := []struct {
		name    string
		body    map[string]any
		valid   bool
		headers int
	}{
		{"empty headers array", map[string]any{"url": "https://codeload.example/a.tar.gz", "headers": []string{}, "format": "tarball"}, true, 0},
		{"headers object", map[string]any{"url": "https://bitbucket.example/a.tar.gz", "headers": map[string]string{"Authorization": "Bearer x"}, "format": "tarball"}, true, 1},
		{"file url", map[string]any{"url": "file:///etc/passwd", "format": "tarball"}, false, 0},
		{"no url", map[string]any{"format": "tarball"}, false, 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := newFake(t)
			server.handle("GET "+leasePath+"/source", func(writer http.ResponseWriter, _ *http.Request) {
				respond(t, writer, http.StatusOK, test.body)
			})

			source, err := server.session(t).Source(context.Background())

			if (err == nil) != test.valid {
				t.Fatalf("Source error = %v, want valid %v", err, test.valid)
			}
			if err != nil {
				if strings.Contains(err.Error(), "passwd") {
					t.Errorf("error leaks the url: %v", err)
				}
				return
			}
			if source.URL != test.body["url"] || source.Format != "tarball" || len(source.Headers) != test.headers {
				t.Errorf("source = %#v (url %q)", source, source.URL)
			}
		})
	}
}

func TestSourceRequiresHTTPS(t *testing.T) {
	server := newFake(t)
	server.handle("GET "+leasePath+"/source", func(writer http.ResponseWriter, _ *http.Request) {
		respond(t, writer, http.StatusOK, map[string]any{"url": "http://codeload.example/a.tar.gz", "format": "tarball"})
	})
	session := server.session(t)
	session.client.allowHTTP = false

	if _, err := session.Source(context.Background()); err == nil {
		t.Error("Source accepted an http url without allowHTTP")
	}
}

func TestRequeueHandsTheLeaseBack(t *testing.T) {
	server := newFake(t)
	server.handle("PATCH "+leasePath+"/status", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})

	if err := server.session(t).Requeue(context.Background()); err != nil {
		t.Fatalf("Requeue: %v", err)
	}

	requests := server.matching(http.MethodPatch, leasePath+"/status")
	if len(requests) != 1 {
		t.Fatalf("requeue requests = %d, want 1", len(requests))
	}
	request := requests[0]
	bearer(t, request, testLeaseToken)
	if got := request.Header.Get("X-Appwrite-Project"); got != testProject {
		t.Errorf("X-Appwrite-Project = %q, want %q", got, testProject)
	}
	if got := request.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if string(request.Body) != `{"status":"queued"}` {
		t.Errorf("body = %s, want the queued status", request.Body)
	}
}

func TestRequeueRetriesServerErrorsAndReportsRefusals(t *testing.T) {
	server := newFake(t)
	var attempts atomic.Int32
	server.handle("PATCH "+leasePath+"/status", func(writer http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		respond(t, writer, http.StatusNotFound, map[string]string{"message": "Lease not found.", "type": "distribution_lease_not_found"})
	})

	err := server.session(t).Requeue(context.Background())

	var response *ResponseError
	if !errors.As(err, &response) || response.Status != http.StatusNotFound || !response.Permanent() {
		t.Fatalf("Requeue = %v, want the permanent 404", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want the 503 retried once", got)
	}
}
