package handler

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

type namedEvents struct {
	Events []auditEventResponse `json:"events"`
}

func (w *accessWorld) auditAsAdmin(query string) namedEvents {
	w.t.Helper()
	rec := w.admin(http.MethodGet, "/api/audit?"+query)
	if rec.Code != http.StatusOK {
		w.t.Fatalf("admin /api/audit?%s = %d %s", query, rec.Code, rec.Body)
	}
	var out namedEvents
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func (w *accessWorld) auditAsOwner(user, query string) namedEvents {
	w.t.Helper()
	rec := w.api(user, http.MethodGet, "/api/audit?"+query, nil)
	if rec.Code != http.StatusOK {
		w.t.Fatalf("%s /api/audit?%s = %d %s", user, query, rec.Code, rec.Body)
	}
	var out namedEvents
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// Admins search the audit log by owner, site, person, action and date, and
// see names; an owner sees who among themselves and their team made a
// change, never which visitor wrote their site's saved data.
func TestAuditSearchAndNames(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	w.setAccess("alice", "/api/collaboration/sites/alice/demo/access", map[string]any{"level": "company"}, http.StatusOK)
	if rec := w.siteRequest("vera", http.MethodPut, "demo.alice."+accessBase, "/api/sites/demo/state", `{"n":1}`); rec.Code != http.StatusOK {
		t.Fatalf("vera writes alice's state = %d %s", rec.Code, rec.Body)
	}
	w.newTeam("crew", "mo", "olly")
	w.deploy("mo", "/api/collaboration/sites/team-crew/board")

	// Admin: by owner and site, with names.
	got := w.auditAsAdmin("owner=alice&site=demo")
	if len(got.Events) == 0 {
		t.Fatal("no events for alice/demo")
	}
	var sawVera bool
	for _, e := range got.Events {
		if e.OwnerName != "alice" || e.SiteName != "demo" {
			t.Fatalf("event %s names owner %q site %q", e.Action, e.OwnerName, e.SiteName)
		}
		if e.Action == "state_write" && e.ActorName == "vera" {
			sawVera = true
		}
	}
	if !sawVera {
		t.Fatalf("admin does not see vera's state_write by name: %+v", got.Events)
	}
	// By person and action.
	got = w.auditAsAdmin("actor=vera&action=state_write")
	if len(got.Events) != 1 || got.Events[0].ActorName != "vera" {
		t.Fatalf("actor=vera&action=state_write = %+v", got.Events)
	}
	// By date: today includes them, a past day does not.
	today := time.Now().UTC().Format("2006-01-02")
	if got := w.auditAsAdmin("actor=vera&from=" + today + "&to=" + today); len(got.Events) != 1 {
		t.Fatalf("today's events = %d, want 1", len(got.Events))
	}
	if got := w.auditAsAdmin("actor=vera&to=2020-01-01"); len(got.Events) != 0 {
		t.Fatalf("events before 2020 = %d, want 0", len(got.Events))
	}
	if got := w.auditAsAdmin("owner=nobody-by-that-name"); len(got.Events) != 0 {
		t.Fatal("an unknown owner matched events")
	}

	// Owner: their own changes named, a visitor's write not.
	for _, e := range w.auditAsOwner("alice", "owner=alice&site=demo").Events {
		switch e.Action {
		case "state_write":
			if e.ActorName != "" {
				t.Fatalf("owner sees the visitor who wrote saved data: %q", e.ActorName)
			}
		case "site_create":
			if e.ActorName != "alice" {
				t.Fatalf("owner's own create names %q", e.ActorName)
			}
		}
	}
	// Team member: sees another member's change by name.
	var named bool
	for _, e := range w.auditAsOwner("olly", "owner=team-crew&site=board").Events {
		if e.Action == "site_create" && e.ActorName == "mo" {
			named = true
		}
	}
	if !named {
		t.Fatal("a team member does not see which member created the team's site")
	}

	// Export: the same filters, with names.
	rec := w.admin(http.MethodGet, "/api/admin/export?kind=audit&format=csv&actor=vera")
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered export = %d %s", rec.Code, rec.Body)
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "actor_name,owner_name,site_name") || !strings.HasSuffix(lines[1], "vera,alice,demo") {
		t.Fatalf("filtered export = %q", lines)
	}
	if rec := w.admin(http.MethodGet, "/api/admin/export?kind=audit&format=csv&actor=nobody-by-that-name"); rec.Code != http.StatusOK || strings.Count(strings.TrimSpace(rec.Body.String()), "\n") != 0 {
		t.Fatalf("export matching nobody = %d %q", rec.Code, rec.Body)
	}
}
