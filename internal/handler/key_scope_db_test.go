package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// addKey mints a key of scope for user straight into the database and
// registers it in w.apiKeys under name, so w.api(name, ...) presents it.
func (w *accessWorld) addKey(name, user, scope string) string {
	w.t.Helper()
	key := "key-" + name + "-" + strings.Repeat("1", 40)
	created, err := db.CreateAPIKey(context.Background(), w.database, w.users[user], name, db.HashAPIKey(key), key[:8], time.Now().Add(time.Hour), scope)
	if err != nil {
		w.t.Fatal(err)
	}
	w.apiKeys[name] = key
	return created.ID
}

func TestAPIKeyScopes(t *testing.T) {
	w := newAccessWorld(t)
	w.addKey("alice-publish", "alice", db.APIKeyScopePublish)
	w.addKey("root-offboard", "root", db.APIKeyScopeOffboard)

	// Publish: deploy, update, list, versions, rollback, and the
	// site's saved data and assets through the host gate.
	w.deploy("alice-publish", "/api/sites/demo")
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/sites"},
		{http.MethodGet, "/api/sites/demo/versions"},
		{http.MethodGet, "/api/collaboration/sites"},
		{http.MethodGet, "/api/collaboration/sites/alice/demo"},
	} {
		if rec := w.api("alice-publish", c.method, c.path, nil); rec.Code != http.StatusOK {
			t.Errorf("publish key %s %s = %d %s, want 200", c.method, c.path, rec.Code, rec.Body)
		}
	}
	state := func(key, method, body string) int {
		r := httptest.NewRequest(method, "https://alice."+accessBase+"/api/sites/demo/state", strings.NewReader(body))
		r.Header.Set("X-API-Key", w.apiKeys[key])
		r.Header.Set("Content-Type", "application/json")
		return w.do(r).Code
	}
	if got := state("alice-publish", http.MethodPut, `{"n":1}`); got != http.StatusOK {
		t.Errorf("publish key PUT state = %d, want 200", got)
	}
	if got := state("alice-publish", http.MethodGet, ""); got != http.StatusOK {
		t.Errorf("publish key GET state = %d, want 200", got)
	}
	if got := state("root-offboard", http.MethodGet, ""); got != http.StatusForbidden {
		t.Errorf("offboard key GET state = %d, want 403", got)
	}

	// Not publish: delete, access, viewers, teams, audit, search, admin.
	for _, c := range []struct{ method, path string }{
		{http.MethodDelete, "/api/sites/demo"},
		{http.MethodDelete, "/api/collaboration/sites/alice/demo"},
		{http.MethodPost, "/api/sites/demo/access"},
		{http.MethodGet, "/api/teams"},
		{http.MethodGet, "/api/audit"},
		{http.MethodGet, "/api/admin/export"},
		{http.MethodPost, "/api/admin/users/disable"},
	} {
		rec := w.api("alice-publish", c.method, c.path, map[string]any{"level": "company"})
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"scope":"publish"`) {
			t.Errorf("publish key %s %s = %d %s, want 403 naming the scope", c.method, c.path, rec.Code, rec.Body)
		}
	}
	// A full key reaches them, except administration.
	if rec := w.api("alice", http.MethodGet, "/api/teams", nil); rec.Code != http.StatusOK {
		t.Errorf("full key GET /api/teams = %d, want 200", rec.Code)
	}
	for _, path := range []string{"/api/admin/export?kind=audit&format=jsonl", "/api/admin/users/disable"} {
		method := http.MethodGet
		if strings.HasSuffix(path, "/disable") {
			method = http.MethodPost
		}
		if rec := w.api("root", method, path, map[string]string{"email": "vera@example.com"}); rec.Code != http.StatusForbidden {
			t.Errorf("admin's full key %s %s = %d, want 403", method, path, rec.Code)
		}
	}
	// An offboard key reaches its one route and nothing else (/api/me is in
	// internal/mcp's test, whose router has it).
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/sites"},
		{http.MethodPost, "/api/admin/users/" + "vera" + "/disable"},
	} {
		if rec := w.api("root-offboard", c.method, c.path, nil); rec.Code != http.StatusForbidden {
			t.Errorf("offboard key %s %s = %d, want 403", c.method, c.path, rec.Code)
		}
	}
	if rec := w.api("alice-publish", http.MethodDelete, "/api/sites/demo", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("site survived? delete by publish key = %d", rec.Code)
	}
	if rec := w.api("alice", http.MethodDelete, "/api/sites/demo", nil); rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Errorf("full key DELETE site = %d %s", rec.Code, rec.Body)
	}
}

func TestMintKeyScope(t *testing.T) {
	w := newAccessWorld(t)
	hosts, err := NewHostModel("https://" + accessBase)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewKeysHandler(w.database, audit.NewDBRecorder(w.database), hosts, "https://"+accessBase).Register(mux, auth.Middleware(w.database, w.keys, time.Hour))
	mint := func(user string, body string) (int, map[string]any) {
		r := httptest.NewRequest(http.MethodPost, "https://"+accessBase+"/api/keys", strings.NewReader(body))
		r.AddCookie(w.cookie(user, ""))
		r.Header.Set("Origin", "https://"+accessBase)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, out := mint("alice", `{"name":"ci"}`); code != http.StatusCreated || out["scope"] != "publish" {
		t.Errorf("default mint = %d %v, want 201 publish", code, out)
	}
	if code, out := mint("alice", `{"name":"ci","scope":"full"}`); code != http.StatusCreated || out["scope"] != "full" {
		t.Errorf("full mint = %d %v", code, out)
	}
	if code, _ := mint("alice", `{"scope":"offboard"}`); code != http.StatusForbidden {
		t.Errorf("offboard mint by a non-admin = %d, want 403", code)
	}
	if code, _ := mint("alice", `{"scope":"admin"}`); code != http.StatusBadRequest {
		t.Errorf("unknown scope = %d, want 400", code)
	}
	if code, out := mint("root", `{"name":"hr","scope":"offboard"}`); code != http.StatusCreated || out["scope"] != "offboard" {
		t.Errorf("offboard mint by an admin = %d %v", code, out)
	}
	r := httptest.NewRequest(http.MethodGet, "https://"+accessBase+"/api/keys", nil)
	r.AddCookie(w.cookie("alice", ""))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if !strings.Contains(rec.Body.String(), `"scope":"publish"`) || !strings.Contains(rec.Body.String(), `"scope":"full"`) {
		t.Errorf("key list does not show scopes: %s", rec.Body)
	}
}

func TestOffboardByEmail(t *testing.T) {
	w := newAccessWorld(t)
	ctx := context.Background()
	offboardKeyID := w.addKey("root-offboard", "root", db.APIKeyScopeOffboard)
	// Vera's credentials: a session, a key and a connected app.
	veraCookie := w.cookie("vera", "")
	if rec := w.api("vera", http.MethodGet, "/api/sites", nil); rec.Code != http.StatusOK {
		t.Fatalf("vera's key before = %d", rec.Code)
	}
	if _, err := w.database.Exec(`INSERT INTO oauth_clients (client_id, client_name, redirect_uris) VALUES ('c1', 'app', '[]')`); err != nil {
		t.Fatal(err)
	}
	tx, _ := w.database.Begin()
	if _, _, err := db.InsertOAuthGrant(ctx, tx, w.users["vera"], "c1", "https://"+accessBase+"/mcp"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	offboard := func(key, email string) (int, map[string]any) {
		body, _ := json.Marshal(map[string]string{"email": email})
		r := httptest.NewRequest(http.MethodPost, "https://"+accessBase+"/api/admin/users/disable", bytes.NewReader(body))
		r.Header.Set("X-API-Key", w.apiKeys[key])
		r.Header.Set("Content-Type", "application/json")
		rec := w.do(r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	if code, _ := offboard("root-offboard", "nobody@example.com"); code != http.StatusNotFound {
		t.Errorf("unknown email = %d, want 404", code)
	}
	if code, out := offboard("root-offboard", "Vera@Example.com"); code != http.StatusOK || out["status"] != "disabled" {
		t.Fatalf("offboard = %d %v", code, out)
	}
	if code, out := offboard("root-offboard", "vera@example.com"); code != http.StatusOK || out["status"] != "already disabled" {
		t.Errorf("offboard again = %d %v, want 200 already disabled", code, out)
	}

	// Everything Vera held stopped working.
	if rec := w.api("vera", http.MethodGet, "/api/sites", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("vera's key after = %d, want 401", rec.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "https://"+accessBase+"/api/sites", nil)
	r.AddCookie(veraCookie)
	if rec := w.do(r); rec.Code != http.StatusUnauthorized {
		t.Errorf("vera's session after = %d, want 401", rec.Code)
	}
	var live, grants, audited int
	_ = w.database.QueryRow(`SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, w.users["vera"]).Scan(&live)
	_ = w.database.QueryRow(`SELECT count(*) FROM oauth_grants WHERE user_id = $1`, w.users["vera"]).Scan(&grants)
	if err := w.database.QueryRow(`SELECT count(*) FROM audit_events WHERE action = 'admin_disable_user'
		AND actor_id = $1 AND actor_kind = 'key' AND key_id = $2 AND detail->>'subject_id' = $3`,
		w.users["root"], offboardKeyID, w.users["vera"]).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if live != 0 || grants != 0 || audited != 1 {
		t.Errorf("after offboarding: live sessions %d, grants %d, audit rows %d; want 0, 0, 1", live, grants, audited)
	}

	// An admin's browser session works too, with the Origin check.
	body := strings.NewReader(`{"email":"olly@example.com"}`)
	r = httptest.NewRequest(http.MethodPost, "https://"+accessBase+"/api/admin/users/disable", body)
	r.AddCookie(w.cookie("root", ""))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://evil.example")
	if rec := w.do(r); rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin session offboard = %d, want 403", rec.Code)
	}
	r = httptest.NewRequest(http.MethodPost, "https://"+accessBase+"/api/admin/users/disable", strings.NewReader(`{"email":"olly@example.com"}`))
	r.AddCookie(w.cookie("root", ""))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://"+accessBase)
	if rec := w.do(r); rec.Code != http.StatusOK {
		t.Errorf("session offboard = %d %s", rec.Code, rec.Body)
	}
	// The per-username route writes its audit row in the same transaction.
	if rec := w.admin(http.MethodPost, "/api/admin/users/mo/disable"); rec.Code != http.StatusOK {
		t.Fatalf("disable mo = %d %s", rec.Code, rec.Body)
	}
	if err := w.database.QueryRow(`SELECT count(*) FROM audit_events WHERE action = 'admin_disable_user'
		AND actor_kind = 'person' AND detail->>'subject_id' = $1`, w.users["mo"]).Scan(&audited); err != nil || audited != 1 {
		t.Errorf("audit rows for disabling mo = %d (%v), want 1", audited, err)
	}
	// A non-admin's offboard key (only reachable by hand-written SQL) is
	// refused by RequireAdmin.
	w.addKey("alice-offboard", "alice", db.APIKeyScopeOffboard)
	if code, _ := offboard("alice-offboard", "root@example.com"); code != http.StatusForbidden {
		t.Errorf("non-admin offboard key = %d, want 403", code)
	}
}
