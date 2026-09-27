package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oplimits"
	"github.com/vsriram/simple-host/internal/storage"
)

// Deleting a site stops it serving and frees nothing but its place in the
// lists; restoring it brings back its files, saved data, access level and
// viewers, and it serves again.
func TestDeleteThenRestoreBringsTheSiteBackWhole(t *testing.T) {
	w := newAccessWorld(t)
	ctx := context.Background()
	w.deploy("alice", "/api/sites/demo")
	id := w.siteID("alice", "demo")
	if err := db.UpdateSiteState(ctx, w.database, "alice", "demo", json.RawMessage(`{"n":7}`)); err != nil {
		t.Fatal(err)
	}
	w.setAccess("alice", "/api/collaboration/sites/alice/demo/access", map[string]any{"level": "specific"}, http.StatusOK)
	if rec := w.api("alice", http.MethodPost, "/api/collaboration/sites/alice/demo/viewers", map[string]any{"usernames": []string{"vera"}}); rec.Code != http.StatusOK {
		t.Fatalf("grant viewer = %d %s", rec.Code, rec.Body)
	}
	if code := w.view("vera", "alice", "demo", false); code != http.StatusOK {
		t.Fatalf("viewer before delete = %d", code)
	}

	if rec := w.api("alice", http.MethodDelete, "/api/sites/demo", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	if code := w.view("alice", "alice", "demo", false); code == http.StatusOK {
		t.Fatal("a deleted site still serves to its owner")
	}
	if code := w.view("vera", "alice", "demo", false); code == http.StatusOK {
		t.Fatal("a deleted site still serves to its viewer")
	}
	if n := w.count(`SELECT count(*) FROM storage_retired WHERE object_key = $1`, "sites/"+id+"/"); n != 0 {
		t.Fatalf("deleted site's objects queued for the sweep (%d) inside the recovery window", n)
	}
	var listed []map[string]any
	_ = json.Unmarshal(w.api("alice", http.MethodGet, "/api/collaboration/sites", nil).Body.Bytes(), &listed)
	if len(listed) != 0 {
		t.Fatalf("deleted site still listed: %v", listed)
	}
	// The name stays held.
	if rec := w.api("alice", http.MethodPost, "/api/sites/demo", zipOf(t, map[string][]byte{"index.html": []byte("<p>new</p>")})); rec.Code != http.StatusConflict || !json.Valid(rec.Body.Bytes()) || !containsCode(rec.Body.Bytes(), "name_held") {
		t.Fatalf("create over a held name = %d %s, want 409 name_held", rec.Code, rec.Body)
	}
	var deleted []map[string]any
	rec := w.api("alice", http.MethodGet, "/api/deleted-sites", nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &deleted)
	if len(deleted) != 1 || deleted[0]["site"] != "demo" || deleted[0]["owner"] != "alice" || deleted[0]["deleted_by"] != "alice" {
		t.Fatalf("deleted sites = %d %s", rec.Code, rec.Body)
	}
	// Somebody else can neither see nor restore it.
	var others []map[string]any
	_ = json.Unmarshal(w.api("vera", http.MethodGet, "/api/deleted-sites", nil).Body.Bytes(), &others)
	if len(others) != 0 {
		t.Fatalf("another person sees alice's deleted sites: %v", others)
	}
	if rec := w.api("vera", http.MethodPost, "/api/collaboration/sites/alice/demo/restore", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger restore = %d %s, want 404", rec.Code, rec.Body)
	}

	rec = w.api("alice", http.MethodPost, "/api/sites/demo/restore", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", rec.Code, rec.Body)
	}
	var restored restoredSiteResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &restored)
	if restored.Access != db.AccessSpecific || restored.ActiveVersion != 1 || restored.URL == "" {
		t.Fatalf("restored = %+v", restored)
	}
	if w.siteID("alice", "demo") != id {
		t.Fatal("restore made a new site rather than bringing the old one back")
	}
	if code := w.view("vera", "alice", "demo", false); code != http.StatusOK {
		t.Fatalf("viewer after restore = %d", code)
	}
	state, _, err := db.GetSiteState(ctx, w.database, "alice", "demo")
	if err != nil || string(state) != `{"n": 7}` && string(state) != `{"n":7}` {
		t.Fatalf("state after restore = %s, %v", state, err)
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_restore' AND site_id = $1`, id); n != 1 {
		t.Fatalf("site_restore audit rows = %d, want 1", n)
	}
	if rec := w.api("alice", http.MethodPost, "/api/sites/demo/restore", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("restoring a live site = %d, want 404", rec.Code)
	}
}

