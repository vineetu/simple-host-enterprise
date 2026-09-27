package handler

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	db "github.com/vsriram/simple-host/internal/db"
)

// An admin renames a person's address after a name change: the sites move
// with the name, old addresses redirect only for people who may open the
// site, the old owner page redirects for anyone signed in, the old name is
// held against everyone else, collisions are refused, and it is audited.
func TestAdminRenamePerson(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/tracker")
	w.setAccess("alice", "/api/sites/tracker/access", map[string]any{"level": "company"}, http.StatusOK)
	w.deploy("alice", "/api/sites/private")
	rename := func(user, name string) *httptestResult {
		rec := w.adminForm("/api/admin/users/"+user+"/rename", url.Values{"name": {name}})
		return &httptestResult{code: rec.Code, body: rec.Body.String()}
	}

	// Refusals first: nothing changes.
	for _, c := range []struct{ user, name string }{
		{"alice", "vera"},       // another person has it
		{"alice", "Bad Name"},   // not an address
		{"alice", "a.b"},        // no dots
		{"alice", "team-alice"}, // teams only
		{"alice", "alice"},      // same name
		{"nobody", "someone"},   // no such person
	} {
		if got := rename(c.user, c.name); got.code < 400 {
			t.Fatalf("rename %s to %q = %d %s, want a refusal", c.user, c.name, got.code, got.body)
		}
	}

	if got := rename("alice", "alicia"); got.code != http.StatusOK {
		t.Fatalf("rename = %d %s", got.code, got.body)
	}
	var owner string
	_ = w.database.QueryRow(`SELECT u.username FROM sites s JOIN users u ON u.id = s.user_id WHERE s.name = 'tracker'`).Scan(&owner)
	if owner != "alicia" {
		t.Fatalf("tracker's owner = %q, want alicia", owner)
	}
	if code := w.view("vera", "alicia", "tracker", false); code != http.StatusOK {
		t.Fatalf("new address = %d", code)
	}

	// The old site address: a redirect for someone who may open the site...
	old := w.siteRequest("vera", http.MethodGet, "tracker.alice."+accessBase, "/page?x=1", "")
	if old.Code != http.StatusMovedPermanently || old.Header().Get("Location") != "https://tracker.alicia."+accessBase+"/page?x=1" {
		t.Fatalf("old address = %d %q", old.Code, old.Header().Get("Location"))
	}
	// ...and not for someone who may not (only_me), nor for nobody.
	if rec := w.siteRequest("vera", http.MethodGet, "private.alice."+accessBase, "/", ""); rec.Code != http.StatusNotFound || rec.Header().Get("Location") != "" {
		t.Fatalf("old address of a site vera may not open = %d %q, want 404", rec.Code, rec.Header().Get("Location"))
	}
	if rec := w.siteRequest("", http.MethodGet, "tracker.alice."+accessBase, "/", ""); rec.Code != http.StatusNotFound || rec.Header().Get("Location") != "" {
		t.Fatalf("old address with nobody signed in = %d %q, want 404", rec.Code, rec.Header().Get("Location"))
	}
	// The owner may still reach their only-me site from its old address.
	if rec := w.siteRequest("alice", http.MethodGet, "private.alice."+accessBase, "/", ""); rec.Code != http.StatusMovedPermanently || !strings.HasPrefix(rec.Header().Get("Location"), "https://private.alicia.") {
		t.Fatalf("owner at the old address = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// The old owner page, for someone signed in.
	if rec := w.hostGet("vera", "alice."+accessBase, "/"); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "https://alicia."+accessBase+"/" {
		t.Fatalf("old owner page = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// Both labels need a certificate: the new one for the sites, the old one
	// so its redirects answer over TLS.
	labels, err := db.OwnerLabelsWithSites(context.Background(), w.database)
	if err != nil || !containsString(labels, "alice") || !containsString(labels, "alicia") {
		t.Fatalf("owner labels = %v, %v", labels, err)
	}

	// The old name is held: no sign-in and no rename takes it.
	if _, err := db.CreateOIDCUser(context.Background(), w.database, "alice", "sub-new-alice", "new-alice@example.com", false); err == nil {
		t.Fatal("a new sign-in took the held name alice")
	}
	if got := rename("vera", "alice"); got.code != http.StatusConflict {
		t.Fatalf("vera renamed to the held alice = %d %s, want 409", got.code, got.body)
	}

	// Audited.
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'admin_rename_user' AND detail::text LIKE '%alicia%'`); n != 1 {
		t.Fatalf("admin_rename_user rows = %d, want 1", n)
	}

	// Renaming back takes the old name again and holds the other one.
	if got := rename("alicia", "alice"); got.code != http.StatusOK {
		t.Fatalf("rename back = %d %s", got.code, got.body)
	}
	var held []string
	rows, _ := w.database.Query(`SELECT owner_label FROM renamed_owner_labels ORDER BY 1`)
	for rows.Next() {
		var l string
		_ = rows.Scan(&l)
		held = append(held, l)
	}
	rows.Close()
	if len(held) != 1 || held[0] != "alicia" {
		t.Fatalf("held labels = %v, want [alicia]", held)
	}
	if code := w.view("vera", "alice", "tracker", false); code != http.StatusOK {
		t.Fatalf("tracker back at alice = %d", code)
	}
}

// Erasing a renamed person keeps every name they had held.
func TestEraseRenamedPersonHoldsEveryName(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/tracker")
	if rec := w.adminForm("/api/admin/users/alice/rename", url.Values{"name": {"alicia"}}); rec.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body)
	}
	if rec := w.adminForm("/api/admin/users/alicia/disable", nil); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body)
	}
	if rec := w.adminForm("/api/admin/users/alicia/erase", url.Values{"confirm": {"alicia"}}); rec.Code != http.StatusOK {
		t.Fatalf("erase = %d %s", rec.Code, rec.Body)
	}
	if n := w.count(`SELECT count(*) FROM erased_owner_labels WHERE owner_label IN ('alice', 'alicia')`); n != 2 {
		t.Fatalf("erased labels held = %d, want alice and alicia", n)
	}
	if n := w.count(`SELECT count(*) FROM site_redirects WHERE owner_label = 'alice'`); n != 0 {
		t.Fatalf("redirects under the old name after erasure = %d, want 0", n)
	}
	if _, err := db.CreateOIDCUser(context.Background(), w.database, "alice", "sub-new-alice", "new-alice@example.com", false); err == nil {
		t.Fatal("a new sign-in took the erased person's old name")
	}
}

type httptestResult struct {
	code int
	body string
}
