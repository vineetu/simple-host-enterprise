package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// cache is the pod-local copy of immutable objects: unpacked versions (one
// directory each) and asset files. Nothing in it is authoritative — the bucket
// is — and because every entry is keyed by an immutable name (a version number
// that is never reused for a site id, or an asset id) an entry never goes
// stale; it is only ever evicted.
//
// The cache holds at most maxBytes of entries nobody is reading. An entry in
// use is pinned by its lease and never removed from under a reader, so the
// total can exceed maxBytes by what is currently being served; size the
// volume (an emptyDir sizeLimit) with that headroom.
type cache struct {
	root     *os.Root
	maxBytes int64

	mu      sync.Mutex
	entries map[string]*cacheEntry
	fills   map[string]*cacheFill
	used    int64
	clock   func() time.Time
}

type cacheEntry struct {
	name     string
	size     int64
	refs     int
	lastUsed time.Time
}

type cacheFill struct {
	done chan struct{}
	err  error
}

// cacheBlockOverhead approximates the filesystem's per-file allocation, so the
// accounting tracks what a volume's size limit actually measures.
const cacheBlockOverhead = 4096

// cacheFillTimeout bounds one fetch-and-unpack from the bucket.
const cacheFillTimeout = 2 * time.Minute

func openCache(dir string, maxBytes int64) (*cache, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("cache size must be positive")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("open cache dir: %w", err)
	}
	// Start empty. Whatever a previous process left is either complete and
	// cheap to fetch again, or a half-written fill; the accounting starts
	// from what this process wrote.
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("read cache dir: %w", err)
	}
	for _, entry := range entries {
		if err := removeTree(root, entry.Name()); err != nil {
			root.Close()
			return nil, fmt.Errorf("clear cache entry %s: %w", entry.Name(), err)
		}
	}
	return &cache{
		root:     root,
		maxBytes: maxBytes,
		entries:  map[string]*cacheEntry{},
		fills:    map[string]*cacheFill{},
		clock:    time.Now,
	}, nil
}

func (c *cache) close() error { return c.root.Close() }

// acquire returns the named entry, filling it with fill on a miss. fill
// writes the entry under the temporary name it is given (a directory or a
// file directly beneath the cache root) and returns its size. Concurrent
// callers for one name share a single fill. The caller must release the
// returned entry exactly once.
func (c *cache) acquire(ctx context.Context, name string, fill func(ctx context.Context, temporary string) (int64, error)) (*cacheEntry, error) {
	for {
		c.mu.Lock()
		if entry, ok := c.entries[name]; ok {
			entry.refs++
			entry.lastUsed = c.clock()
			c.mu.Unlock()
			return entry, nil
		}
		if pending, ok := c.fills[name]; ok {
			c.mu.Unlock()
			select {
			case <-pending.done:
				if pending.err != nil {
					return nil, pending.err
				}
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		pending := &cacheFill{done: make(chan struct{})}
		c.fills[name] = pending
		c.mu.Unlock()

		entry, err := c.fill(ctx, name, fill)

		c.mu.Lock()
		delete(c.fills, name)
		pending.err = err
		close(pending.done)
		if err != nil {
			c.mu.Unlock()
			return nil, err
		}
		c.entries[name] = entry
		c.used += entry.size
		victims := c.evictLocked()
		c.mu.Unlock()
		c.removeVictims(victims)
		return entry, nil
	}
}

func (c *cache) fill(ctx context.Context, name string, fill func(context.Context, string) (int64, error)) (*cacheEntry, error) {
	temporary, err := uniqueName(".fill-")
	if err != nil {
		return nil, err
	}
	// The fill is shared by every caller waiting on this name, so it must not
	// die with whichever request happened to start it.
	fillCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheFillTimeout)
	defer cancel()
	size, err := fill(fillCtx, temporary)
	if err == nil {
		err = c.root.Rename(temporary, name)
	}
	if err != nil {
		if removeErr := removeTree(c.root, temporary); removeErr != nil {
			log.Printf("storage cache: remove failed fill %s: %v", temporary, removeErr)
		}
		return nil, err
	}
	return &cacheEntry{name: name, size: size, refs: 1, lastUsed: c.clock()}, nil
}

func (c *cache) release(entry *cacheEntry) {
	c.mu.Lock()
	entry.refs--
	entry.lastUsed = c.clock()
	victims := c.evictLocked()
	c.mu.Unlock()
	c.removeVictims(victims)
}

// evictLocked drops least-recently-used unpinned entries until the cache is
// within its bound. Each victim is renamed aside while the lock is held, so a
// concurrent fill of the same name never meets the old one on disk; the
// renamed trees are deleted by removeVictims once the lock is released.
func (c *cache) evictLocked() []string {
	var trash []string
	for c.used > c.maxBytes {
		var victim *cacheEntry
		for _, entry := range c.entries {
			if entry.refs == 0 && (victim == nil || entry.lastUsed.Before(victim.lastUsed)) {
				victim = entry
			}
		}
		if victim == nil {
			return trash
		}
		delete(c.entries, victim.name)
		c.used -= victim.size
		aside, err := uniqueName(".evict-")
		if err == nil {
			err = c.root.Rename(victim.name, aside)
		}
		if err != nil {
			log.Printf("storage cache: evict %s: %v", victim.name, err)
			continue
		}
		trash = append(trash, aside)
	}
	return trash
}

func (c *cache) removeVictims(trash []string) {
	for _, name := range trash {
		if err := removeTree(c.root, name); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("storage cache: remove evicted %s: %v", name, err)
		}
	}
}
