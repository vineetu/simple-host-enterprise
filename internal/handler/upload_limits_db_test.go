package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/storage"
)

// incompressible returns n bytes that gzip cannot shrink, so a deploy's
// stored size is predictable.
func incompressible(seed int64, n int) []byte {
	out := make([]byte, n)
	_, _ = rand.New(rand.NewSource(seed)).Read(out)
	return out
}

func zipOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(content)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var body errorResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Code, body.Error
}

func (w *accessWorld) siteID(owner, site string) string {
	w.t.Helper()
	var id string
	if err := w.database.QueryRow(`SELECT s.id FROM sites s JOIN users u ON u.id = s.user_id WHERE u.username = $1 AND s.name = $2`, owner, site).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

func (w *accessWorld) uploadAsset(user, owner, site, name string, content []byte) *httptest.ResponseRecorder {
	w.t.Helper()
	request := multipartUploadRequest(w.t, name, content)
	request.URL.Host = site + "." + owner + "." + accessBase
	request.Host = request.URL.Host
	request.URL.Scheme = "https"
	request.URL.Path = "/api/sites/" + site + "/assets"
	request.RequestURI = ""
	request.Header.Set("X-API-Key", w.apiKeys[user])
	return w.do(request)
}

func TestQuotaSiteLimitPerOwner(t *testing.T) {
	w := newAccessWorldWith(t, UploadQuota{MaxSites: 2}, nil)
	w.newTeam("crew", "alice")
	w.deploy("alice", "/api/sites/one")
	w.deploy("alice", "/api/collaboration/sites/alice/two")

	for _, path := range []string{"/api/sites/three", "/api/collaboration/sites/alice/three"} {
		rec := w.api("alice", http.MethodPost, path, zipOf(t, map[string][]byte{"index.html": []byte("x")}))
		code, message := errorCode(t, rec)
		if rec.Code != http.StatusConflict || code != "site_limit" || !strings.Contains(message, "too many sites (2 of 2)") {
			t.Fatalf("third site via %s = %d %s", path, rec.Code, rec.Body)
		}
	}
	if n := w.count(`SELECT count(*) FROM sites WHERE user_id = $1`, w.users["alice"]); n != 2 {
		t.Fatalf("alice has %d sites after refusals", n)
	}
	// Updating an existing site is not a new site.
	if rec := w.api("alice", http.MethodPut, "/api/sites/one", zipOf(t, map[string][]byte{"index.html": []byte("v2")})); rec.Code != http.StatusOK {
		t.Fatalf("update at the site limit = %d %s", rec.Code, rec.Body)
	}
	// A team is its own owner with its own count.
	w.deploy("alice", "/api/collaboration/sites/team-crew/a")
	w.deploy("alice", "/api/collaboration/sites/team-crew/b")
	if rec := w.api("alice", http.MethodPost, "/api/collaboration/sites/team-crew/c", zipOf(t, map[string][]byte{"index.html": []byte("x")})); rec.Code != http.StatusConflict {
		t.Fatalf("team's third site = %d %s", rec.Code, rec.Body)
	}

	// GET /api/me reports each namespace's usage.
	rec := w.api("alice", http.MethodGet, "/api/me", nil)
	var me struct {
		Usage []ownerUsage `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil || len(me.Usage) != 2 {
		t.Fatalf("/api/me = %d %s", rec.Code, rec.Body)
	}
	if me.Usage[0].Owner != "alice" || me.Usage[0].Sites != 2 || me.Usage[0].MaxSites != 2 || me.Usage[0].Bytes <= 0 {
		t.Fatalf("alice usage = %+v", me.Usage[0])
	}
	if me.Usage[1].Owner != "team-crew" || me.Usage[1].Sites != 2 {
		t.Fatalf("crew usage = %+v", me.Usage[1])
	}

	// The dashboard shows the same numbers.
	dashboard := http.NewServeMux()
	NewDashboardHandler(w.database, w.keys, time.Hour).WithQuota(UploadQuota{MaxSites: 2}).Register(dashboard, nil)
	page := httptest.NewRequest(http.MethodGet, "https://"+accessBase+"/dashboard", nil)
	page.AddCookie(w.cookie("alice", ""))
	out := httptest.NewRecorder()
	dashboard.ServeHTTP(out, page)
	if !strings.Contains(out.Body.String(), "2 of 2 sites") || !strings.Contains(out.Body.String(), ">team-crew<") {
		t.Fatalf("dashboard usage missing: %d", out.Code)
	}
}

func TestQuotaStorageBytesAndPruning(t *testing.T) {
	const size = 40 << 10
	w := newAccessWorldWith(t, UploadQuota{MaxBytes: 100 << 10, MaxVersions: 1}, nil)
	page := func(seed int64) []byte { return zipOf(t, map[string][]byte{"index.html": incompressible(seed, size)}) }

	if rec := w.api("alice", http.MethodPost, "/api/sites/one", page(1)); rec.Code != http.StatusCreated {
		t.Fatalf("first site = %d %s", rec.Code, rec.Body)
	}
	if rec := w.api("alice", http.MethodPost, "/api/sites/two", page(2)); rec.Code != http.StatusCreated {
		t.Fatalf("second site = %d %s", rec.Code, rec.Body)
	}
	oneID := w.siteID("alice", "one")
	var recorded int64
	if err := w.database.QueryRow(`SELECT size_bytes FROM versions WHERE site_id = $1`, oneID).Scan(&recorded); err != nil || recorded < size {
		t.Fatalf("recorded version size = %d, %v", recorded, err)
	}

	rec := w.api("alice", http.MethodPost, "/api/sites/three", page(3))
	if code, message := errorCode(t, rec); rec.Code != http.StatusRequestEntityTooLarge || code != "storage_quota" || !strings.Contains(message, "of 100.0 KiB") {
		t.Fatalf("third site over the byte quota = %d %s", rec.Code, rec.Body)
	}
	if n := w.count(`SELECT count(*) FROM sites WHERE user_id = $1`, w.users["alice"]); n != 2 {
		t.Fatalf("refused deploy left a site row: %d", n)
	}

	// With one version retained, an update replaces the old version, frees
	// its bytes and so fits; the old object goes to the retire queue.
	if rec := w.api("alice", http.MethodPut, "/api/sites/one", page(4)); rec.Code != http.StatusOK {
		t.Fatalf("update that frees what it adds = %d %s", rec.Code, rec.Body)
	}
	if n := w.count(`SELECT count(*) FROM versions WHERE site_id = $1`, oneID); n != 1 {
		t.Fatalf("versions after update = %d, want 1", n)
	}
	if n := w.count(`SELECT count(*) FROM storage_retired WHERE object_key = $1`, "sites/"+oneID+"/v1.tar.gz"); n != 1 {
		t.Fatalf("pruned version not queued for the sweep")
	}

	// Assets count too.
	if rec := w.uploadAsset("alice", "alice", "one", "big.bin", incompressible(5, 30<<10)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("asset over the quota = %d %s", rec.Code, rec.Body)
	} else if code, _ := errorCode(t, rec); code != "storage_quota" {
		t.Fatalf("asset refusal code = %q", code)
	}
	if rec := w.uploadAsset("alice", "alice", "one", "small.bin", incompressible(6, 4<<10)); rec.Code != http.StatusCreated {
		t.Fatalf("asset within the quota = %d %s", rec.Code, rec.Body)
	}
	usage, err := db.OwnerUsageOf(context.Background(), w.database, w.users["alice"])
	if err != nil {
		t.Fatal(err)
	}
	if usage.Sites != 2 || usage.Bytes < 2*size+4<<10 || usage.Bytes > 100<<10 {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestPruneVersionsKeepsTheActiveOne(t *testing.T) {
	w := newAccessWorldWith(t, UploadQuota{MaxVersions: 20}, nil)
	w.deploy("alice", "/api/sites/demo")
	for i := 2; i <= 4; i++ {
		if rec := w.api("alice", http.MethodPut, "/api/sites/demo", zipOf(t, map[string][]byte{"index.html": []byte(fmt.Sprint(i))})); rec.Code != http.StatusOK {
			t.Fatalf("deploy v%d = %d %s", i, rec.Code, rec.Body)
		}
	}
	if rec := w.api("alice", http.MethodPost, "/api/sites/demo/rollback", map[string]int{"version": 1}); rec.Code != http.StatusOK {
		t.Fatalf("rollback = %d %s", rec.Code, rec.Body)
	}
	siteID := w.siteID("alice", "demo")
	tx, err := w.database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	pruned, err := db.PruneVersions(context.Background(), tx, siteID, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0].VersionNumber != 2 {
		t.Fatalf("pruned = %+v, want only v2 (v1 is live)", pruned)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var left []int
	versions, _ := db.ListVersions(context.Background(), w.database, siteID)
	for _, v := range versions {
		left = append(left, v.VersionNumber)
	}
	if fmt.Sprint(left) != "[4 3 1]" {
		t.Fatalf("versions left = %v", left)
	}
}

func TestDeployPrunesBeyondRetention(t *testing.T) {
	w := newAccessWorldWith(t, UploadQuota{MaxVersions: 3}, nil)
	w.deploy("alice", "/api/sites/demo")
	for i := 2; i <= 5; i++ {
		if rec := w.api("alice", http.MethodPut, "/api/sites/demo", zipOf(t, map[string][]byte{"index.html": []byte(fmt.Sprint(i))})); rec.Code != http.StatusOK {
			t.Fatalf("deploy v%d = %d %s", i, rec.Code, rec.Body)
		}
	}
	siteID := w.siteID("alice", "demo")
	if n := w.count(`SELECT count(*) FROM versions WHERE site_id = $1 AND version_number IN (3, 4, 5)`, siteID); n != 3 {
		t.Fatalf("retained versions = %d", n)
	}
	if n := w.count(`SELECT count(*) FROM versions WHERE site_id = $1`, siteID); n != 3 {
		t.Fatalf("versions = %d, want 3", n)
	}
	if n := w.count(`SELECT count(*) FROM storage_retired WHERE object_key LIKE $1`, "sites/"+siteID+"/v%"); n != 2 {
		t.Fatalf("retired version objects = %d, want 2", n)
	}
}

// Parallel creates in one namespace, as separate replicas would run them:
// the owner lock lets exactly as many through as fit.
func TestQuotaConcurrentCreatesSerializePerOwner(t *testing.T) {
	w := newAccessWorldWith(t, UploadQuota{}, nil)
	owner := w.users["alice"]
	ctx := context.Background()

	run := func(quota UploadQuota, prefix string, size int64) int {
		var wg sync.WaitGroup
		var mu sync.Mutex
		committed := 0
		start := make(chan struct{})
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				tx, err := w.database.BeginTx(ctx, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer tx.Rollback()
				site, err := db.CreateSite(ctx, tx, owner, fmt.Sprintf("%s%d", prefix, i))
				if err != nil {
					t.Error(err)
					return
				}
				version, err := db.CreateVersion(ctx, tx, site.ID, 1, "x", &owner)
				if err != nil {
					t.Error(err)
					return
				}
				if err := db.SetVersionSize(ctx, tx, version.ID, size); err != nil {
					t.Error(err)
					return
				}
				refusal, err := checkOwnerQuota(ctx, tx, quota, owner, true, size, 0)
				if err != nil {
					t.Error(err)
					return
				}
				if refusal != nil {
					return
				}
				if err := tx.Commit(); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				committed++
				mu.Unlock()
			}(i)
		}
		close(start)
		wg.Wait()
		return committed
	}

	if got := run(UploadQuota{MaxSites: 3}, "s", 1); got != 3 {
		t.Fatalf("site limit 3: %d parallel creates committed", got)
	}
	// Three sites of 1 byte already; room for exactly two more of 1000.
	if got := run(UploadQuota{MaxBytes: 2003}, "b", 1000); got != 2 {
		t.Fatalf("byte limit: %d parallel creates committed, want 2", got)
	}
}

// fakeScanner flags any file containing "EICAR" and fails when err is set.
type fakeScanner struct {
	err     error
	scanned int
	mu      sync.Mutex
}

func (f *fakeScanner) Scan(_ context.Context, r io.Reader) (string, error) {
	f.mu.Lock()
	f.scanned++
	f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	data, _ := io.ReadAll(r)
	if bytes.Contains(data, []byte("EICAR")) {
		return "Eicar-Test-Signature", nil
	}
	return "", nil
}

func TestMalwareScanRefusesDeploysAndAssets(t *testing.T) {
	scanner := &fakeScanner{}
	w := newAccessWorldWith(t, UploadQuota{}, scanner)

	rec := w.api("alice", http.MethodPost, "/api/sites/demo", zipOf(t, map[string][]byte{
		"index.html":   []byte("<h1>ok</h1>"),
		"files/bad.js": []byte("x EICAR x"),
	}))
	code, message := errorCode(t, rec)
	if rec.Code != http.StatusUnprocessableEntity || code != "malware_found" || !strings.Contains(message, "files/bad.js") || !strings.Contains(message, "Eicar-Test-Signature") {
		t.Fatalf("infected deploy = %d %s", rec.Code, rec.Body)
	}
	if n := w.count(`SELECT count(*) FROM sites WHERE user_id = $1`, w.users["alice"]); n != 0 {
		t.Fatalf("infected deploy created a site")
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'upload_infected' AND actor_id = $1`, w.users["alice"]); n != 1 {
		t.Fatalf("upload_infected audit rows = %d, want 1", n)
	}

	w.deploy("alice", "/api/sites/demo")
	if scanner.scanned == 0 {
		t.Fatal("clean deploy was not scanned")
	}
	if rec := w.uploadAsset("alice", "alice", "demo", "evil.txt", []byte("EICAR")); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("infected asset = %d %s", rec.Code, rec.Body)
	}
	clean := []byte("plain text, scanned then stored whole")
	if rec := w.uploadAsset("alice", "alice", "demo", "notes.txt", clean); rec.Code != http.StatusCreated {
		t.Fatalf("clean asset = %d %s", rec.Code, rec.Body)
	}
	if n := w.count(`SELECT count(*) FROM site_assets WHERE size = $1`, len(clean)); n != 1 {
		t.Fatalf("clean asset not stored whole after the scan")
	}

	// A configured scanner that is down fails closed.
	scanner.err = errors.New("connection refused")
	rec = w.api("alice", http.MethodPut, "/api/sites/demo", zipOf(t, map[string][]byte{"index.html": []byte("v2")}))
	if code, _ := errorCode(t, rec); rec.Code != http.StatusServiceUnavailable || code != "scanner_unavailable" {
		t.Fatalf("deploy with the scanner down = %d %s", rec.Code, rec.Body)
	}
	if n := w.count(`SELECT count(*) FROM versions WHERE site_id = $1`, w.siteID("alice", "demo")); n != 1 {
		t.Fatalf("deploy with the scanner down stored a version")
	}
	if rec := w.uploadAsset("alice", "alice", "demo", "more.txt", clean); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("asset with the scanner down = %d %s", rec.Code, rec.Body)
	}
}

