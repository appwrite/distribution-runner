package protocol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
	"unicode/utf8"
)

const (
	MaxEvents     = 256
	MaxBatchBytes = 4 << 20
	MaxLineBytes  = 16 << 10
	// MaxEventBytes keeps one event, as PHP's json_encode measures it, under the events action's 1,100,000-byte Assoc limit.
	MaxEventBytes = 1_000_000
)

const (
	TypeStart    = "start"
	TypeLog      = "log"
	TypeArtifact = "artifact"
	TypeExit     = "exit"
	TypeComplete = "complete"
	TypeStage    = "stage"
)

const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

const (
	ArtifactSuccess = "success"
	ArtifactFailed  = "failed"
)

// Code is an orchestrator ErrorCode; the server's SDK decoder rejects any value not listed here.
type Code string

const (
	CodeArchiveEmpty                  Code = "archive_empty"
	CodeArchiveUnknownFormat          Code = "archive_unknown_format"
	CodeArchiveCorrupt                Code = "archive_corrupt"
	CodeArchivePathInvalid            Code = "archive_path_invalid"
	CodeArchiveLayoutMismatch         Code = "archive_layout_mismatch"
	CodeArchiveExtractionFailed       Code = "archive_extraction_failed"
	CodeArchiveCreationFailed         Code = "archive_creation_failed"
	CodeArchiveCompressionUnsupported Code = "archive_compression_unsupported"
	CodeArtifactNotFound              Code = "artifact_not_found"
	CodeArtifactPermissionDenied      Code = "artifact_permission_denied"
	CodeArtifactTimeout               Code = "artifact_timeout"
	CodeArtifactCanceled              Code = "artifact_canceled"
	CodeArtifactReadFailed            Code = "artifact_read_failed"
	CodeArtifactWriteFailed           Code = "artifact_write_failed"
	CodeArtifactStatFailed            Code = "artifact_stat_failed"
	CodeArtifactListFailed            Code = "artifact_list_failed"
	CodeArtifactJSONInvalid           Code = "artifact_json_invalid"
	CodeDownloadFailed                Code = "download_failed"
	CodeDownloadHTTPError             Code = "download_http_error"
	CodeUploadFailed                  Code = "upload_failed"
	CodeCloneFailed                   Code = "clone_failed"
	CodeMountFailed                   Code = "mount_failed"
	CodeArtifactFailed                Code = "artifact_failed"
	CodeJobExitNonzero                Code = "job_exit_nonzero"
	CodeJobFailed                     Code = "job_failed"
	CodeJobOOM                        Code = "job_oom"
)

var codes = map[Code]bool{
	CodeArchiveEmpty: true, CodeArchiveUnknownFormat: true, CodeArchiveCorrupt: true, CodeArchivePathInvalid: true,
	CodeArchiveLayoutMismatch: true, CodeArchiveExtractionFailed: true, CodeArchiveCreationFailed: true,
	CodeArchiveCompressionUnsupported: true, CodeArtifactNotFound: true, CodeArtifactPermissionDenied: true,
	CodeArtifactTimeout: true, CodeArtifactCanceled: true, CodeArtifactReadFailed: true, CodeArtifactWriteFailed: true,
	CodeArtifactStatFailed: true, CodeArtifactListFailed: true, CodeArtifactJSONInvalid: true, CodeDownloadFailed: true,
	CodeDownloadHTTPError: true, CodeUploadFailed: true, CodeCloneFailed: true, CodeMountFailed: true,
	CodeArtifactFailed: true, CodeJobExitNonzero: true, CodeJobFailed: true, CodeJobOOM: true,
}

// Event is one short-typed report. ID and Time are filled in when left empty; Data is the type's data struct.
type Event struct {
	ID   string    `json:"id"`
	Type string    `json:"type"`
	Time time.Time `json:"time"`
	Data any       `json:"data"`
}

type LogData struct {
	Lines  []string `json:"lines"`
	Stream string   `json:"stream"`
}

type ArtifactData struct {
	ArtifactID      string   `json:"artifactId"`
	ArtifactType    string   `json:"artifactType"`
	Status          string   `json:"status"`
	Content         any      `json:"content,omitempty"`
	Format          string   `json:"format,omitempty"`
	DurationSeconds *float64 `json:"durationSeconds,omitempty"`
	Error           *Failure `json:"error,omitempty"`
}

type ExitData struct {
	ExitCode        int      `json:"exitCode"`
	Reason          string   `json:"reason,omitempty"`
	Image           string   `json:"image"`
	DurationSeconds float64  `json:"durationSeconds"`
	Error           *Failure `json:"error,omitempty"`
}

type StageData struct {
	Stage string `json:"stage"`
}

type Failure struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

