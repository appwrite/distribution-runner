package protocol

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func eventsHandler(writer http.ResponseWriter, _ *http.Request) {
	writer.WriteHeader(http.StatusNoContent)
}

func TestReportSplitsAtMaxEvents(t *testing.T) {
	server := newFake(t)
	server.handle("POST "+leasePath+"/events", eventsHandler)
	events := make([]Event, MaxEvents+1)
	for index := range events {
		events[index] = Event{Type: TypeStage, Data: StageData{Stage: "stage-" + strconv.Itoa(index)}}
	}

	if err := server.session(t).Report(context.Background(), events); err != nil {
		t.Fatalf("Report: %v", err)
	}

	requests := server.matching(http.MethodPost, leasePath+"/events")
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	first, second := decodeBatch(t, requests[0].Body), decodeBatch(t, requests[1].Body)
	if len(first.Events) != MaxEvents || len(second.Events) != 1 {
		t.Fatalf("batch sizes = %d, %d", len(first.Events), len(second.Events))
	}
	all := append(first.Events, second.Events...)
	previous := int64(0)
	for index, event := range all {
		var data StageData
		if err := json.Unmarshal(event.Data, &data); err != nil || data.Stage != "stage-"+strconv.Itoa(index) {
			t.Fatalf("event %d out of order: %s", index, event.Data)
		}
		suffix, found := strings.CutPrefix(event.ID, testLeaseID+"-")
		sequence, err := strconv.ParseInt(suffix, 10, 64)
		if !found || err != nil || sequence <= previous {
			t.Fatalf("event %d id %q is not an increasing <leaseId>-<UnixNano>", index, event.ID)
		}
		previous = sequence
		if event.Time.IsZero() || event.Type != TypeStage {
			t.Fatalf("event %d = %+v", index, event)
		}
	}
}

func TestReportSplitsAtMaxBatchBytes(t *testing.T) {
	server := newFake(t)
	server.handle("POST "+leasePath+"/events", eventsHandler)
	line := strings.Repeat("x", MaxLineBytes)
	var events []Event
	for range 5 {
		lines := make([]string, 55)
		for index := range lines {
			lines[index] = line
		}
		events = append(events, Event{Type: TypeLog, Data: LogData{Lines: lines, Stream: StreamStdout}})
	}

	if err := server.session(t).Report(context.Background(), events); err != nil {
		t.Fatalf("Report: %v", err)
	}

	requests := server.matching(http.MethodPost, leasePath+"/events")
	if len(requests) < 2 {
		t.Fatalf("requests = %d, want the 4.5 MB of events split", len(requests))
	}
	total := 0
	for _, request := range requests {
		if len(request.Body) > MaxBatchBytes {
			t.Errorf("request body = %d bytes, over %d", len(request.Body), MaxBatchBytes)
		}
		total += len(decodeBatch(t, request.Body).Events)
	}
	if total != len(events) {
		t.Errorf("events received = %d, want %d", total, len(events))
	}
}

func TestReportEventShapes(t *testing.T) {
	server := newFake(t)
	server.handle("POST "+leasePath+"/events", eventsHandler)
	duration := 1.5
	events := []Event{
		{Type: TypeStart},
		{Type: TypeStage, Data: StageData{Stage: "build"}},
		{Type: TypeLog, Data: LogData{Lines: []string{"a </b> & c"}, Stream: StreamStderr}},
		{Type: TypeExit, Data: ExitData{ExitCode: 65, Image: "macos-27-xcode-27.2", DurationSeconds: 12.5, OccupancySeconds: 40.25, Error: &Failure{Code: CodeJobExitNonzero, Message: "exit 65"}}},
		{Type: TypeArtifact, Data: ArtifactData{ArtifactID: "result", ArtifactType: "read", Status: ArtifactSuccess, Content: map[string]any{"digest": "abc"}, Format: "json", DurationSeconds: &duration}},
		{Type: TypeArtifact, Data: ArtifactData{ArtifactID: "output", ArtifactType: "upload", Status: ArtifactFailed, Error: &Failure{Code: CodeUploadFailed, Message: "upload failed"}}},
		{Type: TypeComplete},
	}
	want := []string{
		`{}`,
		`{"stage":"build"}`,
		`{"lines":["a </b> & c"],"stream":"stderr"}`,
		`{"exitCode":65,"image":"macos-27-xcode-27.2","durationSeconds":12.5,"occupancySeconds":40.25,"error":{"code":"job_exit_nonzero","message":"exit 65"}}`,
		`{"artifactId":"result","artifactType":"read","status":"success","content":{"digest":"abc"},"format":"json","durationSeconds":1.5}`,
		`{"artifactId":"output","artifactType":"upload","status":"failed","error":{"code":"upload_failed","message":"upload failed"}}`,
		`{}`,
	}

	if err := server.session(t).Report(context.Background(), events); err != nil {
		t.Fatalf("Report: %v", err)
	}

	received := decodeBatch(t, server.matching(http.MethodPost, leasePath+"/events")[0].Body).Events
	for index, event := range received {
		if event.Type != events[index].Type || string(event.Data) != want[index] {
			t.Errorf("event %d = %s %s, want %s %s", index, event.Type, event.Data, events[index].Type, want[index])
		}
	}
}

func TestReportRejectsInvalidEvents(t *testing.T) {
	cases := map[string]Event{
		"unknown type":      {Type: "finish"},
		"mismatched data":   {Type: TypeExit, Data: StageData{Stage: "build"}},
		"missing data":      {Type: TypeExit},
		"unknown stream":    {Type: TypeLog, Data: LogData{Lines: []string{"x"}, Stream: "stdin"}},
		"unknown code":      {Type: TypeExit, Data: ExitData{Error: &Failure{Code: "build_failed"}}},
		"unknown status":    {Type: TypeArtifact, Data: ArtifactData{Status: "done"}},
		"map data":          {Type: TypeStage, Data: map[string]string{"stage": "build"}},
		"oversize log data": {Type: TypeLog, Data: LogData{Lines: []string{strings.Repeat("é", MaxEventBytes/6+1)}, Stream: StreamStdout}},
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			server := newFake(t)
			server.handle("POST "+leasePath+"/events", eventsHandler)

			if err := server.session(t).Report(context.Background(), []Event{event}); err == nil {
				t.Fatal("Report accepted an invalid event")
			}
			if requests := server.recorded(); len(requests) != 0 {
				t.Errorf("requests = %d, want none", len(requests))
			}
		})
	}
}

func TestEscapedLengthMatchesPHP(t *testing.T) {
	cases := map[string]int{
		`"plain"`:  7,
		`"a/b"`:    6,
		`"é"`:      8,
		`"€"`:      8,
		`"😀"`:      14,
		`"\n\"\\"`: 8,
	}
	for encoded, want := range cases {
		if got := escapedLength([]byte(encoded)); got != want {
			t.Errorf("escapedLength(%s) = %d, want %d", encoded, got, want)
		}
	}
}
