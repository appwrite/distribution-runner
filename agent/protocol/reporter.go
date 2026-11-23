package protocol

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// eventOverhead bounds everything in a log event except its lines: id, type, time, stream and punctuation.
	eventOverhead = 1024
	// backlogBytes is how much queued output makes Log wait for a send, pushing back on whoever writes it.
	backlogBytes = 4 * MaxBatchBytes
	// LogLimit is the server's default _APP_DISTRIBUTION_LOGS_LIMIT: the bytes of output one lease may report.
	LogLimit = 50_000_000
)

// Reporter queues events and log lines and sends them in order: every interval, as soon as a full request is
// waiting, and on Flush or Close. A batch that fails transiently stays queued and is resent; one the server
// rejects outright is dropped and its error returned by Close. While more than backlogBytes wait to be sent, Log
// blocks; past LogLimit bytes of output it reports one truncation line and drops the rest.
type Reporter struct {
	session *Session
	wake    chan struct{}
	stop    chan struct{}
	abort   context.CancelFunc
	done    chan struct{}
	limit   int64

	sending sync.Mutex

	mutex       sync.Mutex
	drained     *sync.Cond
	closed      bool
	logged      int64
	truncated   bool
	pending     []Event
	pendingSize int
	tailSize    int
	failure     error
	writers     []*lineWriter
}

func NewReporter(session *Session, interval time.Duration) *Reporter {
	ctx, abort := context.WithCancel(context.Background())
	reporter := &Reporter{
		session: session,
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		abort:   abort,
		done:    make(chan struct{}),
		limit:   LogLimit,
	}
	reporter.drained = sync.NewCond(&reporter.mutex)
	go reporter.run(ctx, interval)
	return reporter
}

// Log queues one line of output, folded into pieces of at most MaxLineBytes. It waits while the backlog is full,
// until a send drains it or the reporter closes.
func (r *Reporter) Log(stream string, line string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	for r.pendingSize >= backlogBytes && !r.closed {
		r.notify()
		r.drained.Wait()
	}
	if r.truncated {
		return
	}
	if r.logged+int64(len(line)) > r.limit {
		r.truncated = true
		r.append(StreamStderr, fmt.Sprintf("[distribution] output truncated: the build wrote more than %d bytes", r.limit))
		r.notify()
		return
	}
	r.logged += int64(len(line))
	for _, piece := range fold(line) {
		r.append(stream, piece)
	}
	r.notify()
}

func (r *Reporter) Emit(event Event) {
	r.session.stamp(&event)
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.pending = append(r.pending, event)
	r.pendingSize += eventOverhead
	r.tailSize = 0
	r.notify()
}

// Writer returns an io.Writer that reports each line written to it on stream; a trailing partial line is sent on Close.
func (r *Reporter) Writer(stream string) io.Writer {
	writer := &lineWriter{reporter: r, stream: stream}
	r.mutex.Lock()
	r.writers = append(r.writers, writer)
	r.mutex.Unlock()
	return writer
}

// Flush sends everything queued so far and returns the error that left events unsent, if any.
func (r *Reporter) Flush(ctx context.Context) error {
	return r.flush(ctx)
}

// Close sends the remaining partial lines and events, stops the background sender and returns every error
// that cost events. A send in flight is allowed to finish, so its batch is not sent twice, unless ctx ends first.
func (r *Reporter) Close(ctx context.Context) error {
	r.mutex.Lock()
	r.closed = true
	r.drained.Broadcast()
	writers := r.writers
	r.mutex.Unlock()
	for _, writer := range writers {
		writer.close()
	}
	close(r.stop)
	select {
	case <-r.done:
	case <-ctx.Done():
		r.abort()
		<-r.done
	}
	defer r.abort()
	err := r.flush(ctx)
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return errors.Join(err, r.failure)
}

func (r *Reporter) run(ctx context.Context, interval time.Duration) {
	defer close(r.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
		case <-r.wake:
		}
		_ = r.flush(ctx)
	}
}

