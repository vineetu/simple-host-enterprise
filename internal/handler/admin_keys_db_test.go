package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
)

// mintTestKey stores a key for user the way the mint route does (hash, hash
// prefix, the key's own last four) and returns the plaintext.
func (w *accessWorld) mintTestKey(user, name string, expires time.Time) string {
	w.t.Helper()
	key := "shk_" + strings.Repeat("ab", 30) + name[:4]
	if _, err := db.CreateAPIKey(context.Background(), w.database, w.users[user], name, db.HashAPIKey(key), db.KeyPrefix(db.HashAPIKey(key)), key[len(key)-4:], expires, db.APIKeyScopeFull); err != nil {
		w.t.Fatal(err)
	}
	return key
}

func (w *accessWorld) withKey(key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "https://"+accessBase+"/api/sites", nil)
	r.Header.Set("X-API-Key", key)
	r.Header.Set("X-Simple-Host-Client", "api")
	return w.do(r)
}

func (w *accessWorld) adminJSON(user, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://"+accessBase+path, strings.NewReader(body))
	r.AddCookie(w.cookie(user, ""))
	r.Header.Set("Origin", "https://"+accessBase)
	r.Header.Set("X-Simple-Host-Client", "control-ui")
	r.Header.Set("Content-Type", "application/json")
	return w.do(r)
}

// An admin pastes a leaked key: that key alone stops working, the owner's
// other key and sign-in are untouched, the audit row names the owner, and
// the key is never echoed back.
func TestAdminRevokesLeakedKey(t *testing.T) {
	w := newAccessWorld(t)
	leaked := w.mintTestKey("alice", "gist", time.Now().Add(30*24*time.Hour))
	other := w.mintTestKey("alice", "laptop", time.Now().Add(30*24*time.Hour))

	body, _ := json.Marshal(map[string]string{"key": leaked})
	if rec := w.adminJSON("vera", "/api/admin/keys/revoke", string(body)); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin revoke = %d %s, want 403", rec.Code, rec.Body)
	}
	rec := w.adminJSON("root", "/api/admin/keys/revoke", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin revoke = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), leaked) || strings.Contains(rec.Body.String(), leaked[4:20]) {
		t.Fatalf("response echoes the key: %s", rec.Body)
	}
	var out revokedKeyResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Status != "revoked" || out.Owner != "alice" || out.Name != "gist" || out.Label != "ends …"+leaked[len(leaked)-4:] {
		t.Fatalf("revoke answer = %+v", out)
	}
	if got := w.withKey(leaked); got.Code != http.StatusUnauthorized || !containsCode(got.Body.Bytes(), "key_revoked") {
		t.Fatalf("leaked key after revoke = %d %s, want 401 key_revoked", got.Code, got.Body)
	}
	if got := w.withKey(other); got.Code != http.StatusOK {
		t.Fatalf("owner's other key = %d %s, want 200", got.Code, got.Body)
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'admin_key_revoke' AND detail->>'owner' = 'alice' AND detail->>'key_name' = 'gist'`); n != 1 {
		var raw string
		_ = w.database.QueryRow(`SELECT detail::text FROM audit_events WHERE action = 'admin_key_revoke'`).Scan(&raw)
		t.Fatalf("admin_key_revoke rows naming alice = %d (detail %s)", n, raw)
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE detail::text LIKE '%' || $1 || '%'`, leaked[4:]); n != 0 {
		t.Fatal("the key itself reached the audit log")
	}

	again := w.adminJSON("root", "/api/admin/keys/revoke", string(body))
	_ = json.Unmarshal(again.Body.Bytes(), &out)
	if again.Code != http.StatusOK || out.Status != "already revoked" {
		t.Fatalf("second revoke = %d %s", again.Code, again.Body)
	}
	if rec := w.adminJSON("root", "/api/admin/keys/revoke", `{"key":"shk_nothing"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown key = %d, want 404", rec.Code)
	}
}

// A refused key says why; a key close to expiry says so on every success.
func TestKeyRefusalSaysWhy(t *testing.T) {
	w := newAccessWorld(t)
	soon := w.mintTestKey("alice", "soon", time.Now().Add(3*24*time.Hour))
	later := w.mintTestKey("alice", "later", time.Now().Add(60*24*time.Hour))
	expired := w.mintTestKey("alice", "past", time.Now().Add(-time.Hour))
	disabled := w.mintTestKey("olly", "olly", time.Now().Add(60*24*time.Hour))

	rec := w.withKey(soon)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Key-Expires") == "" || !strings.Contains(rec.Header().Get("X-Simple-Host-Notice"), "expires on") {
		t.Fatalf("key expiring soon = %d, headers %v", rec.Code, rec.Header())
	}
	if rec := w.withKey(later); rec.Code != http.StatusOK || rec.Header().Get("X-Key-Expires") != "" {
		t.Fatalf("key with months left = %d, X-Key-Expires %q", rec.Code, rec.Header().Get("X-Key-Expires"))
	}
	rec = w.withKey(expired)
	if rec.Code != http.StatusUnauthorized || !containsCode(rec.Body.Bytes(), "key_expired") || !strings.Contains(rec.Body.String(), "mint a new one on the dashboard") {
		t.Fatalf("expired key = %d %s", rec.Code, rec.Body)
	}
	w.disable("olly")
	if rec := w.withKey(disabled); !containsCode(rec.Body.Bytes(), "key_owner_disabled") {
		t.Fatalf("disabled owner's key = %d %s", rec.Code, rec.Body)
	}
	if rec := w.withKey("shk_" + strings.Repeat("0", 64)); rec.Code != http.StatusUnauthorized || !containsCode(rec.Body.Bytes(), "key_not_recognised") {
		t.Fatalf("unknown key = %d %s", rec.Code, rec.Body)
	}
}
