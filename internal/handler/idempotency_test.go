package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oplimits"
	sp "github.com/vsriram/simple-host/internal/storagepolicy"
)

func TestCreateReplayAcrossPodsCapsAndExpiry(t *testing.T) {
	database := connectorTestDB(t)
	user, err := db.CreateOIDCUser(context.Background(), database, "alice", "replay-alice", "alice@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateOIDCUser(context.Background(), database, "bob", "replay-bob", "bob@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	podA, podB := appRoleDB(t, database), appRoleDB(t, database)
	var calls atomic.Int32
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("ETag", `"created"`)
		writeJSON(w, 201, map[string]int{"id": 7})
	})
	request := func(account, path, key, body string, pod int) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		database := podA
		if pod == 1 {
			database = podB
		}
		replayCreate(rec, r, database, account, "site", next)
		return rec
	}
	results := make([]*httptest.ResponseRecorder, 8)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i] = request(user.ID, "/create?a=1", "same", "body", i%2) }(i)
	}
	wg.Wait()
	for _, rec := range results {
		if rec.Code != 201 || rec.Body.String() != results[0].Body.String() || rec.Header().Get("ETag") != `"created"` {
			t.Fatalf("replay: %d %s", rec.Code, rec.Body.String())
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("mutation called %d times", calls.Load())
	}
	if got := request(user.ID, "/create?a=1", "same", "changed", 1); got.Code != 422 {
		t.Fatalf("body conflict %d", got.Code)
	}
	for _, args := range [][2]string{{other.ID, "/create?a=1"}, {user.ID, "/else"}, {user.ID, "/create?a=2"}} {
		if got := request(args[0], args[1], "same", "body", 1); got.Code != 201 {
			t.Fatalf("identity/route separation %d", got.Code)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("separated calls %d", calls.Load())
	}
	old := oplimits.Get()
	defer oplimits.Set(old)
	limits := old
	limits.CreateReplayPerAccount = 3
	limits.CreateReplayTotal = 4
	oplimits.Set(limits)
	if got := request(user.ID, "/new", "new", "body", 0); got.Code != 429 {
		t.Fatalf("account cap %d", got.Code)
	}
	if got := request(other.ID, "/new", "new", "body", 0); got.Code != 429 {
		t.Fatalf("total cap %d", got.Code)
	}
	if got := request(user.ID, "/create?a=1", "same", "body", 1); got.Code != 201 {
		t.Fatalf("existing key at capacity %d", got.Code)
	}
	if _, err := database.Exec(`UPDATE create_idempotency SET expires_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if got := request(user.ID, "/create?a=1", "same", "changed", 1); got.Code != 201 {
		t.Fatalf("expired key %d", got.Code)
	}
	if calls.Load() != 5 {
		t.Fatalf("expiry calls %d", calls.Load())
	}
}

func TestCreateReplayRoutesAndLiveChecks(t *testing.T) {
	w := newAccessWorld(t)
	var archive bytes.Buffer
	z := zip.NewWriter(&archive)
	f, _ := z.Create("index.html")
	_, _ = f.Write([]byte("hello"))
	_ = z.Close()
	post := func(user, path, key string, body []byte, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://"+accessBase+path, bytes.NewReader(body))
		r.Header.Set("X-API-Key", w.apiKeys[user])
		r.Header.Set("X-Simple-Host-Client", "api")
		r.Header.Set("Idempotency-Key", key)
		if etag != "" {
			r.Header.Set("If-Match", etag)
		}
		return w.do(r)
	}
	for _, path := range []string{"/api/sites/replay-a", "/api/collaboration/sites/alice/replay-b"} {
		one := post("alice", path, "create", archive.Bytes(), "")
		two := post("alice", path, "create", archive.Bytes(), "")
		if one.Code != 201 || two.Code != 201 || one.Body.String() != two.Body.String() {
			t.Fatalf("site replay %s: %d %d %s", path, one.Code, two.Code, two.Body.String())
		}
		if rec := post("alice", path, "create", archive.Bytes(), `"stale"`); rec.Code != 412 {
			t.Fatalf("stale If-Match %d", rec.Code)
		}
	}
	if rec := post("vera", "/api/collaboration/sites/alice/replay-b", "create", archive.Bytes(), ""); rec.Code != 404 {
		t.Fatalf("stranger replay %d", rec.Code)
	}
	link := "/api/collaboration/sites/alice/replay-a/export-link"
	one := post("alice", link, "export", nil, "")
	two := post("alice", link, "export", nil, "")
	if one.Code != 200 || one.Body.String() != two.Body.String() {
		t.Fatalf("export replay %d %d", one.Code, two.Code)
	}
	if rec := w.api("mo", "POST", "/api/teams", map[string]any{"name": "replay"}); rec.Code != 201 {
		t.Fatal(rec.Body.String())
	}
	if rec := w.api("mo", "POST", "/api/teams/team-replay/members", map[string]any{"usernames": []string{"alice"}}); rec.Code != 201 && rec.Code != 200 {
		t.Fatalf("member add %d %s", rec.Code, rec.Body.String())
	}
	teamPath := "/api/collaboration/sites/team-replay/replayed"
	if rec := post("alice", teamPath, "team-create", archive.Bytes(), ""); rec.Code != 201 {
		t.Fatalf("team create %d %s", rec.Code, rec.Body.String())
	}
	if rec := w.api("mo", "DELETE", "/api/teams/team-replay/members/alice", nil); rec.Code != 200 && rec.Code != 204 {
		t.Fatalf("member remove %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("alice", teamPath, "team-create", archive.Bytes(), ""); rec.Code != 404 {
		t.Fatalf("former member replay %d", rec.Code)
	}
	siteID := w.storageSite("replay-rows", db.AccessNetwork)
	res := db.StorageResource{SiteID: siteID, Name: "r", Kind: "db", Preset: "private", Access: sp.Presets["private"], Settings: db.StorageSettings{Tables: map[string]db.StorageTable{"t": {Preset: "board", Access: sp.Presets["board"], Columns: map[string]sp.Column{"v": {Type: "text"}}}}}}
	tx, _ := w.database.Begin()
	if _, err := db.PutStorageResource(context.Background(), tx, res); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	add := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://replay-rows.alice."+accessBase+"/api/site/storage/db/r/tables/t/rows", strings.NewReader(`{"v":"one"}`))
		r.Header.Set("X-API-Key", w.apiKeys["vera"])
		r.Header.Set("Idempotency-Key", "row")
		return w.do(r)
	}
	one = add()
	two = add()
	if one.Code != 201 || two.Body.String() != one.Body.String() {
		t.Fatalf("row replay %d %d %s", one.Code, two.Code, two.Body.String())
	}
	res.Settings.Tables["t"] = db.StorageTable{Preset: "private", Access: sp.Presets["private"], Columns: map[string]sp.Column{"v": {Type: "text"}}}
	tx, _ = w.database.Begin()
	if _, err := db.PutStorageResource(context.Background(), tx, res); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if rec := add(); rec.Code != 403 {
		t.Fatalf("revoked row access replay %d %s", rec.Code, rec.Body.String())
	}
	var count int
	if err := w.database.QueryRow(`SELECT count(*) FROM site_storage_rows WHERE site_id=$1`, siteID).Scan(&count); err != nil || count != 1 {
		t.Fatal(fmt.Sprintf("row count %d: %v", count, err))
	}
}

func TestCreateReplayPhotos(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	body, kind := photoMultipart(t, []string{"front.jpg"}, [][]byte{photoJPEG(t, 8, 6)})
	upload := func(etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://"+accessBase+"/v1/sites/demo/photos", strings.NewReader(body))
		r.Header.Set("Content-Type", kind)
		r.Header.Set("X-API-Key", w.apiKeys["alice"])
		r.Header.Set("X-Simple-Host-Client", "api")
		r.Header.Set("Idempotency-Key", "photos")
		if etag != "" {
			r.Header.Set("If-Match", etag)
		}
		return w.do(r)
	}
	one, two := upload(""), upload("")
	if one.Code != 200 || two.Code != 200 || one.Body.String() != two.Body.String() {
		t.Fatalf("photos replay %d %d %s", one.Code, two.Code, two.Body.String())
	}
	if rec := upload(`"stale"`); rec.Code != 412 {
		t.Fatalf("photo precondition %d", rec.Code)
	}
	site, err := db.GetSite(context.Background(), w.database, w.users["alice"], "demo")
	if err != nil || site.ActiveVersion != 2 {
		t.Fatalf("photo versions %d: %v", site.ActiveVersion, err)
	}
}
