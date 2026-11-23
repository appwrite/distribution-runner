package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

type receivedEvent struct {
	Type   string
	Lines  []string
	Stream string
	Stage  string
}

func receivedEvents(t *testing.T, server *fake) []receivedEvent {
	t.Helper()
	var events []receivedEvent
	for _, request := range server.matching(http.MethodPost, leasePath+"/events") {
		for _, event := range decodeBatch(t, request.Body).Events {
			var data struct {
				Lines  []string `json:"lines"`
				Stream string   `json:"stream"`
				Stage  string   `json:"stage"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatalf("decode data: %v", err)
			}
			events = append(events, receivedEvent{Type: event.Type, Lines: data.Lines, Stream: data.Stream, Stage: data.Stage})
		}
	}
	return events
}

func TestFoldFortyKiBLine(t *testing.T) {
	line := strings.Repeat("a", 40<<10)

	pieces := fold(line)

	if len(pieces) != 3 || len(pieces[0]) != MaxLineBytes || len(pieces[1]) != MaxLineBytes || len(pieces[2]) != 8<<10 {
		t.Fatalf("pieces = %d with lengths %v", len(pieces), lengths(pieces))
	}
}

func TestFoldKeepsRunesWhole(t *testing.T) {
	for _, character := range []string{"é", "€", "😀"} {
		line := "x" + strings.Repeat(character, 3*MaxLineBytes/len(character))

		pieces := fold(line)

		if strings.Join(pieces, "") != line {
			t.Fatalf("%s: pieces do not rejoin to the line", character)
		}
		for index, piece := range pieces {
			if len(piece) > MaxLineBytes || !utf8.ValidString(piece) {
				t.Fatalf("%s: piece %d has %d bytes, valid UTF-8 %v", character, index, len(piece), utf8.ValidString(piece))
			}
		}
	}
}

func TestFoldInvalidUTF8CutsAtLimit(t *testing.T) {
	line := strings.Repeat("\x80", MaxLineBytes+10)

	pieces := fold(line)

	if len(pieces) != 2 || len(pieces[0]) != MaxLineBytes || len(pieces[1]) != 10 {
		t.Fatalf("lengths = %v", lengths(pieces))
	}
}

func lengths(pieces []string) []int {
	result := make([]int, len(pieces))
	for index, piece := range pieces {
		result[index] = len(piece)
	}
	return result
}

func TestReporterFoldsLongLine(t *testing.T) {
	server := newFake(t)
	server.handle("POST "+leasePath+"/events", eventsHandler)
	reporter := NewReporter(server.session(t), time.Hour)

	reporter.Log(StreamStdout, strings.Repeat("a", 40<<10))
	if err := reporter.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := receivedEvents(t, server)
	if len(events) != 1 || len(events[0].Lines) != 3 {
		t.Fatalf("events = %d, want one log event with 3 folded lines", len(events))
	}
	if got := lengths(events[0].Lines); got[0] != MaxLineBytes || got[1] != MaxLineBytes || got[2] != 8<<10 {
		t.Errorf("line lengths = %v", got)
	}
}

func TestReporterCoalescesLinesInOrder(t *testing.T) {
	server := newFake(t)
	server.handle("POST "+leasePath+"/events", eventsHandler)
	reporter := NewReporter(server.session(t), time.Hour)

	reporter.Log(StreamStdout, "a")
	reporter.Log(StreamStdout, "b")
	reporter.Log(StreamStderr, "c")
	reporter.Emit(Event{Type: TypeStage, Data: StageData{Stage: "build"}})
	reporter.Log(StreamStdout, "d")
	if err := reporter.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := receivedEvents(t, server)
	want := []receivedEvent{
		{Type: TypeLog, Lines: []string{"a", "b"}, Stream: StreamStdout},
		{Type: TypeLog, Lines: []string{"c"}, Stream: StreamStderr},
		{Type: TypeStage, Stage: "build"},
		{Type: TypeLog, Lines: []string{"d"}, Stream: StreamStdout},
	}
	if len(got) != len(want) {
		t.Fatalf("events = %+v", got)
	}
	for index := range want {
		if got[index].Type != want[index].Type || strings.Join(got[index].Lines, ",") != strings.Join(want[index].Lines, ",") ||
			got[index].Stream != want[index].Stream || got[index].Stage != want[index].Stage {
			t.Errorf("event %d = %+v, want %+v", index, got[index], want[index])
		}
	}
}

func TestReporterSends257EventsInTwoRequests(t *testing.T) {
	server := newFake(t)
	server.handle("POST "+leasePath+"/events", eventsHandler)
	reporter := NewReporter(server.session(t), time.Hour)

	for index := range MaxEvents + 1 {
		reporter.Emit(Event{Type: TypeStage, Data: StageData{Stage: strconv.Itoa(index)}})
	}
	if err := reporter.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	requests := server.matching(http.MethodPost, leasePath+"/events")
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	events := receivedEvents(t, server)
	if len(events) != MaxEvents+1 {
		t.Fatalf("events = %d, want %d", len(events), MaxEvents+1)
	}
	for index, event := range events {
		if event.Stage != strconv.Itoa(index) {
			t.Fatalf("event %d has stage %s", index, event.Stage)
		}
	}
}

func TestReporterSplitsLargeOutput(t *testing.T) {
	server := newFake(t)
	server.handle("POST "+leasePath+"/events", eventsHandler)
	reporter := NewReporter(server.session(t), time.Hour)
	line := strings.Repeat("y", MaxLineBytes)
	count := 300

	for range count {
		reporter.Log(StreamStdout, line)
	}
	if err := reporter.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	requests := server.matching(http.MethodPost, leasePath+"/events")
	if len(requests) < 2 {
		t.Fatalf("requests = %d, want the 4.9 MB of output split", len(requests))
	}
	lines := 0
	for _, request := range requests {
		if len(request.Body) > MaxBatchBytes {
			t.Errorf("request body = %d bytes, over %d", len(request.Body), MaxBatchBytes)
		}
		for _, event := range decodeBatch(t, request.Body).Events {
			if length := escapedLength(event.Data); length > MaxEventBytes {
				t.Errorf("event data = %d escaped bytes, over %d", length, MaxEventBytes)
			}
		}
	}
	for _, event := range receivedEvents(t, server) {
		lines += len(event.Lines)
	}
	if lines != count {
		t.Errorf("lines = %d, want %d", lines, count)
	}
}

func TestReporterFlushesOnInterval(t *testing.T) {
	server := newFake(t)
	server.handle("POST "+leasePath+"/events", eventsHandler)
	reporter := NewReporter(server.session(t), 10*time.Millisecond)
	defer reporter.Close(context.Background())

	reporter.Log(StreamStdout, "tick")

	deadline := time.Now().Add(2 * time.Second)
	for len(server.matching(http.MethodPost, leasePath+"/events")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no events sent before Close")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestReporterKeepsEventsAfterTransientFailure(t *testing.T) {
	server := newFake(t)
	var failing atomic.Bool
	failing.Store(true)
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	server.handle("POST "+leasePath+"/events", func(writer http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		if failing.Load() {
			respond(t, writer, http.StatusServiceUnavailable, nil)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	reporter := NewReporter(server.session(t), time.Hour)

	reporter.Log(StreamStdout, "first")
	flushed := make(chan error)
	go func() { flushed <- reporter.Flush(context.Background()) }()
	<-entered
	reporter.Log(StreamStdout, "second")
	close(release)
	err := <-flushed
	var response *ResponseError
	if !errors.As(err, &response) || response.Status != http.StatusServiceUnavailable {
		t.Fatalf("Flush error = %v, want 503", err)
	}
	failing.Store(false)
	if err := reporter.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	requests := server.matching(http.MethodPost, leasePath+"/events")
	last := decodeBatch(t, requests[len(requests)-1].Body).Events
	var lines []string
	for _, event := range last {
		var data LogData
		if err := json.Unmarshal(event.Data, &data); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, data.Lines...)
	}
	if got := strings.Join(lines, ","); got != "first,second" {
		t.Errorf("delivered lines = %q, want the retained line first, then the new one", got)
	}
}

func TestReporterDropsRejectedBatch(t *testing.T) {
	server := newFake(t)
	server.handle("POST "+leasePath+"/events", func(writer http.ResponseWriter, _ *http.Request) {
		respond(t, writer, http.StatusBadRequest, map[string]any{"message": "Invalid events", "type": "general_argument_invalid"})
	})
	reporter := NewReporter(server.session(t), time.Hour)

	reporter.Log(StreamStdout, "line")
	if err := reporter.Flush(context.Background()); err != nil {
		t.Fatalf("Flush returned %v for a dropped batch, want it kept for Close", err)
	}
	err := reporter.Close(context.Background())

	var response *ResponseError
	if !errors.As(err, &response) || response.Status != http.StatusBadRequest {
		t.Fatalf("Close error = %v, want the 400", err)
	}
	if requests := server.matching(http.MethodPost, leasePath+"/events"); len(requests) != 1 {
		t.Errorf("requests = %d, want the rejected batch sent once", len(requests))
	}
}

func TestReporterWriterSplitsLines(t *testing.T) {
	server := newFake(t)
	server.handle("POST "+leasePath+"/events", eventsHandler)
	reporter := NewReporter(server.session(t), time.Hour)
	writer := reporter.Writer(StreamStderr)

	for _, chunk := range []string{"one\ntw", "o\r\n", "\nthr", "ee"} {
		if _, err := io.WriteString(writer, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := io.WriteString(writer, strings.Repeat("z", MaxLineBytes+1)+"\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(writer, "tail"); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := receivedEvents(t, server)
	if len(events) != 1 || events[0].Stream != StreamStderr {
		t.Fatalf("events = %+v", events)
	}
	got := events[0].Lines
	want := []string{"one", "two", "", "three" + strings.Repeat("z", MaxLineBytes-5), strings.Repeat("z", 6), "tail"}
	if len(got) != len(want) {
		t.Fatalf("lines = %v", lengths(got))
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("line %d = %.20q (%d bytes), want %.20q (%d bytes)", index, got[index], len(got[index]), want[index], len(want[index]))
		}
	}
}

func TestReporterTruncatesPastTheLogLimit(t *testing.T) {
	server := newFake(t)
	server.handle("POST "+leasePath+"/events", eventsHandler)
	reporter := NewReporter(server.session(t), time.Hour)
	reporter.limit = 2 * (5 + lineOverhead)

	for _, line := range []string{"12345", "67890", "abc", "more"} {
		reporter.Log(StreamStdout, line)
	}
	if err := reporter.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := receivedEvents(t, server)
	if len(events) != 2 || strings.Join(events[0].Lines, ",") != "12345,67890" {
		t.Fatalf("events = %+v, want the lines under the limit, then one truncation line", events)
	}
	if events[1].Stream != StreamStderr || len(events[1].Lines) != 1 || !strings.Contains(events[1].Lines[0], "output truncated") {
		t.Errorf("truncation event = %+v", events[1])
	}
}

func TestReporterTruncatesAFloodOfEmptyLines(t *testing.T) {
	server := newFake(t)
	server.handle("POST "+leasePath+"/events", eventsHandler)
	reporter := NewReporter(server.session(t), time.Hour)
	reporter.limit = 100 * lineOverhead
	writer := reporter.Writer(StreamStdout)

	if _, err := io.WriteString(writer, strings.Repeat("\n", 10_000)); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var empty, truncation int
	for _, event := range receivedEvents(t, server) {
		for _, line := range event.Lines {
			switch {
			case line == "":
				empty++
			case strings.Contains(line, "output truncated"):
				truncation++
			}
		}
	}
	if empty != 100 || truncation != 1 {
		t.Fatalf("delivered %d empty lines and %d truncation lines, want 100 and 1", empty, truncation)
	}
}

func TestReporterHoldsTheBacklogWhileSendsFail(t *testing.T) {
	server := newFake(t)
	var failing atomic.Bool
	failing.Store(true)
	var failures, delivered atomic.Int32
	server.handle("POST "+leasePath+"/events", func(writer http.ResponseWriter, request *http.Request) {
		if failing.Load() {
			failures.Add(1)
			respond(t, writer, http.StatusServiceUnavailable, nil)
			return
		}
		var received batch
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("decode events: %v", err)
		}
		for _, event := range received.Events {
			var data LogData
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Errorf("decode log data: %v", err)
			}
			delivered.Add(int32(len(data.Lines)))
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	session := server.session(t)
	session.client.retry = retry{attempts: 1, backoff: time.Millisecond, ceiling: time.Millisecond}
	reporter := NewReporter(session, 5*time.Millisecond)
	lines := 2 * backlogBytes / MaxLineBytes
	bound := backlogBytes + weight(Event{Data: LogData{Lines: []string{strings.Repeat("x", MaxLineBytes)}}})

	written, done := logLines(reporter, lines)
	peak := 0
	deadline := time.Now().Add(30 * time.Second)
	for failures.Load() < 5 {
		if time.Now().After(deadline) {
			t.Fatalf("the server refused only %d sends, want the flood to span several retry cycles", failures.Load())
		}
		reporter.mutex.Lock()
		peak = max(peak, reporter.backlog())
		reporter.mutex.Unlock()
		time.Sleep(time.Millisecond)
	}
	accepted := int(written.Load())
	if peak > bound {
		t.Fatalf("backlog peaked at %d bytes while every send failed, want at most %d", peak, bound)
	}
	if accepted*MaxLineBytes > bound {
		t.Fatalf("Log accepted %d lines (%d bytes) while nothing was delivered, want at most %d bytes", accepted, accepted*MaxLineBytes, bound)
	}
	failing.Store(false)
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the writer stayed blocked after the server recovered")
	}
	if err := reporter.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := int(delivered.Load()); got != lines {
		t.Errorf("delivered %d lines, want each of the %d exactly once", got, lines)
	}
	if reporter.backlog() != 0 {
		t.Errorf("backlog = %d after Close, want 0", reporter.backlog())
	}
}

// logLines writes count lines of MaxLineBytes each on its own goroutine and reports how many Log accepted.
func logLines(reporter *Reporter, count int) (*atomic.Int32, chan struct{}) {
	var written atomic.Int32
	done := make(chan struct{})
	line := strings.Repeat("x", MaxLineBytes)
	go func() {
		defer close(done)
		for range count {
			reporter.Log(StreamStdout, line)
			written.Add(1)
		}
	}()
	return &written, done
}

func TestReporterBlocksWritersWhileTheBacklogIsFull(t *testing.T) {
	server := newFake(t)
	release := make(chan struct{})
	server.handle("POST "+leasePath+"/events", func(writer http.ResponseWriter, _ *http.Request) {
		<-release
		writer.WriteHeader(http.StatusNoContent)
	})
	reporter := NewReporter(server.session(t), time.Hour)
	lines := 2 * backlogBytes / MaxLineBytes

	written, done := logLines(reporter, lines)
	time.Sleep(300 * time.Millisecond)
	stalled := written.Load()
	if int(stalled) >= lines {
		t.Fatalf("all %d lines were queued while the server held the first batch", lines)
	}
	if queued := int(stalled) * MaxLineBytes; queued > 2*backlogBytes {
		t.Fatalf("%d bytes were queued, want the backlog to stop the writer near %d", queued, backlogBytes)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the writer stayed blocked after the server answered")
	}
	if err := reporter.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	delivered := 0
	for _, event := range receivedEvents(t, server) {
		delivered += len(event.Lines)
	}
	if delivered != lines {
		t.Errorf("delivered %d lines, want %d", delivered, lines)
	}
}

func TestReporterCloseReleasesABlockedWriter(t *testing.T) {
	server := newFake(t)
	server.handle("POST "+leasePath+"/events", func(writer http.ResponseWriter, _ *http.Request) {
		respond(t, writer, http.StatusServiceUnavailable, nil)
	})
	session := server.session(t)
	session.client.retry = retry{attempts: 1, backoff: time.Millisecond, ceiling: time.Millisecond}
	reporter := NewReporter(session, time.Hour)

	_, done := logLines(reporter, 2*backlogBytes/MaxLineBytes)
	time.Sleep(200 * time.Millisecond)
	closing, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = reporter.Close(closing)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close left the writer blocked")
	}
}

func TestReporterCloseLetsASendInFlightFinish(t *testing.T) {
	server := newFake(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	server.handle("POST "+leasePath+"/events", func(writer http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		writer.WriteHeader(http.StatusNoContent)
	})
	reporter := NewReporter(server.session(t), 5*time.Millisecond)
	reporter.Log(StreamStdout, "once")
	<-entered

	closed := make(chan error, 1)
	go func() { closed <- reporter.Close(context.Background()) }()
	time.Sleep(100 * time.Millisecond)
	close(release)
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}

	if requests := server.matching(http.MethodPost, leasePath+"/events"); len(requests) != 1 {
		t.Fatalf("requests = %d, want the batch in flight sent once", len(requests))
	}
}
