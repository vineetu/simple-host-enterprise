package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/db"
)

func ownerIndexTestSites() []db.Site {
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	return []db.Site{
		{ID: "s-shared", Name: "gantt", ActiveVersion: 3, Public: true, UpdatedAt: base},
		{ID: "s-private", Name: "draft-notes", ActiveVersion: 1, Public: false, UpdatedAt: base.Add(time.Hour)},
		{ID: "s-restricted", Name: "payroll", ActiveVersion: 2, Public: true, UpdatedAt: base.Add(2 * time.Hour)},
		{ID: "s-unpublished", Name: "empty", ActiveVersion: 0, Public: true, UpdatedAt: base.Add(3 * time.Hour)},
	}
}

var ownerIndexTestRestricted = map[string]bool{"s-restricted": true}

func entryNames(entries []ownerIndexEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.name)
	}
	return names
}

// Somebody who is not the owner sees only what the owner shared, and never a
// restricted site — its existence belongs to its own host's viewer list.
func TestOwnerIndexHidesPrivateAndRestrictedFromOthers(t *testing.T) {
	entries := ownerIndexVisible("alice", ownerIndexTestSites(), ownerIndexTestRestricted, false, testHostModel(t))
	got := entryNames(entries)
	if len(got) != 1 || got[0] != "gantt" {
		t.Fatalf("visible to a non-owner = %v, want [gantt] only", got)
	}
}

// The owner sees everything they have published, tagged by who can reach it.
func TestOwnerIndexShowsOwnerEverythingPublished(t *testing.T) {
	entries := ownerIndexVisible("alice", ownerIndexTestSites(), ownerIndexTestRestricted, true, testHostModel(t))
	got := entryNames(entries)
	// Newest first: payroll, draft-notes, gantt. "empty" has no active
	// version and is listed for nobody.
	want := []string{"payroll", "draft-notes", "gantt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("visible to the owner = %v, want %v", got, want)
	}
	for _, e := range entries {
		if e.name == "payroll" && !e.restricted {
			t.Fatalf("payroll should be marked restricted")
		}
	}
}

// A site with nothing published has no address to link to, for anyone.
func TestOwnerIndexSkipsUnpublishedForOwner(t *testing.T) {
	entries := ownerIndexVisible("alice", ownerIndexTestSites(), ownerIndexTestRestricted, true, testHostModel(t))
	for _, e := range entries {
		if e.name == "empty" {
			t.Fatalf("a site with no active version must not be listed")
		}
	}
}

// A restricted site is linked at its own host, not the owner's short path,
// which no longer serves it.
func TestOwnerIndexLinksRestrictedSiteToItsOwnHost(t *testing.T) {
	hosts := testHostModel(t)
	entries := ownerIndexVisible("alice", ownerIndexTestSites(), ownerIndexTestRestricted, true, hosts)
	for _, e := range entries {
		if e.name != "payroll" {
			continue
		}
		if want := hosts.SiteURL("alice", "payroll", true); e.url != want {
			t.Fatalf("restricted site url = %q, want %q", e.url, want)
		}
		return
	}
	t.Fatalf("payroll not listed")
}

func ownerIndexGate(t *testing.T, ownerUserID string) http.Handler {
	t.Helper()
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "gantt", "gantt-index")
	gate := testHostGate(t, store, testHostModel(t))
	gate.ownerIndex = func(_ context.Context, owner string) (string, []db.Site, map[string]bool, error) {
		return ownerUserID, ownerIndexTestSites(), ownerIndexTestRestricted, nil
	}
	return gate.wrap(newHostGateTestMux())
}

// The root of an owner host is that person's index, not a 404.
func TestOwnerHostRootRendersIndex(t *testing.T) {
	handler := ownerIndexGate(t, "user-1")
	response := gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/")
	if response.Code != http.StatusOK {
		t.Fatalf("owner host root: status = %d, want 200", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, "gantt") {
		t.Fatalf("index does not list the owner's site: %s", body)
	}
}

// The index is behind the same host session every other byte of that host is.
func TestOwnerHostRootRequiresSession(t *testing.T) {
	handler := ownerIndexGate(t, "user-1")
	response := gateRequest(handler, http.MethodGet, "alice.foo.example", "/")
	if response.Code == http.StatusOK {
		t.Fatalf("owner host root served without a session: status = %d", response.Code)
	}
}

// Somebody else's index must not name a site they were never shown. The
// signed-in caller here is "user-1"; the owner is somebody else.
func TestOwnerHostRootDoesNotLeakPrivateNames(t *testing.T) {
	handler := ownerIndexGate(t, "user-2")
	response := gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	body := response.Body.String()
	for _, hidden := range []string{"draft-notes", "payroll", "Private", "Named viewers"} {
		if strings.Contains(body, hidden) {
			t.Fatalf("index leaked %q to a non-owner: %s", hidden, body)
		}
	}
	if !strings.Contains(body, "gantt") {
		t.Fatalf("index should still show the shared site: %s", body)
	}
}

// A write to an owner host's root is refused before anything is resolved.
func TestOwnerHostRootRejectsNonGET(t *testing.T) {
	handler := ownerIndexGate(t, "user-1")
	response := gateAuthedRequest(t, handler, http.MethodPost, "alice.foo.example", "/")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST to owner host root: status = %d, want 405", response.Code)
	}
}

