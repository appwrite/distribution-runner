package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type Lease struct {
	LeaseID     string      `json:"leaseId"`
	Token       string      `json:"token"`
	ExpiresAt   time.Time   `json:"expiresAt"`
	Endpoint    string      `json:"endpoint"`
	Query       Map[string] `json:"query"`
	BuildID     string      `json:"buildId"`
	Attempt     int         `json:"attempt"`
	Timeout     int         `json:"timeout"`
	Platform    string      `json:"platform"`
	Framework   string      `json:"framework"`
	Source      Checkout    `json:"source"`
	Environment Map[string] `json:"environment"`
	Commands    Commands    `json:"commands"`
	Signing     Signing     `json:"signing"`
	Version     Version     `json:"version"`
}

func (l Lease) String() string {
	return fmt.Sprintf("Lease{LeaseID: %s, BuildID: %s, Attempt: %d}", l.LeaseID, l.BuildID, l.Attempt)
}

func (l Lease) GoString() string {
	return l.String()
}

func (l Lease) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("leaseId", l.LeaseID),
		slog.String("buildId", l.BuildID),
		slog.Int("attempt", l.Attempt),
	)
}

type Checkout struct {
	RootDirectory string `json:"rootDirectory"`
	Commit        string `json:"commit"`
}

type Commands struct {
	Install string `json:"install"`
	Build   string `json:"build"`
}

// Signing holds the decoded PKCS#12 certificate and provisioning profiles keyed by bundle identifier.
type Signing struct {
	Certificate []byte      `json:"certificate"`
	Password    string      `json:"password"`
	Profiles    Map[[]byte] `json:"profiles"`
	TeamID      string      `json:"teamId"`
}

func (s Signing) String() string {
	return fmt.Sprintf("Signing{TeamID: %s, Profiles: %d}", s.TeamID, len(s.Profiles))
}

func (s Signing) GoString() string {
	return s.String()
}

func (s Signing) LogValue() slog.Value {
	return slog.GroupValue(slog.String("teamId", s.TeamID), slog.Int("profiles", len(s.Profiles)))
}

type Version struct {
	Name string `json:"name"`
	Code int    `json:"code"`
}

// Map is a JSON object that also accepts the empty array PHP encodes for an empty one.
type Map[V any] map[string]V

func (m *Map[V]) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[]")) {
		*m = Map[V]{}
		return nil
	}
	var decoded map[string]V
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = decoded
	return nil
}

type Extension struct {
	ExpiresAt time.Time `json:"expiresAt"`
	Canceled  bool      `json:"canceled"`
}

// Source describes where to download the build's tarball. Its URL is a short-lived credential and is never logged.
type Source struct {
	URL     string      `json:"url"`
	Headers Map[string] `json:"headers"`
	Format  string      `json:"format"`
}

func (s Source) String() string {
	return fmt.Sprintf("Source{Format: %s}", s.Format)
}

func (s Source) GoString() string {
	return s.String()
}

func (s Source) LogValue() slog.Value {
	return slog.GroupValue(slog.String("format", s.Format))
}

// Session makes every call for one lease: to the lease's endpoint, with the lease token and the lease's query headers.
type Session struct {
	client  *Client
	leaseID string
	url     string
	token   string
	headers http.Header

	mutex    sync.Mutex
	sequence int64
}

func (s *Session) String() string {
	return fmt.Sprintf("Session{LeaseID: %s}", s.leaseID)
}

func (s *Session) GoString() string {
	return s.String()
}

func (c *Client) Session(credentials Credentials, lease Lease) (*Session, error) {
	if credentials.RunnerID == "" {
		return nil, errors.New("distribution: credentials have no runner id")
	}
	if lease.LeaseID == "" || lease.Token == "" {
		return nil, errors.New("distribution: lease has no id or token")
	}
	endpoint, err := parseEndpoint(lease.Endpoint, c.allowHTTP)
	if err != nil {
		return nil, err
	}
	headers, err := queryHeaders(lease.Query)
	if err != nil {
		return nil, err
	}
	return &Session{
		client:  c,
		leaseID: lease.LeaseID,
		url:     endpoint + "/distribution/runners/" + url.PathEscape(credentials.RunnerID) + "/leases/" + url.PathEscape(lease.LeaseID),
		token:   lease.Token,
		headers: headers,
	}, nil
}

// Extend renews the lease. The build must stop when the result says it was canceled.
func (s *Session) Extend(ctx context.Context) (Extension, error) {
	var extension Extension
	err := s.client.sendJSON(ctx, s.call(http.MethodPatch, ""), struct{}{}, &extension)
	return extension, err
}

// Source asks for a fresh download descriptor; ask again after a failed download, because the URL expires within minutes.
func (s *Session) Source(ctx context.Context) (Source, error) {
	response, err := s.client.send(ctx, s.call(http.MethodGet, "/source"))
	if err != nil {
		return Source{}, err
	}
	defer discard(response)
	var source Source
	if err := json.NewDecoder(response.Body).Decode(&source); err != nil {
		return Source{}, errors.New("distribution: decode source descriptor")
	}
	parsed, err := url.Parse(source.URL)
	if err != nil || parsed.Host == "" {
		return Source{}, errors.New("distribution: source descriptor has an invalid url")
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !s.client.allowHTTP) {
		return Source{}, errors.New("distribution: source url must use https")
	}
	return source, nil
}

func (s *Session) call(method string, path string) call {
	return call{
		method:  method,
		url:     s.url + path,
		token:   s.token,
		headers: s.headers,
		retry:   true,
	}
}

// nextID mints a unique, increasing event id in the orchestrator's <jobId>-<UnixNano> shape, the lease being the job.
func (s *Session) nextID(now time.Time) string {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	sequence := now.UnixNano()
	if sequence <= s.sequence {
		sequence = s.sequence + 1
	}
	s.sequence = sequence
	return fmt.Sprintf("%s-%d", s.leaseID, sequence)
}

func queryHeaders(query map[string]string) (http.Header, error) {
	headers := http.Header{}
	for key, value := range query {
		if !headerToken(key) {
			return nil, fmt.Errorf("distribution: lease query key %q is not a valid header suffix", key)
		}
		name := http.CanonicalHeaderKey("X-Appwrite-" + key)
		for _, forbidden := range forbiddenHeaders {
			if name == forbidden {
				return nil, fmt.Errorf("distribution: lease query key %q maps to forbidden header %s", key, name)
			}
		}
		headers.Set(name, value)
	}
	return headers, nil
}

func headerToken(key string) bool {
	if key == "" {
		return false
	}
	for _, character := range key {
		letter := (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z')
		digit := character >= '0' && character <= '9'
		if !letter && !digit && character != '-' {
			return false
		}
	}
	return true
}
