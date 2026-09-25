package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// gatedObjects blocks every Get until release is closed.
type gatedObjects struct {
	*recordingObjects
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *gatedObjects) Get(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	g.once.Do(func() { close(g.started) })
	<-g.release
	return g.recordingObjects.Get(ctx, key, maxBytes)
}

func TestCacheConcurrentOpensShareOneFetch(t *testing.T) {
	memory := NewMemoryObjects()
	gated := &gatedObjects{recordingObjects: newRecordingObjects(memory), started: make(chan struct{}), release: make(chan struct{})}
	store, _ := newTestStore(t, gated, nil, 1<<30)
	mustPutVersion(t, store, testSiteA, 1, map[string][]byte{"index.html": []byte("shared")})

	const readers = 16
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := store.OpenVersion(context.Background(), testSiteA, 1)
			if err != nil {
				errs <- err
				return
			}
			defer lease.Close()
			if got := readLeaseFile(t, lease, "index.html"); string(got) != "shared" {
				errs <- errors.New("wrong content: " + string(got))
			}
		}()
	}
	<-gated.started
	time.Sleep(50 * time.Millisecond) // let the others queue on the fill
	close(gated.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	key, _ := VersionKey(testSiteA, 1)
	if n := gated.getCount(key); n != 1 {
		t.Fatalf("bucket Get called %d times, want 1", n)
	}
}

// useFakeClock makes lastUsed strictly increase per call, so LRU order is
// exactly the order of operations.
func useFakeClock(store *Store) {
	var mu sync.Mutex
	now := time.Unix(1_000_000, 0)
	store.cache.clock = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Second)
		return now
	}
}

func entryDir(cacheDir, siteID string, version int) string {
	return filepath.Join(cacheDir, siteID+".v"+strconv.Itoa(version))
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	recording := newRecordingObjects(NewMemoryObjects())
	// One 100-byte file costs 100 + 2*4096 bytes; room for two entries.
	store, cacheDir := newTestStore(t, recording, nil, 2*(100+2*cacheBlockOverhead)+100)
	useFakeClock(store)
	for version := 1; version <= 3; version++ {
		mustPutVersion(t, store, testSiteA, version, map[string][]byte{"f": make([]byte, 100)})
	}
	open := func(version int) { mustOpenVersion(t, store, testSiteA, version).Close() }

	open(1)
	open(2)
	open(1) // v2 is now least recently used
	open(3)
	if exists(entryDir(cacheDir, testSiteA, 2)) {
		t.Fatal("v2 (least recently used) was not evicted from disk")
	}
	if !exists(entryDir(cacheDir, testSiteA, 1)) || !exists(entryDir(cacheDir, testSiteA, 3)) {
		t.Fatal("recently used entries were evicted")
	}
	key1, _ := VersionKey(testSiteA, 1)
	key2, _ := VersionKey(testSiteA, 2)
	if recording.getCount(key1) != 1 {
		t.Fatalf("v1 fetched %d times, want 1 (cache hit)", recording.getCount(key1))
	}
	open(2)
	if recording.getCount(key2) != 2 {
		t.Fatalf("v2 fetched %d times, want 2 (refetch after eviction)", recording.getCount(key2))
	}
	assertNoTemporaries(t, cacheDir)
}

func TestCacheNeverEvictsPinnedLease(t *testing.T) {
	store, cacheDir := newTestStore(t, NewMemoryObjects(), nil, 1)
	useFakeClock(store)
	mustPutVersion(t, store, testSiteA, 1, map[string][]byte{"pinned.txt": []byte("still here")})
	mustPutVersion(t, store, testSiteA, 2, map[string][]byte{"x": []byte("x")})
	mustPutVersion(t, store, testSiteA, 3, map[string][]byte{"y": []byte("y")})

	pinned := mustOpenVersion(t, store, testSiteA, 1)
	mustOpenVersion(t, store, testSiteA, 2).Close()
	mustOpenVersion(t, store, testSiteA, 3).Close()
	if exists(entryDir(cacheDir, testSiteA, 2)) || exists(entryDir(cacheDir, testSiteA, 3)) {
		t.Fatal("unpinned entries over the bound were kept")
	}
	if got := readLeaseFile(t, pinned, "pinned.txt"); string(got) != "still here" {
		t.Fatalf("pinned file = %q", got)
	}
	if !exists(entryDir(cacheDir, testSiteA, 1)) {
		t.Fatal("pinned entry was evicted")
	}
	if err := pinned.Close(); err != nil {
		t.Fatal(err)
	}
	if exists(entryDir(cacheDir, testSiteA, 1)) {
		t.Fatal("entry was not evicted once its lease closed")
	}
	assertEmptyDir(t, cacheDir)
}

func TestCacheFailedFillIsRetried(t *testing.T) {
	memory := NewMemoryObjects()
	store, cacheDir := newTestStore(t, memory, nil, 1<<30)
	mustPutVersion(t, store, testSiteA, 1, map[string][]byte{"index.html": []byte("back")})

	down := errors.New("bucket unreachable")
	memory.Fail = down
	if _, err := store.OpenVersion(context.Background(), testSiteA, 1); !errors.Is(err, down) {
		t.Fatalf("OpenVersion error = %v, want the bucket error", err)
	}
	assertEmptyDir(t, cacheDir)
	memory.Fail = nil
	lease := mustOpenVersion(t, store, testSiteA, 1)
	defer lease.Close()
	if got := readLeaseFile(t, lease, "index.html"); string(got) != "back" {
		t.Fatalf("index.html = %q", got)
	}
}

func TestNewEmptiesCacheDir(t *testing.T) {
	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	outside := filepath.Join(base, "keep.txt")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cacheDir, testSiteA+".v1", "deep", "er"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".fill-leftover", testSiteA + ".v1/deep/er/f"} {
		if err := os.WriteFile(filepath.Join(cacheDir, name), []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(cacheDir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base, filepath.Join(cacheDir, testSiteA+".v1", "dirlink")); err != nil {
		t.Fatal(err)
	}

	store, err := New(Options{Objects: NewMemoryObjects(), Index: mapIndex{}, CacheDir: cacheDir, CacheMaxBytes: 1 << 20})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()
	assertEmptyDir(t, cacheDir)
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatalf("clearing the cache touched a symlink target: %q, %v", data, err)
	}

	if _, err := New(Options{Objects: NewMemoryObjects(), Index: mapIndex{}, CacheDir: cacheDir, CacheMaxBytes: 0}); err == nil {
		t.Fatal("New accepted a zero cache size")
	}
	if _, err := New(Options{Index: mapIndex{}, CacheDir: cacheDir, CacheMaxBytes: 1}); err == nil {
		t.Fatal("New accepted nil Objects")
	}
}

func assertNoTemporaries(t *testing.T, cacheDir string) {
	t.Helper()
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name := entry.Name(); name[0] == '.' {
			t.Errorf("leftover temporary %s", name)
		}
	}
}
