package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	db "github.com/vsriram/simple-host/internal/db"
)

// siteRequest calls a site's own host by API key.
func (w *accessWorld) siteRequest(user, method, host, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://"+host+path, strings.NewReader(body))
	if user != "" {
		r.Header.Set("X-API-Key", w.apiKeys[user])
	}
	r.Header.Set("Content-Type", "application/json")
	return w.do(r)
}

func (w *accessWorld) move(user, path string, body map[string]any, want int) map[string]any {
	w.t.Helper()
	rec := w.api(user, http.MethodPost, path, body)
	if rec.Code != want {
		w.t.Fatalf("POST %s %v = %d %s, want %d", path, body, rec.Code, rec.Body, want)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func TestTransferSiteToTeamKeepsEverythingAndRedirects(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "alice", "mo")
	w.deploy("alice", "/api/sites/tracker")
	if rec := w.api("alice", http.MethodPost, "/api/collaboration/sites/alice/tracker/viewers", map[string]any{"usernames": []string{"vera"}}); rec.Code != http.StatusOK {
		t.Fatalf("grant viewer = %d %s", rec.Code, rec.Body)
	}
	w.setAccess("alice", "/api/sites/tracker/access", map[string]any{"level": "company"}, http.StatusOK)
	if rec := w.siteRequest("alice", http.MethodPut, "tracker.alice."+accessBase, "/api/site/state/versioned", `{"version":0,"state":{"n":1}}`); rec.Code != http.StatusOK {
		t.Fatalf("save state = %d %s", rec.Code, rec.Body)
	}
	var siteID string
	_ = w.database.QueryRow(`SELECT id::text FROM sites WHERE name = 'tracker'`).Scan(&siteID)

	out := w.move("alice", "/api/sites/tracker/transfer", map[string]any{"to": "crew"}, http.StatusOK)
	if out["owner"] != "team-crew" || out["url"] != "https://tracker.team-crew."+accessBase+"/" ||
		out["previous_url"] != "https://tracker.alice."+accessBase+"/" || out["previous_url_status"] != "redirects" {
		t.Fatalf("transfer answered %v", out)
	}

	// The same row, now the team's: access level, viewers, saved data and its
	// history all kept.
	var owner, access string
	_ = w.database.QueryRow(`SELECT u.username, s.access FROM sites s JOIN users u ON u.id = s.user_id WHERE s.id = $1`, siteID).Scan(&owner, &access)
	if owner != "team-crew" || access != db.AccessCompany {
		t.Errorf("site now %s at %s", owner, access)
	}
	if n := w.count(`SELECT count(*) FROM site_viewers WHERE site_id = $1`, siteID); n != 1 {
		t.Errorf("viewers after transfer = %d, want 1", n)
	}
	if n := w.count(`SELECT count(*) FROM site_state_history WHERE site_id = $1`, siteID); n != 1 {
		t.Errorf("saved-data history after transfer = %d, want 1", n)
	}
	rec := w.siteRequest("mo", http.MethodGet, "tracker.team-crew."+accessBase, "/api/site/state/versioned", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"n":1`) {
		t.Errorf("team member reads state = %d %s", rec.Code, rec.Body)
	}
	if code := w.view("mo", "team-crew", "tracker", false); code != http.StatusOK {
		t.Errorf("team member opens the moved site = %d", code)
	}

	// The old address redirects, path and query kept; a write is a 308.
	old := w.siteRequest("", http.MethodGet, "tracker.alice."+accessBase, "/page?x=1", "")
	if old.Code != http.StatusMovedPermanently || old.Header().Get("Location") != "https://tracker.team-crew."+accessBase+"/page?x=1" {
		t.Errorf("old address = %d %q", old.Code, old.Header().Get("Location"))
	}
	put := w.siteRequest("alice", http.MethodPut, "tracker.alice."+accessBase, "/api/sites/tracker/state/versioned", `{}`)
	if put.Code != http.StatusPermanentRedirect || put.Header().Get("Location") != "https://tracker.team-crew."+accessBase+"/api/site/state/versioned" {
		t.Errorf("old address state write = %d %q", put.Code, put.Header().Get("Location"))
	}
	// The pre-v1.3 owner path goes through the old site host.
	if code := w.view("olly", "alice", "tracker", true); code != http.StatusMovedPermanently {
		t.Errorf("old owner path = %d", code)
	}

	// Alice owns nothing now, but her label keeps its certificate so the old
	// address still answers.
	labels, err := db.OwnerLabelsWithSites(context.Background(), w.database)
	if err != nil || !containsString(labels, "alice") || !containsString(labels, "team-crew") {
		t.Errorf("owner labels = %v, %v", labels, err)
	}

	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_transfer' AND site_id = $1 AND detail->>'from' = 'alice' AND detail->>'to' = 'team-crew'`, siteID); n != 1 {
		t.Errorf("site_transfer audit rows = %d, want 1", n)
	}
	if n := w.count(`SELECT count(*) FROM site_search_queue WHERE site_id = $1`, siteID); n != 1 {
		t.Errorf("search queue rows = %d, want 1", n)
	}

	// A new site under the old name takes the address back.
	w.deploy("alice", "/api/sites/tracker")
	if code := w.view("alice", "alice", "tracker", false); code != http.StatusOK {
		t.Errorf("reused name = %d, want served", code)
	}
}

func TestTransferSiteRefusals(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "mo")
	w.deploy("alice", "/api/sites/board")
	w.deploy("vera", "/api/sites/board")

	for _, tc := range []struct {
		to   string
		want int
		code string
	}{
		{"crew", http.StatusNotFound, "destination_not_found"},   // alice is not in it
		{"nobody", http.StatusNotFound, "destination_not_found"}, // no such account
		{"alice", http.StatusBadRequest, "same_owner"},
		{"vera", http.StatusConflict, "name_conflict"},
	} {
		out := w.move("alice", "/api/sites/board/transfer", map[string]any{"to": tc.to}, tc.want)
		if out["code"] != tc.code {
			t.Errorf("to %s: %v", tc.to, out)
		}
	}
	w.disable("olly")
	if out := w.move("alice", "/api/sites/board/transfer", map[string]any{"to": "olly"}, http.StatusConflict); out["code"] != "destination_inactive" {
		t.Errorf("to a disabled person: %v", out)
	}
	// Somebody else's site is not found.
	w.move("vera", "/api/collaboration/sites/alice/board/transfer", map[string]any{"to": "vera"}, http.StatusNotFound)
	// To an enabled person works.
	if out := w.move("alice", "/api/sites/board/transfer", map[string]any{"to": "mo"}, http.StatusOK); out["owner"] != "mo" {
		t.Errorf("to a person: %v", out)
	}
}

