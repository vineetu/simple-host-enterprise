package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vsriram/simple-host/internal/audit"
	db "github.com/vsriram/simple-host/internal/db"
)

// personWithData gives alice a live site with saved data and an asset, a
// recently deleted site, a team, viewer access to vera's site, and a team
// site she deleted; then disables her.
func personWithData(t *testing.T) *accessWorld {
	t.Helper()
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/tracker")
	if rec := w.siteRequest("alice", http.MethodPut, "tracker.alice."+accessBase, "/api/site/state/versioned", `{"version":0,"state":{"n":1}}`); rec.Code != http.StatusOK {
		t.Fatalf("save state = %d %s", rec.Code, rec.Body)
	}
	if rec := w.uploadAsset("alice", "alice", "tracker", "photo.txt", []byte("asset bytes")); rec.Code >= 300 {
		t.Fatalf("upload asset = %d %s", rec.Code, rec.Body)
	}
	w.deploy("alice", "/api/sites/old")
	if rec := w.api("alice", http.MethodDelete, "/api/sites/old", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete old = %d %s", rec.Code, rec.Body)
	}
	w.newTeam("crew", "alice", "mo")
	w.deploy("mo", "/api/collaboration/sites/team-crew/board")
	if rec := w.api("alice", http.MethodDelete, "/api/collaboration/sites/team-crew/board", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete team site = %d %s", rec.Code, rec.Body)
	}
	// A site she handed to her team: her old address redirects to it.
	w.deploy("alice", "/api/sites/handed")
	w.move("alice", "/api/sites/handed/transfer", map[string]any{"to": "crew"}, http.StatusOK)
	w.deploy("vera", "/api/sites/notes")
	if rec := w.api("vera", http.MethodPost, "/api/collaboration/sites/vera/notes/viewers", map[string]any{"usernames": []string{"alice"}}); rec.Code != http.StatusOK {
		t.Fatalf("grant viewer = %d %s", rec.Code, rec.Body)
	}
	w.cookie("alice", "")
	return w
}