// An unauthenticated caller must not learn which usernames exist: a label
// with no account behind it answers exactly as one with an account does.
func TestOwnerHostRootDoesNotEnumerateUsersWithoutSession(t *testing.T) {
	handler := ownerIndexGate(t, "user-1")
	known := gateRequest(handler, http.MethodGet, "alice.foo.example", "/")
	unknown := gateRequest(handler, http.MethodGet, "nobody.foo.example", "/")
	if known.Code != unknown.Code {
		t.Fatalf("unauthenticated: known user = %d, unknown user = %d; the two must match", known.Code, unknown.Code)
	}
	if known.Body.String() != unknown.Body.String() {
		t.Fatalf("unauthenticated bodies differ between a known and an unknown user")
	}
}

// Every response past the session check is written to the access log, the
// way every other hosted-content response is (design.md 8.2). A page that
// discloses one person's site list to a named caller is exactly what the
// audit trail exists to record.
func TestOwnerHostRootIsAccessLogged(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "gantt", "gantt-index")
	gate := testHostGate(t, store, testHostModel(t))
	gate.ownerIndex = func(_ context.Context, owner string) (string, []db.Site, map[string]bool, error) {
		return "user-1", ownerIndexTestSites(), ownerIndexTestRestricted, nil
	}
	var logged []audit.AccessEvent
	gate.recordAccess = func(e audit.AccessEvent) { logged = append(logged, e) }
	handler := gate.wrap(newHostGateTestMux())

	response := gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if len(logged) != 1 {
		t.Fatalf("access events = %d, want exactly 1", len(logged))
	}
	event := logged[0]
	if event.OwnerLabel != "alice" || event.Path != "/" || event.Method != http.MethodGet {
		t.Fatalf("event = %+v, want owner alice, path /, method GET", event)
	}
	if event.UserID != "user-1" || event.SessionID != "session-1" {
		t.Fatalf("event did not attribute the caller: %+v", event)
	}
	if event.Status != http.StatusOK || event.Bytes != int64(response.Body.Len()) {
		t.Fatalf("event status/bytes = %d/%d, want 200/%d", event.Status, event.Bytes, response.Body.Len())
	}
	if event.SiteName != "" {
		t.Fatalf("SiteName = %q, want empty: the index is the host, not a site", event.SiteName)
	}
}

// A refusal is logged too, not just a successful render.
func TestOwnerHostRootLogsRefusal(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "gantt", "gantt-index")
	gate := testHostGate(t, store, testHostModel(t))
	gate.ownerIndex = func(_ context.Context, owner string) (string, []db.Site, map[string]bool, error) {
		return "user-1", ownerIndexTestSites(), ownerIndexTestRestricted, nil
	}
	var logged []audit.AccessEvent
	gate.recordAccess = func(e audit.AccessEvent) { logged = append(logged, e) }
	handler := gate.wrap(newHostGateTestMux())

	response := gateAuthedRequest(t, handler, http.MethodGet, "nobody.foo.example", "/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if len(logged) != 1 || logged[0].Status != http.StatusNotFound {
		t.Fatalf("a refused index visit must still be logged, got %+v", logged)
	}
}

// A person's name is credited wherever their work is listed, and the credit
// links to their own page — otherwise the index is reachable only by someone
// who already knows the URL.
func TestOwnerPageURLIsTheRootOfTheirHost(t *testing.T) {
	hosts := testHostModel(t)
	if got, want := hosts.OwnerPageURL("alice"), "https://alice.foo.example/"; got != want {
		t.Fatalf("OwnerPageURL = %q, want %q", got, want)
	}
	// The index lives at the root of the same host a short site path hangs
	// off, so the two must agree on the origin.
	site := hosts.SiteURL("alice", "gantt", false)
	if !strings.HasPrefix(site, hosts.OwnerPageURL("alice")) {
		t.Fatalf("site url %q is not under the owner page %q", site, hosts.OwnerPageURL("alice"))
	}
}
