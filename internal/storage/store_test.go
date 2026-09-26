package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

const (
	testSiteA = "0b6f7e2a-3c4d-4e5f-8a9b-0c1d2e3f4a5b"
	testSiteB = "1c7a8f3b-4d5e-4f60-9b0c-1d2e3f4a5b6c"
	testSiteC = "2d8b9a4c-5e6f-4071-8c1d-2e3f4a5b6c7d"
)

// mapIndex is SiteIndex over a map keyed "owner/site".
type mapIndex map[string]struct {
	id      string
	version int
}

func (m mapIndex) ServingSite(_ context.Context, owner, site string) (string, int, bool, error) {
	entry, ok := m[owner+"/"+site]
	return entry.id, entry.version, ok, nil
}

func (m mapIndex) Owners(context.Context) ([]string, error)        { return nil, nil }
func (m mapIndex) Sites(context.Context, string) ([]string, error) { return nil, nil }

// recordingObjects wraps an Objects, counting Gets and remembering the
// content type of every Put, and can fail Deletes of chosen keys.
type recordingObjects struct {
	Objects
	mu          sync.Mutex
	gets        map[string]int
	types       map[string]string
	failDeletes map[string]bool
}

func newRecordingObjects(inner Objects) *recordingObjects {
	return &recordingObjects{Objects: inner, gets: map[string]int{}, types: map[string]string{}, failDeletes: map[string]bool{}}
}

func (r *recordingObjects) Get(ctx context.Context, key string, maxBytes int64) ([]byte, error) {
	r.mu.Lock()
	r.gets[key]++
	r.mu.Unlock()
	return r.Objects.Get(ctx, key, maxBytes)
}

func (r *recordingObjects) GetTo(ctx context.Context, key string, maxBytes int64, w io.Writer) (int64, error) {
	r.mu.Lock()
	r.gets[key]++
	r.mu.Unlock()
	return r.Objects.GetTo(ctx, key, maxBytes, w)
}

func (r *recordingObjects) Put(ctx context.Context, key string, body []byte, contentType string) error {
	r.mu.Lock()
	r.types[key] = contentType
	r.mu.Unlock()
	return r.Objects.Put(ctx, key, body, contentType)
}

func (r *recordingObjects) Delete(ctx context.Context, key string) error {
	r.mu.Lock()
	fail := r.failDeletes[key]
	r.mu.Unlock()
	if fail {
		return errors.New("injected delete failure")
	}
	return r.Objects.Delete(ctx, key)
}

func (r *recordingObjects) getCount(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gets[key]
}

