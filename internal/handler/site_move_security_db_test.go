package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	db "github.com/vsriram/simple-host/internal/db"
)

// openOld requests an old address as user: by host session cookie as a
// navigation when cookie is set, by API key otherwise ("" is nobody).
func (w *accessWorld) openOld(user, host string, cookie bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "https://"+host+"/", nil)
	if cookie {
		r.Header.Set("Accept", "text/html")
		r.Header.Set("Sec-Fetch-Mode", "navigate")
		r.Header.Set("Sec-Fetch-Dest", "document")
		if user != "" {
			r.AddCookie(w.cookie(user, host))
		}
	} else if user != "" {
		r.Header.Set("X-API-Key", w.apiKeys[user])
	}
	return w.do(r)
}

// The old address of a renamed or handed-over site names where it went
// only to somebody who could open it there; anyone else gets exactly what an
// address nothing ever held gets.
func TestMovedSiteRedirectOnlyForViewers(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/secret")
	w.setAccess("alice", "/api/sites/secret/access", map[string]any{"level": "only_me"}, http.StatusOK)
	w.move("alice", "/api/sites/secret/rename", map[string]any{"name": "secret-two"}, http.StatusOK)
	oldHost := "secret.alice." + accessBase
	neverHost := "never-was.alice." + accessBase
	target := "https://secret-two.alice." + accessBase + "/"

	if rec := w.openOld("alice", oldHost, false); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != target {
		t.Fatalf("owner by key = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := w.openOld("alice", oldHost, true); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != target {
		t.Fatalf("owner signed in on the old host = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, tc := range []struct {
		name   string
		user   string
		cookie bool
	}{
		{"another person by key", "vera", false},
		{"another person signed in", "vera", true},
		{"nobody, not a navigation", "", false},
	} {
		got := w.openOld(tc.user, oldHost, tc.cookie)
		never := w.openOld(tc.user, neverHost, tc.cookie)
		if got.Code != http.StatusNotFound || got.Code != never.Code || got.Header().Get("Location") != "" || got.Body.String() != never.Body.String() {
			t.Errorf("%s: old address = %d %q, never-held address = %d", tc.name, got.Code, got.Header().Get("Location"), never.Code)
		}
	}
	// Nobody signed in, navigating: the usual sign-in on this host, which
	// names nothing new.
	if rec := w.openOld("", oldHost, true); rec.Code != http.StatusFound || strings.Contains(rec.Header().Get("Location"), "secret-two") {
		t.Errorf("navigation with no session = %d %q, want the sign-in hand-off", rec.Code, rec.Header().Get("Location"))
	}
	// The pre-v1.3 shape goes to the old address's current shape, never to
	// the new name.
	if rec := w.openOld("", "alice--secret."+accessBase, false); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "https://"+oldHost+"/" {
		t.Errorf("legacy address = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// Open to the network, anybody may follow it.
	if _, err := w.database.Exec(`UPDATE sites SET access = 'network', public = true WHERE name = 'secret-two'`); err != nil {
		t.Fatal(err)
	}
	if rec := w.openOld("", oldHost, false); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != target {
		t.Errorf("network site, nobody signed in = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// A hand-over is in the audit log of the namespace it left as well as the
// one it reached: a member of the old team who was not in the new one sees
// it.
func TestTransferAuditedInBothNamespaces(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("old", "mo", "vera")
	w.newTeam("new", "mo")
	w.deploy("mo", "/api/collaboration/sites/team-old/board")
	w.move("mo", "/api/collaboration/sites/team-old/board/transfer", map[string]any{"to": "new"}, http.StatusOK)

	rec := w.api("vera", http.MethodGet, "/api/audit?action=site_transfer", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("audit = %d %s", rec.Code, rec.Body)
	}
	var page struct {
		Events []struct {
			Action string         `json:"action"`
			Detail map[string]any `json:"detail"`
		} `json:"events"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Events) != 1 || page.Events[0].Detail["from"] != "team-old" || page.Events[0].Detail["to"] != "team-new" {
		t.Fatalf("vera's audit = %s, want the one hand-over out of team-old", rec.Body)
	}
}

// Network access is approved for an owner, not for whoever holds the site
// next: a hand-over drops it to company (and withdraws a pending request); a
// rename keeps it.
func TestTransferDropsNetworkAccess(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "alice")
	w.deploy("alice", "/api/sites/open")
	w.deploy("alice", "/api/sites/asked")
	w.deploy("alice", "/api/sites/kept")
	if _, err := w.database.Exec(`UPDATE sites SET access = 'network', public = true WHERE name IN ('open', 'kept')`); err != nil {
		t.Fatal(err)
	}
	w.setAccess("alice", "/api/sites/asked/access", map[string]any{"level": "network", "reason": "event"}, http.StatusAccepted)

	w.move("alice", "/api/sites/open/transfer", map[string]any{"to": "crew"}, http.StatusOK)
	w.move("alice", "/api/sites/asked/transfer", map[string]any{"to": "crew"}, http.StatusOK)
	w.move("alice", "/api/sites/kept/rename", map[string]any{"name": "kept-two"}, http.StatusOK)

	if got := w.siteAccess("team-crew", "open"); got != db.AccessCompany {
		t.Errorf("handed-over network site is %s, want company", got)
	}
	if n := w.count(`SELECT count(*) FROM sites WHERE name = 'asked' AND network_requested_at IS NOT NULL`); n != 0 {
		t.Error("a pending network request survived the hand-over")
	}
	if got := w.siteAccess("alice", "kept-two"); got != db.AccessNetwork {
		t.Errorf("renamed network site is %s, want network", got)
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'network_access_reverted' AND detail->>'reason' = 'site_transfer'`); n != 2 {
		t.Errorf("network_access_reverted rows for the hand-overs = %d, want 2", n)
	}
}

// A recently deleted site keeps counting toward its owner's quota for its
// recovery window, so deleting does not make room; its restore passes.
func TestDeletedSitesCountTowardQuota(t *testing.T) {
	w := newAccessWorldWith(t, UploadQuota{MaxSites: 1}, nil)
	w.deploy("alice", "/api/sites/one")
	if rec := w.api("alice", http.MethodDelete, "/api/sites/one", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	rec := w.api("alice", http.MethodPost, "/api/sites/two", zipOf(t, map[string][]byte{"index.html": []byte("x")}))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"site_limit"`) {
		t.Fatalf("deploy while a deleted site holds the quota = %d %s, want 409 site_limit", rec.Code, rec.Body)
	}
	if rec := w.api("alice", http.MethodPost, "/api/sites/one/restore", nil); rec.Code != http.StatusOK {
		t.Fatalf("restore at the limit = %d %s", rec.Code, rec.Body)
	}
}

// Past its recovery window a site is not restorable, even before the
// sweeper has purged it.
func TestRestoreRefusedPastWindow(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/late")
	if rec := w.api("alice", http.MethodDelete, "/api/sites/late", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	if _, err := w.database.Exec(`UPDATE sites SET deleted_at = now() - interval '31 days', purge_at = now() - interval '1 day' WHERE name = 'late'`); err != nil {
		t.Fatal(err)
	}
	if rec := w.api("alice", http.MethodPost, "/api/sites/late/restore", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("restore past the window = %d %s, want 404", rec.Code, rec.Body)
	}
}
