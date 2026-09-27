package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
)

func (w *accessWorld) exportLink(user, owner, site string) (int, string) {
	w.t.Helper()
	rec := w.api(user, http.MethodPost, "/api/collaboration/sites/"+owner+"/"+site+"/export-link", nil)
	var body siteExportLinkResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body.URL
}

// fetchAnonymous GETs an absolute address with no key and no cookie.
func (w *accessWorld) fetchAnonymous(rawURL string) *httptest.ResponseRecorder {
	return w.do(httptest.NewRequest(http.MethodGet, rawURL, nil))
}

func zipEntries(t *testing.T, body []byte) map[string]string {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range reader.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		out[f.Name] = string(data)
	}
	return out
}

// The owner's download link, opened with no credentials, is one zip of the
// live files, saved data and its history, versions and uploads. A stranger
// gets no link; a tampered, expired or deleted-site link opens nothing.
func TestSiteExportDownload(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	id := w.siteID("alice", "demo")
	if err := db.UpdateSiteState(context.Background(), w.database, "alice", "demo", json.RawMessage(`{"n":7}`)); err != nil {
		t.Fatal(err)
	}
	if rec := w.uploadAsset("alice", "alice", "demo", "photo.png", []byte("png-bytes")); rec.Code != http.StatusCreated {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}

	code, link := w.exportLink("alice", "alice", "demo")
	if code != http.StatusOK || !strings.HasPrefix(link, "https://"+accessBase+"/api/site-export/") {
		t.Fatalf("export link = %d %q", code, link)
	}
	// HEAD: the headers, no zip, no audit row, and the link still works.
	head := w.do(httptest.NewRequest(http.MethodHead, link, nil))
	if head.Code != http.StatusOK || head.Header().Get("Content-Type") != "application/zip" || head.Body.Len() != 0 {
		t.Fatalf("HEAD = %d %q %d bytes", head.Code, head.Header().Get("Content-Type"), head.Body.Len())
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_export'`); n != 0 {
		t.Fatalf("HEAD audited %d exports", n)
	}
	rec := w.fetchAnonymous(link)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("download = %d %s", rec.Code, rec.Body)
	}
	entries := zipEntries(t, rec.Body.Bytes())
	for _, name := range []string{"site.json", "saved-data.json", "saved-data-history.json", "versions.json", "assets.json", "files/index.html"} {
		if _, ok := entries[name]; !ok {
			t.Errorf("zip lacks %s (has %v)", name, keysOf(entries))
		}
	}
	if !strings.Contains(entries["saved-data.json"], `"n"`) || entries["files/index.html"] != "<h1>hi</h1>" || !strings.Contains(entries["assets.json"], "photo.png") {
		t.Errorf("zip contents = %v", entries)
	}
	var assetFiles int
	for name, data := range entries {
		if strings.HasPrefix(name, "assets/") && data == "png-bytes" {
			assetFiles++
		}
	}
	if assetFiles != 1 {
		t.Errorf("uploaded file not in zip: %v", keysOf(entries))
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_export' AND site_id = $1`, id); n != 1 {
		t.Errorf("site_export audit rows = %d, want 1", n)
	}

	// A link works once.
	if again := w.fetchAnonymous(link); again.Code != http.StatusGone {
		t.Errorf("second download = %d %s, want 410", again.Code, again.Body)
	}
	// Another spelling of the same signature is the same, used, link.
	if i := strings.LastIndex(link, "."); i > 0 {
		sig, _ := base64.RawURLEncoding.DecodeString(link[i+1:])
		last := link[len(link)-1]
		for _, c := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_" {
			if byte(c) == last {
				continue
			}
			other := link[:len(link)-1] + string(c)
			if got, err := base64.RawURLEncoding.DecodeString(other[i+1:]); err == nil && bytes.Equal(got, sig) {
				if rec := w.fetchAnonymous(other); rec.Code == http.StatusOK {
					t.Errorf("respelled link downloaded again")
				}
			}
		}
	}
	if code, _ := w.exportLink("vera", "alice", "demo"); code != http.StatusNotFound {
		t.Errorf("stranger export link = %d, want 404", code)
	}
	if rec := w.fetchAnonymous(link + "x"); rec.Code != http.StatusNotFound {
		t.Errorf("tampered link = %d, want 404", rec.Code)
	}
	var aliceKeyID string
	if err := w.database.QueryRow(`SELECT id::text FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL LIMIT 1`, w.users["alice"]).Scan(&aliceKeyID); err != nil {
		t.Fatal(err)
	}
	expired, err := signSiteExport(w.keys, siteExportClaims{SiteID: id, ActorID: w.users["alice"], Owner: "alice", Site: "demo", Exp: time.Now().Add(-time.Minute).Unix(), CredKind: db.CredentialKey, CredID: aliceKeyID, Nonce: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := w.fetchAnonymous("https://" + accessBase + "/api/site-export/" + expired); rec.Code != http.StatusNotFound {
		t.Errorf("expired link = %d, want 404", rec.Code)
	}
	// A session cookie signed with the same key is not a download link.
	if rec := w.fetchAnonymous("https://" + accessBase + "/api/site-export/" + w.cookie("alice", "").Value); rec.Code != http.StatusNotFound {
		t.Errorf("session cookie as a link = %d, want 404", rec.Code)
	}
	_, link = w.exportLink("alice", "alice", "demo")
	if rec := w.api("alice", http.MethodDelete, "/api/sites/demo", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := w.fetchAnonymous(link); rec.Code != http.StatusNotFound {
		t.Errorf("link to a deleted site = %d, want 404", rec.Code)
	}
}

// A team member downloads a team site; a person who has left the team no
// longer can, even with a link minted while they were in it.
func TestSiteExportFollowsTeamMembership(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "mo", "olly")
	w.deploy("mo", "/api/collaboration/sites/team-crew/board")
	code, link := w.exportLink("olly", "team-crew", "board")
	if code != http.StatusOK {
		t.Fatalf("member export link = %d", code)
	}
	if rec := w.api("olly", http.MethodPost, "/api/teams/crew/leave", nil); rec.Code != http.StatusOK {
		t.Fatalf("leave = %d %s", rec.Code, rec.Body)
	}
	if rec := w.fetchAnonymous(link); rec.Code != http.StatusNotFound {
		t.Errorf("link after leaving = %d, want 404", rec.Code)
	}
}

// include=shared lists the sites a person, or a team they are in, is a named
// viewer of, as viewer entries after their own; a site set back to only_me
// drops out, and the plain list never has them.
func TestSharedWithMeListing(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "mo")
	w.deploy("alice", "/api/sites/demo")
	w.deploy("vera", "/api/sites/mine")
	w.setAccess("alice", "/api/collaboration/sites/alice/demo/access", map[string]any{"level": "specific"}, http.StatusOK)
	if rec := w.api("alice", http.MethodPost, "/api/collaboration/sites/alice/demo/viewers", map[string]any{"usernames": []string{"vera", "team-crew"}}); rec.Code != http.StatusOK {
		t.Fatalf("grant = %d %s", rec.Code, rec.Body)
	}
	list := func(user, query string) []map[string]any {
		var out []map[string]any
		_ = json.Unmarshal(w.api(user, http.MethodGet, "/api/collaboration/sites"+query, nil).Body.Bytes(), &out)
		return out
	}
	if got := list("vera", ""); len(got) != 1 || got[0]["name"] != "mine" {
		t.Fatalf("plain list = %v", got)
	}
	got := list("vera", "?include=shared")
	if len(got) != 2 || got[1]["name"] != "demo" || got[1]["access_role"] != "viewer" || got[1]["shared_via"] != "" || got[1]["url"] == "" {
		t.Fatalf("vera shared list = %v", got)
	}
	if got := list("mo", "?include=shared"); len(got) != 1 || got[0]["shared_via"] != "team-crew" {
		t.Fatalf("mo shared list = %v", got)
	}
	if got := list("alice", "?include=shared"); len(got) != 1 || got[0]["access_role"] != "owner" {
		t.Fatalf("owner's list repeats a site she owns: %v", got)
	}
	// A viewer entry is a listing only: management still says not found.
	if rec := w.api("vera", http.MethodGet, "/api/collaboration/sites/alice/demo", nil); rec.Code != http.StatusNotFound {
		t.Errorf("viewer get_site = %d, want 404", rec.Code)
	}
	w.setAccess("alice", "/api/collaboration/sites/alice/demo/access", map[string]any{"level": "only_me"}, http.StatusOK)
	if got := list("vera", "?include=shared"); len(got) != 1 {
		t.Fatalf("only_me site still shared: %v", got)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A link stops working when the key that asked for it is revoked; asking
// for it is audited, and the download is recorded as the key's. Two links
// asked for in the same second are two links.
func TestSiteExportLinkFollowsItsCredential(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	id := w.siteID("alice", "demo")
	_, first := w.exportLink("alice", "alice", "demo")
	_, second := w.exportLink("alice", "alice", "demo")
	if first == "" || first == second {
		t.Fatalf("links = %q, %q: want two distinct links", first, second)
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_export_link' AND site_id = $1 AND actor_kind = 'key' AND key_id IS NOT NULL`, id); n != 2 {
		t.Errorf("site_export_link audit rows = %d, want 2", n)
	}
	if rec := w.fetchAnonymous(first); rec.Code != http.StatusOK {
		t.Fatalf("download = %d %s", rec.Code, rec.Body)
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_export' AND site_id = $1 AND actor_kind = 'key' AND key_id IS NOT NULL`, id); n != 1 {
		t.Errorf("site_export rows as the key = %d, want 1", n)
	}
	if _, err := w.database.Exec(`UPDATE api_keys SET revoked_at = now() WHERE user_id = $1`, w.users["alice"]); err != nil {
		t.Fatal(err)
	}
	if rec := w.fetchAnonymous(second); rec.Code != http.StatusNotFound {
		t.Errorf("link after its key was revoked = %d, want 404", rec.Code)
	}
}

// A used link whose marker is swept after it expires can never be used
// again: an expired link is refused at the insert itself.
func TestUseSiteExportLinkRefusesExpired(t *testing.T) {
	w := newAccessWorld(t)
	ctx := context.Background()
	soon := time.Now().Add(2 * time.Second)
	if ok, err := db.UseSiteExportLink(ctx, w.database, "sig-a", soon); err != nil || !ok {
		t.Fatalf("first use = %v %v", ok, err)
	}
	if ok, _ := db.UseSiteExportLink(ctx, w.database, "sig-a", soon); ok {
		t.Fatal("second use before expiry succeeded")
	}
	time.Sleep(3 * time.Second)
	if ok, err := db.UseSiteExportLink(ctx, w.database, "sig-a", soon); err != nil || ok {
		t.Fatalf("use after expiry (marker swept) = %v %v, want refused", ok, err)
	}
}