func TestExportPersonData(t *testing.T) {
	w := personWithData(t)
	if rec := w.admin(http.MethodGet, "/api/admin/users/alice/export"); rec.Code != http.StatusConflict {
		t.Fatalf("export of a person who can sign in = %d %s, want 409", rec.Code, rec.Body)
	}
	w.disable("alice")
	rec := w.admin(http.MethodGet, "/api/admin/users/alice/export")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("export = %d %s", rec.Code, rec.Header())
	}
	archive, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range archive.File {
		r, _ := f.Open()
		body, _ := io.ReadAll(r)
		r.Close()
		files[f.Name] = string(body)
	}
	for name, want := range map[string]string{
		"account.json":                          `"email":"alice@example.com"`,
		"teams.json":                            `"team":"team-crew"`,
		"viewer-grants.json":                    `"owner":"vera"`,
		"api-keys.json":                         `"name":"test"`,
		"sessions.json":                         `"created_at"`,
		"connected-apps.json":                   `[]`,
		"sites/tracker/site.json":               `"name":"tracker"`,
		"sites/tracker/saved-data.json":         `"n": 1`,
		"sites/tracker/saved-data-history.json": `"n": 1`,
		"sites/tracker/versions.json":           `"version":1`,
		"sites/tracker/assets.json":             `"name":"photo.txt"`,
		"sites/tracker/files/index.html":        `<h1>hi</h1>`,
		"sites/old/site.json":                   `"name":"old"`,
		"audit-events.jsonl":                    `"action":"site_create"`,
	} {
		if !strings.Contains(files[name], want) {
			t.Errorf("%s = %q, want it to contain %q", name, files[name], want)
		}
	}
	if strings.Contains(files["sites/old/site.json"], `"deleted_at":null`) {
		t.Errorf("recently deleted site not marked deleted: %s", files["sites/old/site.json"])
	}
	assetFiles := 0
	for name, body := range files {
		if strings.HasPrefix(name, "sites/tracker/assets/") {
			assetFiles++
			if body != "asset bytes" {
				t.Errorf("asset %s = %q", name, body)
			}
		}
	}
	if assetFiles != 1 {
		t.Errorf("asset files = %d, want 1", assetFiles)
	}
	// Never a secret.
	all := strings.Join(func() []string {
		var out []string
		for _, b := range files {
			out = append(out, b)
		}
		return out
	}(), "\n")
	if strings.Contains(all, w.apiKeys["alice"]) || strings.Contains(all, "key_hash") {
		t.Error("export carries key material")
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'admin_user_export' AND detail->>'subject_id' = $1`, w.users["alice"]); n != 1 {
		t.Errorf("admin_user_export rows = %d, want 1", n)
	}
}

func TestErasePersonRefusals(t *testing.T) {
	w := newAccessWorld(t)
	if rec := w.adminPost("/api/admin/users/alice/erase", "confirm=alice"); rec.Code != http.StatusConflict {
		t.Errorf("erase of a person who can sign in = %d %s, want 409", rec.Code, rec.Body)
	}
	w.disable("alice")
	if rec := w.adminPost("/api/admin/users/alice/erase", "confirm=alic"); rec.Code != http.StatusBadRequest {
		t.Errorf("erase with the wrong confirmation = %d %s, want 400", rec.Code, rec.Body)
	}
	// alice is the last member of team-solo: refused until the team goes.
	w.newTeam("solo", "mo")
	if _, err := w.database.Exec(`UPDATE team_members SET user_id = $1 WHERE team_id = (SELECT id FROM users WHERE username = 'team-solo')`, w.users["alice"]); err != nil {
		t.Fatal(err)
	}
	rec := w.adminPost("/api/admin/users/alice/erase", "confirm=alice")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "team-solo") {
		t.Errorf("erase of a team's last member = %d %s, want 409 naming the team", rec.Code, rec.Body)
	}
	if n := w.count(`SELECT count(*) FROM users WHERE id = $1`, w.users["alice"]); n != 1 {
		t.Error("a refused erasure removed the account")
	}
	if rec := w.adminPost("/api/admin/teams/team-solo/delete", ""); rec.Code != http.StatusOK {
		t.Fatalf("delete team = %d %s", rec.Code, rec.Body)
	}
	if rec := w.adminPost("/api/admin/users/alice/erase", "confirm=alice"); rec.Code != http.StatusOK {
		t.Errorf("erase after the team went = %d %s", rec.Code, rec.Body)
	}
	if rec := w.adminPost("/api/admin/users/nobody/erase", "confirm=nobody"); rec.Code != http.StatusNotFound {
		t.Errorf("erase of a missing name = %d", rec.Code)
	}
}

func TestErasePersonRemovesEverything(t *testing.T) {
	w := personWithData(t)
	w.disable("alice")
	alice := w.users["alice"]
	ctx := context.Background()
	siteIDs := []string{w.siteID("alice", "tracker")}
	var oldID string
	_ = w.database.QueryRow(`SELECT id::text FROM sites WHERE user_id = $1 AND name = 'old'`, alice).Scan(&oldID)
	siteIDs = append(siteIDs, oldID)
	notes := w.siteID("vera", "notes")
	for _, stmt := range []string{
		`INSERT INTO pending_site_viewers (site_id, email) VALUES ('` + notes + `', 'alice@example.com')`,
		`INSERT INTO access_log (user_id, owner_label, site_name, path, method, status) VALUES ('` + alice + `', 'vera', 'notes', '/', 'GET', 200)`,
		`INSERT INTO access_log (user_id, owner_label, site_name, path, method, status) VALUES ('` + w.users["mo"] + `', 'vera', 'notes', '/', 'GET', 200)`,
		`INSERT INTO oauth_clients (client_id, client_name, redirect_uris) VALUES ('c1', 'Agent', '[]')`,
		`INSERT INTO oauth_grants (user_id, client_id, resource) VALUES ('` + alice + `', 'c1', 'https://x')`,
	} {
		if _, err := w.database.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	oldHost := "handed.alice." + accessBase
	if rec := w.openOld("mo", oldHost, false); rec.Code != http.StatusMovedPermanently {
		t.Fatalf("old address of the handed-over site before erasure = %d", rec.Code)
	}

	rec := w.adminPost("/api/admin/users/alice/erase", "confirm=alice")
	if rec.Code != http.StatusOK {
		t.Fatalf("erase = %d %s", rec.Code, rec.Body)
	}
	// Her old address no longer sends her links to the team's site; the
	// site itself lives on with the team.
	if rec := w.openOld("mo", oldHost, false); rec.Code != http.StatusNotFound || rec.Header().Get("Location") != "" {
		t.Errorf("old address after erasure = %d %q, want 404", rec.Code, rec.Header().Get("Location"))
	}
	if n := w.count(`SELECT count(*) FROM site_redirects WHERE owner_label = 'alice'`); n != 0 {
		t.Errorf("redirects under her label left: %d", n)
	}
	if code := w.view("mo", "team-crew", "handed", false); code != http.StatusOK {
		t.Errorf("handed-over site after erasure = %d, want served", code)
	}

	// Nothing outside the audit log names her id.
	for _, ref := range []string{
		"users.id", "sites.user_id", "sites.deleted_by", "sites.network_requested_by", "versions.uploaded_by",
		"site_assets.created_by", "site_state_history.written_by", "site_viewers.principal_id", "site_viewers.added_by",
		"team_members.user_id", "team_members.added_by", "pending_site_viewers.added_by", "pending_team_members.added_by",
		"sessions.user_id", "api_keys.user_id", "oauth_grants.user_id", "oauth_codes.user_id", "access_log.user_id",
		"team_audit.actor_id", "team_audit.subject_id", "network_access_approvals.admin_id",
	} {
		table, column, _ := strings.Cut(ref, ".")
		if n := w.count(`SELECT count(*) FROM `+table+` WHERE `+column+` = $1`, alice); n != 0 {
			t.Errorf("%s still names her: %d rows", ref, n)
		}
	}
	if n := w.count(`SELECT count(*) FROM rate_limit_counters WHERE strpos(key, $1) > 0`, alice); n != 0 {
		t.Errorf("rate-limit rows left: %d", n)
	}
	if n := w.count(`SELECT count(*) FROM pending_site_viewers WHERE email = 'alice@example.com'`); n != 0 {
		t.Errorf("pending grants naming her email left: %d", n)
	}
	if n := w.count(`SELECT count(*) FROM access_log WHERE user_id = $1`, w.users["mo"]); n != 1 {
		t.Errorf("somebody else's visits were removed: %d left, want 1", n)
	}
	for _, id := range siteIDs {
		if n := w.count(`SELECT count(*) FROM storage_retired WHERE object_key = $1`, "sites/"+id+"/"); n != 1 {
			t.Errorf("site %s files queued %d times, want 1", id, n)
		}
	}
	// The team she was in lives on with mo, and the site she deleted there
	// is still restorable by its team.
	if !w.teamExists("team-crew") || w.count(`SELECT count(*) FROM sites s JOIN users u ON u.id = s.user_id WHERE u.username = 'team-crew' AND s.deleted_at IS NOT NULL`) != 1 {
		t.Error("the team or its recently deleted site went with her")
	}

	// One user_erased row, naming her by id only.
	var detail string
	if err := w.database.QueryRow(`SELECT detail::text FROM audit_events WHERE action = 'user_erased'`).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(detail, alice) || strings.Contains(detail, "alice@") || strings.Contains(detail, `"alice"`) || !strings.Contains(detail, `"sites_deleted": 2`) {
		t.Errorf("user_erased detail = %s", detail)
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE actor_id = $1`, alice); n == 0 {
		t.Error("her earlier audit rows were removed")
	}
	report, err := audit.VerifyChain(ctx, w.database, audit.ChainExpectation{})
	if err != nil || report.Break != nil {
		t.Fatalf("audit chain after erase: %+v %v", report, err)
	}

	// Her name stays held: a new sign-in gets a suffixed name, not her links.
	_, err = db.CreateOIDCUser(ctx, w.database, "alice", "sub-new", "alice@example.com", false)
	if !isOwnerLabelViolation(err) {
		t.Errorf("new account under the erased name: %v", err)
	}
	// Signed in, the old address is the no-access page (a signed-out
	// browser is first sent to sign in, as for any site host).
	if code := w.view("vera", "alice", "tracker", false); code != http.StatusNotFound {
		t.Errorf("old address = %d, want 404", code)
	}
}

