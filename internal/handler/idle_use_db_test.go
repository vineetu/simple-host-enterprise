package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oplimits"
)

// What counts as use for the idle cleanup: the owner or a team member
// opening the site, a saved-data read, and a held (publish=false) deploy
// all do; a bot opening it does not. A site whose use was never recorded
// (one that existed before sites.last_used_at) is not idle on that alone.
func TestIdleCleanupCountsEveryUse(t *testing.T) {
	w := newAccessWorld(t)
	ctx := context.Background()
	for _, name := range []string{"mine", "bots", "data", "held", "legacy"} {
		w.deploy("alice", "/api/sites/"+name)
	}
	w.newTeam("crew", "mo", "olly")
	w.deploy("mo", "/api/collaboration/sites/team-crew/board")
	w.makeUnused("mine", "bots", "data", "held", "board")
	// As migration 0053 leaves an old site: created and changed long ago,
	// last_used_at the time the column was added.
	if _, err := w.database.Exec(`UPDATE sites SET created_at = now() - interval '400 days', updated_at = now() - interval '400 days' WHERE name = 'legacy'`); err != nil {
		t.Fatal(err)
	}

	if code := w.browse("alice", "mine.alice."+accessBase, "/"); code != http.StatusOK {
		t.Fatalf("owner visit = %d", code)
	}
	if code := w.browse("olly", "board.team-crew."+accessBase, "/"); code != http.StatusOK {
		t.Fatalf("team member visit = %d", code)
	}
	if code := w.view("alice", "alice", "bots", false); code != http.StatusOK { // no User-Agent: a bot
		t.Fatalf("bot visit = %d", code)
	}
	r := httptest.NewRequest(http.MethodGet, "https://data.alice."+accessBase+"/api/site/state/versioned", nil)
	r.AddCookie(w.cookie("alice", "data.alice."+accessBase))
	if rec := w.do(r); rec.Code != http.StatusOK {
		t.Fatalf("saved-data read = %d %s", rec.Code, rec.Body)
	}
	if rec := w.api("alice", http.MethodPut, "/api/sites/held?publish=false", zipOf(t, map[string][]byte{"index.html": []byte("draft")})); rec.Code != http.StatusOK {
		t.Fatalf("held deploy = %d %s", rec.Code, rec.Body)
	}
	for _, name := range []string{"mine", "board", "data", "held"} {
		w.waitUsed(name)
	}

	cleanup := NewIdleCleanup(w.database, audit.NewDBRecorder(w.database), 60, nil, "https://"+accessBase)
	if err := cleanup.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"mine": false, "board": false, "data": false, "held": false, "legacy": false, "bots": true} {
		if marked, _, _ := w.idleSince(name); marked != want {
			t.Errorf("%s marked = %v, want %v", name, marked, want)
		}
	}

	// A held deploy after the mark (and after the grace) saves the site too.
	if _, err := w.database.Exec(`UPDATE sites SET idle_since = now() - interval '31 days', idle_delete_at = now() - interval '1 day' WHERE name = 'bots'`); err != nil {
		t.Fatal(err)
	}
	if rec := w.api("alice", http.MethodPut, "/api/sites/bots?publish=false", zipOf(t, map[string][]byte{"index.html": []byte("draft")})); rec.Code != http.StatusOK {
		t.Fatalf("held deploy = %d %s", rec.Code, rec.Body)
	}
	if err := cleanup.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if marked, _, deleted := w.idleSince("bots"); marked || deleted {
		t.Fatalf("site deployed to during its grace: marked %v deleted %v", marked, deleted)
	}
}

