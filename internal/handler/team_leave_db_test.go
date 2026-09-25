package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	db "github.com/vsriram/simple-host/internal/db"
)

// newTeam creates team name with creator and the other members, over the
// real database.
func (w *accessWorld) newTeam(name, creator string, others ...string) {
	w.t.Helper()
	if rec := w.api(creator, http.MethodPost, "/api/teams", map[string]string{"name": name}); rec.Code != http.StatusCreated {
		w.t.Fatalf("create team = %d %s", rec.Code, rec.Body)
	}
	if len(others) > 0 {
		if rec := w.api(creator, http.MethodPost, "/api/teams/"+name+"/members", map[string]any{"usernames": others}); rec.Code != http.StatusOK {
			w.t.Fatalf("add members = %d %s", rec.Code, rec.Body)
		}
	}
}

func (w *accessWorld) count(query string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.database.QueryRow(query, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *accessWorld) teamExists(name string) bool {
	return w.count(`SELECT count(*) FROM users WHERE username = $1 AND kind = 'team'`, name) == 1
}

func (w *accessWorld) disable(user string) {
	w.t.Helper()
	if err := db.SetUserDisabled(context.Background(), w.database, w.users[user], true); err != nil {
		w.t.Fatal(err)
	}
}

// assertTeamGone checks the team, its sites and their search rows are gone,
// every site's files are queued for retirement, and the audit log says so.
func (w *accessWorld) assertTeamGone(name string, siteIDs []string, reason string) {
	w.t.Helper()
	if w.teamExists(name) {
		w.t.Fatalf("team %s still exists", name)
	}
	for _, id := range siteIDs {
		if n := w.count(`SELECT count(*) FROM sites WHERE id = $1`, id); n != 0 {
			w.t.Errorf("site %s survived its team", id)
		}
		if n := w.count(`SELECT count(*) FROM storage_retired WHERE object_key = $1`, "sites/"+id+"/"); n != 1 {
			w.t.Errorf("site %s files queued %d times, want 1", id, n)
		}
		if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_delete' AND site_id = $1`, id); n != 1 {
			w.t.Errorf("site %s has %d site_delete audit rows, want 1", id, n)
		}
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'team_delete' AND detail->>'note' = $1 AND detail->>'reason' = $2`, name, reason); n != 1 {
		w.t.Errorf("team %s has %d team_delete audit rows for %s, want 1", name, n, reason)
	}
}

func (w *accessWorld) teamSiteIDs(name string) []string {
	rows, err := w.database.Query(`SELECT s.id::text FROM sites s JOIN users u ON u.id = s.user_id WHERE u.username = $1`, name)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

func TestLeaveTeamAsNonLastMember(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "mo", "alice")
	w.deploy("mo", "/api/collaboration/sites/crew/board")

	rec := w.api("alice", http.MethodPost, "/api/teams/crew/leave", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("leave = %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Members     []teamMemberResponse `json:"members"`
		TeamDeleted bool                 `json:"team_deleted"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.TeamDeleted || len(out.Members) != 1 || out.Members[0].Username != "mo" {
		t.Errorf("leave answered %s", rec.Body)
	}
	if !w.teamExists("crew") || len(w.teamSiteIDs("crew")) != 1 {
		t.Fatal("leaving as a non-last member touched the team or its sites")
	}
	// Alice is out: the team is now a 404 to her.
	if rec := w.api("alice", http.MethodGet, "/api/teams/crew/members", nil); rec.Code != http.StatusNotFound {
		t.Errorf("former member lists members = %d", rec.Code)
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'member_remove' AND actor_id = $1 AND detail->>'subject_id' = $1::text`, w.users["alice"]); n != 1 {
		t.Errorf("leave audit rows = %d, want 1", n)
	}
}

func TestLastMemberLeaveNeedsConfirmAndDeletesSites(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "mo")
	w.deploy("mo", "/api/collaboration/sites/crew/board")
	w.deploy("mo", "/api/collaboration/sites/crew/wiki")
	ids := w.teamSiteIDs("crew")

	for _, path := range []string{"/api/teams/crew/leave", "/api/teams/crew/leave?confirm_name=crow"} {
		rec := w.api("mo", http.MethodPost, path, nil)
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s = %d %s", path, rec.Code, rec.Body)
		}
		var out struct {
			Error     string `json:"error"`
			Code      string `json:"code"`
			SiteCount int    `json:"site_count"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if out.Code != "confirm_team_delete" || out.SiteCount != 2 || !strings.Contains(out.Error, "2 sites") {
			t.Errorf("%s answered %s", path, rec.Body)
		}
	}
	if !w.teamExists("crew") || len(w.teamSiteIDs("crew")) != 2 {
		t.Fatal("an unconfirmed leave deleted something")
	}

	rec := w.api("mo", http.MethodPost, "/api/teams/crew/leave?confirm_name=crew", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"team_deleted":true`) {
		t.Fatalf("confirmed leave = %d %s", rec.Code, rec.Body)
	}
	w.assertTeamGone("crew", ids, "last_member_left")
}

// Removing yourself through the members route is leaving; removing the last
// *other* member never deletes anything, because you are still in it.
func TestRemoveMemberPaths(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "mo", "alice")
	w.deploy("mo", "/api/collaboration/sites/crew/board")
	ids := w.teamSiteIDs("crew")

	if rec := w.api("mo", http.MethodDelete, "/api/teams/crew/members/alice", nil); rec.Code != http.StatusOK {
		t.Fatalf("remove other = %d %s", rec.Code, rec.Body)
	}
	if !w.teamExists("crew") || len(w.teamSiteIDs("crew")) != 1 {
		t.Fatal("removing the last other member deleted the team")
	}

	rec := w.api("mo", http.MethodDelete, "/api/teams/crew/members/mo", nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "confirm_team_delete") {
		t.Fatalf("remove self as last = %d %s", rec.Code, rec.Body)
	}
	if rec := w.api("mo", http.MethodDelete, "/api/teams/crew/members/mo?confirm_name=crew", nil); rec.Code != http.StatusOK {
		t.Fatalf("confirmed remove self = %d %s", rec.Code, rec.Body)
	}
	w.assertTeamGone("crew", ids, "last_member_left")
}

// A disabled account cannot sign in, so it does not keep a team alive.
func TestDisabledMembersDoNotCountAsLastMember(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "mo", "alice", "vera")
	w.deploy("mo", "/api/collaboration/sites/crew/board")
	ids := w.teamSiteIDs("crew")
	w.disable("alice")
	w.disable("vera")

	rec := w.api("mo", http.MethodPost, "/api/teams/crew/leave", nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"site_count":1`) {
		t.Fatalf("leave with only disabled members left = %d %s", rec.Code, rec.Body)
	}
	if rec := w.api("mo", http.MethodPost, "/api/teams/crew/leave?confirm_name=crew", nil); rec.Code != http.StatusOK {
		t.Fatalf("confirmed leave = %d %s", rec.Code, rec.Body)
	}
	w.assertTeamGone("crew", ids, "last_member_left")
}

func TestDeleteTeamWithSitesNeedsConfirm(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "mo", "alice")
	w.deploy("mo", "/api/collaboration/sites/crew/board")
	ids := w.teamSiteIDs("crew")

	if rec := w.api("alice", http.MethodDelete, "/api/teams/crew", nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"site_count":1`) {
		t.Fatalf("unconfirmed delete = %d %s", rec.Code, rec.Body)
	}
	if rec := w.api("alice", http.MethodDelete, "/api/teams/crew?confirm_name=crew", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("confirmed delete = %d %s", rec.Code, rec.Body)
	}
	w.assertTeamGone("crew", ids, "deleted")

	// A team with no sites still deletes without ceremony.
	w.newTeam("empty", "mo")
	if rec := w.api("mo", http.MethodDelete, "/api/teams/empty", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete empty team = %d %s", rec.Code, rec.Body)
	}
}

func TestAdminDeletesTeamOnlyWhenNoMemberIsActive(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "mo", "alice")
	w.deploy("mo", "/api/collaboration/sites/crew/board")
	ids := w.teamSiteIDs("crew")

	w.disable("mo")
	if rec := w.admin(http.MethodPost, "/api/admin/teams/crew/delete"); rec.Code != http.StatusConflict {
		t.Fatalf("admin delete with an active member = %d %s", rec.Code, rec.Body)
	}
	if !w.teamExists("crew") {
		t.Fatal("admin deleted a team that still had an active member")
	}
	w.disable("alice")
	if rec := w.admin(http.MethodPost, "/api/admin/teams/crew/delete"); rec.Code != http.StatusOK {
		t.Fatalf("admin delete = %d %s", rec.Code, rec.Body)
	}
	w.assertTeamGone("crew", ids, "admin_no_active_members")
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'team_delete' AND actor_id = $1`, w.users["root"]); n != 1 {
		t.Errorf("admin team_delete audit rows = %d, want 1", n)
	}

	// Not an admin: refused, and nothing happens.
	w.newTeam("other", "vera")
	r := w.api("vera", http.MethodPost, "/api/admin/teams/other/delete", nil)
	if r.Code < 400 || !w.teamExists("other") {
		t.Fatalf("non-admin delete = %d", r.Code)
	}
}

// A deploy whose site was deleted under it fails rather than answering success.
func TestUpdateSiteActiveVersionFailsClosedOnMissingSite(t *testing.T) {
	w := newAccessWorld(t)
	err := db.UpdateSiteActiveVersion(context.Background(), w.database, "00000000-0000-4000-8000-000000000001", 2)
	if err != sql.ErrNoRows {
		t.Fatalf("UpdateSiteActiveVersion on a missing site = %v, want sql.ErrNoRows", err)
	}
}