// A pending grant naming an address the provider never vouched was hers
// may be meant for whoever really holds it: erasure leaves it.
func TestEraseKeepsPendingGrantsForAnUnvouchedAddress(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("vera", "/api/sites/notes")
	w.disable("alice")
	if _, err := w.database.Exec(`UPDATE users SET email_source = 'inferred' WHERE id = $1`, w.users["alice"]); err != nil {
		t.Fatal(err)
	}
	if _, err := w.database.Exec(`INSERT INTO pending_site_viewers (site_id, email) VALUES ($1, 'alice@example.com')`, w.siteID("vera", "notes")); err != nil {
		t.Fatal(err)
	}
	if rec := w.adminPost("/api/admin/users/alice/erase", "confirm=alice"); rec.Code != http.StatusOK {
		t.Fatalf("erase = %d %s", rec.Code, rec.Body)
	}
	if n := w.count(`SELECT count(*) FROM pending_site_viewers WHERE email = 'alice@example.com'`); n != 1 {
		t.Errorf("pending grants for an unvouched address = %d, want 1 kept", n)
	}
}

// An erasure holds the person's row before their sites' names, so an
// admin's hand-over of one of their sites waits for it instead of slipping
// in between (or deadlocking).
func TestEraseAndConcurrentHandOverSerialize(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/aaa")
	w.deploy("alice", "/api/sites/sss")
	w.disable("alice")
	release := w.holdSiteName(w.users["alice"], "aaa")
	erased := make(chan *httptest.ResponseRecorder, 1)
	go func() { erased <- w.adminPost("/api/admin/users/alice/erase", "confirm=alice") }()
	if !waitFor(func() bool { return w.lockWaiters() >= 1 }) {
		release()
		t.Fatal("erasure never waited on the held site name")
	}
	moved := make(chan *httptest.ResponseRecorder, 1)
	go func() { moved <- w.adminPost("/api/admin/sites/alice/sss/transfer", "to=mo") }()
	var early *httptest.ResponseRecorder
	waitFor(func() bool {
		select {
		case early = <-moved:
			return true
		default:
			return w.lockWaiters() >= 2
		}
	})
	release()
	if rec := <-erased; rec.Code != http.StatusOK {
		t.Fatalf("erase = %d %s", rec.Code, rec.Body)
	}
	move := early
	if move == nil {
		move = <-moved
	}
	if move.Code == http.StatusOK || move.Code >= 500 {
		t.Errorf("hand-over during erasure = %d %s, want refused", move.Code, move.Body)
	}
	if n := w.count(`SELECT count(*) FROM sites WHERE name IN ('aaa', 'sss')`); n != 0 {
		t.Errorf("%d of her sites survived", n)
	}
	if n := w.liveSitesQueuedForRetirement(); n != 0 {
		t.Errorf("%d live sites queued for retirement", n)
	}
}