// Versions recorded without a size (before migration 0037, or by restore)
// get it from the bucket in the background.
func TestFillVersionSizesFromBucket(t *testing.T) {
	database := connectorTestDB(t)
	ctx := context.Background()
	store, err := storage.New(storage.Options{
		Objects: storage.NewMemoryObjects(), Index: storage.NewDBIndex(database),
		CacheDir: t.TempDir(), CacheMaxBytes: 16 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	user, err := db.CreateOIDCUser(ctx, database, "alice", "sub-alice", "alice@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	site, err := db.CreateSite(ctx, database, user.ID, "demo")
	if err != nil {
		t.Fatal(err)
	}
	put, err := store.PutVersion(ctx, site.ID, 1, map[string][]byte{"index.html": incompressible(9, 2000)})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1, 10} { // v10 has no object
		if _, err := db.CreateVersion(ctx, database, site.ID, n, "x", nil); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := store.FillVersionSizes(ctx, database); err != nil || n != 2 {
		t.Fatalf("FillVersionSizes = %d, %v", n, err)
	}
	var v1, v10 int64
	if err := database.QueryRow(`SELECT size_bytes FROM versions WHERE site_id = $1 AND version_number = 1`, site.ID).Scan(&v1); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT size_bytes FROM versions WHERE site_id = $1 AND version_number = 10`, site.ID).Scan(&v10); err != nil {
		t.Fatal(err)
	}
	if v1 != put.StoredBytes || v10 != 0 {
		t.Fatalf("sizes v1=%d (want %d) v10=%d (want 0)", v1, put.StoredBytes, v10)
	}
	if n, err := store.FillVersionSizes(ctx, database); err != nil || n != 0 {
		t.Fatalf("second pass = %d, %v; want nothing left", n, err)
	}
}
