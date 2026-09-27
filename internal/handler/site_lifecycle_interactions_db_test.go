package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	db "github.com/vsriram/simple-host/internal/db"
)

// A recently deleted site cannot be handed over or renamed, and the name it
// holds cannot be taken by a hand-over or a rename either.
func TestDeletedSitesAndMoves(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "alice")
	w.deploy("alice", "/api/sites/gone")
	w.deploy("alice", "/api/sites/live")
	w.deploy("vera", "/api/sites/gone")
	if rec := w.api("alice", http.MethodDelete, "/api/sites/gone", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}

	w.move("alice", "/api/sites/gone/transfer", map[string]any{"to": "crew"}, http.StatusNotFound)
	w.move("alice", "/api/sites/gone/rename", map[string]any{"name": "other"}, http.StatusNotFound)

	if out := w.move("alice", "/api/sites/live/rename", map[string]any{"name": "gone"}, http.StatusConflict); out["code"] != "name_held" {
		t.Errorf("rename onto a held name: %v", out)
	}
	// Only an admin's leaver flow hands a site to a person.
	w.disable("vera")
	if rec := w.adminPost("/api/admin/sites/vera/gone/transfer", "to=alice"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "recently deleted") {
		t.Errorf("hand-over onto a held name = %d %s", rec.Code, rec.Body)
	}

	// Restored, it is whole and movable again.
	if rec := w.api("alice", http.MethodPost, "/api/sites/gone/restore", nil); rec.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", rec.Code, rec.Body)
	}
	if out := w.move("alice", "/api/sites/gone/transfer", map[string]any{"to": "crew"}, http.StatusOK); out["owner"] != "team-crew" {
		t.Errorf("transfer after restore: %v", out)
	}
}

// A restore whose address a live site of the owner now holds is refused
// clearly and restores nothing. Held names keep the API from ever getting
// here, so the live site is a legacy name ("Board") whose address is the
// deleted site's.
func TestRestoreRefusedWhenNameTaken(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/board")
	id := w.siteID("alice", "board")
	if rec := w.api("alice", http.MethodDelete, "/api/sites/board", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	w.deploy("alice", "/api/sites/legacy")
	if _, err := w.database.Exec(`UPDATE sites SET name = 'Board' WHERE name = 'legacy'`); err != nil {
		t.Fatal(err)
	}
	rec := w.api("alice", http.MethodPost, "/api/sites/board/restore", nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"name_taken"`) {
		t.Fatalf("restore over a taken address = %d %s, want 409 name_taken", rec.Code, rec.Body)
	}
	if n := w.count(`SELECT count(*) FROM sites WHERE id = $1 AND deleted_at IS NOT NULL`, id); n != 1 {
		t.Fatal("a refused restore undeleted the site")
	}
}

// A redirect whose site is recently deleted answers the ordinary not-found,
// the same as an address that never existed; restored, it redirects again.
func TestRedirectToDeletedSiteIsNotFound(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/tracker")
	w.move("alice", "/api/sites/tracker/rename", map[string]any{"name": "tracker-two"}, http.StatusOK)
	old := w.siteRequest("alice", http.MethodGet, "tracker.alice."+accessBase, "/", "")
	if old.Code != http.StatusMovedPermanently {
		t.Fatalf("old address before delete = %d", old.Code)
	}
	never := w.siteRequest("alice", http.MethodGet, "never-was.alice."+accessBase, "/", "")

	if rec := w.api("alice", http.MethodDelete, "/api/sites/tracker-two", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	gone := w.siteRequest("alice", http.MethodGet, "tracker.alice."+accessBase, "/", "")
	if gone.Code != never.Code || gone.Header().Get("Location") != "" || strings.Contains(gone.Body.String(), "tracker-two") {
		t.Fatalf("old address of a deleted site = %d %q, want %d like an unknown address", gone.Code, gone.Header().Get("Location"), never.Code)
	}

	if rec := w.api("alice", http.MethodPost, "/api/sites/tracker-two/restore", nil); rec.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", rec.Code, rec.Body)
	}
	back := w.siteRequest("alice", http.MethodGet, "tracker.alice."+accessBase, "/", "")
	if back.Code != http.StatusMovedPermanently || back.Header().Get("Location") != "https://tracker-two.alice."+accessBase+"/" {
		t.Fatalf("old address after restore = %d %q", back.Code, back.Header().Get("Location"))
	}
}

// While an admin's restriction stands, the owner (or a team member) can
// neither hand the site over nor rename it: either would change who can open
// it without an admin. Lifted, both work again.
func TestRestrictionBlocksTransferAndRename(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "alice", "mo")
	w.deploy("alice", "/api/sites/page")
	w.setAccess("alice", "/api/sites/page/access", map[string]any{"level": "company"}, http.StatusOK)
	if rec := w.adminForm("/api/admin/sites/alice/page/restrict", url.Values{"reason": {"leaks a roster"}}); rec.Code != http.StatusOK {
		t.Fatalf("restrict = %d %s", rec.Code, rec.Body)
	}
	for _, tc := range []struct{ path, key, value string }{
		{"/api/sites/page/transfer", "to", "crew"},
		{"/api/sites/page/rename", "name", "page-two"},
	} {
		out := w.move("alice", tc.path, map[string]any{tc.key: tc.value}, http.StatusConflict)
		if out["code"] != "site_restricted_by_admin" || out["reason"] != "leaks a roster" {
			t.Errorf("%s while restricted: %v", tc.path, out)
		}
	}
	if got := w.siteAccess("alice", "page"); got != db.AccessOnlyMe {
		t.Fatalf("a refused move changed the site: %s", got)
	}
	if rec := w.adminForm("/api/admin/sites/alice/page/unrestrict", url.Values{}); rec.Code != http.StatusOK {
		t.Fatalf("unrestrict = %d %s", rec.Code, rec.Body)
	}
	if out := w.move("alice", "/api/sites/page/transfer", map[string]any{"to": "crew"}, http.StatusOK); out["owner"] != "team-crew" {
		t.Fatalf("transfer after lift: %v", out)
	}
	if got := w.siteAccess("team-crew", "page"); got != db.AccessCompany {
		t.Fatalf("access after lift and move = %s, want company", got)
	}
}
