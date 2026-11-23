package protocol

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewValidatesEndpoint(t *testing.T) {
	cases := []struct {
		endpoint  string
		allowHTTP bool
		valid     bool
	}{
		{"https://cloud.example/v1", false, true},
		{"https://cloud.example/v1/", false, true},
		{"http://localhost/v1", true, true},
		{"http://localhost/v1", false, false},
		{"ftp://cloud.example/v1", true, false},
		{"https:///v1", false, false},
		{"https://user:secret@cloud.example/v1", false, false},
		{"https://cloud.example/v1?project=x", false, false},
		{"https://cloud.example/v1#fragment", false, false},
		{"://", false, false},
	}
	for _, test := range cases {
		_, err := New(test.endpoint, nil, test.allowHTTP)
		if (err == nil) != test.valid {
			t.Errorf("New(%q, allowHTTP=%v) error = %v, want valid %v", test.endpoint, test.allowHTTP, err, test.valid)
		}
	}
}

func TestEnrol(t *testing.T) {
	server := newFake(t)
	server.handle("POST /v1/distribution/runners", func(writer http.ResponseWriter, _ *http.Request) {
		respond(t, writer, http.StatusCreated, map[string]string{"runnerId": testRunnerID, "token": testRunnerKey})
	})

	credentials, err := server.client(t).Enrol(context.Background(), "enrol-secret", Registration{
		Name:     "mac-1",
		Platform: PlatformMacOS,
		Capacity: 2,
		Version:  "1.0.0",
	})
	if err != nil {
		t.Fatalf("Enrol: %v", err)
	}

	if credentials != (Credentials{RunnerID: testRunnerID, Token: testRunnerKey}) {
		t.Errorf("credentials = %+v", credentials)
	}
	requests := server.recorded()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	bearer(t, requests[0], "enrol-secret")
	if got := requests[0].Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	want := `{"name":"mac-1","platform":"macos","labels":[],"capacity":2,"version":"1.0.0"}`
	if string(requests[0].Body) != want {
		t.Errorf("body = %s, want %s", requests[0].Body, want)
	}
}

func TestEnrolIsNotRetried(t *testing.T) {
	server := newFake(t)
	server.handle("POST /v1/distribution/runners", func(writer http.ResponseWriter, _ *http.Request) {
		respond(t, writer, http.StatusServiceUnavailable, nil)
	})

	_, err := server.client(t).Enrol(context.Background(), "enrol-secret", Registration{})

	var response *ResponseError
	if !errors.As(err, &response) || response.Status != http.StatusServiceUnavailable {
		t.Fatalf("error = %v, want 503 ResponseError", err)
	}
	if requests := server.recorded(); len(requests) != 1 {
		t.Errorf("requests = %d, want 1", len(requests))
	}
}

func TestEnrolRejectsIncompleteCredentials(t *testing.T) {
	server := newFake(t)
	server.handle("POST /v1/distribution/runners", func(writer http.ResponseWriter, _ *http.Request) {
		respond(t, writer, http.StatusCreated, map[string]string{"runnerId": testRunnerID})
	})

	if _, err := server.client(t).Enrol(context.Background(), "enrol-secret", Registration{}); err == nil {
		t.Fatal("Enrol accepted a response without a token")
	}
}

func TestHeartbeat(t *testing.T) {
	server := newFake(t)
	server.handle("PATCH /v1/distribution/runners/"+testRunnerID, func(writer http.ResponseWriter, _ *http.Request) {
		respond(t, writer, http.StatusOK, map[string]string{"$id": testRunnerID})
	})

	err := server.client(t).Heartbeat(context.Background(), Credentials{RunnerID: testRunnerID, Token: testRunnerKey}, Status{
		Status:  StatusDraining,
		Version: "1.0.0",
		Labels:  []string{"xcode-27.2", "arm64"},
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	requests := server.recorded()
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	bearer(t, requests[0], testRunnerKey)
	want := `{"status":"draining","version":"1.0.0","labels":["xcode-27.2","arm64"]}`
	if string(requests[0].Body) != want {
		t.Errorf("body = %s, want %s", requests[0].Body, want)
	}
}

func TestHeartbeatRetriesServerErrors(t *testing.T) {
	server := newFake(t)
	var attempts atomic.Int32
	server.handle("PATCH /v1/distribution/runners/"+testRunnerID, func(writer http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			respond(t, writer, http.StatusBadGateway, nil)
			return
		}
		respond(t, writer, http.StatusOK, nil)
	})

	err := server.client(t).Heartbeat(context.Background(), Credentials{RunnerID: testRunnerID, Token: testRunnerKey}, Status{Status: StatusOnline})

	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3", attempts.Load())
	}
}

