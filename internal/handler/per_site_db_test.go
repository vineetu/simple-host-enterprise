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

// get sends a GET to rawURL, signed in as user on that URL's host when user
// is not "".
func (w *accessWorld) get(user, rawURL string) *httptest.ResponseRecorder {
	w.t.Helper()
	r := httptest.NewRequest(http.MethodGet, rawURL, nil)
	r.Header.Set("Accept", "text/html")
	if user != "" {
		r.AddCookie(w.cookie(user, r.Host))
	}
	return w.do(r)
}

func wantRedirect(t *testing.T, rec *httptest.ResponseRecorder, code int, location string) {
	t.Helper()
	if rec.Code != code || rec.Header().Get("Location") != location {
		t.Errorf("got %d Location %q, want %d %q", rec.Code, rec.Header().Get("Location"), code, location)
	}
}

// Every site answers on its own host; the pre-v1.3 owner-host path redirects
// there with path and query, and the owner host keeps the index.
func TestEverySiteHasItsOwnOrigin(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	w.deploy("alice", "/api/sites/other")
	w.setAccess("alice", "/api/sites/demo/access", map[string]any{"level": "company"}, http.StatusOK)

	if rec := w.get("vera", "https://demo.alice."+accessBase+"/"); rec.Code != http.StatusOK {
		t.Fatalf("company site on its own host = %d", rec.Code)
	}
	wantRedirect(t, w.get("", "https://alice."+accessBase+"/demo/page.html?x=1"), http.StatusMovedPermanently, "https://demo.alice."+accessBase+"/page.html?x=1")
	// Not even existence is checked, so the answer confirms nothing.
	wantRedirect(t, w.get("", "https://alice."+accessBase+"/nope/"), http.StatusMovedPermanently, "https://nope.alice."+accessBase+"/")
	if rec := w.get("alice", "https://alice."+accessBase+"/"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://demo.alice."+accessBase+"/") {
		t.Errorf("owner index = %d, want it to link the site host", rec.Code)
	}
	// A cookie for one of alice's sites is not a session on another.
	r := httptest.NewRequest(http.MethodGet, "https://other.alice."+accessBase+"/", nil)
	r.Header.Set("Accept", "text/html")
	r.AddCookie(w.cookie("alice", "demo.alice."+accessBase))
	if rec := w.do(r); rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "/auth/handoff") {
		t.Errorf("sibling site's cookie = %d %q, want the hand-off", rec.Code, rec.Header().Get("Location"))
	}
	// The deploy response names the site host.
	rec := w.api("alice", http.MethodGet, "/api/sites", nil)
	if !strings.Contains(rec.Body.String(), "https://demo.alice."+accessBase+"/") {
		t.Errorf("list sites does not carry the site host: %s", rec.Body)
	}
}

// New site names are their own address; a name another older site's address
// already has is refused.
func TestNewSiteNamesAreAddresses(t *testing.T) {
	w := newAccessWorld(t)
	for _, name := range []string{"My%20Site", "Demo", "my.site", strings.Repeat("a", 64), "xn--abc"} {
		rec := w.api("alice", http.MethodPost, "/api/sites/"+name, zipOf(t, map[string][]byte{"index.html": []byte("x")}))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_site_name") {
			t.Errorf("create %q = %d %s, want 400 invalid_site_name", name, rec.Code, rec.Body)
		}
	}
	if _, err := w.database.Exec(`INSERT INTO sites (user_id, name) VALUES ($1, 'Notes')`, w.users["alice"]); err != nil {
		t.Fatal(err)
	}
	rec := w.api("alice", http.MethodPost, "/api/sites/notes", zipOf(t, map[string][]byte{"index.html": []byte("x")}))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "name_conflict") {
		t.Errorf("create notes beside Notes = %d %s, want 409 name_conflict", rec.Code, rec.Body)
	}
	w.deploy("alice", "/api/sites/"+strings.Repeat("a", 63))
}

// Teams are named "team-<name>" whichever spelling is typed.
func TestTeamNamesTakeThePrefix(t *testing.T) {
	w := newAccessWorld(t)
	rec := w.api("mo", http.MethodPost, "/api/teams", map[string]string{"name": "Sales"})
	var created struct{ Name string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if rec.Code != http.StatusCreated || created.Name != "team-sales" {
		t.Fatalf("create Sales = %d %s, want team-sales", rec.Code, rec.Body)
	}
	if rec := w.api("mo", http.MethodPost, "/api/teams", map[string]string{"name": "team-sales"}); rec.Code != http.StatusConflict {
		t.Errorf("create team-sales again = %d, want 409", rec.Code)
	}
	for _, name := range []string{"sales", "team-sales"} {
		if rec := w.api("mo", http.MethodGet, "/api/teams/"+name+"/members", nil); rec.Code != http.StatusOK {
			t.Errorf("members of %s = %d", name, rec.Code)
		}
	}
	w.deploy("mo", "/api/collaboration/sites/team-sales/board")
	if rec := w.get("mo", "https://board.team-sales."+accessBase+"/"); rec.Code != http.StatusOK {
		t.Errorf("team site on its own host = %d", rec.Code)
	}
}

// A person's handle never begins "team-".
func TestPersonHandleNeverTakesTheTeamPrefix(t *testing.T) {
	database := connectorTestDB(t)
	h := newTestAuthHandler(database, "example.com")
	user, _, err := h.createUser(context.Background(), "sub-team-alpha", "team-alpha@example.com", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if user.Username != "teamalpha" {
		t.Errorf("username = %q, want teamalpha", user.Username)
	}
}

// A pre-v1.3 team address redirects to the renamed team's owner host, and a
// v1.2 "<team>--<site>" address to the site, until somebody takes the old
// name.
func TestLegacyTeamAddressesRedirect(t *testing.T) {
	w := newAccessWorld(t)
	tx, _ := w.database.Begin()
	if _, err := db.CreateTeam(context.Background(), tx, "team-crew", w.users["mo"]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	w.deploy("mo", "/api/collaboration/sites/team-crew/board")

	wantRedirect(t, w.get("", "https://crew."+accessBase+"/board/p?q=1"), http.StatusMovedPermanently, "https://team-crew."+accessBase+"/board/p?q=1")
	// The v1.2 "<owner>--<site>" address, too.
	wantRedirect(t, w.get("", "https://crew--board."+accessBase+"/p?q=1"), http.StatusMovedPermanently, "https://board.team-crew."+accessBase+"/p?q=1")
	// The new site-host shape carries no legacy mapping.
	if rec := w.get("mo", "https://board.crew."+accessBase+"/"); rec.Code != http.StatusNotFound {
		t.Errorf("board.crew = %d, want 404", rec.Code)
	}
	wantRedirect(t, w.get("mo", "https://crew."+accessBase+"/"), http.StatusMovedPermanently, "https://team-crew."+accessBase+"/")

	if _, err := db.CreateOIDCUser(context.Background(), w.database, "crew", "sub-crew", "crew@example.com", false); err != nil {
		t.Fatal(err)
	}
	wantRedirect(t, w.get("", "https://crew."+accessBase+"/board/"), http.StatusMovedPermanently, "https://board.crew."+accessBase+"/")
}
