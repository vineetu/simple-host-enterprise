package storage

import (
	"context"
	"sync"
)

// envelopeFillBudget is how many bytes of enveloped object bodies one process
// holds in memory at once while filling the cache. An enveloped body is
// buffered whole (see S3Objects.GetTo) and decrypted in place, so one fill
// costs about its object's size. One GiB lets a single largest-possible
// version (maxVersionObjectBytes) through at a time, or many small ones
// together, and leaves the other half of the 2 GiB pod memory limit
// (deploy/base/deployment.yaml) to everything else. Plain objects stream to
// disk and are not counted.
const envelopeFillBudget = maxVersionObjectBytes

// memoryBudget is a weighted semaphore over bytes. A request larger than the
// whole budget is treated as needing all of it, so it still runs, alone.
type memoryBudget struct {
	mu      sync.Mutex
	total   int64
	free    int64
	changed chan struct{} // closed and replaced on every release
}

func newMemoryBudget(total int64) *memoryBudget {
	return &memoryBudget{total: total, free: total, changed: make(chan struct{})}
}

func (b *memoryBudget) clamp(n int64) int64 {
	if n > b.total {
		return b.total
	}
	if n < 0 {
		return 0
	}
	return n
}

func (b *memoryBudget) tryAcquire(n int64) bool {
	n = b.clamp(n)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.free < n {
		return false
	}
	b.free -= n
	return true
}

// acquire waits until n bytes are free or ctx ends.
func (b *memoryBudget) acquire(ctx context.Context, n int64) error {
	n = b.clamp(n)
	for {
		b.mu.Lock()
		if b.free >= n {
			b.free -= n
			b.mu.Unlock()
			return nil
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (b *memoryBudget) release(n int64) {
	n = b.clamp(n)
	b.mu.Lock()
	b.free += n
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
}