func TestRetryHonoursRetryAfter(t *testing.T) {
	server := newFake(t)
	var attempts atomic.Int32
	server.handle("PATCH /v1/distribution/runners/"+testRunnerID, func(writer http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			writer.Header().Set("Retry-After", "1")
			respond(t, writer, http.StatusTooManyRequests, nil)
			return
		}
		respond(t, writer, http.StatusOK, nil)
	})

	started := time.Now()
	err := server.client(t).Heartbeat(context.Background(), Credentials{RunnerID: testRunnerID, Token: testRunnerKey}, Status{})
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if attempts.Load() != 2 {
		t.Errorf("attempts = %d, want 2", attempts.Load())
	}
	if elapsed < time.Second {
		t.Errorf("retried after %v, want at least the 1s Retry-After", elapsed)
	}
}

func TestRetryGivesUpWithResponseError(t *testing.T) {
	server := newFake(t)
	server.handle("PATCH /v1/distribution/runners/"+testRunnerID, func(writer http.ResponseWriter, _ *http.Request) {
		respond(t, writer, http.StatusInternalServerError, map[string]any{
			"message": "Server Error",
			"code":    500,
			"type":    "general_unknown",
		})
	})
	client := server.client(t)

	err := client.Heartbeat(context.Background(), Credentials{RunnerID: testRunnerID, Token: testRunnerKey}, Status{})

	var response *ResponseError
	if !errors.As(err, &response) {
		t.Fatalf("error = %v, want ResponseError", err)
	}
	if response.Status != http.StatusInternalServerError || response.Type != "general_unknown" || response.Message != "Server Error" {
		t.Errorf("ResponseError = %+v", response)
	}
	if requests := server.recorded(); len(requests) != client.retry.attempts {
		t.Errorf("requests = %d, want %d", len(requests), client.retry.attempts)
	}
}

func TestRetryStopsWhenContextEnds(t *testing.T) {
	server := newFake(t)
	server.handle("PATCH /v1/distribution/runners/"+testRunnerID, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "60")
		respond(t, writer, http.StatusServiceUnavailable, nil)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := server.client(t).Heartbeat(ctx, Credentials{RunnerID: testRunnerID, Token: testRunnerKey}, Status{})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want deadline exceeded", err)
	}
}

func TestClaimNoContent(t *testing.T) {
	server := newFake(t)
	server.handle("POST /v1/distribution/runners/"+testRunnerID+"/leases", func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})

	lease, err := server.client(t).Claim(context.Background(), Credentials{RunnerID: testRunnerID, Token: testRunnerKey}, nil)

	if err != nil || lease != nil {
		t.Fatalf("Claim = %v, %v, want nil, nil", lease, err)
	}
	requests := server.recorded()
	bearer(t, requests[0], testRunnerKey)
	if string(requests[0].Body) != `{"labels":[]}` {
		t.Errorf("body = %s", requests[0].Body)
	}
}