// A team member restores a team site; an admin restores anyone's from
// /admin; a non-admin cannot use the admin route.
func TestRestoreByTeamMemberAndAdmin(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "mo", "olly")
	w.deploy("mo", "/api/collaboration/sites/team-crew/board")
	if rec := w.api("mo", http.MethodDelete, "/api/collaboration/sites/team-crew/board", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete team site = %d %s", rec.Code, rec.Body)
	}
	var listed []map[string]any
	_ = json.Unmarshal(w.api("olly", http.MethodGet, "/api/deleted-sites", nil).Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0]["owner"] != "team-crew" {
		t.Fatalf("member's deleted list = %v", listed)
	}
	if rec := w.api("olly", http.MethodPost, "/api/collaboration/sites/team-crew/board/restore", nil); rec.Code != http.StatusOK {
		t.Fatalf("member restore = %d %s", rec.Code, rec.Body)
	}

	w.deploy("alice", "/api/sites/demo")
	if rec := w.api("alice", http.MethodDelete, "/api/sites/demo", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := w.adminAs("vera", http.MethodPost, "/api/admin/deleted-sites/alice/demo/restore"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin restore = %d %s, want 403", rec.Code, rec.Body)
	}
	if rec := w.admin(http.MethodGet, "/api/admin/deleted-sites"); rec.Code != http.StatusOK || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("admin list = %d %s", rec.Code, rec.Body)
	}
	if rec := w.admin(http.MethodPost, "/api/admin/deleted-sites/alice/demo/restore"); rec.Code != http.StatusOK {
		t.Fatalf("admin restore = %d %s", rec.Code, rec.Body)
	}
	if code := w.view("alice", "alice", "demo", false); code != http.StatusOK {
		t.Fatalf("after admin restore = %d", code)
	}
}

