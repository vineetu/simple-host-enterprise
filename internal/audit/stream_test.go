package audit

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingWriter blocks every Write until release is closed.
type blockingWriter struct {
	release chan struct{}
	mu      sync.Mutex
	buf     bytes.Buffer
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// A stalled stdout must never block the caller (who may hold the audit
// chain's head lock): lines past the buffer are dropped and counted.
func TestStreamNeverBlocksAndCountsDrops(t *testing.T) {
	w := &blockingWriter{release: make(chan struct{})}
	s := NewStream(slog.New(slog.NewJSONHandler(w, nil)), 2)
	var logged bytes.Buffer
	restore := captureLog(&logged)
	defer restore()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			s.emit([]slog.Attr{slog.String("type", "audit"), slog.Int("n", i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("emit blocked on a stalled writer")
	}
	// One line is held by the blocked writer, two are buffered.
	if got := s.Dropped(); got < 7 || got > 8 {
		t.Fatalf("dropped = %d, want 7 or 8", got)
	}
	if !strings.Contains(logged.String(), "audit line(s) dropped") {
		t.Fatalf("no drop log line: %q", logged.String())
	}
	close(w.release)
	s.Close(time.Second)
	w.mu.Lock()
	lines := strings.Count(w.buf.String(), `"type":"audit"`)
	w.mu.Unlock()
	if uint64(lines)+s.Dropped() != 10 {
		t.Fatalf("written %d + dropped %d != 10", lines, s.Dropped())
	}
}

// Drops are logged at most once a minute.
func TestStreamDropLogIsRateLimited(t *testing.T) {
	w := &blockingWriter{release: make(chan struct{})}
	s := NewStream(slog.New(slog.NewJSONHandler(w, nil)), 1)
	clock := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return clock }
	var logged bytes.Buffer
	restore := captureLog(&logged)
	defer restore()
	for i := 0; i < 20; i++ {
		s.emit(nil)
	}
	first := strings.Count(logged.String(), "dropped so far")
	clock = clock.Add(61 * time.Second)
	s.emit(nil)
	if first != 1 || strings.Count(logged.String(), "dropped so far") != 2 {
		t.Fatalf("drop log lines: %q", logged.String())
	}
	close(w.release)
	s.Close(time.Second)
}

// Close writes everything still queued.
func TestStreamCloseFlushes(t *testing.T) {
	var buf bytes.Buffer
	s := NewStream(slog.New(slog.NewJSONHandler(&buf, nil)), 0)
	for i := 0; i < 100; i++ {
		s.emit([]slog.Attr{slog.String("type", "audit")})
	}
	s.Close(time.Second)
	if n := strings.Count(buf.String(), "\n"); n != 100 || s.Dropped() != 0 {
		t.Fatalf("flushed %d lines, dropped %d", n, s.Dropped())
	}
}