func (r *Reporter) append(stream string, line string) {
	encoded, _ := encode(line)
	size := escapedLength(encoded) + 1
	last := len(r.pending) - 1
	if last >= 0 && r.tailSize > 0 && r.tailSize+size <= MaxEventBytes {
		if data, ok := r.pending[last].Data.(LogData); ok && data.Stream == stream {
			data.Lines = append(data.Lines, line)
			r.pending[last].Data = data
			r.tailSize += size
			r.pendingSize += size
			return
		}
	}
	event := Event{Type: TypeLog, Data: LogData{Lines: []string{line}, Stream: stream}}
	r.session.stamp(&event)
	r.pending = append(r.pending, event)
	r.tailSize = eventOverhead + size
	r.pendingSize += eventOverhead + size
}

func (r *Reporter) notify() {
	if len(r.pending) < MaxEvents && r.pendingSize < MaxBatchBytes {
		return
	}
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Reporter) flush(ctx context.Context) error {
	r.sending.Lock()
	defer r.sending.Unlock()
	events := r.take()
	if len(events) == 0 {
		return nil
	}
	unsent, err := r.send(ctx, events)
	if len(unsent) > 0 {
		r.restore(unsent)
	}
	return err
}

func (r *Reporter) take() []Event {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	events := r.pending
	r.pending = nil
	r.pendingSize = 0
	r.tailSize = 0
	r.drained.Broadcast()
	return events
}

func (r *Reporter) restore(events []Event) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.pending = append(events, r.pending...)
	for _, event := range events {
		r.pendingSize += weight(event)
	}
	r.tailSize = 0
}

// weight is the share of the backlog an event takes: its overhead plus its escaped lines.
func weight(event Event) int {
	size := eventOverhead
	if data, ok := event.Data.(LogData); ok {
		for _, line := range data.Lines {
			encoded, _ := encode(line)
			size += escapedLength(encoded) + 1
		}
	}
	return size
}

// send reports events batch by batch and returns the events still to be sent after a transient failure.
func (r *Reporter) send(ctx context.Context, events []Event) ([]Event, error) {
	encoded := make([][]byte, 0, len(events))
	kept := make([]Event, 0, len(events))
	for _, event := range events {
		data, err := r.session.encodeEvent(event)
		if err != nil {
			r.fail(err)
			continue
		}
		encoded = append(encoded, data)
		kept = append(kept, event)
	}
	sent := 0
	for _, batch := range batches(encoded) {
		err := r.session.sendBatch(ctx, batch)
		if err != nil && !permanent(err) {
			return kept[sent:], err
		}
		if err != nil {
			r.fail(err)
		}
		sent += len(batch)
	}
	return nil, nil
}

func (r *Reporter) fail(err error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.failure = errors.Join(r.failure, err)
}

func permanent(err error) bool {
	var response *ResponseError
	return errors.As(err, &response) && response.Permanent()
}

// fold cuts a line into pieces of at most MaxLineBytes, moving each cut back to the start of a UTF-8 sequence
// so a rune is never split; input that is not UTF-8 is cut at exactly MaxLineBytes.
func fold(line string) []string {
	if len(line) <= MaxLineBytes {
		return []string{line}
	}
	pieces := make([]string, 0, len(line)/MaxLineBytes+1)
	for len(line) > MaxLineBytes {
		cut := MaxLineBytes
		for back := 0; back < utf8.UTFMax-1 && !utf8.RuneStart(line[cut]); back++ {
			cut--
		}
		if !utf8.RuneStart(line[cut]) {
			cut = MaxLineBytes
		}
		pieces = append(pieces, line[:cut])
		line = line[cut:]
	}
	return append(pieces, line)
}

type lineWriter struct {
	reporter *Reporter
	stream   string

	mutex  sync.Mutex
	buffer []byte
}

func (w *lineWriter) Write(data []byte) (int, error) {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	w.buffer = append(w.buffer, data...)
	for {
		index := bytes.IndexByte(w.buffer, '\n')
		if index < 0 {
			break
		}
		line := w.buffer[:index]
		if length := len(line); length > 0 && line[length-1] == '\r' {
			line = line[:length-1]
		}
		w.reporter.Log(w.stream, string(line))
		w.buffer = w.buffer[index+1:]
	}
	if len(w.buffer) > MaxLineBytes {
		pieces := fold(string(w.buffer))
		for _, piece := range pieces[:len(pieces)-1] {
			w.reporter.Log(w.stream, piece)
		}
		w.buffer = []byte(pieces[len(pieces)-1])
	}
	return len(data), nil
}

func (w *lineWriter) close() {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	if len(w.buffer) > 0 {
		w.reporter.Log(w.stream, string(w.buffer))
		w.buffer = nil
	}
}