// A restore counts toward the owner's quota again and is refused whole when
// it does not fit.
func TestRestoreRespectsSiteLimit(t *testing.T) {
	w := newAccessWorldWith(t, UploadQuota{MaxSites: 1}, nil)
	w.deploy("alice", "/api/sites/demo")
	if rec := w.api("alice", http.MethodDelete, "/api/sites/demo", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	// A deleted site keeps counting (TestDeletedSitesCountTowardQuota), so
	// only an owner already over the limit (a site made before the deleted
	// ones counted, or a lowered limit) is refused a restore.
	if _, err := db.CreateSite(context.Background(), w.database, w.users["alice"], "other"); err != nil {
		t.Fatal(err)
	}
	rec := w.api("alice", http.MethodPost, "/api/sites/demo/restore", nil)
	if rec.Code != http.StatusConflict || !containsCode(rec.Body.Bytes(), "site_limit") {
		t.Fatalf("restore over the limit = %d %s, want 409 site_limit", rec.Code, rec.Body)
	}
	var listed []map[string]any
	_ = json.Unmarshal(w.api("alice", http.MethodGet, "/api/deleted-sites", nil).Body.Bytes(), &listed)
	if len(listed) != 1 {
		t.Fatalf("refused restore left deleted list = %v", listed)
	}
}

// After the window the sweeper purges the row and queues the objects; the
// name is free again and the site can no longer be restored.
func TestPurgeAfterRetentionWindow(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	id := w.siteID("alice", "demo")
	if rec := w.api("alice", http.MethodDelete, "/api/sites/demo", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	store := w.store
	manifestKey, _ := storage.ManifestKey(id)
	if n, err := store.PurgeDeletedSites(context.Background(), w.database); err != nil || n != 0 {
		t.Fatalf("purge inside the window = %d, %v", n, err)
	}
	if _, err := w.database.Exec(`UPDATE sites SET deleted_at = now() - interval '31 days', purge_at = now() - interval '1 day' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if n, err := store.PurgeDeletedSites(context.Background(), w.database); err != nil || n != 1 {
		t.Fatalf("purge after the window = %d, %v", n, err)
	}
	if n := w.count(`SELECT count(*) FROM sites WHERE id = $1`, id); n != 0 {
		t.Fatal("purged site's row remains")
	}
	// Its manifest goes at once, so a rebuild never brings it back.
	if _, err := store.Objects().Get(context.Background(), manifestKey, 1<<20); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("purged site's manifest: %v", err)
	}
	if n := w.count(`SELECT count(*) FROM storage_retired WHERE object_key = $1`, "sites/"+id+"/"); n != 1 {
		t.Fatalf("purged site's objects queued %d times, want 1", n)
	}
	if rec := w.api("alice", http.MethodPost, "/api/sites/demo/restore", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("restore after purge = %d, want 404", rec.Code)
	}
	w.deploy("alice", "/api/sites/demo") // the name is free again
}

func containsCode(body []byte, code string) bool {
	var out struct {
		Code string `json:"code"`
	}
	return json.Unmarshal(body, &out) == nil && out.Code == code
}

// A deleted site keeps the purge date it was given when it was deleted: a
// shorter DELETED_RETENTION_DAYS set later applies to new deletions only.
// A row deleted before the date was stored falls back to the setting.
func TestPurgeKeepsPromisedDate(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/kept")
	w.deploy("alice", "/api/sites/legacy")
	kept, legacy := w.siteID("alice", "kept"), w.siteID("alice", "legacy")
	for _, name := range []string{"kept", "legacy"} {
		if rec := w.api("alice", http.MethodDelete, "/api/sites/"+name, nil); rec.Code != http.StatusNoContent {
			t.Fatalf("delete %s = %d", name, rec.Code)
		}
	}
	before := oplimits.Get()
	t.Cleanup(func() { oplimits.Set(before) })
	shorter := before
	shorter.DeletedRetentionDays = 1
	oplimits.Set(shorter)
	if _, err := w.database.Exec(`UPDATE sites SET deleted_at = now() - interval '2 days' WHERE id IN ($1, $2)`, kept, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := w.database.Exec(`UPDATE sites SET purge_at = NULL WHERE id = $1`, legacy); err != nil {
		t.Fatal(err)
	}
	store, err := storage.New(storage.Options{Objects: storage.NewMemoryObjects(), Index: storage.NewDBIndex(w.database), CacheDir: t.TempDir(), CacheMaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if n, err := store.PurgeDeletedSites(context.Background(), w.database); err != nil || n != 1 {
		t.Fatalf("purge = %d, %v; want only the site without a stored date", n, err)
	}
	if n := w.count(`SELECT count(*) FROM sites WHERE id = $1`, kept); n != 1 {
		t.Fatal("a site was purged before the date it was given")
	}
	var listed []struct {
		Name    string    `json:"site"`
		PurgeAt time.Time `json:"restorable_until"`
	}
	_ = json.Unmarshal(w.api("alice", http.MethodGet, "/api/deleted-sites", nil).Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0].Name != "kept" || listed[0].PurgeAt.Before(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("deleted list = %+v, want kept with its 30-day date", listed)
	}
	if rec := w.api("alice", http.MethodPost, "/api/sites/kept/restore", nil); rec.Code != http.StatusOK {
		t.Fatalf("restore inside the promised window = %d %s", rec.Code, rec.Body)
	}
}