// An erased person still in the identity provider cannot sign straight
// back in to a fresh account, by subject or by their verified email, until
// an admin allows it.
func TestErasedPersonCannotSignBackIn(t *testing.T) {
	w := newAccessWorld(t)
	w.disable("alice")
	if rec := w.adminPost("/api/admin/users/alice/erase", "confirm=alice"); rec.Code != http.StatusOK {
		t.Fatalf("erase = %d %s", rec.Code, rec.Body)
	}
	if n := w.count(`SELECT count(*) FROM erased_identities WHERE subject_hash = $1 AND email_hash = $2 AND erased_by = $3`,
		db.ErasedSubjectHash(accessIssuer, "sub-alice"), db.ErasedEmailHash("alice@example.com"), w.users["root"]); n != 1 {
		t.Fatalf("held identities = %d, want 1", n)
	}
	h := &AuthHandler{database: w.database, audit: audit.NewDBRecorder(w.database), claims: OIDCClaimConfig{Issuer: accessIssuer}}
	ctx := context.Background()
	if _, _, err := h.resolveUser(ctx, "sub-alice", "alice.new@example.com", "", false); !errors.Is(err, db.ErrIdentityErased) {
		t.Errorf("sign-in by the erased subject = %v, want refused", err)
	}
	if _, _, err := h.resolveUser(ctx, "sub-other", "Alice@Example.com", "", false); !errors.Is(err, db.ErrIdentityErased) {
		t.Errorf("sign-in by the erased email = %v, want refused", err)
	}
	if _, _, err := h.resolveUser(ctx, "sub-alice", "alice@example.com", "", false); !errors.Is(err, db.ErrIdentityErased) {
		t.Errorf("sign-in = %v", err)
	}
	// Somebody else signs in as before.
	if _, _, err := h.resolveUser(ctx, "sub-vera", "vera@example.com", "", false); err != nil {
		t.Errorf("unrelated sign-in: %v", err)
	}

	// The admin page lists it, by fingerprint only.
	page := w.admin(http.MethodGet, "/admin").Body.String()
	if !strings.Contains(page, "Erased people") || strings.Contains(page, "alice@example.com") {
		t.Errorf("admin page erased card missing or naming her")
	}
	rec := w.admin(http.MethodGet, "/api/admin/erased-identities")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"erased_by":"root"`) {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	var id string
	_ = w.database.QueryRow(`SELECT id::text FROM erased_identities`).Scan(&id)
	if rec := w.adminPost("/api/admin/erased-identities/"+id+"/allow", ""); rec.Code != http.StatusOK {
		t.Fatalf("allow = %d %s", rec.Code, rec.Body)
	}
	if rec := w.adminPost("/api/admin/erased-identities/"+id+"/allow", ""); rec.Code != http.StatusNotFound {
		t.Errorf("allow twice = %d, want 404", rec.Code)
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'erased_identity_allowed'`); n != 1 {
		t.Errorf("erased_identity_allowed rows = %d, want 1", n)
	}
	user, notice, err := h.resolveUser(ctx, "sub-alice", "alice@example.com", "", false)
	if err != nil || user.ID == w.users["alice"] || user.Username == "alice" || notice != "username_suffixed" {
		t.Errorf("sign-in after allow = %+v %q %v, want a new, suffixed account", user, notice, err)
	}
}

