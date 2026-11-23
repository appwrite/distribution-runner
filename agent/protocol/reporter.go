package protocol

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"
)

// eventOverhead bounds everything in a log event except its lines: id, type, time, stream and punctuation.
const eventOverhead = 1024

// Reporter queues events and log lines and sends them in order: every interval, as soon as a full request is
// waiting, and on Flush or Close. A batch that fails transiently stays queued and is resent; one the server
// rejects outright is dropped and its error returned by Close.
type Reporter struct {
	session *Session
	wake    chan struct{}
	stop    context.CancelFunc
	done    chan struct{}

	sending sync.Mutex

	mutex       sync.Mutex
	pending     []Event
	pendingSize int
	tailSize    int
	failure     error
	writers     []*lineWriter
}

func NewReporter(session *Session, interval time.Duration) *Reporter {
	ctx, stop := context.WithCancel(context.Background())
	reporter := &Reporter{
		session: session,
		wake:    make(chan struct{}, 1),
		stop:    stop,
		done:    make(chan struct{}),
	}
	go reporter.run(ctx, interval)
	return reporter
}

// Log queues one line of output, folded into pieces of at most MaxLineBytes.
func (r *Reporter) Log(stream string, line string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
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
// that cost events.
func (r *Reporter) Close(ctx context.Context) error {
	r.mutex.Lock()
	writers := r.writers
	r.mutex.Unlock()
	for _, writer := range writers {
		writer.close()
	}
	r.stop()
	<-r.done
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
		case <-ctx.Done():
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
	return events
}

func (r *Reporter) restore(events []Event) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.pending = append(events, r.pending...)
	r.pendingSize += len(events) * eventOverhead
	r.tailSize = 0
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
		if err != nil && !rejected(err) {
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

// rejected reports whether the server refused the batch itself, so resending it cannot succeed.
func rejected(err error) bool {
	var response *ResponseError
	if !errors.As(err, &response) {
		return false
	}
	return response.Status >= 400 && response.Status < 500 &&
		response.Status != http.StatusTooManyRequests && response.Status != http.StatusRequestTimeout
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
