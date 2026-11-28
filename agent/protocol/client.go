package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	PlatformMacOS = "macos"

	StatusOnline   = "online"
	StatusDraining = "draining"
	StatusOffline  = "offline"
)

const (
	errorBodyBytes = 64 << 10
	// RequestTimeout bounds one attempt of a JSON call, response body included.
	RequestTimeout = 30 * time.Second
	// UploadTimeout bounds one attempt of a chunk upload, and how long any request waits for response headers.
	UploadTimeout = 5 * time.Minute
)

// forbiddenHeaders are the headers RFC §7.3 forbids the agent to send.
var forbiddenHeaders = []string{
	"X-Appwrite-Key",
	"X-Appwrite-Jwt",
	"X-Appwrite-Mode",
	"X-Appwrite-Response-Format",
}

// reservedHeaders are set by the agent itself, so a lease query key must not name them either.
var reservedHeaders = []string{
	"X-Appwrite-Id",
}

type Client struct {
	endpoint  string
	http      *http.Client
	allowHTTP bool
	retry     retry
}

type retry struct {
	attempts int
	backoff  time.Duration
	ceiling  time.Duration
}

type Registration struct {
	Name     string   `json:"name"`
	Platform string   `json:"platform"`
	Labels   []string `json:"labels"`
	Capacity int      `json:"capacity"`
	Version  string   `json:"version"`
}

type Credentials struct {
	RunnerID string `json:"runnerId"`
	Token    string `json:"token"`
}

func (c Credentials) String() string {
	return fmt.Sprintf("Credentials{RunnerID: %s}", c.RunnerID)
}

func (c Credentials) GoString() string {
	return c.String()
}

func (c Credentials) LogValue() slog.Value {
	return slog.GroupValue(slog.String("runnerId", c.RunnerID))
}

type Status struct {
	Status  string   `json:"status"`
	Version string   `json:"version"`
	Labels  []string `json:"labels"`
}

// ResponseError is a non-2xx answer, carrying the Appwrite error type and message when the body has them.
type ResponseError struct {
	Status  int
	Type    string
	Message string
}

// Permanent reports whether the server refused the request itself, so repeating it cannot succeed.
func (e *ResponseError) Permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests && e.Status != http.StatusRequestTimeout
}

func (e *ResponseError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("distribution: unexpected status %d", e.Status)
	}
	if e.Type == "" {
		return fmt.Sprintf("distribution: status %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("distribution: status %d (%s): %s", e.Status, e.Type, e.Message)
}

type call struct {
	method      string
	url         string
	token       string
	headers     http.Header
	contentType string
	body        func() io.Reader
	length      int64
	retry       bool
	timeout     time.Duration
}

// New returns a client for an Appwrite endpoint that includes /v1. The endpoint must be HTTPS unless allowHTTP is set.
// The client never follows redirects, so a bearer token cannot be forwarded to another URL. Every attempt of every
// call has its own deadline, so a server that accepts a connection and never answers cannot wedge the caller.
func New(endpoint string, client *http.Client, allowHTTP bool) (*Client, error) {
	base, err := parseEndpoint(endpoint, allowHTTP)
	if err != nil {
		return nil, err
	}
	if client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = UploadTimeout
		client = &http.Client{Transport: transport}
	}
	copied := *client
	copied.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		endpoint:  base,
		http:      &copied,
		allowHTTP: allowHTTP,
		retry: retry{
			attempts: 5,
			backoff:  time.Second,
			ceiling:  30 * time.Second,
		},
	}, nil
}

func (c *Client) Enrol(ctx context.Context, secret string, registration Registration) (Credentials, error) {
	if secret == "" {
		return Credentials{}, errors.New("distribution: enrolment secret is empty")
	}
	registration.Labels = nonNil(registration.Labels)
	var credentials Credentials
	err := c.sendJSON(ctx, call{
		method: http.MethodPost,
		url:    c.endpoint + "/distribution/runners",
		token:  secret,
	}, registration, &credentials)
	if err != nil {
		return Credentials{}, err
	}
	if credentials.RunnerID == "" || credentials.Token == "" {
		return Credentials{}, errors.New("distribution: enrolment response has no runner id or token")
	}
	return credentials, nil
}

func (c *Client) Heartbeat(ctx context.Context, credentials Credentials, status Status) error {
	status.Labels = nonNil(status.Labels)
	return c.sendJSON(ctx, call{
		method: http.MethodPatch,
		url:    c.runnerURL(credentials),
		token:  credentials.Token,
		retry:  true,
	}, status, nil)
}