func TestTransferRespectsDestinationQuota(t *testing.T) {
	w := newAccessWorldWith(t, UploadQuota{MaxSites: 1}, nil)
	w.deploy("alice", "/api/sites/one")
	w.deploy("mo", "/api/sites/two")
	if out := w.move("alice", "/api/sites/one/transfer", map[string]any{"to": "mo"}, http.StatusConflict); out["code"] != "site_limit" {
		t.Errorf("over quota: %v", out)
	}
	if n := w.count(`SELECT count(*) FROM sites s JOIN users u ON u.id = s.user_id WHERE u.username = 'alice'`); n != 1 {
		t.Error("a refused transfer moved the site")
	}
}

func TestLastMemberMovesSitesOutThenLeaves(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "mo")
	w.deploy("mo", "/api/collaboration/sites/team-crew/board")

	rec := w.api("mo", http.MethodPost, "/api/teams/crew/leave", nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "transfer_site") {
		t.Fatalf("leave as last member = %d %s, want the move offered", rec.Code, rec.Body)
	}
	w.move("mo", "/api/collaboration/sites/team-crew/board/transfer", map[string]any{"to": "mo"}, http.StatusOK)
	if rec := w.api("mo", http.MethodPost, "/api/teams/crew/leave?confirm_name=team-crew", nil); rec.Code != http.StatusOK {
		t.Fatalf("leave with no sites = %d %s", rec.Code, rec.Body)
	}
	if w.teamExists("team-crew") {
		t.Error("the team outlived its last member")
	}
	if code := w.view("mo", "mo", "board", false); code != http.StatusOK {
		t.Errorf("moved site = %d", code)
	}
	old := w.siteRequest("", http.MethodGet, "board.team-crew."+accessBase, "/", "")
	if old.Code != http.StatusMovedPermanently || old.Header().Get("Location") != "https://board.mo."+accessBase+"/" {
		t.Errorf("closed team's old address = %d %q", old.Code, old.Header().Get("Location"))
	}
}

