package audit

import (
	"context"
	"log"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Stream writes the SIEM lines (one "type":"audit" JSON line per event) off
// the caller's goroutine. DBRecorder emits while the caller's transaction
// still holds the audit chain's head lock, so a stdout that blocks (a full
// pipe, a stalled log agent) must never stall the writer: lines go through
// a buffered channel drained by one goroutine, and a line that finds the
// buffer full is dropped and counted rather than waited for. The database
// row, not the line, is the record; Dropped is on /metrics
// (simplehost_audit_stream_dropped_total), and drops are logged at most
// once a minute.
type Stream struct {
	logger  *slog.Logger
	lines   chan []slog.Attr
	dropped atomic.Uint64
	quit    chan struct{}
	done    chan struct{}
	once    sync.Once

	lastLogged atomic.Int64 // unix seconds of the last drop log line
	now        func() time.Time
}

// defaultStreamBuffer holds a burst of events while stdout catches up.
const defaultStreamBuffer = 4096

// NewStream starts the goroutine that writes to logger. size <= 0 means
// defaultStreamBuffer. Close flushes it.
func NewStream(logger *slog.Logger, size int) *Stream {
	if size <= 0 {
		size = defaultStreamBuffer
	}
	s := &Stream{logger: logger, lines: make(chan []slog.Attr, size), quit: make(chan struct{}), done: make(chan struct{}), now: time.Now}
	go s.run()
	return s
}

func (s *Stream) run() {
	defer close(s.done)
	for {
		select {
		case attrs := <-s.lines:
			s.write(attrs)
		case <-s.quit:
			for {
				select {
				case attrs := <-s.lines:
					s.write(attrs)
				default:
					return
				}
			}
		}
	}
}

func (s *Stream) write(attrs []slog.Attr) {
	s.logger.LogAttrs(context.Background(), slog.LevelInfo, "audit", attrs...)
}

// emit queues one line without blocking.
func (s *Stream) emit(attrs []slog.Attr) {
	select {
	case s.lines <- attrs:
	default:
		total := s.dropped.Add(1)
		now := s.now().Unix()
		last := s.lastLogged.Load()
		if now-last >= 60 && s.lastLogged.CompareAndSwap(last, now) {
			log.Printf("audit: stdout stream is not keeping up; %d audit line(s) dropped so far (the database rows are intact)", total)
		}
	}
}

// Dropped is how many lines were dropped because the buffer was full.
func (s *Stream) Dropped() uint64 { return s.dropped.Load() }

// Close writes what is queued (waiting up to timeout) and stops the
// goroutine. A line emitted after Close is queued and never written, which
// only a writer still running at shutdown can hit.
func (s *Stream) Close(timeout time.Duration) {
	s.once.Do(func() { close(s.quit) })
	select {
	case <-s.done:
	case <-time.After(timeout):
		log.Printf("audit: stdout stream did not drain within %s", timeout)
	}
}
