package handler

import (
	"sync"
	"time"
)

const (
	defaultStateUsageMaxCompleted = 10_000
	defaultStateUsageCompletedTTL = time.Hour
)

// stateUsageMarker runs best-effort, idempotent analytics markers without
// letting unauthenticated state reads create an unbounded number of goroutines
// or database calls. A saturated attempt is dropped and may be retried by a
// later read. Successful keys are retained in a bounded TTL cache so a hot
// site's reads do not repeatedly write while deleted sites eventually age out.
type stateUsageMarker struct {
	mu           sync.Mutex
	completed    map[string]time.Time
	running      map[string]struct{}
	slots        chan struct{}
	maxCompleted int
	completedTTL time.Duration
	now          func() time.Time
}

func newStateUsageMarker(concurrency int) *stateUsageMarker {
	return newStateUsageMarkerWith(
		concurrency,
		defaultStateUsageMaxCompleted,
		defaultStateUsageCompletedTTL,
		time.Now,
	)
}

func newStateUsageMarkerWith(concurrency, maxCompleted int, completedTTL time.Duration, now func() time.Time) *stateUsageMarker {
	if concurrency <= 0 {
		panic("handler: state usage marker concurrency must be positive")
	}
	if maxCompleted <= 0 {
		panic("handler: state usage marker completed capacity must be positive")
	}
	if completedTTL <= 0 {
		panic("handler: state usage marker completed TTL must be positive")
	}
	if now == nil {
		panic("handler: state usage marker clock must not be nil")
	}
	return &stateUsageMarker{
		completed:    make(map[string]time.Time),
		running:      make(map[string]struct{}),
		slots:        make(chan struct{}, concurrency),
		maxCompleted: maxCompleted,
		completedTTL: completedTTL,
		now:          now,
	}
}

// Schedule returns true only when work was admitted. Duplicate, completed, or
// saturated attempts return immediately without starting a goroutine.
func (m *stateUsageMarker) Schedule(key string, work func() error) bool {
	m.mu.Lock()
	m.removeExpiredLocked(m.now())
	if _, ok := m.completed[key]; ok {
		m.mu.Unlock()
		return false
	}
	if _, ok := m.running[key]; ok {
		m.mu.Unlock()
		return false
	}
	select {
	case m.slots <- struct{}{}:
		m.running[key] = struct{}{}
		m.mu.Unlock()
	default:
		m.mu.Unlock()
		return false
	}

	go func() {
		err := work()
		m.mu.Lock()
		delete(m.running, key)
		if err == nil {
			completedAt := m.now()
			m.removeExpiredLocked(completedAt)
			if _, exists := m.completed[key]; !exists && len(m.completed) >= m.maxCompleted {
				m.evictOldestLocked()
			}
			m.completed[key] = completedAt
		}
		m.mu.Unlock()
		<-m.slots
	}()
	return true
}

func (m *stateUsageMarker) removeExpiredLocked(now time.Time) {
	for key, completedAt := range m.completed {
		if !now.Before(completedAt.Add(m.completedTTL)) {
			delete(m.completed, key)
		}
	}
}

func (m *stateUsageMarker) evictOldestLocked() {
	var oldestKey string
	var oldestAt time.Time
	for key, completedAt := range m.completed {
		if oldestKey == "" || completedAt.Before(oldestAt) || (completedAt.Equal(oldestAt) && key < oldestKey) {
			oldestKey = key
			oldestAt = completedAt
		}
	}
	if oldestKey != "" {
		delete(m.completed, oldestKey)
	}
}