func TestRenameSite(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/test2")
	w.deploy("alice", "/api/sites/taken")

	w.move("alice", "/api/sites/test2/rename", map[string]any{"name": "Bad Name"}, http.StatusBadRequest)
	if out := w.move("alice", "/api/sites/test2/rename", map[string]any{"name": "taken"}, http.StatusConflict); out["code"] != "name_conflict" {
		t.Errorf("rename onto a taken name: %v", out)
	}
	out := w.move("alice", "/api/collaboration/sites/alice/test2/rename", map[string]any{"name": "tracker"}, http.StatusOK)
	if out["name"] != "tracker" || out["url"] != "https://tracker.alice."+accessBase+"/" {
		t.Fatalf("rename answered %v", out)
	}
	if code := w.view("alice", "alice", "tracker", false); code != http.StatusOK {
		t.Errorf("renamed site = %d", code)
	}
	old := w.siteRequest("", http.MethodGet, "test2.alice."+accessBase, "/", "")
	if old.Code != http.StatusMovedPermanently || old.Header().Get("Location") != "https://tracker.alice."+accessBase+"/" {
		t.Errorf("old name = %d %q", old.Code, old.Header().Get("Location"))
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_rename' AND detail->>'from_name' = 'test2' AND detail->>'name' = 'tracker'`); n != 1 {
		t.Errorf("site_rename audit rows = %d", n)
	}
	// Renaming back to the old name clears its redirect: the live site wins.
	w.move("alice", "/api/sites/tracker/rename", map[string]any{"name": "test2"}, http.StatusOK)
	if n := w.count(`SELECT count(*) FROM site_redirects WHERE owner_label = 'alice' AND site_part = 'test2'`); n != 0 {
		t.Errorf("redirect rows for a live address = %d", n)
	}
	if code := w.view("alice", "alice", "test2", false); code != http.StatusOK {
		t.Errorf("renamed back = %d", code)
	}
}

func TestAdminMovesAndDeletesLeaverSites(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "mo")
	w.deploy("alice", "/api/sites/one")
	w.deploy("alice", "/api/sites/two")
	w.deploy("vera", "/api/sites/three")
	w.deploy("vera", "/api/sites/four")
	w.deploy("mo", "/api/sites/keep")

	// An enabled person's sites are theirs to hand over.
	if rec := w.adminPost("/api/admin/users/alice/transfer-sites", "to=crew"); rec.Code != http.StatusConflict {
		t.Fatalf("admin moves an active person's sites = %d %s", rec.Code, rec.Body)
	}
	w.disable("alice")
	// Not to a disabled person.
	w.disable("olly")
	if rec := w.adminPost("/api/admin/users/alice/transfer-sites", "to=olly"); rec.Code != http.StatusConflict {
		t.Errorf("admin moves to a disabled person = %d %s", rec.Code, rec.Body)
	}
	if rec := w.adminPost("/api/admin/users/alice/transfer-sites", "to=crew"); rec.Code != http.StatusOK {
		t.Fatalf("admin moves a leaver's sites = %d %s", rec.Code, rec.Body)
	}
	if ids := w.teamSiteIDs("team-crew"); len(ids) != 2 {
		t.Fatalf("team now has %d sites, want 2", len(ids))
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_transfer' AND actor_id = $1 AND detail->>'by_admin' = 'true'`, w.users["root"]); n != 2 {
		t.Errorf("admin site_transfer audit rows = %d, want 2", n)
	}
	// A non-admin is refused.
	if rec := w.adminAsPost("vera", "/api/admin/users/alice/delete-sites", ""); rec.Code != http.StatusForbidden {
		t.Errorf("non-admin = %d", rec.Code)
	}

	// Single site, then delete what is left.
	w.disable("vera")
	if rec := w.adminPost("/api/admin/sites/vera/three/transfer", "to=mo"); rec.Code != http.StatusOK {
		t.Fatalf("admin moves one site = %d %s", rec.Code, rec.Body)
	}
	if rec := w.adminPost("/api/admin/users/vera/delete-sites", ""); rec.Code != http.StatusOK {
		t.Fatalf("admin deletes a leaver's sites = %d %s", rec.Code, rec.Body)
	}
	// An admin's delete is the owner's: recoverable for 30 days.
	if n := w.count(`SELECT count(*) FROM sites s JOIN users u ON u.id = s.user_id WHERE u.username = 'vera' AND s.deleted_at IS NULL`); n != 0 {
		t.Errorf("vera still has %d live sites", n)
	}
	if n := w.count(`SELECT count(*) FROM sites s JOIN users u ON u.id = s.user_id WHERE u.username = 'vera' AND s.deleted_at IS NOT NULL`); n != 1 {
		t.Errorf("vera has %d recently deleted sites, want four", n)
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_delete' AND detail->>'by_admin' = 'true' AND detail ? 'restorable_until'`); n != 1 {
		t.Errorf("admin site_delete audit rows = %d, want 1", n)
	}
	if n := w.count(`SELECT count(*) FROM sites s JOIN users u ON u.id = s.user_id WHERE u.username = 'mo'`); n != 2 {
		t.Errorf("mo has %d sites, want keep and three", n)
	}
}

// adminPost submits an admin dashboard form as root.
func (w *accessWorld) adminPost(path, form string) *httptest.ResponseRecorder {
	return w.adminAsPost("root", path, form)
}

func (w *accessWorld) adminAsPost(user, path, form string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://"+accessBase+path, strings.NewReader(form))
	r.AddCookie(w.cookie(user, ""))
	r.Header.Set("Origin", "https://"+accessBase)
	r.Header.Set("X-Simple-Host-Client", "control-ui")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return w.do(r)
}
