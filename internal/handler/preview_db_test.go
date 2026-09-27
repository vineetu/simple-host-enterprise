package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
)

// Preview before live: a deploy with publish=false stores a version without
// changing what visitors see; the owner or a team member opens any kept
// version through a signed, short-lived link that still needs their own
// sign-in; saves from it are refused; a rollback makes it live.
func TestPreviewBeforeLive(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	siteID := w.siteID("alice", "demo")
	host := "demo.alice." + accessBase

	// A new site cannot be held back: its first version is its only one.
	if rec := w.api("alice", http.MethodPost, "/api/sites/fresh?publish=false", zipOf(t, map[string][]byte{"index.html": []byte("x")})); rec.Code != http.StatusBadRequest || !containsCode(rec.Body.Bytes(), "publish_required") {
		t.Fatalf("create with publish=false = %d %s, want 400 publish_required", rec.Code, rec.Body)
	}
	if rec := w.api("alice", http.MethodPut, "/api/sites/demo?publish=maybe", zipOf(t, map[string][]byte{"index.html": []byte("x")})); rec.Code != http.StatusBadRequest {
		t.Fatalf("publish=maybe = %d %s, want 400", rec.Code, rec.Body)
	}

	rec := w.api("alice", http.MethodPut, "/api/sites/demo?publish=false", zipOf(t, map[string][]byte{"index.html": []byte("<h1>draft</h1>")}))
	var held map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &held)
	if rec.Code != http.StatusOK || held["active_version"] != float64(1) || held["new_version"] != float64(2) {
		t.Fatalf("update with publish=false = %d %s, want v1 live and new_version 2", rec.Code, rec.Body)
	}
	if body := w.page("alice", host, "/"); !strings.Contains(body, "hi") || strings.Contains(body, "draft") {
		t.Fatalf("live site after a held deploy = %q, want the old version", body)
	}
	var versions []map[string]any
	_ = json.Unmarshal(w.api("alice", http.MethodGet, "/api/collaboration/sites/alice/demo/versions", nil).Body.Bytes(), &versions)
	if len(versions) != 2 || versions[0]["live"] != false || versions[1]["live"] != true {
		t.Fatalf("versions = %v, want v2 not live and v1 live", versions)
	}
	var events []map[string]any
	var page struct {
		Events []map[string]any `json:"events"`
	}
	_ = json.Unmarshal(w.api("alice", http.MethodGet, "/api/audit?owner=alice&site=demo&action=site_update", nil).Body.Bytes(), &page)
	events = page.Events
	if len(events) != 1 || events[0]["actor_name"] != "alice" || events[0]["detail"].(map[string]any)["published"] != false {
		t.Fatalf("audit of the held deploy = %v, want one site_update by alice with published false", events)
	}

	// The link: only for the owner or team, and only for a kept version.
	if rec := w.api("vera", http.MethodGet, "/api/collaboration/sites/alice/demo/versions/2/preview", nil); rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
		t.Fatalf("preview link for a stranger = %d %s", rec.Code, rec.Body)
	}
	if rec := w.api("alice", http.MethodGet, "/api/collaboration/sites/alice/demo/versions/9/preview", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("preview link for a version that is not kept = %d %s, want 404", rec.Code, rec.Body)
	}
	rec = w.api("alice", http.MethodGet, "/api/collaboration/sites/alice/demo/versions/2/preview", nil)
	var link map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &link)
	if rec.Code != http.StatusOK || link["live"] != false || link["version"] != float64(2) {
		t.Fatalf("preview link = %d %s", rec.Code, rec.Body)
	}
	parsed, err := url.Parse(link["url"].(string))
	if err != nil || parsed.Host != host || !strings.HasPrefix(parsed.Path, "/_preview/2-") {
		t.Fatalf("preview url = %v, want the site's own host under /_preview/", link["url"])
	}
	expires, _ := time.Parse(time.RFC3339, link["expires_at"].(string))
	if d := time.Until(expires); d < 55*time.Minute || d > time.Hour+time.Minute {
		t.Fatalf("preview expires in %v, want an hour", d)
	}

	// The owner sees the held version, never cached or indexed.
	resp := w.hostGet("alice", host, parsed.Path)
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "draft") {
		t.Fatalf("owner preview = %d %q", resp.Code, resp.Body)
	}
	if got := resp.Header().Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Fatalf("preview X-Robots-Tag = %q", got)
	}
	if got := resp.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("preview Cache-Control = %q", got)
	}
	// Without the trailing slash it is sent to the directory.
	if resp := w.hostGet("alice", host, strings.TrimSuffix(parsed.Path, "/")); resp.Code != http.StatusMovedPermanently {
		t.Fatalf("preview without slash = %d", resp.Code)
	}
	// Anyone else who can open the site cannot open the preview.
	w.setAccess("alice", "/api/sites/demo/access", map[string]any{"level": "company"}, http.StatusOK)
	if resp := w.hostGet("vera", host, parsed.Path); resp.Code != http.StatusNotFound {
		t.Fatalf("preview for a company viewer = %d, want 404", resp.Code)
	}
	if body := w.page("vera", host, "/"); !strings.Contains(body, "hi") {
		t.Fatalf("company viewer of the live site = %q", body)
	}
	// Nobody signed in is sent to sign in first, as for any site.
	r := httptest.NewRequest(http.MethodGet, "https://"+host+parsed.Path, nil)
	r.Header.Set("Accept", "text/html")
	if code := w.do(r).Code; code != http.StatusFound {
		t.Fatalf("anonymous preview = %d, want the sign-in hand-off", code)
	}
	// A forged signature is no preview: the path is ordinary content, which
	// does not exist.
	forged := strings.TrimSuffix(parsed.Path, "/")
	forged = forged[:len(forged)-2] + "AA/"
	if resp := w.hostGet("alice", host, forged); resp.Code != http.StatusNotFound || strings.Contains(resp.Body.String(), "draft") {
		t.Fatalf("forged preview = %d %q, want 404", resp.Code, resp.Body)
	}
	// An expired link says so.
	old := "/_preview/" + previewToken(w.keys, siteID, 2, time.Now().Add(-time.Minute)) + "/"
	if resp := w.hostGet("alice", host, old); resp.Code != http.StatusGone {
		t.Fatalf("expired preview = %d, want 410", resp.Code)
	}
	// Signed for another site, it is nothing here.
	w.deploy("alice", "/api/sites/other")
	stranger := "/_preview/" + previewToken(w.keys, w.siteID("alice", "other"), 1, time.Now().Add(time.Hour)) + "/"
	if resp := w.hostGet("alice", host, stranger); resp.Code != http.StatusNotFound {
		t.Fatalf("another site's preview token = %d, want 404", resp.Code)
	}

	// Saves from a preview page are refused; the live page still saves.
	if code := w.saveState("alice", host, "https://"+host+parsed.Path); code != http.StatusForbidden {
		t.Fatalf("save from a preview page = %d, want 403", code)
	}
	if code := w.saveState("alice", host, "https://"+host+"/"); code != http.StatusOK {
		t.Fatalf("save from the live page = %d, want 200", code)
	}

	// Make live is the rollback to it.
	w.setAccess("alice", "/api/sites/demo/rollback", map[string]any{"version": 2}, http.StatusOK)
	if body := w.page("alice", host, "/"); !strings.Contains(body, "draft") {
		t.Fatalf("live site after making v2 live = %q", body)
	}
}

