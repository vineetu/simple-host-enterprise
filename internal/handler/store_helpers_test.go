package handler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/vsriram/simple-host/internal/storage"
)

// testSiteIndex is a map-backed storage.SiteIndex: the tests' stand-in for
// the sites table the production index reads.
type testSiteIndex struct {
	mu      sync.Mutex
	sites   map[[2]string]testIndexedSite
	failing map[[2]string]bool
}

type testIndexedSite struct {
	id      string
	version int
}

func (i *testSiteIndex) set(owner, site, siteID string, version int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.sites[[2]string{owner, site}] = testIndexedSite{id: siteID, version: version}
}

func (i *testSiteIndex) remove(owner, site string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.sites, [2]string{owner, site})
}

// fail makes every lookup of owner/site return an error, the way a database
// failure would.
func (i *testSiteIndex) fail(owner, site string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.failing[[2]string{owner, site}] = true
}

func (i *testSiteIndex) ServingSite(_ context.Context, owner, site string) (string, int, bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.failing[[2]string{owner, site}] {
		return "", 0, false, errors.New("index unavailable")
	}
	entry, ok := i.sites[[2]string{owner, site}]
	return entry.id, entry.version, ok, nil
}

func (i *testSiteIndex) Owners(context.Context) ([]string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	seen := map[string]bool{}
	var owners []string
	for key := range i.sites {
		if !seen[key[0]] {
			seen[key[0]] = true
			owners = append(owners, key[0])
		}
	}
	sort.Strings(owners)
	return owners, nil
}

func (i *testSiteIndex) Sites(_ context.Context, owner string) ([]string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var names []string
	for key := range i.sites {
		if key[0] == owner {
			names = append(names, key[1])
		}
	}
	sort.Strings(names)
	return names, nil
}

// testStore is a site store on an in-memory bucket and a map index.
type testStore struct {
	*storage.Store
	objects *storage.MemoryObjects
	index   *testSiteIndex
}

func newTestStore(t *testing.T) *testStore {
	t.Helper()
	objects := storage.NewMemoryObjects()
	index := &testSiteIndex{sites: map[[2]string]testIndexedSite{}, failing: map[[2]string]bool{}}
	store, err := storage.New(storage.Options{
		Objects:       objects,
		Index:         index,
		CacheDir:      t.TempDir(),
		CacheMaxBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &testStore{Store: store, objects: objects, index: index}
}

// publish uploads files as siteID's version and makes it the live one for
// owner/site, which is what a committed deploy amounts to.
func (s *testStore) publish(t *testing.T, owner, site, siteID string, version int, files map[string]string) {
	t.Helper()
	contents := make(map[string][]byte, len(files))
	for name, body := range files {
		contents[name] = []byte(body)
	}
	if _, err := s.PutVersion(context.Background(), siteID, version, contents); err != nil {
		t.Fatal(err)
	}
	s.index.set(owner, site, siteID, version)
}

// testSiteID derives a stable site id from owner and site, for tests that
// only care that each site has its own.
func testSiteID(owner, site string) string {
	hex := fmt.Sprintf("%x", sha256.Sum256([]byte(owner+"/"+site)))
	return hex[0:8] + "-" + hex[8:12] + "-4" + hex[13:16] + "-8" + hex[17:20] + "-" + hex[20:32]
}
