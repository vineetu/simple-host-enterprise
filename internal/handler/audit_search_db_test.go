package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	db "github.com/vsriram/simple-host/internal/db"
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
// change and who saved their site's data, never who only opened it.
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

	// Owner: their own changes named, and whoever saved their site's data
	// (an author of a change, as "written by" on saved-data versions); a
	// visitor who was refused is not named, and nothing else on that row
	// tells visitors apart.
	// A refusal in alice's namespace (the gate records refusals without an
	// owner today; this one carries it, so the rule is tested either way).
	vera, _ := db.GetUserByUsername(context.Background(), w.database, "vera")
	alice, _ := db.GetUserByUsername(context.Background(), w.database, "alice")
	audit.NewDBRecorder(w.database).Record(context.Background(), audit.Event{
		ActorID: vera.ID, Action: "access_denied", OwnerID: alice.ID, SiteID: w.siteID("alice", "demo"),
		IP: "192.0.2.7", UserAgent: "Mozilla/5.0 vera",
	})
	var sawWriter, sawDenied bool
	for _, e := range w.auditAsOwner("alice", "owner=alice").Events {
		switch e.Action {
		case "state_write":
			sawWriter = sawWriter || e.ActorName == "vera"
		case "access_denied":
			sawDenied = true
			if e.ActorName != "" || e.ActorID != "" || e.KeyID != "" || e.IP != "" || e.UserAgent != "" {
				t.Fatalf("owner sees who was refused: %+v", e)
			}
		case "site_create":
			if e.ActorName != "alice" || e.ActorID == "" {
				t.Fatalf("owner's own create names %q (%q)", e.ActorName, e.ActorID)
			}
		}
	}
	if !sawWriter || !sawDenied {
		t.Fatalf("owner's events: saved-data author named %v, refusal listed %v", sawWriter, sawDenied)
	}
	// A non-admin filters by a person only for themselves or a teammate.
	if got := w.auditAsOwner("alice", "owner=alice&actor=vera"); len(got.Events) != 0 {
		t.Fatalf("alice filtered her site's events by a visitor: %+v", got.Events)
	}
	if got := w.auditAsOwner("alice", "owner=alice&actor=alice"); len(got.Events) == 0 {
		t.Fatal("alice cannot filter by herself")
	}
	if got := w.auditAsOwner("olly", "owner=team-crew&actor=mo"); len(got.Events) == 0 {
		t.Fatal("olly cannot filter the team's events by a teammate")
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
	rec := w.admin(http.MethodGet, "/api/admin/export?kind=audit&format=csv&actor=vera&action=state_write")
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

// A site handed to another namespace is not named in its old owner's
// history: its current name belongs to the new owner (who may have renamed
// or deleted it since), and pairing it with the old owner would be false.
func TestAuditNamesSiteOnlyUnderItsCurrentOwner(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "alice", "mo")
	w.deploy("alice", "/api/sites/doc")
	id := w.siteID("alice", "doc")
	w.move("alice", "/api/collaboration/sites/alice/doc/transfer", map[string]any{"to": "crew"}, http.StatusOK)
	var old, current int
	for _, e := range w.auditAsOwner("alice", "owner=alice").Events {
		if e.SiteID == id {
			old++
			if e.SiteName != "" {
				t.Fatalf("alice's %s event names the site now owned by the team: %q", e.Action, e.SiteName)
			}
		}
	}
	for _, e := range w.auditAsOwner("mo", "owner=team-crew").Events {
		if e.SiteID == id {
			current++
			if e.SiteName != "doc" {
				t.Fatalf("the team's %s event names %q, want doc", e.Action, e.SiteName)
			}
		}
	}
	if old == 0 || current == 0 {
		t.Fatalf("events under alice %d, under the team %d", old, current)
	}
}