func newTestStore(t *testing.T, objects Objects, index SiteIndex, cacheMaxBytes int64) (*Store, string) {
	t.Helper()
	if index == nil {
		index = mapIndex{}
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	store, err := New(Options{Objects: objects, Index: index, CacheDir: cacheDir, CacheMaxBytes: cacheMaxBytes})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store, cacheDir
}

func mustPutVersion(t *testing.T, store *Store, siteID string, version int, files map[string][]byte) {
	t.Helper()
	if _, err := store.PutVersion(context.Background(), siteID, version, files); err != nil {
		t.Fatalf("PutVersion(%s, %d): %v", siteID, version, err)
	}
}

func mustOpenVersion(t *testing.T, store *Store, siteID string, version int) *VersionLease {
	t.Helper()
	lease, err := store.OpenVersion(context.Background(), siteID, version)
	if err != nil {
		t.Fatalf("OpenVersion(%s, %d): %v", siteID, version, err)
	}
	return lease
}

func readLeaseFile(t *testing.T, lease *VersionLease, name string) []byte {
	t.Helper()
	data, err := fs.ReadFile(lease.FS(), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

func TestStoreVersionRoundTrip(t *testing.T) {
	objects := NewMemoryObjects()
	store, _ := newTestStore(t, objects, nil, 1<<30)
	files := map[string][]byte{
		"index.html":         []byte("<!doctype html><h1>hello</h1>"),
		"assets/app.css":     []byte("body{margin:0}"),
		"assets/deep/x.json": []byte(`{"a":[1,2,3]}`),
		"empty.txt":          {},
		"binary.bin":         {0x00, 0x01, 0xff, 0xfe, 0x00},
	}
	put, err := store.PutVersion(context.Background(), testSiteA, 3, files)
	total := put.FileBytes
	if err != nil {
		t.Fatalf("PutVersion: %v", err)
	}
	var want int64
	for _, content := range files {
		want += int64(len(content))
	}
	if total != want {
		t.Fatalf("PutVersion total = %d, want %d", total, want)
	}
	if keys := objects.Keys(); len(keys) != 1 || keys[0] != "sites/"+testSiteA+"/v3.tar.gz" {
		t.Fatalf("bucket keys = %v", keys)
	}
	if listed, _ := objects.List(context.Background(), "sites/"+testSiteA+"/v3.tar.gz"); len(listed) != 1 || listed[0].Size != put.StoredBytes {
		t.Fatalf("stored bytes = %d, bucket has %v", put.StoredBytes, listed)
	}

	lease := mustOpenVersion(t, store, testSiteA, 3)
	defer lease.Close()
	count := 0
	err = fs.WalkDir(lease.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		count++
		wantContent, ok := files[name]
		if !ok {
			t.Errorf("unexpected file %s", name)
			return nil
		}
		if got := readLeaseFile(t, lease, name); !bytes.Equal(got, wantContent) {
			t.Errorf("%s = %q, want %q", name, got, wantContent)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != len(files) {
		t.Fatalf("walked %d files, want %d", count, len(files))
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestStoreOpenCurrentAndNotExist(t *testing.T) {
	objects := NewMemoryObjects()
	index := mapIndex{
		"alice/demo": {id: testSiteA, version: 2},
		"bob/gone":   {id: testSiteB, version: 1}, // row exists, object does not
	}
	store, _ := newTestStore(t, objects, index, 1<<30)
	mustPutVersion(t, store, testSiteA, 1, map[string][]byte{"index.html": []byte("one")})
	mustPutVersion(t, store, testSiteA, 2, map[string][]byte{"index.html": []byte("two")})

	lease, err := store.OpenCurrent(context.Background(), "alice", "demo")
	if err != nil {
		t.Fatalf("OpenCurrent: %v", err)
	}
	if got := readLeaseFile(t, lease, "index.html"); string(got) != "two" {
		t.Fatalf("OpenCurrent served %q, want two", got)
	}
	lease.Close()

	if _, err := store.OpenCurrent(context.Background(), "alice", "missing"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing site error = %v, want fs.ErrNotExist", err)
	}
	if _, err := store.OpenCurrent(context.Background(), "bob", "gone"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing object error = %v, want fs.ErrNotExist", err)
	}
	if _, err := store.OpenVersion(context.Background(), testSiteA, 9); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing version error = %v, want fs.ErrNotExist", err)
	}
	for _, bad := range [][2]string{{"..", "demo"}, {"alice", "../demo"}, {"a/b", "demo"}, {"", "demo"}} {
		if _, err := store.OpenCurrent(context.Background(), bad[0], bad[1]); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("OpenCurrent(%q, %q) error = %v, want an identity error", bad[0], bad[1], err)
		}
	}
	if version, ok, err := store.CurrentVersion("alice", "demo"); err != nil || !ok || version != 2 {
		t.Fatalf("CurrentVersion = %d, %v, %v", version, ok, err)
	}
}

func TestStoreLeaseRootCannotEscape(t *testing.T) {
	store, cacheDir := newTestStore(t, NewMemoryObjects(), nil, 1<<30)
	mustPutVersion(t, store, testSiteA, 1, map[string][]byte{"index.html": []byte("home")})
	mustPutVersion(t, store, testSiteB, 1, map[string][]byte{"secret.txt": []byte("other site")})
	other := mustOpenVersion(t, store, testSiteB, 1)
	defer other.Close()
	lease := mustOpenVersion(t, store, testSiteA, 1)
	defer lease.Close()

	outside := filepath.Join(filepath.Dir(cacheDir), "outside.txt")
	if err := os.WriteFile(outside, []byte("host file"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Plant symlinks inside the unpacked version, as a compromised or buggy
	// fill might; the lease's os.Root must refuse to follow them out.
	entryDir := filepath.Join(cacheDir, testSiteA+".v1")
	if err := os.Symlink(outside, filepath.Join(entryDir, "abs-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../"+testSiteB+".v1/secret.txt", filepath.Join(entryDir, "rel-link")); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"../" + testSiteB + ".v1/secret.txt", "../../outside.txt", "/etc/passwd", "abs-link", "rel-link"} {
		if file, err := lease.Root().Open(name); err == nil {
			file.Close()
			t.Errorf("Root().Open(%q) escaped the version", name)
		}
		if _, err := fs.ReadFile(lease.FS(), name); err == nil {
			t.Errorf("FS() read %q outside the version", name)
		}
	}
}

func TestPutVersionRejectsBadInput(t *testing.T) {
	objects := NewMemoryObjects()
	store, _ := newTestStore(t, objects, nil, 1<<30)
	ctx := context.Background()
	if _, err := store.PutVersion(ctx, "not-a-uuid", 1, map[string][]byte{"a": nil}); err == nil {
		t.Error("PutVersion accepted a malformed site id")
	}
	if _, err := store.PutVersion(ctx, testSiteA, 0, map[string][]byte{"a": nil}); err == nil {
		t.Error("PutVersion accepted version 0")
	}
	if _, err := store.PutVersion(ctx, testSiteA, 1, map[string][]byte{"../a": nil}); err == nil {
		t.Error("PutVersion accepted a traversal path")
	}
	if keys := objects.Keys(); len(keys) != 0 {
		t.Fatalf("refused PutVersion wrote objects: %v", keys)
	}
}

func TestStoreCopyVersionAndUsage(t *testing.T) {
	objects := NewMemoryObjects()
	store, _ := newTestStore(t, objects, nil, 1<<30)
	ctx := context.Background()
	mustPutVersion(t, store, testSiteA, 1, map[string][]byte{"index.html": []byte("restored")})
	if err := CopyVersion(ctx, store.Objects(), testSiteA, 1, testSiteB, 4); err != nil {
		t.Fatalf("CopyVersion: %v", err)
	}
	lease := mustOpenVersion(t, store, testSiteB, 4)
	if got := readLeaseFile(t, lease, "index.html"); string(got) != "restored" {
		t.Fatalf("copied version = %q", got)
	}
	lease.Close()
	if err := objects.Put(ctx, "sites/"+testSiteB+"/assets/"+testSiteC, []byte("12345"), ""); err != nil {
		t.Fatal(err)
	}
	if err := objects.Put(ctx, "sites/not-a-site/v1.tar.gz", []byte("x"), ""); err != nil {
		t.Fatal(err)
	}

	usage, err := store.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 2 {
		t.Fatalf("usage covers %d sites, want 2: %v", len(usage), usage)
	}
	b := usage[testSiteB]
	if len(b.VersionBytes) != 1 || b.VersionBytes[4] == 0 || b.TotalBytes != b.VersionBytes[4]+5 {
		t.Fatalf("site B usage = %+v", b)
	}

	if err := store.DeleteVersionObject(ctx, testSiteA, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := objects.Get(ctx, "sites/"+testSiteA+"/v1.tar.gz", 1<<20); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("version object still present: %v", err)
	}
}