// Claim returns the next lease, or nil when nothing is queued. A claim is never retried, because a lost
// response would leave a lease held until it expires; the caller's claim loop asks again.
func (c *Client) Claim(ctx context.Context, credentials Credentials, labels []string) (*Lease, error) {
	body, err := json.Marshal(struct {
		Labels []string `json:"labels"`
	}{nonNil(labels)})
	if err != nil {
		return nil, err
	}
	response, err := c.send(ctx, jsonCall(call{
		method: http.MethodPost,
		url:    c.runnerURL(credentials) + "/leases",
		token:  credentials.Token,
	}, body))
	if err != nil {
		return nil, err
	}
	defer discard(response)
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	var lease Lease
	if err := json.NewDecoder(response.Body).Decode(&lease); err != nil {
		return nil, fmt.Errorf("distribution: decode lease: %w", err)
	}
	if lease.LeaseID == "" || lease.Token == "" || lease.Endpoint == "" {
		return nil, errors.New("distribution: lease has no id, token or endpoint")
	}
	lease.ExpiresAt = hostTime(lease.ExpiresAt, response.Header)
	return &lease, nil
}

func (c *Client) runnerURL(credentials Credentials) string {
	return c.endpoint + "/distribution/runners/" + url.PathEscape(credentials.RunnerID)
}

func (c *Client) sendJSON(ctx context.Context, request call, input any, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	response, err := c.send(ctx, jsonCall(request, body))
	if err != nil {
		return err
	}
	defer discard(response)
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		return fmt.Errorf("distribution: decode response: %w", err)
	}
	return nil
}

func jsonCall(request call, body []byte) call {
	request.contentType = "application/json"
	request.body = func() io.Reader { return bytes.NewReader(body) }
	request.length = int64(len(body))
	return request
}

// send performs the call and returns a 2xx response whose body the caller must close.
// Retryable calls are repeated on transport errors, 429 and 5xx, waiting for Retry-After when the server names it.
func (c *Client) send(ctx context.Context, request call) (*http.Response, error) {
	attempts := 1
	if request.retry {
		attempts = c.retry.attempts
	}
	for attempt := 1; ; attempt++ {
		response, err := c.attempt(ctx, request)
		if err != nil {
			if ctx.Err() != nil || attempt >= attempts {
				return nil, err
			}
			if err := sleep(ctx, c.retry.delay(attempt, "")); err != nil {
				return nil, err
			}
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, nil
		}
		retryable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		if !retryable || attempt >= attempts {
			defer discard(response)
			return nil, responseError(response)
		}
		wait := c.retry.delay(attempt, response.Header.Get("Retry-After"))
		discard(response)
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// attempt sends the call once under its own deadline, which ends when the response body is closed.
func (c *Client) attempt(ctx context.Context, request call) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, positive(request.timeout, RequestTimeout))
	response, err := c.do(ctx, request)
	if err != nil {
		cancel()
		return nil, err
	}
	response.Body = &bounded{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

func (c *Client) do(ctx context.Context, request call) (*http.Response, error) {
	var body io.Reader
	if request.body != nil {
		body = request.body()
	}
	outgoing, err := http.NewRequestWithContext(ctx, request.method, request.url, body)
	if err != nil {
		return nil, err
	}
	if request.body != nil {
		outgoing.ContentLength = request.length
		outgoing.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(request.body()), nil
		}
		outgoing.Header.Set("Content-Type", request.contentType)
	}
	for name, values := range request.headers {
		outgoing.Header[name] = values
	}
	outgoing.Header.Set("Accept", "application/json")
	outgoing.Header.Set("Authorization", "Bearer "+request.token)
	return c.http.Do(outgoing)
}

// bounded releases its attempt's deadline when the body is closed.
type bounded struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *bounded) Close() error {
	defer b.cancel()
	return b.ReadCloser.Close()
}

// hostTime moves a server timestamp onto the host clock using the response's Date header, so a host clock that runs
// ahead or behind does not shorten or stretch the lease. Without a usable Date it returns the timestamp unchanged.
func hostTime(at time.Time, header http.Header) time.Time {
	if at.IsZero() {
		return at
	}
	served, err := http.ParseTime(header.Get("Date"))
	if err != nil {
		return at
	}
	return time.Now().Add(at.Sub(served))
}

func positive(value time.Duration, otherwise time.Duration) time.Duration {
	if value <= 0 {
		return otherwise
	}
	return value
}

func (r retry) delay(attempt int, retryAfter string) time.Duration {
	if wait, ok := parseRetryAfter(retryAfter); ok {
		return wait
	}
	wait := r.backoff << (attempt - 1)
	if wait > r.ceiling || wait <= 0 {
		return r.ceiling
	}
	return wait
}

func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(time.Until(at), 0), true
	}
	return 0, false
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func responseError(response *http.Response) error {
	result := &ResponseError{Status: response.StatusCode}
	var body struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, errorBodyBytes)).Decode(&body) == nil {
		result.Message = body.Message
		result.Type = body.Type
	}
	return result
}

func discard(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, errorBodyBytes))
	_ = response.Body.Close()
}

func parseEndpoint(endpoint string, allowHTTP bool) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("distribution: invalid endpoint: %w", err)
	}
	switch {
	case parsed.Scheme == "https":
	case parsed.Scheme == "http" && allowHTTP:
	case parsed.Scheme == "http":
		return "", errors.New("distribution: endpoint must use https")
	default:
		return "", fmt.Errorf("distribution: unsupported endpoint scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", errors.New("distribution: endpoint has no host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("distribution: endpoint must not carry credentials, a query or a fragment")
	}
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
