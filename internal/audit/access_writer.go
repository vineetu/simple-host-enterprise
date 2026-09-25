package audit

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"sync/atomic"
	"time"

	dbstore "github.com/vsriram/simple-host/internal/db"
)

// AccessEvent is one visit for access_log. ClientKind is a
// plain string, not internal/handler's clientKind type: that type is
// unexported, and this package must not depend on internal/handler (the
// dependency runs the other way — a handler calls into audit, never the
// reverse), so the caller converts its own classification to a string
// before enqueuing.
type AccessEvent struct {
	At         time.Time
	UserID     string
	SessionID  string
	OwnerLabel string
	SiteName   string
	Path       string
	Method     string
	Status     int
	Bytes      int64
	IP         string
	UserAgent  string
	ClientKind string
}

const (
	// AccessWriterQueueSize bounds how many unflushed visits can queue up
	// before Enqueue starts dropping. Generous relative to
	// fileDownloadRecorder's 128 (download_recorder.go): access_log gets
	// every visit, not just file downloads.
	AccessWriterQueueSize = 4096
	// AccessWriterBatchSize is the largest single INSERT this writer sends;
	// hit under load, missed (and flushed on the interval below) when
	// quiet.
	AccessWriterBatchSize = 200
	// AccessWriterFlushInterval bounds how stale the log can get on a quiet
	// site: even one visit is written within this long of arriving.
	AccessWriterFlushInterval = 2 * time.Second
	accessWriterWriteTimeout  = 5 * time.Second
)

// AccessWriter batches access_log inserts off the request path: written
// from serveSite's status recorder through a batching writer that is best
// effort, never blocks serving, and drops with a counter under pressure.
// Enqueue never blocks; a full queue increments Dropped and returns false
// rather than waiting for room.
//
// Call Close only after every caller that might still call Enqueue has
// stopped — in practice, only after http.Server.Shutdown (or equivalent)
// has returned, since a handler still draining an in-flight request can
// call Enqueue right up until then. Close itself is safe to call
// concurrently with Enqueue (a mutex, not the channel close, is what makes
// that safe — see Close and Enqueue below); the ordering requirement is
// about not losing visits still being recorded during shutdown, not about
// a race panic.
type AccessWriter struct {
	events  chan AccessEvent
	dropped atomic.Int64
	wg      sync.WaitGroup
	// mu guards closed, and is held (as a reader) around the channel send
	// in Enqueue so that Close cannot close w.events between Enqueue's
	// closed check and its send — the send-on-a-closed-channel panic this
	// exists to prevent.
	mu     sync.RWMutex
	closed bool
}

// NewAccessWriter starts one background worker writing through database.
// database must not be nil.
func NewAccessWriter(database *sql.DB) *AccessWriter {
	if database == nil {
		panic("audit: NewAccessWriter requires a non-nil database")
	}
	return newAccessWriterWith(AccessWriterQueueSize, AccessWriterBatchSize, AccessWriterFlushInterval,
		func(ctx context.Context, batch []AccessEvent) error {
			return dbstore.InsertAccessLogBatch(ctx, database, toDBAccessLogEvents(batch))
		})
}

// newAccessWriterWith is NewAccessWriter's constructor with an injectable
// write function and tunables, for tests — the same shape
// newFileDownloadRecorderWith uses (internal/handler/download_recorder.go).
func newAccessWriterWith(queueSize, batchSize int, flushInterval time.Duration, write func(context.Context, []AccessEvent) error) *AccessWriter {
	if queueSize <= 0 || batchSize <= 0 || flushInterval <= 0 || write == nil {
		panic("audit: access writer requires positive queue/batch size, interval, and a write func")
	}
	w := &AccessWriter{events: make(chan AccessEvent, queueSize)}
	w.wg.Add(1)
	go w.run(batchSize, flushInterval, write)
	return w
}

// Enqueue admits event if there is room, or increments Dropped and returns
// false immediately if not — including after Close, which Enqueue treats
// the same as a full queue rather than panicking. Never blocks.
func (w *AccessWriter) Enqueue(event AccessEvent) bool {
	if w == nil {
		return false
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		w.dropped.Add(1)
		return false
	}
	select {
	case w.events <- event:
		return true
	default:
		w.dropped.Add(1)
		return false
	}
}

// Dropped is the running count of events Enqueue could not admit because
// the queue was full — dropping with a counter under pressure, rather
// than blocking.
func (w *AccessWriter) Dropped() int64 {
	if w == nil {
		return 0
	}
	return w.dropped.Load()
}

// Close stops accepting new work (a concurrent or subsequent Enqueue is
// dropped and counted, never a panic — see Enqueue) and flushes whatever
// is already queued before returning. Idempotent: a second Close is a
// no-op. Safe to call concurrently with Enqueue; see the type doc comment
// for when to call it relative to server shutdown.
func (w *AccessWriter) Close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.events)
	w.mu.Unlock()
	w.wg.Wait()
}

func (w *AccessWriter) run(batchSize int, flushInterval time.Duration, write func(context.Context, []AccessEvent) error) {
	defer w.wg.Done()
	batch := make([]AccessEvent, 0, batchSize)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), accessWriterWriteTimeout)
		if err := write(ctx, batch); err != nil {
			log.Printf("audit: access log batch of %d event(s) failed: %v", len(batch), err)
		}
		cancel()
		batch = batch[:0]
	}

	for {
		select {
		case event, ok := <-w.events:
			if !ok {
				flush()
				return
			}
			batch = append(batch, event)
			if len(batch) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func toDBAccessLogEvents(events []AccessEvent) []dbstore.AccessLogEvent {
	out := make([]dbstore.AccessLogEvent, len(events))
	for i, e := range events {
		out[i] = dbstore.AccessLogEvent{
			At:         e.At,
			UserID:     e.UserID,
			SessionID:  e.SessionID,
			OwnerLabel: e.OwnerLabel,
			SiteName:   e.SiteName,
			Path:       e.Path,
			Method:     e.Method,
			Status:     e.Status,
			Bytes:      e.Bytes,
			IP:         e.IP,
			UserAgent:  e.UserAgent,
			ClientKind: e.ClientKind,
		}
	}
	return out
}
