package protocol

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	ChunkSize = 5_242_880
	// UploadConcurrency is how many chunks are in flight at once after the first one has named the upload.
	UploadConcurrency = 4
	UploadField       = "file"
)

// Upload sends the file at path in exact ChunkSize chunks under an upload id the agent mints, so a first chunk
// resent after a lost response names the same upload instead of creating another. The first chunk goes alone, so the
// server creates the upload once, then the rest concurrently. It returns the response that reports every chunk
// uploaded, and fails when none does.
func (s *Session) Upload(ctx context.Context, path string) (map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	information, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := information.Size()
	if size == 0 {
		return nil, errors.New("distribution: refusing to upload an empty file")
	}
	upload := chunkedUpload{
		session: s,
		file:    file,
		name:    filepath.Base(path),
		size:    size,
		id:      strings.ToLower(rand.Text()),
	}
	chunks := (size + ChunkSize - 1) / ChunkSize

	first, err := upload.send(ctx, 0)
	if err != nil {
		return nil, err
	}
	if id, ok := first["$id"].(string); ok && id != upload.id {
		return nil, fmt.Errorf("distribution: the server named the upload %q, not %q", id, upload.id)
	}
	responses := make([]map[string]any, chunks)
	responses[0] = first
	if err := upload.rest(ctx, chunks, responses); err != nil {
		return nil, err
	}
	return final(responses)
}

type chunkedUpload struct {
	session *Session
	file    *os.File
	name    string
	size    int64
	id      string
}

func (u chunkedUpload) rest(ctx context.Context, chunks int64, responses []map[string]any) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	indexes := make(chan int64)
	var group sync.WaitGroup
	for range min(UploadConcurrency, chunks-1) {
		group.Go(func() {
			for index := range indexes {
				response, err := u.send(ctx, index)
				if err != nil {
					cancel(err)
					continue
				}
				responses[index] = response
			}
		})
	}
	for index := int64(1); index < chunks && ctx.Err() == nil; index++ {
		select {
		case indexes <- index:
		case <-ctx.Done():
		}
	}
	close(indexes)
	group.Wait()
	return context.Cause(ctx)
}

func (u chunkedUpload) send(ctx context.Context, index int64) (map[string]any, error) {
	start := index * ChunkSize
	length := min(ChunkSize, u.size-start)
	preamble, epilogue, contentType, err := u.multipart()
	if err != nil {
		return nil, err
	}
	request := u.session.call(http.MethodPost, "/artifacts")
	request.headers = request.headers.Clone()
	request.headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, u.size))
	request.headers.Set("X-Appwrite-Id", u.id)
	request.timeout = UploadTimeout
	request.contentType = contentType
	request.length = int64(len(preamble)) + length + int64(len(epilogue))
	request.body = func() io.Reader {
		return io.MultiReader(bytes.NewReader(preamble), io.NewSectionReader(u.file, start, length), bytes.NewReader(epilogue))
	}
	response, err := u.session.client.send(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("distribution: upload chunk %d: %w", index+1, err)
	}
	defer discard(response)
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("distribution: decode chunk %d response: %w", index+1, err)
	}
	return decoded, nil
}

// multipart returns the bytes before and after the file content of a one-part form, so the chunk itself
// streams from disk with an exact Content-Length.
func (u chunkedUpload) multipart() ([]byte, []byte, string, error) {
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, UploadField, quote(u.name)))
	header.Set("Content-Type", "application/octet-stream")
	if _, err := writer.CreatePart(header); err != nil {
		return nil, nil, "", err
	}
	preamble := bytes.Clone(buffer.Bytes())
	buffer.Reset()
	if err := writer.Close(); err != nil {
		return nil, nil, "", err
	}
	return preamble, bytes.Clone(buffer.Bytes()), writer.FormDataContentType(), nil
}

func quote(name string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\r", "", "\n", "").Replace(name)
}

// final picks the response that counts every chunk; with concurrent chunks it need not be the last one sent.
func final(responses []map[string]any) (map[string]any, error) {
	for _, response := range responses {
		uploaded, _ := response["chunksUploaded"].(float64)
		total, _ := response["chunksTotal"].(float64)
		if total > 0 && uploaded == total && int(total) == len(responses) {
			return response, nil
		}
	}
	return nil, fmt.Errorf("distribution: no chunk response reports all %d chunks uploaded", len(responses))
}
