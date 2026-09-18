package audit

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestAccessWriterEnqueueIsBoundedAndNonblocking(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	flushed := make(chan int, 4)
	// batchSize 1 so every single admitted event triggers an immediate
	// flush, the same one-event-per-write-call shape
	// TestFileDownloadRecorderIsBoundedAndNonblocking
	// (internal/handler/download_recorder_test.go) uses to make the worker
	// block deterministically inside the write callback.
	writer := newAccessWriterWith(1, 1, time.Hour, func(_ context.Context, batch []AccessEvent) error {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		flushed <- len(batch)
		return nil
	})
	defer func() {
		close(release)
		writer.Close()
	}()

	// The queue holds 1; the first Enqueue is drained into the worker's own
	// in-flight batch almost immediately, so it and one more should both be
	// admitted before the queue is actually full.
	if !writer.Enqueue(AccessEvent{Path: "/one"}) {
		t.Fatal("first event was not admitted")
	}
	<-started // worker is now blocked in the write func, holding nothing else
	if !writer.Enqueue(AccessEvent{Path: "/two"}) {
		t.Fatal("event queued behind the blocked worker was not admitted")
	}

	start := time.Now()
	admitted := writer.Enqueue(AccessEvent{Path: "/three"})
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("Enqueue on a saturated writer blocked for %v", elapsed)
	}
	if admitted {
		t.Fatal("event exceeded queue bound but was admitted anyway")
	}
	if got := writer.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}
}

func TestAccessWriterFlushesAtBatchSize(t *testing.T) {
	flushed := make(chan []AccessEvent, 4)
	writer := newAccessWriterWith(16, 3, time.Hour, func(_ context.Context, batch []AccessEvent) error {
		cp := append([]AccessEvent(nil), batch...)
		flushed <- cp
		return nil
	})
	defer writer.Close()

	for _, p := range []string{"/a", "/b", "/c"} {
		if !writer.Enqueue(AccessEvent{Path: p}) {
			t.Fatalf("Enqueue(%q) was not admitted", p)
		}
	}

	select {
	case batch := <-flushed:
		if len(batch) != 3 {
			t.Fatalf("flushed batch has %d event(s), want 3", len(batch))
		}
	case <-time.After(time.Second):
		t.Fatal("batch size was reached but no flush happened")
	}
}

func TestAccessWriterFlushesOnInterval(t *testing.T) {
	flushed := make(chan []AccessEvent, 4)
	writer := newAccessWriterWith(16, 100, 20*time.Millisecond, func(_ context.Context, batch []AccessEvent) error {
		cp := append([]AccessEvent(nil), batch...)
		flushed <- cp
		return nil
	})
	defer writer.Close()

	if !writer.Enqueue(AccessEvent{Path: "/lonely"}) {
		t.Fatal("event was not admitted")
	}

	select {
	case batch := <-flushed:
		if len(batch) != 1 || batch[0].Path != "/lonely" {
			t.Fatalf("flushed batch = %#v, want one event for /lonely", batch)
		}
	case <-time.After(time.Second):
		t.Fatal("flush interval elapsed with a queued event but no flush happened")
	}
}

func TestAccessWriterCloseFlushesRemainder(t *testing.T) {
	flushed := make(chan []AccessEvent, 4)
	writer := newAccessWriterWith(16, 100, time.Hour, func(_ context.Context, batch []AccessEvent) error {
		cp := append([]AccessEvent(nil), batch...)
		flushed <- cp
		return nil
	})

	writer.Enqueue(AccessEvent{Path: "/never-hits-batch-size"})
	writer.Close()

	select {
	case batch := <-flushed:
		if len(batch) != 1 {
			t.Fatalf("flushed batch has %d event(s), want 1", len(batch))
		}
	default:
		t.Fatal("Close returned without flushing the queued event")
	}
}

func TestNewAccessWriterPanicsOnNilDatabase(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewAccessWriter(nil) did not panic")
		}
	}()
	NewAccessWriter(nil)
}

func TestNilAccessWriterEnqueueAndDroppedAreSafe(t *testing.T) {
	var w *AccessWriter
	if w.Enqueue(AccessEvent{}) {
		t.Fatal("nil *AccessWriter.Enqueue returned true")
	}
	if got := w.Dropped(); got != 0 {
		t.Fatalf("nil *AccessWriter.Dropped() = %d, want 0", got)
	}
	w.Close() // must not panic
}

func TestAccessWriterEnqueueAfterCloseDropsInsteadOfPanicking(t *testing.T) {
	writer := newAccessWriterWith(16, 100, time.Hour, func(context.Context, []AccessEvent) error { return nil })
	writer.Close()

	if writer.Enqueue(AccessEvent{Path: "/too-late"}) {
		t.Fatal("Enqueue after Close was admitted")
	}
	if got := writer.Dropped(); got != 1 {
		t.Fatalf("Dropped() after a post-Close Enqueue = %d, want 1", got)
	}
}

func TestAccessWriterCloseIsIdempotent(t *testing.T) {
	writer := newAccessWriterWith(16, 100, time.Hour, func(context.Context, []AccessEvent) error { return nil })
	writer.Close()
	writer.Close() // must not panic (double close(w.events))
}

// TestAccessWriterConcurrentEnqueueAndCloseNeverPanics is the regression
// test for the review finding: Enqueue and Close racing used to be able to
// panic on a send to a closed channel. Run with -race to also confirm the
// mutex actually serializes the check against Close, not just that nothing
// panics by luck of scheduling.
func TestAccessWriterConcurrentEnqueueAndCloseNeverPanics(t *testing.T) {
	writer := newAccessWriterWith(16, 100, time.Hour, func(context.Context, []AccessEvent) error { return nil })

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					writer.Enqueue(AccessEvent{Path: "/racing"})
				}
			}
		}()
	}

	time.Sleep(5 * time.Millisecond)
	writer.Close()
	close(stop)
	wg.Wait()
}
