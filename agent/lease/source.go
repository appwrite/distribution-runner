package lease

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/appwrite/distribution-runner/agent/protocol"
)

const (
	sourceFormat   = "tarball"
	sourceAttempts = 3
	sourceBackoff  = time.Second
	sourceTimeout  = 15 * time.Minute
	redirectLimit  = 10
)

var gzipMagic = []byte{0x1f, 0x8b}

// statusError is a download answered with a status other than 200.
type statusError struct {
	status int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("source download answered status %d", e.status)
}

// fetch downloads the lease's source tarball on the host to destination. Each attempt asks for a fresh descriptor,
// because the provider's URL expires within minutes. Neither the URL nor its headers ever reach an error or a log.
func (e *execution) fetch(ctx context.Context, destination string) error {
	var last error
	for attempt := 1; attempt <= sourceAttempts; attempt++ {
		if attempt > 1 {
			if err := sleep(ctx, sourceBackoff*time.Duration(attempt-1)); err != nil {
				return e.failed(ctx, err, "source", protocol.CodeDownloadFailed, sourceArtifact)
			}
		}
		descriptor, err := e.session.Source(ctx)
		if err == nil && descriptor.Format != sourceFormat {
			return e.failed(ctx, fmt.Errorf("source format %q is not %q", descriptor.Format, sourceFormat),
				"source", protocol.CodeArchiveUnknownFormat, sourceArtifact)
		}
		if err == nil {
			err = e.download(ctx, descriptor, destination)
		}
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return e.failed(ctx, err, "source", protocol.CodeDownloadFailed, sourceArtifact)
		}
		var unknown *formatError
		if errors.As(err, &unknown) {
			return e.failed(ctx, err, "source", protocol.CodeArchiveUnknownFormat, sourceArtifact)
		}
		e.logger.Warn("source download failed", "attempt", attempt, "error", err)
		last = err
	}
	code := protocol.CodeDownloadFailed
	var status *statusError
	if errors.As(last, &status) {
		code = protocol.CodeDownloadHTTPError
	}
	return e.failed(ctx, last, "source", code, sourceArtifact)
}

type formatError struct{}

func (*formatError) Error() string {
	return "source is not a gzip archive"
}

func (e *execution) download(ctx context.Context, descriptor protocol.Source, destination string) error {
	ctx, cancel := context.WithTimeout(ctx, sourceTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, descriptor.URL, nil)
	if err != nil {
		return redact(err)
	}
	for name, value := range descriptor.Headers {
		request.Header.Set(name, value)
	}
	response, err := e.runner.downloader().Do(request)
	if err != nil {
		return redact(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &statusError{status: response.StatusCode}
	}

	temporary, err := os.CreateTemp(filepath.Dir(destination), ".source-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	written, copyErr := io.Copy(temporary, response.Body)
	if closeErr := temporary.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return redact(copyErr)
	}
	if written < int64(len(gzipMagic)) {
		return &formatError{}
	}
	header := make([]byte, len(gzipMagic))
	if err := readHeader(temporary.Name(), header); err != nil {
		return err
	}
	if !bytes.Equal(header, gzipMagic) {
		return &formatError{}
	}
	return os.Rename(temporary.Name(), destination)
}

func readHeader(path string, header []byte) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.ReadFull(file, header)
	return err
}

// redact drops the URL that net/http and net/url put in their errors; the source URL is a credential.
func redact(err error) error {
	var located *url.Error
	if errors.As(err, &located) {
		return fmt.Errorf("source download: %s: %w", located.Op, located.Err)
	}
	return err
}

// downloader is the client for the provider's tarball. A redirect must stay on HTTPS (or HTTP when allowed); net/http
// already drops Authorization and cookie headers when a redirect leaves the original host.
func (r *Runner) downloader() *http.Client {
	client := http.Client{}
	if r.Download != nil {
		client = *r.Download
	}
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= redirectLimit {
			return errors.New("too many redirects")
		}
		scheme := request.URL.Scheme
		if scheme != "https" && (scheme != "http" || !r.AllowHTTP) {
			return errors.New("redirect leaves https")
		}
		return nil
	}
	return &client
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}