// A team member previews a team site's held version; the site's quota and
// pruning never drop the live version for a held one.
func TestPreviewTeamSite(t *testing.T) {
	w := newAccessWorld(t)
	ctx := context.Background()
	tx, _ := w.database.Begin()
	team, err := db.CreateTeam(ctx, tx, "crew", w.users["mo"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTeamMembers(ctx, tx, team.ID, []string{"alice"}, w.users["mo"]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	w.deploy("mo", "/api/collaboration/sites/crew/board")
	var site map[string]any
	_ = json.Unmarshal(w.api("mo", http.MethodGet, "/api/collaboration/sites/crew/board", nil).Body.Bytes(), &site)
	r := httptest.NewRequest(http.MethodPut, "https://"+accessBase+"/api/collaboration/sites/crew/board?publish=false", strings.NewReader(string(zipOf(t, map[string][]byte{"index.html": []byte("<h1>team draft</h1>")}))))
	r.Header.Set("X-API-Key", w.apiKeys["mo"])
	r.Header.Set("X-Simple-Host-Client", "control-ui")
	r.Header.Set("If-Match", site["etag"].(string))
	rec := w.do(r)
	var held map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &held)
	if rec.Code != http.StatusOK || held["active_version"] != float64(1) || held["new_version"] != float64(2) || held["etag"] != site["etag"] {
		t.Fatalf("team update with publish=false = %d %s", rec.Code, rec.Body)
	}
	var link map[string]any
	_ = json.Unmarshal(w.api("alice", http.MethodGet, "/api/collaboration/sites/crew/board/versions/2/preview", nil).Body.Bytes(), &link)
	parsed, err := url.Parse(link["url"].(string))
	if err != nil {
		t.Fatalf("preview link = %v", link)
	}
	if resp := w.hostGet("alice", parsed.Host, parsed.Path); resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "team draft") {
		t.Fatalf("member preview = %d %q", resp.Code, resp.Body)
	}
}

// hostGet fetches path on host as user with a host session.
func (w *accessWorld) hostGet(user, host, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
	r.Header.Set("Accept", "text/html")
	r.AddCookie(w.cookie(user, host))
	return w.do(r)
}

// page is hostGet's body, failing on anything but 200.
func (w *accessWorld) page(user, host, path string) string {
	w.t.Helper()
	rec := w.hostGet(user, host, path)
	if rec.Code != http.StatusOK {
		w.t.Fatalf("GET %s%s as %s = %d", host, path, user, rec.Code)
	}
	return rec.Body.String()
}

// saveState writes the site's saved data as user from a page at referer.
func (w *accessWorld) saveState(user, host, referer string) int {
	var current map[string]any
	r := httptest.NewRequest(http.MethodGet, "https://"+host+"/api/site/state/versioned", nil)
	r.AddCookie(w.cookie(user, host))
	_ = json.Unmarshal(w.do(r).Body.Bytes(), &current)
	body, _ := json.Marshal(map[string]any{"version": current["version"], "state": map[string]any{"n": 1}})
	r = httptest.NewRequest(http.MethodPut, "https://"+host+"/api/site/state/versioned", strings.NewReader(string(body)))
	r.AddCookie(w.cookie(user, host))
	r.Header.Set("Origin", "https://"+host)
	r.Header.Set("Referer", referer)
	r.Header.Set("Content-Type", "application/json")
	return w.do(r).Code
}