// The export covers what an erasure deletes: their visits and the grants
// waiting for their email.
func TestExportIncludesVisitsAndPendingGrants(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("vera", "/api/sites/notes")
	w.newTeam("crew", "vera")
	w.disable("alice")
	for _, stmt := range []string{
		`INSERT INTO pending_site_viewers (site_id, email) VALUES ('` + w.siteID("vera", "notes") + `', 'alice@example.com')`,
		`INSERT INTO pending_team_members (team_id, email) VALUES ((SELECT id FROM users WHERE username = 'team-crew'), 'alice@example.com')`,
		`INSERT INTO access_log (user_id, owner_label, site_name, path, method, status) VALUES ('` + w.users["alice"] + `', 'vera', 'notes', '/p', 'GET', 200)`,
		`INSERT INTO access_log (user_id, owner_label, site_name, path, method, status) VALUES ('` + w.users["mo"] + `', 'vera', 'notes', '/other', 'GET', 200)`,
	} {
		if _, err := w.database.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	rec := w.admin(http.MethodGet, "/api/admin/users/alice/export")
	archive, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("export = %d: %v", rec.Code, err)
	}
	files := map[string]string{}
	for _, f := range archive.File {
		r, _ := f.Open()
		body, _ := io.ReadAll(r)
		r.Close()
		files[f.Name] = string(body)
	}
	if v := files["visits.jsonl"]; strings.Count(v, "\n") != 1 || !strings.Contains(v, `"path":"/p"`) || strings.Contains(v, "/other") {
		t.Errorf("visits.jsonl = %q", v)
	}
	if p := files["pending-grants.json"]; !strings.Contains(p, `"site":"notes"`) || !strings.Contains(p, `"team":"team-crew"`) {
		t.Errorf("pending-grants.json = %q", p)
	}
}

// An erased person's old label does not pass to a same-named pre-v1.3 team.
func TestErasedLabelDoesNotResolveToALegacyTeam(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("alice", "mo")
	w.disable("alice")
	if rec := w.adminPost("/api/admin/users/alice/erase", "confirm=alice"); rec.Code != http.StatusOK {
		t.Fatalf("erase = %d %s", rec.Code, rec.Body)
	}
	if name, ok, err := db.LegacyTeamName(context.Background(), w.database, "alice"); err != nil || ok {
		t.Errorf("legacy team for the erased label = %q %v %v, want none", name, ok, err)
	}
	if name, ok, _ := db.LegacyTeamName(context.Background(), w.database, "nobody"); ok {
		t.Errorf("legacy team for an unheld label = %q", name)
	}
}
