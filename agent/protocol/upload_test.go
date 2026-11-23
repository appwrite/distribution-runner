package protocol

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type chunk struct {
	start  int64
	end    int64
	total  int64
	id     string
	named  bool
	length int64
}

type uploadServer struct {
	t       *testing.T
	content []byte
	delay   time.Duration
	fail    func(start int64, attempt int32) int

	mutex    sync.Mutex
	chunks   []chunk
	attempts map[int64]int32
	stored   map[int64]bool

	inFlight atomic.Int32
	peak     atomic.Int32
}

func (u *uploadServer) handler(writer http.ResponseWriter, request *http.Request) {
	current := u.inFlight.Add(1)
	defer u.inFlight.Add(-1)
	for {
		peak := u.peak.Load()
		if current <= peak || u.peak.CompareAndSwap(peak, current) {
			break
		}
	}
	var start, end, total int64
	if _, err := fmt.Sscanf(request.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total); err != nil {
		u.t.Errorf("Content-Range %q: %v", request.Header.Get("Content-Range"), err)
		respond(u.t, writer, http.StatusBadRequest, nil)
		return
	}
	if request.ContentLength <= 0 {
		u.t.Errorf("chunk at %d sent without a Content-Length", start)
	}
	data := u.part(request)

	_, named := request.Header["X-Appwrite-Id"]
	u.mutex.Lock()
	u.attempts[start]++
	attempt := u.attempts[start]
	u.chunks = append(u.chunks, chunk{start: start, end: end, total: total, id: request.Header.Get("X-Appwrite-Id"), named: named, length: int64(len(data))})
	u.mutex.Unlock()

	if !bytes.Equal(data, u.content[start:min(end+1, int64(len(u.content)))]) {
		u.t.Errorf("chunk at %d carried the wrong bytes", start)
	}
	time.Sleep(u.delay)
	if u.fail != nil {
		if status := u.fail(start, attempt); status != 0 {
			writer.Header().Set("Retry-After", "0")
			respond(u.t, writer, status, map[string]string{"message": "try again"})
			return
		}
	}

	u.mutex.Lock()
	u.stored[start] = true
	uploaded := len(u.stored)
	u.mutex.Unlock()
	chunks := (total + ChunkSize - 1) / ChunkSize
	respond(u.t, writer, http.StatusAccepted, map[string]any{"$id": "upload1", "chunksTotal": chunks, "chunksUploaded": uploaded})
}

type uploadState struct {
	chunks   []chunk
	attempts map[int64]int32
	stored   int
}

func (u *uploadServer) state() uploadState {
	u.mutex.Lock()
	defer u.mutex.Unlock()
	attempts := make(map[int64]int32, len(u.attempts))
	for start, count := range u.attempts {
		attempts[start] = count
	}
	return uploadState{chunks: append([]chunk(nil), u.chunks...), attempts: attempts, stored: len(u.stored)}
}

func (u *uploadServer) part(request *http.Request) []byte {
	mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		u.t.Errorf("Content-Type = %q", request.Header.Get("Content-Type"))
		return nil
	}
	reader := multipart.NewReader(request.Body, parameters["boundary"])
	part, err := reader.NextPart()
	if err != nil {
		u.t.Errorf("multipart: %v", err)
		return nil
	}
	if part.FormName() != UploadField || part.FileName() != "app.ipa" {
		u.t.Errorf("part field %q file %q, want %q app.ipa", part.FormName(), part.FileName(), UploadField)
	}
	data, err := io.ReadAll(part)
	if err != nil {
		u.t.Errorf("read part: %v", err)
	}
	if _, err := reader.NextPart(); err != io.EOF {
		u.t.Errorf("form has more than one part: %v", err)
	}
	return data
}

func newUpload(t *testing.T, size int) (*fake, *uploadServer, string) {
	t.Helper()
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.ipa")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	upload := &uploadServer{t: t, content: content, attempts: map[int64]int32{}, stored: map[int64]bool{}}
	server := newFake(t)
	server.handle("POST "+leasePath+"/artifacts", upload.handler)
	return server, upload, path
}