func TestClaimCreated(t *testing.T) {
	server := newFake(t)
	certificate := []byte("p12-certificate-sentinel")
	profile := []byte("profile-sentinel")
	body := fmt.Sprintf(`{
		"leaseId": %q, "token": %q, "expiresAt": "2026-11-23T10:05:00.000+00:00",
		"endpoint": "https://fra.cloud.example/v1",
		"query": {"project": %q, "environment": %q}, "buildId": "build1", "attempt": 2, "timeout": 3600,
		"platform": "ios", "framework": "flutter",
		"source": {"rootDirectory": "apps/mobile", "commit": "abc123"},
		"environment": [],
		"commands": {"install": "flutter pub get", "build": "flutter build ipa"},
		"signing": {"certificate": %q, "password": "secret", "profiles": {"com.example.app": %q}, "teamId": "ABCDE12345"},
		"version": {"name": "1.4.0", "code": 57}
	}`, testLeaseID, testLeaseToken, testProject, testRegion,
		base64.StdEncoding.EncodeToString(certificate), base64.StdEncoding.EncodeToString(profile))
	server.handle("POST /v1/distribution/runners/"+testRunnerID+"/leases", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Date", "Mon, 23 Nov 2026 10:00:00 GMT")
		writer.WriteHeader(http.StatusCreated)
		_, _ = writer.Write([]byte(body))
	})

	lease, err := server.client(t).Claim(context.Background(), Credentials{RunnerID: testRunnerID, Token: testRunnerKey}, []string{"xcode-27.2"})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if lease.LeaseID != testLeaseID || lease.Token != testLeaseToken || lease.BuildID != "build1" || lease.Attempt != 2 || lease.Timeout != 3600 {
		t.Errorf("lease = %+v", *lease)
	}
	if remaining := time.Until(lease.ExpiresAt); remaining < 4*time.Minute || remaining > 5*time.Minute {
		t.Errorf("ExpiresAt is %v away, want the 5 minutes the server's own clock gives", remaining)
	}
	if lease.Query["project"] != testProject || lease.Query["environment"] != testRegion {
		t.Errorf("Query = %v", lease.Query)
	}
	if lease.Environment == nil || len(lease.Environment) != 0 {
		t.Errorf("Environment = %#v, want an empty map from []", lease.Environment)
	}
	if lease.Source != (Checkout{RootDirectory: "apps/mobile", Commit: "abc123"}) || lease.Commands.Build != "flutter build ipa" {
		t.Errorf("source or commands = %+v %+v", lease.Source, lease.Commands)
	}
	if !bytes.Equal(lease.Signing.Certificate, certificate) || !bytes.Equal(lease.Signing.Profiles["com.example.app"], profile) {
		t.Errorf("signing was not base64-decoded")
	}
	if lease.Signing.TeamID != "ABCDE12345" || lease.Version != (Version{Name: "1.4.0", Code: 57}) {
		t.Errorf("team or version = %s %+v", lease.Signing.TeamID, lease.Version)
	}
	if string(server.recorded()[0].Body) != `{"labels":["xcode-27.2"]}` {
		t.Errorf("claim body = %s", server.recorded()[0].Body)
	}
}

func TestClaimIsNotRetried(t *testing.T) {
	server := newFake(t)
	server.handle("POST /v1/distribution/runners/"+testRunnerID+"/leases", func(writer http.ResponseWriter, _ *http.Request) {
		respond(t, writer, http.StatusInternalServerError, nil)
	})

	_, err := server.client(t).Claim(context.Background(), Credentials{RunnerID: testRunnerID, Token: testRunnerKey}, nil)

	if err == nil {
		t.Fatal("Claim succeeded on a 500")
	}
	if requests := server.recorded(); len(requests) != 1 {
		t.Errorf("requests = %d, want 1", len(requests))
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	server := newFake(t)
	server.handle("PATCH /v1/distribution/runners/"+testRunnerID, func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/elsewhere", http.StatusTemporaryRedirect)
	})

	err := server.client(t).Heartbeat(context.Background(), Credentials{RunnerID: testRunnerID, Token: testRunnerKey}, Status{})

	var response *ResponseError
	if !errors.As(err, &response) || response.Status != http.StatusTemporaryRedirect {
		t.Fatalf("error = %v, want 307 ResponseError", err)
	}
	if requests := server.matching(http.MethodPatch, "/elsewhere"); len(requests) != 0 {
		t.Errorf("redirect was followed with the bearer token")
	}
}

func TestHTTPS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		respond(t, writer, http.StatusCreated, map[string]string{"runnerId": testRunnerID, "token": testRunnerKey})
	}))
	defer server.Close()
	client, err := New(server.URL+"/v1", server.Client(), false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := client.Enrol(context.Background(), "enrol-secret", Registration{}); err != nil {
		t.Fatalf("Enrol over TLS: %v", err)
	}
}

