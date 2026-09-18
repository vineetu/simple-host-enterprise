package handler

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestStateUsageMarkerCoalescesAndDeduplicates(t *testing.T) {
	marker := newStateUsageMarker(1)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	if !marker.Schedule("alice/site/simple", func() error {
		close(started)
		<-release
		close(done)
		return nil
	}) {
		t.Fatal("first marker was not admitted")
	}
	<-started
	if marker.Schedule("alice/site/simple", func() error { return nil }) {
		t.Fatal("concurrent duplicate marker was admitted")
	}
	close(release)
	<-done
	waitForMarker(t, func() bool {
		return !marker.Schedule("alice/site/simple", func() error { return nil })
	})
}

func TestStateUsageMarkerCapsConcurrencyAndRetriesFailures(t *testing.T) {
	marker := newStateUsageMarker(2)
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	work := func() error {
		started <- struct{}{}
		<-release
		return nil
	}
	if !marker.Schedule("one", work) || !marker.Schedule("two", work) {
		t.Fatal("initial marker work was not admitted")
	}
	<-started
	<-started
	var saturatedRan atomic.Bool
	if marker.Schedule("three", func() error {
		saturatedRan.Store(true)
		return nil
	}) {
		t.Fatal("marker exceeded its concurrency cap")
	}
	if saturatedRan.Load() {
		t.Fatal("saturated marker work ran")
	}
	close(release)

	failed := make(chan struct{})
	waitForMarker(t, func() bool {
		return marker.Schedule("retry", func() error {
			close(failed)
			return errors.New("temporary failure")
		})
	})
	<-failed

	retried := make(chan struct{})
	waitForMarker(t, func() bool {
		return marker.Schedule("retry", func() error {
			close(retried)
			return nil
		})
	})
	<-retried
}

func TestStateUsageMarkerExpiresCompletedEntries(t *testing.T) {
	clock := newTestMarkerClock(time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC))
	marker := newStateUsageMarkerWith(1, 10, time.Hour, clock.Now)
	key := "site-id/simple"

	runMarkerWork(t, marker, key, nil)
	if marker.Schedule(key, func() error { return nil }) {
		t.Fatal("completed marker was admitted before its TTL")
	}

	clock.Advance(time.Hour - time.Nanosecond)
	if marker.Schedule(key, func() error { return nil }) {
		t.Fatal("completed marker was admitted just before its TTL")
	}

	clock.Advance(time.Nanosecond)
	runMarkerWork(t, marker, key, nil)
}

func TestStateUsageMarkerEvictsDeterministicOldestAtCapacity(t *testing.T) {
	clock := newTestMarkerClock(time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC))
	marker := newStateUsageMarkerWith(1, 2, time.Hour, clock.Now)

	// Equal completion timestamps exercise the deterministic key tie-breaker.
	runMarkerWork(t, marker, "b", nil)
	runMarkerWork(t, marker, "a", nil)
	runMarkerWork(t, marker, "c", nil)

	marker.mu.Lock()
	_, hasA := marker.completed["a"]
	_, hasB := marker.completed["b"]
	_, hasC := marker.completed["c"]
	completedLen := len(marker.completed)
	marker.mu.Unlock()
	if hasA || !hasB || !hasC || completedLen != 2 {
		t.Fatalf("completed cache after eviction: a=%t b=%t c=%t len=%d", hasA, hasB, hasC, completedLen)
	}

	if marker.Schedule("b", func() error { return nil }) {
		t.Fatal("non-evicted completed marker was admitted")
	}
	runMarkerWork(t, marker, "a", nil)
}

func TestStateUsageMarkerUsesImmutableSiteIdentity(t *testing.T) {
	clock := newTestMarkerClock(time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC))
	marker := newStateUsageMarkerWith(1, 10, time.Hour, clock.Now)

	oldKey := stateUsageMarkerKey("old-site-id", false)
	recreatedKey := stateUsageMarkerKey("recreated-site-id", false)
	runMarkerWork(t, marker, oldKey, nil)
	if marker.Schedule(oldKey, func() error { return nil }) {
		t.Fatal("old immutable site ID was not deduplicated")
	}
	runMarkerWork(t, marker, recreatedKey, nil)

	if stateUsageMarkerKey("old-site-id", true) == oldKey {
		t.Fatal("simple and versioned variants share a marker key")
	}
}

func TestStateUsageMarkerDefaultsAreBounded(t *testing.T) {
	marker := newStateUsageMarker(4)
	if cap(marker.slots) != 4 {
		t.Fatalf("marker concurrency = %d, want 4", cap(marker.slots))
	}
	if marker.maxCompleted != 10_000 {
		t.Fatalf("completed capacity = %d, want 10000", marker.maxCompleted)
	}
	if marker.completedTTL != time.Hour {
		t.Fatalf("completed TTL = %v, want %v", marker.completedTTL, time.Hour)
	}
}

func runMarkerWork(t *testing.T, marker *stateUsageMarker, key string, workErr error) {
	t.Helper()
	done := make(chan struct{})
	if !marker.Schedule(key, func() error {
		close(done)
		return workErr
	}) {
		t.Fatalf("marker %q was not admitted", key)
	}
	<-done
	waitForMarker(t, func() bool {
		marker.mu.Lock()
		defer marker.mu.Unlock()
		_, running := marker.running[key]
		_, completed := marker.completed[key]
		return !running && completed == (workErr == nil)
	})
}

type testMarkerClock struct {
	nanos atomic.Int64
}

func newTestMarkerClock(now time.Time) *testMarkerClock {
	clock := &testMarkerClock{}
	clock.nanos.Store(now.UnixNano())
	return clock
}

func (c *testMarkerClock) Now() time.Time {
	return time.Unix(0, c.nanos.Load()).UTC()
}

func (c *testMarkerClock) Advance(delta time.Duration) {
	c.nanos.Add(int64(delta))
}

func waitForMarker(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("marker condition was not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