func TestUploadChunks(t *testing.T) {
	size := 6*ChunkSize + 1234
	server, upload, path := newUpload(t, size)
	upload.delay = 50 * time.Millisecond

	result, err := server.session(t).Upload(context.Background(), path)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	state := upload.state()
	if result["chunksUploaded"] != float64(7) || result["chunksTotal"] != float64(7) || result["$id"] != "upload1" {
		t.Errorf("result = %v, want the response that counts all 7 chunks", result)
	}
	if len(state.chunks) != 7 {
		t.Fatalf("chunks = %d, want 7", len(state.chunks))
	}
	first := state.chunks[0]
	if first.start != 0 || first.named {
		t.Errorf("first request = %+v, want the chunk at 0 with no x-appwrite-id", first)
	}
	seen := map[int64]bool{}
	for index, received := range state.chunks {
		want := int64(ChunkSize)
		if received.start == 6*ChunkSize {
			want = 1234
		}
		if received.start%ChunkSize != 0 || received.length != want || received.end != received.start+want-1 || received.total != int64(size) {
			t.Errorf("chunk %d = %+v, want an exact %d-byte chunk", index, received, want)
		}
		if index > 0 && (!received.named || received.id != "upload1") {
			t.Errorf("chunk %d x-appwrite-id = %q, want the first response's $id", index, received.id)
		}
		seen[received.start] = true
	}
	if len(seen) != 7 {
		t.Errorf("distinct chunks = %d, want 7", len(seen))
	}
	if peak := upload.peak.Load(); peak > 4 || peak < 2 {
		t.Errorf("peak concurrency = %d, want between 2 and 4", peak)
	}
	for _, request := range server.recorded() {
		bearer(t, request, testLeaseToken)
	}
}

func TestUploadSingleChunk(t *testing.T) {
	server, upload, path := newUpload(t, 100)

	result, err := server.session(t).Upload(context.Background(), path)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	if state := upload.state(); len(state.chunks) != 1 || state.chunks[0] != (chunk{start: 0, end: 99, total: 100, length: 100}) {
		t.Errorf("chunks = %+v", state.chunks)
	}
	if result["chunksUploaded"] != float64(1) {
		t.Errorf("result = %v", result)
	}
}

func TestUploadRetriesChunks(t *testing.T) {
	server, upload, path := newUpload(t, 2*ChunkSize+10)
	upload.fail = func(start int64, attempt int32) int {
		if start == ChunkSize && attempt == 1 {
			return http.StatusTooManyRequests
		}
		if start == ChunkSize && attempt == 2 {
			return http.StatusServiceUnavailable
		}
		return 0
	}

	if _, err := server.session(t).Upload(context.Background(), path); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	state := upload.state()
	if attempts := state.attempts[ChunkSize]; attempts != 3 {
		t.Errorf("attempts for the second chunk = %d, want 3", attempts)
	}
	if state.stored != 3 {
		t.Errorf("stored chunks = %d, want 3", state.stored)
	}
}

func TestUploadStopsOnRejectedChunk(t *testing.T) {
	server, upload, path := newUpload(t, 3*ChunkSize)
	upload.fail = func(start int64, _ int32) int {
		if start == ChunkSize {
			return http.StatusBadRequest
		}
		return 0
	}

	_, err := server.session(t).Upload(context.Background(), path)

	var response *ResponseError
	if !errors.As(err, &response) || response.Status != http.StatusBadRequest {
		t.Fatalf("error = %v, want the 400", err)
	}
	if attempts := upload.state().attempts[ChunkSize]; attempts != 1 {
		t.Errorf("a 400 chunk was retried %d times", attempts)
	}
}

func TestUploadRefusesEmptyFile(t *testing.T) {
	server, _, _ := newUpload(t, 0)
	path := filepath.Join(t.TempDir(), "empty.ipa")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := server.session(t).Upload(context.Background(), path); err == nil {
		t.Fatal("Upload accepted an empty file")
	}
	if requests := server.recorded(); len(requests) != 0 {
		t.Errorf("requests = %d, want none", len(requests))
	}
}