func TestSecretsAreRedactedFromLogs(t *testing.T) {
	server := newFake(t)
	credentials := Credentials{RunnerID: testRunnerID, Token: testRunnerKey}
	lease := server.lease()
	lease.Signing = Signing{Certificate: []byte("certificate-sentinel"), Password: "password-sentinel", Profiles: Map[[]byte]{"com.example.app": []byte("profile-sentinel")}}
	source := Source{URL: "https://codeload.example/archive?token=url-sentinel", Headers: Map[string]{"Authorization": "Bearer header-sentinel"}, Format: "tarball"}
	session := server.session(t)
	values := []any{credentials, lease, lease.Signing, source, session}

	var output strings.Builder
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	for _, value := range values {
		fmt.Fprintf(&output, "%v %+v %#v %s\n", value, value, value, value)
		logger.Info("value", "value", value)
	}
	text := slog.New(slog.NewTextHandler(&output, nil))
	for _, value := range values {
		text.Info("value", "value", value)
	}

	for _, secret := range []string{testRunnerKey, testLeaseToken, "certificate-sentinel", "password-sentinel", "profile-sentinel", "url-sentinel", "header-sentinel", "codeload.example"} {
		if strings.Contains(output.String(), secret) {
			t.Errorf("log output contains %q:\n%s", secret, output.String())
		}
	}
	if !strings.Contains(output.String(), testRunnerID) || !strings.Contains(output.String(), testLeaseID) {
		t.Errorf("log output lost the identifiers:\n%s", output.String())
	}
}

func TestCredentialsMarshalForState(t *testing.T) {
	encoded, err := json.Marshal(Credentials{RunnerID: testRunnerID, Token: testRunnerKey})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"runnerId":"runner1","token":"runner-token-sentinel"}` {
		t.Errorf("encoded = %s", encoded)
	}
}

func TestHostTimeKeepsTimestampsWithoutDate(t *testing.T) {
	at := time.Date(2026, 11, 23, 10, 5, 0, 0, time.UTC)
	if got := hostTime(at, http.Header{}); !got.Equal(at) {
		t.Errorf("hostTime without Date = %v, want %v", got, at)
	}
	if got := hostTime(at, http.Header{"Date": {"not a date"}}); !got.Equal(at) {
		t.Errorf("hostTime with an invalid Date = %v, want %v", got, at)
	}
}

func TestResponseErrorPermanent(t *testing.T) {
	for status, permanent := range map[int]bool{
		http.StatusBadRequest:          true,
		http.StatusUnauthorized:        true,
		http.StatusNotFound:            true,
		http.StatusConflict:            true,
		http.StatusRequestTimeout:      false,
		http.StatusTooManyRequests:     false,
		http.StatusInternalServerError: false,
		http.StatusServiceUnavailable:  false,
	} {
		if got := (&ResponseError{Status: status}).Permanent(); got != permanent {
			t.Errorf("Permanent() for %d = %v, want %v", status, got, permanent)
		}
	}
}

func TestAttemptTimesOutASilentServer(t *testing.T) {
	release := make(chan struct{})
	server := newFake(t)
	server.handle("POST /v1/distribution/runners/"+testRunnerID+"/leases", func(http.ResponseWriter, *http.Request) {
		<-release
	})
	t.Cleanup(func() { close(release) })
	client := server.client(t)

	started := time.Now()
	_, err := client.send(context.Background(), call{
		method:  http.MethodPost,
		url:     client.endpoint + "/distribution/runners/" + testRunnerID + "/leases",
		token:   testRunnerKey,
		timeout: 50 * time.Millisecond,
	})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("send = %v, want the attempt deadline", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("send took %v", elapsed)
	}
}

func TestDefaultTransportBoundsResponseHeaders(t *testing.T) {
	client, err := New("https://cloud.example/v1", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok || transport.ResponseHeaderTimeout != UploadTimeout {
		t.Fatalf("transport = %#v, want a response header timeout of %v", client.http.Transport, UploadTimeout)
	}
}