// The cleanup's last check and its delete are one step against use: a use
// in flight (here a transaction holding the site row, as a visit or a
// saved-data write does) makes the cleanup wait, and once it commits the
// site is no longer due.
func TestIdleCleanupWaitsForUseInFlight(t *testing.T) {
	w := newAccessWorld(t)
	ctx := context.Background()
	w.deploy("alice", "/api/sites/due")
	w.makeUnused("due")
	if _, err := w.database.Exec(`UPDATE sites SET idle_since = now() - interval '31 days', idle_delete_at = now() - interval '1 day' WHERE name = 'due'`); err != nil {
		t.Fatal(err)
	}
	cleanup := NewIdleCleanup(w.database, audit.NewDBRecorder(w.database), 60, nil, "https://"+accessBase)
	due, err := db.DueIdleSites(ctx, w.database)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %v, %v", due, err)
	}

	use, err := w.database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := use.Exec(`UPDATE sites SET last_used_at = now() WHERE name = 'due'`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cleanup.moveToRecentlyDeleted(ctx, due[0]) }()
	if !waitFor(func() bool { return w.lockWaiters() >= 1 }) {
		_ = use.Rollback()
		t.Fatal("the cleanup did not wait for the use in flight")
	}
	if err := use.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, _, deleted := w.idleSince("due"); deleted {
		t.Fatal("a site used while the cleanup was deciding was deleted")
	}
}

// Keep waits for the site's lock and re-checks: a Keep that lands after the
// site was deleted (by the cleanup or anyone) answers 404 and changes
// nothing.
func TestKeepAfterDeleteRefused(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/gone")
	alice, _ := db.GetUserByUsername(context.Background(), w.database, "alice")
	release := w.holdSiteName(alice.ID, "gone")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- w.api("alice", http.MethodPost, "/api/sites/gone/keep", map[string]any{"keep": true}) }()
	if !waitFor(func() bool { return w.lockWaiters() >= 1 }) {
		release()
		t.Fatal("Keep did not wait for the site's lock")
	}
	if _, err := w.database.Exec(`UPDATE sites SET deleted_at = now() WHERE name = 'gone'`); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	if rec := <-done; rec.Code != http.StatusNotFound {
		t.Fatalf("Keep on a site deleted meanwhile = %d %s, want 404", rec.Code, rec.Body)
	}
	if _, kept, _ := w.idleSince("gone"); kept {
		t.Fatal("a deleted site was kept")
	}
}

// A site marked idle keeps the delete date it was given: a shorter
// IDLE_CLEANUP_GRACE_DAYS set after the mark does not bring it forward.
func TestIdleMarkKeepsPromisedDate(t *testing.T) {
	w := newAccessWorld(t)
	ctx := context.Background()
	w.deploy("alice", "/api/sites/marked")
	w.makeUnused("marked")
	candidates, err := db.IdleMarkCandidates(ctx, w.database, time.Hour, 0)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates = %+v, %v; want one", candidates, err)
	}
	marked, ok, err := db.MarkIdleSite(ctx, w.database, candidates[0].ID, time.Hour)
	if err != nil || !ok || marked.DeleteOn().Before(time.Now().Add(29*24*time.Hour)) {
		t.Fatalf("marked = %+v, %v, %v; want one site due in 30 days", marked, ok, err)
	}
	before := oplimits.Get()
	t.Cleanup(func() { oplimits.Set(before) })
	shorter := before
	shorter.IdleGraceDays = 1
	oplimits.Set(shorter)
	if _, err := w.database.Exec(`UPDATE sites SET idle_since = now() - interval '2 days' WHERE name = 'marked'`); err != nil {
		t.Fatal(err)
	}
	if due, err := db.DueIdleSites(ctx, w.database); err != nil || len(due) != 0 {
		t.Fatalf("due = %v, %v; want none before the date given", due, err)
	}
	if _, err := w.database.Exec(`UPDATE sites SET idle_delete_at = NULL WHERE name = 'marked'`); err != nil {
		t.Fatal(err)
	}
	if due, err := db.DueIdleSites(ctx, w.database); err != nil || len(due) != 1 {
		t.Fatalf("due = %v, %v; a mark without a stored date follows the setting", due, err)
	}
}