// Report sends events in order, as many requests as the 256-event and 4 MiB limits need.
func (s *Session) Report(ctx context.Context, events []Event) error {
	encoded, err := s.encodeEvents(events)
	if err != nil {
		return err
	}
	for _, batch := range batches(encoded) {
		if err := s.sendBatch(ctx, batch); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) encodeEvents(events []Event) ([][]byte, error) {
	encoded := make([][]byte, 0, len(events))
	for _, event := range events {
		data, err := s.encodeEvent(event)
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, data)
	}
	return encoded, nil
}

// Check reports whether the event can be sent: its data is valid and it encodes within MaxEventBytes.
func (s *Session) Check(event Event) error {
	_, err := s.encodeEvent(event)
	return err
}

func (s *Session) encodeEvent(event Event) ([]byte, error) {
	if err := validate(event); err != nil {
		return nil, err
	}
	s.stamp(&event)
	if event.Data == nil {
		event.Data = struct{}{}
	}
	data, err := encode(event)
	if err != nil {
		return nil, fmt.Errorf("distribution: encode %s event: %w", event.Type, err)
	}
	if escapedLength(data) > MaxEventBytes {
		return nil, fmt.Errorf("distribution: %s event exceeds %d bytes", event.Type, MaxEventBytes)
	}
	return data, nil
}

func (s *Session) stamp(event *Event) {
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
	}
	if event.ID == "" {
		event.ID = s.nextID(event.Time)
	}
}

func (s *Session) sendBatch(ctx context.Context, batch [][]byte) error {
	body := envelope(batch)
	request := s.call(http.MethodPost, "/events")
	request.contentType = "application/json"
	request.body = func() io.Reader { return bytes.NewReader(body) }
	request.length = int64(len(body))
	response, err := s.client.send(ctx, request)
	if err != nil {
		return err
	}
	discard(response)
	return nil
}

func validate(event Event) error {
	var failure *Failure
	switch data := event.Data.(type) {
	case nil:
		if event.Type != TypeStart && event.Type != TypeComplete {
			return fmt.Errorf("distribution: %s event needs data", event.Type)
		}
		return nil
	case LogData:
		if event.Type != TypeLog {
			return mismatch(event.Type, "LogData")
		}
		if data.Stream != StreamStdout && data.Stream != StreamStderr {
			return fmt.Errorf("distribution: unknown log stream %q", data.Stream)
		}
		return nil
	case StageData:
		if event.Type != TypeStage {
			return mismatch(event.Type, "StageData")
		}
		return nil
	case ArtifactData:
		if event.Type != TypeArtifact {
			return mismatch(event.Type, "ArtifactData")
		}
		if data.Status != ArtifactSuccess && data.Status != ArtifactFailed {
			return fmt.Errorf("distribution: unknown artifact status %q", data.Status)
		}
		failure = data.Error
	case ExitData:
		if event.Type != TypeExit {
			return mismatch(event.Type, "ExitData")
		}
		failure = data.Error
	default:
		return fmt.Errorf("distribution: unsupported data %T for %s event", event.Data, event.Type)
	}
	if failure != nil && !codes[failure.Code] {
		return fmt.Errorf("distribution: unknown error code %q", failure.Code)
	}
	return nil
}

func mismatch(eventType string, data string) error {
	return fmt.Errorf("distribution: %s event cannot carry %s", eventType, data)
}

var (
	envelopeOpen  = []byte(`{"events":[`)
	envelopeClose = []byte(`]}`)
)

func envelope(batch [][]byte) []byte {
	return bytes.Join([][]byte{envelopeOpen, bytes.Join(batch, []byte(",")), envelopeClose}, nil)
}

// batches groups encoded events in order so that each request holds at most MaxEvents events and MaxBatchBytes bytes.
func batches(encoded [][]byte) [][][]byte {
	var result [][][]byte
	var current [][]byte
	size := len(envelopeOpen) + len(envelopeClose)
	for _, event := range encoded {
		separator := 0
		if len(current) > 0 {
			separator = 1
		}
		if len(current) == MaxEvents || (len(current) > 0 && size+separator+len(event) > MaxBatchBytes) {
			result = append(result, current)
			current = nil
			size = len(envelopeOpen) + len(envelopeClose)
			separator = 0
		}
		current = append(current, event)
		size += separator + len(event)
	}
	if len(current) > 0 {
		result = append(result, current)
	}
	return result
}

func encode(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// escapedLength is the length PHP's json_encode gives the same JSON with its default flags: it escapes "/" and
// writes every non-ASCII character as \uXXXX, which is what the server's Assoc validator measures.
func escapedLength(encoded []byte) int {
	length := 0
	for index := 0; index < len(encoded); {
		character := encoded[index]
		if character < utf8.RuneSelf {
			if character == '/' {
				length += 2
			} else {
				length++
			}
			index++
			continue
		}
		decoded, size := utf8.DecodeRune(encoded[index:])
		index += size
		if decoded > 0xFFFF {
			length += 12
		} else {
			length += 6
		}
	}
	return length
}
