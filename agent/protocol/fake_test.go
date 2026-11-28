package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const (
	testRunnerID   = "runner1"
	testRunnerKey  = "runner-token-sentinel"
	testLeaseID    = "lease1"
	testLeaseToken = "lease-token-sentinel"
	testProject    = "project1"
	testRegion     = "environment1"
	leasePath      = "/v1/distribution/runners/" + testRunnerID + "/leases/" + testLeaseID
)

type recorded struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

type fake struct {
	t      *testing.T
	server *httptest.Server
	mux    *http.ServeMux

	mutex    sync.Mutex
	requests []recorded
}

// newFake starts a server that records every request and fails the test when one carries a forbidden header.
func newFake(t *testing.T) *fake {
	t.Helper()
	server := &fake{t: t, mux: http.NewServeMux()}
	server.server = httptest.NewServer(http.HandlerFunc(server.serve))
	t.Cleanup(server.server.Close)
	return server
}

func (f *fake) serve(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		// The client abandoned the request mid-body, as Upload does to its other chunks once one is rejected.
		return
	}
	for _, name := range forbiddenHeaders {
		if values, ok := request.Header[http.CanonicalHeaderKey(name)]; ok {
			f.t.Errorf("%s %s carried forbidden header %s: %v", request.Method, request.URL.Path, name, values)
		}
	}
	f.mutex.Lock()
	f.requests = append(f.requests, recorded{
		Method: request.Method,
		Path:   request.URL.Path,
		Header: request.Header.Clone(),
		Body:   body,
	})
	f.mutex.Unlock()
	request.Body = io.NopCloser(bytes.NewReader(body))
	f.mux.ServeHTTP(writer, request)
}

func (f *fake) handle(pattern string, handler http.HandlerFunc) {
	f.mux.HandleFunc(pattern, handler)
}

func (f *fake) recorded() []recorded {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]recorded(nil), f.requests...)
}

func (f *fake) matching(method string, path string) []recorded {
	var matches []recorded
	for _, request := range f.recorded() {
		if request.Method == method && request.Path == path {
			matches = append(matches, request)
		}
	}
	return matches
}

func (f *fake) client(t *testing.T) *Client {
	t.Helper()
	client, err := New(f.server.URL+"/v1", f.server.Client(), true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client.retry.backoff = time.Millisecond
	client.retry.ceiling = 5 * time.Millisecond
	return client
}

func (f *fake) lease() Lease {
	return Lease{
		LeaseID:  testLeaseID,
		Token:    testLeaseToken,
		Endpoint: f.server.URL + "/v1",
		Query:    Map[string]{"project": testProject, "environment": testRegion},
	}
}

func (f *fake) session(t *testing.T) *Session {
	t.Helper()
	session, err := f.client(t).Session(Credentials{RunnerID: testRunnerID, Token: testRunnerKey}, f.lease())
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	return session
}

func respond(t *testing.T, writer http.ResponseWriter, status int, body any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if body != nil {
		if err := json.NewEncoder(writer).Encode(body); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}
}

func bearer(t *testing.T, request recorded, token string) {
	t.Helper()
	if got := request.Header.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("%s %s: Authorization = %q, want bearer %q", request.Method, request.Path, got, token)
	}
}

type batch struct {
	Events []struct {
		ID   string          `json:"id"`
		Type string          `json:"type"`
		Time time.Time       `json:"time"`
		Data json.RawMessage `json:"data"`
	} `json:"events"`
}

func decodeBatch(t *testing.T, body []byte) batch {
	t.Helper()
	var decoded batch
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode events body: %v", err)
	}
	return decoded
}
