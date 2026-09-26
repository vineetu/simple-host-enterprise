package handler

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

func countRows(t *testing.T, database *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := database.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Key mint, key revoke and session revoke commit with their audit row, or
// not at all.
func TestKeyAndSessionAuditsAreTransactional(t *testing.T) {
	database := connectorTestDB(t)
	ctx := context.Background()
	user, err := db.CreateOIDCUser(ctx, database, "alice", "sub-alice", "alice@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	withUser := func(r *http.Request) *http.Request {
		return r.WithContext(auth.ContextWithTestAuth(r.Context(), &user, "s1", ""))
	}

	// Audit sink down: the key is not minted.
	down := NewKeysHandler(database, &failingRecorder{}, HostModel{}, "https://example.com")
	rec := httptest.NewRecorder()
	down.mint(rec, withUser(httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"ci"}`))))
	if rec.Code != http.StatusInternalServerError || countRows(t, database, `SELECT count(*) FROM api_keys WHERE user_id = $1`, user.ID) != 0 {
		t.Fatalf("mint with the audit sink down = %d, keys = %d; want 500 and no key", rec.Code, countRows(t, database, `SELECT count(*) FROM api_keys WHERE user_id = $1`, user.ID))
	}

	keys := NewKeysHandler(database, audit.NewDBRecorder(database), HostModel{}, "https://example.com")
	rec = httptest.NewRecorder()
	keys.mint(rec, withUser(httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"ci"}`))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint = %d %s", rec.Code, rec.Body.String())
	}
	var keyID string
	if err := database.QueryRow(`SELECT id FROM api_keys WHERE user_id = $1`, user.ID).Scan(&keyID); err != nil {
		t.Fatal(err)
	}

	revoke := func(h *KeysHandler) int {
		r := withUser(httptest.NewRequest(http.MethodDelete, "/api/keys/"+keyID, nil))
		r.SetPathValue("id", keyID)
		rec := httptest.NewRecorder()
		h.revoke(rec, r)
		return rec.Code
	}
	if code := revoke(down); code != http.StatusInternalServerError || countRows(t, database, `SELECT count(*) FROM api_keys WHERE id = $1 AND revoked_at IS NULL`, keyID) != 1 {
		t.Fatalf("revoke with the audit sink down = %d; want 500 and the key still live", code)
	}
	if code := revoke(keys); code != http.StatusOK {
		t.Fatalf("revoke = %d", code)
	}

	session, err := db.CreateSession(ctx, database, user.ID, time.Now().Add(time.Hour), "", "test")
	if err != nil {
		t.Fatal(err)
	}
	revokeSession := func(recorder audit.Recorder) int {
		h := &AuthHandler{database: database, audit: recorder}
		r := withUser(httptest.NewRequest(http.MethodPost, "/auth/sessions/"+session.ID+"/revoke", nil))
		r.SetPathValue("id", session.ID)
		rec := httptest.NewRecorder()
		h.revokeSession(rec, r)
		return rec.Code
	}
	if code := revokeSession(&failingRecorder{}); code != http.StatusInternalServerError || countRows(t, database, `SELECT count(*) FROM sessions WHERE id = $1 AND revoked_at IS NULL`, session.ID) != 1 {
		t.Fatalf("session revoke with the audit sink down = %d; want 500 and the session still live", code)
	}
	if code := revokeSession(audit.NewDBRecorder(database)); code != http.StatusSeeOther {
		t.Fatalf("session revoke = %d", code)
	}

	for _, action := range []string{"key_mint", "key_revoke", "session_revoke"} {
		if n := countRows(t, database, `SELECT count(*) FROM audit_events WHERE action = $1 AND actor_id = $2`, action, user.ID); n != 1 {
			t.Errorf("%s audit rows = %d, want 1", action, n)
		}
	}
	report, err := audit.VerifyChain(ctx, database, audit.ChainExpectation{})
	if err != nil || report.Break != nil || report.Rows != 3 {
		t.Fatalf("chain = %+v, %v; want 3 clean rows", report, err)
	}
}
