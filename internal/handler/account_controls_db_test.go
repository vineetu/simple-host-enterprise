package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// connectApp gives userID a connected app named name, with one live access
// token, and returns the grant id.
func connectApp(t *testing.T, database *sql.DB, userID, clientID, name string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := db.GetOAuthClient(ctx, database, clientID); err != nil {
		if err := db.InsertOAuthClient(ctx, database, db.OAuthClient{ClientID: clientID, Name: name, RedirectURIs: []string{"https://app.example/cb"}, TokenEndpointAuthMethod: "none"}); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	grantID, _, err := db.InsertOAuthGrant(ctx, tx, userID, clientID, "https://example.com/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertOAuthToken(ctx, tx, []byte("token-"+grantID), grantID, "access", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return grantID
}

// A person lists their connected apps and disconnects one; another
// person's app is not theirs to see or disconnect. Audited.
func TestConnectedAppsListAndDisconnect(t *testing.T) {
	database := connectorTestDB(t)
	ctx := context.Background()
	alice, err := db.CreateOIDCUser(ctx, database, "alice", "sub-alice", "alice@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := db.CreateOIDCUser(ctx, database, "bob", "sub-bob", "bob@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	aliceGrant := connectApp(t, database, alice.ID, "client-1", "Team Chat")
	bobGrant := connectApp(t, database, bob.ID, "client-1", "Team Chat")

	h := NewKeysHandler(database, audit.NewDBRecorder(database), HostModel{}, "https://example.com")
	as := func(r *http.Request) *http.Request {
		return r.WithContext(auth.ContextWithTestAuth(r.Context(), &alice, "s1", ""))
	}
	rec := httptest.NewRecorder()
	h.listConnections(rec, as(httptest.NewRequest(http.MethodGet, "/api/me/connections", nil)))
	var listed []connectionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	if len(listed) != 1 || listed[0].ID != aliceGrant || listed[0].Name != "Team Chat" || listed[0].ConnectedAt == "" || listed[0].LastUsedAt == "" {
		t.Fatalf("list = %+v, want alice's one app", listed)
	}

	disconnect := func(id string) int {
		r := as(httptest.NewRequest(http.MethodDelete, "/api/me/connections/"+id, nil))
		r.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.disconnect(rec, r)
		return rec.Code
	}
	if code := disconnect(bobGrant); code != http.StatusNotFound {
		t.Fatalf("disconnect someone else's app = %d, want 404", code)
	}
	if code := disconnect("not-a-uuid"); code != http.StatusNotFound {
		t.Fatalf("disconnect a bad id = %d, want 404", code)
	}
	if code := disconnect(aliceGrant); code != http.StatusOK {
		t.Fatalf("disconnect = %d", code)
	}
	if n := countRows(t, database, `SELECT count(*) FROM oauth_grants WHERE id = $1`, aliceGrant); n != 0 {
		t.Fatal("alice's grant survived disconnect")
	}
	if n := countRows(t, database, `SELECT count(*) FROM oauth_tokens WHERE grant_id = $1`, aliceGrant); n != 0 {
		t.Fatal("alice's app token survived disconnect")
	}
	if n := countRows(t, database, `SELECT count(*) FROM oauth_grants WHERE id = $1`, bobGrant); n != 1 {
		t.Fatal("bob's grant was touched")
	}
	if n := countRows(t, database, `SELECT count(*) FROM audit_events WHERE action = 'connector_revoke' AND actor_id = $1 AND detail->>'app' = 'Team Chat'`, alice.ID); n != 1 {
		t.Fatalf("connector_revoke audit rows = %d, want 1", n)
	}

	// The sessions page lists what is left, with the sign-out-everywhere form.
	connectApp(t, database, alice.ID, "client-2", "Chat <b>app</b>")
	page := httptest.NewRecorder()
	(&AuthHandler{database: database}).listSessions(page, as(httptest.NewRequest(http.MethodGet, "/auth/sessions", nil)))
	body := page.Body.String()
	for _, want := range []string{"Connected apps", "Chat &lt;b&gt;app&lt;/b&gt;", `class="btn-reject disconnect-app"`, `action="/auth/sessions/revoke-all"`, `name="credentials" value="1" checked`} {
		if !strings.Contains(body, want) {
			t.Errorf("sessions page lacks %q", want)
		}
	}
}

// Sign out everywhere revokes every session and, with the box ticked, every
// key and connected app, in one audited transaction; nobody else's.
func TestSignOutEverywhere(t *testing.T) {
	database := connectorTestDB(t)
	ctx := context.Background()
	alice, _ := db.CreateOIDCUser(ctx, database, "alice", "sub-alice", "alice@example.com", false)
	bob, _ := db.CreateOIDCUser(ctx, database, "bob", "sub-bob", "bob@example.com", false)
	for _, u := range []db.User{alice, alice, bob} {
		if _, err := db.CreateSession(ctx, database, u.ID, time.Now().Add(time.Hour), "", "test"); err != nil {
			t.Fatal(err)
		}
	}
	for i, u := range []db.User{alice, bob} {
		key := "shk_" + strings.Repeat(string(rune('a'+i)), 40)
		if _, err := db.CreateAPIKey(ctx, database, u.ID, "ci", db.HashAPIKey(key), key[:8], time.Now().Add(time.Hour), db.APIKeyScopePublish); err != nil {
			t.Fatal(err)
		}
	}
	connectApp(t, database, alice.ID, "client-1", "Team Chat")
	connectApp(t, database, bob.ID, "client-1", "Team Chat")

	h := &AuthHandler{database: database, audit: audit.NewDBRecorder(database)}
	signOut := func(form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/auth/sessions/revoke-all", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r = r.WithContext(auth.ContextWithTestAuth(r.Context(), &alice, "s1", ""))
		rec := httptest.NewRecorder()
		h.signOutEverywhere(rec, r)
		return rec
	}

	// Box cleared: sessions only.
	if rec := signOut(url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("sign out everywhere = %d %s", rec.Code, rec.Body)
	}
	live := func(table, userID string) int {
		return countRows(t, database, `SELECT count(*) FROM `+table+` WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	}
	if live("sessions", alice.ID) != 0 || live("api_keys", alice.ID) != 1 || countRows(t, database, `SELECT count(*) FROM oauth_grants WHERE user_id = $1`, alice.ID) != 1 {
		t.Fatal("without the box: want sessions revoked, key and app kept")
	}

	// Box ticked (the default): keys and apps too.
	if _, err := db.CreateSession(ctx, database, alice.ID, time.Now().Add(time.Hour), "", "test"); err != nil {
		t.Fatal(err)
	}
	rec := signOut(url.Values{"credentials": {"1"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("sign out everywhere = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	if c := rec.Result().Cookies(); len(c) != 1 || c[0].Name != auth.SessionCookieName || c[0].MaxAge >= 0 {
		t.Fatalf("cookies = %v, want the session cookie cleared", c)
	}
	if live("sessions", alice.ID) != 0 || live("api_keys", alice.ID) != 0 || countRows(t, database, `SELECT count(*) FROM oauth_grants WHERE user_id = $1`, alice.ID) != 0 {
		t.Fatal("with the box: want sessions, keys and apps all gone")
	}
	if live("sessions", bob.ID) != 1 || live("api_keys", bob.ID) != 1 || countRows(t, database, `SELECT count(*) FROM oauth_grants WHERE user_id = $1`, bob.ID) != 1 {
		t.Fatal("bob's credentials were touched")
	}
	if got := countRows(t, database, `SELECT count(*) FROM audit_events WHERE action = 'sign_out_everywhere' AND actor_id = $1 AND detail->>'keys_and_apps' = 'true'`, alice.ID); got != 1 {
		t.Fatalf("audited sign_out_everywhere with keys_and_apps = %d, want 1", got)
	}

	// Audit sink down: nothing is revoked.
	if _, err := db.CreateSession(ctx, database, alice.ID, time.Now().Add(time.Hour), "", "test"); err != nil {
		t.Fatal(err)
	}
	h.audit = &failingRecorder{}
	if rec := signOut(url.Values{"credentials": {"1"}}); rec.Code != http.StatusInternalServerError || live("sessions", alice.ID) != 1 {
		t.Fatalf("with the audit sink down = %d, live sessions %d; want 500 and the session kept", rec.Code, live("sessions", alice.ID))
	}
}

// Each sign-in refreshes the stored email from the verified claim, unless
// another person already holds that address.
func TestSignInRefreshesEmail(t *testing.T) {
	database := connectorTestDB(t)
	ctx := context.Background()
	alice, _ := db.CreateOIDCUser(ctx, database, "alice", "sub-alice", "alice@corp.example", false)
	if _, err := db.CreateOIDCUser(ctx, database, "bob", "sub-bob", "Shared@corp.example", false); err != nil {
		t.Fatal(err)
	}
	h := &AuthHandler{database: database, audit: audit.NewDBRecorder(database)}
	email := func() string {
		var e string
		if err := database.QueryRow(`SELECT email FROM users WHERE id = $1`, alice.ID).Scan(&e); err != nil {
			t.Fatal(err)
		}
		return e
	}

	user, _, err := h.resolveUser(ctx, "sub-alice", "priya@corp.example", "", false)
	if err != nil || user.ID != alice.ID || user.Email != "priya@corp.example" || email() != "priya@corp.example" {
		t.Fatalf("resolve = %+v, %v; stored %q; want the new address stored", user, err, email())
	}
	if n := countRows(t, database, `SELECT count(*) FROM audit_events WHERE action = 'email_change' AND actor_id = $1 AND detail->>'from' = 'alice@corp.example' AND detail->>'to' = 'priya@corp.example'`, alice.ID); n != 1 {
		t.Fatalf("email_change audit rows = %d, want 1", n)
	}
	// The same address again changes nothing.
	if _, _, err := h.resolveUser(ctx, "sub-alice", "priya@corp.example", "", false); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, database, `SELECT count(*) FROM audit_events WHERE action LIKE 'email_change%'`); n != 1 {
		t.Fatalf("email audit rows after an unchanged sign-in = %d, want 1", n)
	}

	// An address another person holds (any case) is not taken over.
	user, _, err = h.resolveUser(ctx, "sub-alice", "shared@corp.example", "", false)
	if err != nil || user.ID != alice.ID || email() != "priya@corp.example" {
		t.Fatalf("resolve = %+v, %v; stored %q; want the old address kept", user, err, email())
	}
	if n := countRows(t, database, `SELECT count(*) FROM audit_events WHERE action = 'email_change_skipped' AND detail->>'held_by' = 'bob'`); n != 1 {
		t.Fatalf("email_change_skipped audit rows = %d, want 1", n)
	}
	people, err := db.PeopleByEmail(ctx, database, "shared@corp.example")
	if err != nil || len(people) != 1 || people[0].Username != "bob" {
		t.Fatalf("offboarding by the shared address finds %+v, %v; want only bob", people, err)
	}
}

// adminForm posts a dashboard form to an admin route as root.
func (w *accessWorld) adminForm(path string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://"+accessBase+path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(w.cookie("root", ""))
	r.Header.Set("Origin", "https://"+accessBase)
	r.Header.Set("X-Simple-Host-Client", "control-ui")
	return w.do(r)
}

func (w *accessWorld) decision(owner, site string) map[string]any {
	w.t.Helper()
	rec := w.api(owner, http.MethodGet, "/api/collaboration/sites/"+owner+"/"+site, nil)
	if rec.Code != http.StatusOK {
		w.t.Fatalf("get site = %d %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	d, _ := out["access_decision"].(map[string]any)
	return d
}

func (w *accessWorld) siteAccess(owner, site string) string {
	w.t.Helper()
	var access string
	if err := w.database.QueryRow(`SELECT s.access FROM sites s JOIN users u ON u.id = s.user_id WHERE u.username = $1 AND s.name = $2`, owner, site).Scan(&access); err != nil {
		w.t.Fatal(err)
	}
	return access
}

// An admin restricts any site to only-me with a reason the owner sees;
// lifting restores its level; the owner cannot lift it.
func TestAdminRestrictSite(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/mine")
	w.setAccess("alice", "/api/sites/mine/access", map[string]any{"level": "listed"}, http.StatusOK)
	if code := w.view("vera", "alice", "mine", false); code != http.StatusOK {
		t.Fatalf("colleague view of a listed site = %d", code)
	}

	if rec := w.adminForm("/api/admin/sites/alice/mine/restrict", url.Values{"reason": {"  "}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("restrict without a reason = %d, want 400", rec.Code)
	}
	if rec := w.adminForm("/api/admin/sites/alice/nope/restrict", url.Values{"reason": {"x"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("restrict a missing site = %d, want 404", rec.Code)
	}
	if rec := w.adminForm("/api/admin/sites/alice/mine/restrict", url.Values{"reason": {"exposes\ncustomer data"}}); rec.Code != http.StatusOK {
		t.Fatalf("restrict = %d %s", rec.Code, rec.Body)
	}
	if got := w.siteAccess("alice", "mine"); got != db.AccessOnlyMe {
		t.Fatalf("access after restrict = %s", got)
	}
	if code := w.view("vera", "alice", "mine", false); code == http.StatusOK {
		t.Fatal("a colleague still opens a restricted site")
	}
	if d := w.decision("alice", "mine"); d["decision"] != "restricted" || d["reason"] != "exposes customer data" || d["at"] == nil {
		t.Fatalf("owner sees access_decision %v", d)
	}
	page := w.admin(http.MethodGet, "/admin").Body.String()
	if !strings.Contains(page, "/api/admin/sites/alice/mine/unrestrict") || !strings.Contains(page, `title="exposes customer data">restricted`) {
		t.Fatal("admin page does not show the restriction with Lift")
	}
	if rec := w.admin(http.MethodPost, "/api/admin/sites/alice/mine/unrestrict"); rec.Code != http.StatusOK {
		t.Fatalf("unrestrict = %d %s", rec.Code, rec.Body)
	}
	if got := w.siteAccess("alice", "mine"); got != db.AccessListed {
		t.Fatalf("access after lift = %s, want listed back", got)
	}
	if d := w.decision("alice", "mine"); d != nil {
		t.Fatalf("decision after lift = %v", d)
	}
	if rec := w.admin(http.MethodPost, "/api/admin/sites/alice/mine/unrestrict"); rec.Code != http.StatusConflict {
		t.Fatalf("unrestrict an unrestricted site = %d, want 409", rec.Code)
	}

	// Restricted twice, the lift still restores the level before the first.
	// The restriction is sticky: the owner can neither raise the level, ask
	// for network access nor add viewers; only_me is still accepted and
	// leaves it in place. Only an admin lifts it.
	w.adminForm("/api/admin/sites/alice/mine/restrict", url.Values{"reason": {"one"}})
	w.adminForm("/api/admin/sites/alice/mine/restrict", url.Values{"reason": {"two"}})
	var previous string
	_ = w.database.QueryRow(`SELECT access_decision_previous FROM sites WHERE name = 'mine'`).Scan(&previous)
	if previous != db.AccessListed {
		t.Fatalf("previous level after two restrictions = %q, want listed", previous)
	}
	for _, body := range []map[string]any{{"level": "company"}, {"level": "specific"}, {"level": "network", "reason": "event"}} {
		rec := w.api("alice", http.MethodPost, "/api/sites/mine/access", body)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"site_restricted_by_admin"`) || !strings.Contains(rec.Body.String(), `"reason":"two"`) {
			t.Fatalf("owner sets %v on a restricted site = %d %s, want 409 site_restricted_by_admin", body, rec.Code, rec.Body)
		}
	}
	if rec := w.api("alice", http.MethodPost, "/api/collaboration/sites/alice/mine/viewers", map[string]any{"usernames": []string{"vera"}}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "site_restricted_by_admin") {
		t.Fatalf("owner adds a viewer to a restricted site = %d %s", rec.Code, rec.Body)
	}
	w.setAccess("alice", "/api/sites/mine/access", map[string]any{"level": "only_me"}, http.StatusOK)
	if d := w.decision("alice", "mine"); d == nil || d["decision"] != "restricted" {
		t.Fatalf("decision after the owner chose only_me = %v, want still restricted", d)
	}
	if got := w.siteAccess("alice", "mine"); got != db.AccessOnlyMe {
		t.Fatalf("access while restricted = %s", got)
	}
	if rec := w.admin(http.MethodPost, "/api/admin/sites/alice/mine/unrestrict"); rec.Code != http.StatusOK {
		t.Fatalf("second unrestrict = %d %s", rec.Code, rec.Body)
	}
	w.setAccess("alice", "/api/sites/mine/access", map[string]any{"level": "company"}, http.StatusOK)
	if d := w.decision("alice", "mine"); d != nil {
		t.Fatalf("decision after the lift and the owner's choice = %v", d)
	}
	if n := countRows(t, w.database, `SELECT count(*) FROM audit_events WHERE action = 'site_restricted' AND detail->>'reason' IS NOT NULL`); n != 3 {
		t.Fatalf("site_restricted audit rows = %d, want 3", n)
	}
	if n := countRows(t, w.database, `SELECT count(*) FROM audit_events WHERE action = 'site_restriction_lifted' AND detail->>'to' = 'listed'`); n != 2 {
		t.Fatalf("site_restriction_lifted audit rows = %d, want 2", n)
	}
	// Not for a non-admin.
	r := httptest.NewRequest(http.MethodPost, "https://"+accessBase+"/api/admin/sites/alice/mine/restrict", strings.NewReader("reason=x"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(w.cookie("vera", ""))
	r.Header.Set("Origin", "https://"+accessBase)
	r.Header.Set("X-Simple-Host-Client", "control-ui")
	if rec := w.do(r); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin restrict = %d, want 403", rec.Code)
	}
}

// A declined or revoked network request stays visible to the owner, with
// the admin's note, until the next request.
func TestNetworkDecisionShownUntilNextRequest(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/mine")
	w.setAccess("alice", "/api/sites/mine/access", map[string]any{"level": "network", "reason": "partners"}, http.StatusAccepted)
	if rec := w.adminForm("/api/admin/access-requests/alice/mine/decline", url.Values{"reason": {"use company access"}}); rec.Code != http.StatusOK {
		t.Fatalf("decline = %d %s", rec.Code, rec.Body)
	}
	if d := w.decision("alice", "mine"); d["decision"] != "declined" || d["reason"] != "use company access" {
		t.Fatalf("after decline: %v", d)
	}
	w.setAccess("alice", "/api/sites/mine/access", map[string]any{"level": "company"}, http.StatusOK)
	if d := w.decision("alice", "mine"); d["decision"] != "declined" {
		t.Fatalf("a level change cleared the decline: %v", d)
	}
	w.setAccess("alice", "/api/sites/mine/access", map[string]any{"level": "network", "reason": "again"}, http.StatusAccepted)
	if d := w.decision("alice", "mine"); d != nil {
		t.Fatalf("a new request kept the old decision: %v", d)
	}
	if rec := w.adminAs("ada", http.MethodPost, "/api/admin/access-requests/alice/mine/approve"); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body)
	}
	if rec := w.adminForm("/api/admin/access-requests/alice/mine/revoke", url.Values{}); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}
	if d := w.decision("alice", "mine"); d["decision"] != "revoked" || d["reason"] != nil {
		t.Fatalf("after a revoke without a note: %v", d)
	}
	if got := w.siteAccess("alice", "mine"); got != db.AccessCompany {
		t.Fatalf("access after revoke = %s", got)
	}
	if n := countRows(t, w.database, `SELECT count(*) FROM audit_events WHERE action = 'network_access_declined' AND detail->>'reason' = 'use company access'`); n != 1 {
		t.Fatalf("declined audit with reason = %d, want 1", n)
	}
	// The admin page asks for the note.
	w.setAccess("alice", "/api/sites/mine/access", map[string]any{"level": "network", "reason": "third"}, http.StatusAccepted)
	if page := w.admin(http.MethodGet, "/admin").Body.String(); !strings.Contains(page, `<input type="hidden" name="reason"><button type="submit" class="btn-reject">Decline</button>`) {
		t.Fatal("decline form carries no note field")
	}
}

// Minting a key never reloads the page: the key stays until dismissed and
// the row is added in place.
func TestDashboardMintKeepsTheKeyOnScreen(t *testing.T) {
	start := strings.Index(dashboardScript, "mintButton.addEventListener('click'")
	end := strings.Index(dashboardScript, "if (list) {")
	if start < 0 || end < start {
		t.Fatal("mint handler not found")
	}
	mint := dashboardScript[start:end]
	if strings.Contains(mint, "location.reload") {
		t.Fatal("the mint handler reloads the page, which hides the key before it can be copied")
	}
	for _, want := range []string{"showMinted(res.body.api_key, payload)", "addKeyRow(res.body)"} {
		if !strings.Contains(mint, want) {
			t.Errorf("mint handler lacks %q", want)
		}
	}
	for _, want := range []string{"done.id = 'mint-done'", "list.insertBefore(row, list.firstChild)", "pre.textContent = text"} {
		if !strings.Contains(dashboardScript, want) {
			t.Errorf("dashboard script lacks %q", want)
		}
	}
}
