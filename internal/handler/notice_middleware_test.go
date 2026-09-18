package handler

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

func TestSkillVersionMiddleware(t *testing.T) {
	middleware, err := SkillVersionMiddleware("0.8.1", "0.8.0")
	if err != nil {
		t.Fatalf("SkillVersionMiddleware: %v", err)
	}

	tests := []struct {
		name          string
		apiKey        string
		skillVersion  string
		client        string
		wantStatus    int
		wantUnchanged bool
	}{
		{name: "cookie browser without API key", wantStatus: http.StatusOK, wantUnchanged: true},
		{name: "missing version", apiKey: "key", wantStatus: http.StatusBadRequest},
		{name: "empty version", apiKey: "key", skillVersion: "  ", wantStatus: http.StatusBadRequest},
		{name: "malformed version", apiKey: "key", skillVersion: "0.8", wantStatus: http.StatusBadRequest},
		{name: "v prefix", apiKey: "key", skillVersion: "v0.8.1", wantStatus: http.StatusBadRequest},
		{name: "prerelease", apiKey: "key", skillVersion: "0.8.1-rc.1", wantStatus: http.StatusBadRequest},
		{name: "build metadata", apiKey: "key", skillVersion: "0.8.1+build", wantStatus: http.StatusBadRequest},
		{name: "leading zero", apiKey: "key", skillVersion: "0.08.1", wantStatus: http.StatusBadRequest},
		{name: "below minimum", apiKey: "key", skillVersion: "0.7.9", wantStatus: http.StatusBadRequest},
		{name: "minimum supported", apiKey: "key", skillVersion: "0.8.0", wantStatus: http.StatusOK, wantUnchanged: true},
		{name: "current", apiKey: "key", skillVersion: "0.8.1", wantStatus: http.StatusOK, wantUnchanged: true},
		{name: "trimmed current", apiKey: "key", skillVersion: " 0.8.1 ", wantStatus: http.StatusOK, wantUnchanged: true},
		{name: "future patch", apiKey: "key", skillVersion: "0.8.2", wantStatus: http.StatusOK, wantUnchanged: true},
		{name: "future major", apiKey: "key", skillVersion: "1.0.0", wantStatus: http.StatusOK, wantUnchanged: true},
		{name: "control UI classification", apiKey: "key", client: "control-ui", wantStatus: http.StatusOK, wantUnchanged: true},
		{name: "direct API classification", apiKey: "key", client: "api", wantStatus: http.StatusOK, wantUnchanged: true},
		{name: "trimmed case-insensitive classification", apiKey: "key", client: " API ", wantStatus: http.StatusOK, wantUnchanged: true},
		{name: "unknown classification", apiKey: "key", client: "other", wantStatus: http.StatusBadRequest},
	}

	const successBody = `["owned"]`
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(successBody))
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
			if tt.apiKey != "" {
				req.Header.Set("X-API-Key", tt.apiKey)
			}
			if tt.skillVersion != "" {
				req.Header.Set(headerSkillVersion, tt.skillVersion)
			}
			if tt.client != "" {
				req.Header.Set(headerSimpleHostClient, tt.client)
			}

			recorder := httptest.NewRecorder()
			middleware(next).ServeHTTP(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if got := recorder.Header().Get(headerLatestSkillVersion); got != "0.8.1" {
				t.Errorf("latest header = %q, want 0.8.1", got)
			}
			if got := recorder.Header().Get(headerMinimumSkillVersion); got != "0.8.0" {
				t.Errorf("minimum header = %q, want 0.8.0", got)
			}
			if got := recorder.Header().Get(headerSkillUpdateURL); got != skillUpdateURL {
				t.Errorf("update URL header = %q, want %q", got, skillUpdateURL)
			}

			if tt.wantUnchanged {
				if got := recorder.Body.String(); got != successBody {
					t.Fatalf("successful body changed: got %q, want %q", got, successBody)
				}
				return
			}

			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			var response skillVersionRequiredResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if response.Code != "skill_version_required" || response.LatestVersion != "0.8.1" ||
				response.MinimumSupportedVersion != "0.8.0" || response.UpdateURL != skillUpdateURL ||
				!strings.Contains(response.Error, "supported Simple Host skill version") {
				t.Fatalf("unexpected error response: %+v", response)
			}
		})
	}
}

type flushTrackingWriter struct {
	*httptest.ResponseRecorder
	flushed bool
}

func (w *flushTrackingWriter) Flush() {
	w.flushed = true
}

func TestSkillVersionMiddlewarePreservesStreamingInterfaces(t *testing.T) {
	middleware, err := SkillVersionMiddleware("0.8.1", MinimumSupportedSkillVersion)
	if err != nil {
		t.Fatalf("SkillVersionMiddleware: %v", err)
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("middleware hid http.Flusher")
		}
		flusher.Flush()
		_, _ = w.Write([]byte("archive-bytes"))
	})

	recorder := &flushTrackingWriter{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "/api/collaboration/sites/alice/demo/versions/1/archive", nil)
	req.Header.Set("X-API-Key", "key")
	req.Header.Set(headerSkillVersion, "0.8.1")
	middleware(next).ServeHTTP(recorder, req)

	if !recorder.flushed {
		t.Fatal("downstream Flush was not called")
	}
	if got := recorder.Body.String(); got != "archive-bytes" {
		t.Fatalf("streaming body = %q, want archive-bytes", got)
	}
}

func TestSkillVersionMiddlewareRejectsInvalidConfiguration(t *testing.T) {
	for _, tt := range []struct {
		server  string
		minimum string
	}{
		{server: "0.8", minimum: "0.8.0"},
		{server: "0.8.1", minimum: "v0.8.0"},
		{server: "0.7.9", minimum: "0.8.0"},
	} {
		if _, err := SkillVersionMiddleware(tt.server, tt.minimum); err == nil {
			t.Fatalf("SkillVersionMiddleware(%q, %q) unexpectedly succeeded", tt.server, tt.minimum)
		}
	}
}

func TestSkillVersionRouteScopeAndAuthenticationOrdering(t *testing.T) {
	skillMiddleware, err := SkillVersionMiddleware("0.8.1", MinimumSupportedSkillVersion)
	if err != nil {
		t.Fatalf("SkillVersionMiddleware: %v", err)
	}
	authMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-Key") != "valid-key" {
				writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	mux := http.NewServeMux()
	NewUserHandler(nil).Register(mux, authMiddleware, skillMiddleware)
	NewSiteHandler(nil, nil, nil, "", HostModel{}).Register(mux, authMiddleware, skillMiddleware)

	t.Run("registration and reset intake are gone", func(t *testing.T) {
		for _, path := range []string{"/api/auth", "/api/reset-requests"} {
			request := httptest.NewRequest(http.MethodPost, path, nil)
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusNotFound {
				t.Errorf("POST %s status = %d, want 404: identity is OIDC sign-in now, not open registration", path, recorder.Code)
			}
		}
	})

	tests := []struct {
		name       string
		method     string
		path       string
		apiKey     string
		version    string
		client     string
		wantStatus int
		wantCode   string
		wantHeader bool
	}{
		{name: "invalid key precedes version guard", method: http.MethodGet, path: "/api/sites", apiKey: "invalid", wantStatus: http.StatusUnauthorized},
		{name: "classification does not authenticate", method: http.MethodGet, path: "/api/sites", apiKey: "invalid", client: "api", wantStatus: http.StatusUnauthorized},
		{name: "control UI classification does not authenticate", method: http.MethodGet, path: "/api/sites", apiKey: "invalid", client: "control-ui", wantStatus: http.StatusUnauthorized},
		{name: "current user is guarded", method: http.MethodGet, path: "/api/me", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "legacy owned-only listing is blocked", method: http.MethodGet, path: "/api/sites", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "create is guarded", method: http.MethodPost, path: "/api/sites/demo", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "legacy update is guarded", method: http.MethodPut, path: "/api/sites/demo", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "delete is guarded", method: http.MethodDelete, path: "/api/sites/demo", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "legacy rollback is guarded", method: http.MethodPost, path: "/api/sites/demo/rollback", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "visibility is guarded", method: http.MethodPost, path: "/api/sites/demo/visibility", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "legacy versions are guarded", method: http.MethodGet, path: "/api/sites/demo/versions", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "canonical listing is also guarded", method: http.MethodGet, path: "/api/collaboration/sites", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "canonical metadata is guarded", method: http.MethodGet, path: "/api/collaboration/sites/alice/demo", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "canonical update is guarded", method: http.MethodPut, path: "/api/collaboration/sites/alice/demo", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "canonical rollback is guarded", method: http.MethodPost, path: "/api/collaboration/sites/alice/demo/rollback", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "canonical versions are guarded", method: http.MethodGet, path: "/api/collaboration/sites/alice/demo/versions", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "archive is guarded without entering handler", method: http.MethodGet, path: "/api/collaboration/sites/alice/demo/versions/1/archive", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "editor list is guarded", method: http.MethodGet, path: "/api/collaboration/sites/alice/demo/editors", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "editor grant is guarded", method: http.MethodPost, path: "/api/collaboration/sites/alice/demo/editors", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "editor revoke is guarded", method: http.MethodDelete, path: "/api/collaboration/sites/alice/demo/editors/bob", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "editor candidates are guarded", method: http.MethodGet, path: "/api/collaboration/sites/alice/demo/editor-candidates", apiKey: "valid-key", wantStatus: http.StatusBadRequest, wantCode: "skill_version_required", wantHeader: true},
		{name: "supported archive reaches authorization", method: http.MethodGet, path: "/api/collaboration/sites/alice/demo/versions/1/archive", apiKey: "valid-key", version: "0.8.1", wantStatus: http.StatusUnauthorized, wantHeader: true},
		// Registration (POST /api/auth) and reset intake (POST
		// /api/reset-requests) are gone: identity is OIDC sign-in now
		// (internal/handler/auth.go), and neither route exists to be
		// versionless about. See TestRegistrationAndResetRoutesAreGone.
		//
		// State used to have its own "remains unguarded" cases here
		// (SiteHandler.Register used to mux-register it directly). Design.md
		// 7.3 moved state and assets off the mux entirely: the host gate
		// dispatches them straight to SiteAPIHandler, which
		// skillVersionMiddleware never wraps because it is never in the
		// chain at all — the property this test file checks for every other
		// route is now true of state by construction, not by a case here.
		// See host_gate_test.go's TestHostGateSiteAPIMethodDispatch and
		// friends for that surface's own coverage.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.apiKey != "" {
				req.Header.Set("X-API-Key", tt.apiKey)
			}
			if tt.version != "" {
				req.Header.Set(headerSkillVersion, tt.version)
			}
			if tt.client != "" {
				req.Header.Set(headerSimpleHostClient, tt.client)
			}
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if got := recorder.Header().Get(headerLatestSkillVersion); (got != "") != tt.wantHeader {
				t.Fatalf("latest-version header = %q; want present=%t", got, tt.wantHeader)
			}
			if tt.wantCode != "" {
				var response skillVersionRequiredResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if response.Code != tt.wantCode {
					t.Fatalf("code = %q, want %q", response.Code, tt.wantCode)
				}
			} else if strings.Contains(recorder.Body.String(), "skill_version_required") {
				t.Fatalf("unexpected skill version error: %s", recorder.Body.String())
			}
		})
	}
}

type noRowsConnector struct{}

func (noRowsConnector) Connect(context.Context) (driver.Conn, error) { return noRowsConn{}, nil }
func (noRowsConnector) Driver() driver.Driver                        { return noRowsDriver{} }

type noRowsDriver struct{}

func (noRowsDriver) Open(string) (driver.Conn, error) { return noRowsConn{}, nil }

type noRowsConn struct{}

func (noRowsConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}
func (noRowsConn) Close() error              { return nil }
func (noRowsConn) Begin() (driver.Tx, error) { return noRowsTx{}, nil }
func (noRowsConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return noRowsTx{}, nil
}
func (noRowsConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return noRowsResult{}, nil
}

type noRowsTx struct{}

func (noRowsTx) Commit() error   { return nil }
func (noRowsTx) Rollback() error { return nil }

type noRowsResult struct{}

func (noRowsResult) Columns() []string {
	return []string{"id", "username", "is_admin", "created_at"}
}
func (noRowsResult) Close() error              { return nil }
func (noRowsResult) Next([]driver.Value) error { return io.EOF }

// Fixed identity for the fake admin below: enough for the tests here to
// build a session cookie and an X-API-Key that both resolve to the same
// admin user through a fake *sql.DB, without a real Postgres.
const (
	fakeAdminUserID    = "aaaaaaaa-0000-4000-8000-000000000001"
	fakeAdminUsername  = "admin"
	fakeAdminSessionID = "bbbbbbbb-0000-4000-8000-000000000002"
	fakeAdminAPIKey    = "admin-key"
)

// adminAuthConnector answers exactly the two queries
// internal/auth.Middleware issues — the api_keys-to-users join for
// X-API-Key, and the sessions-to-users join for the session cookie — with a
// single fixed admin row, and falls through to noRowsConn (no rows) for
// everything else, including the UPDATE ... last_used_at / last_seen_at
// touches, which fail closed (Prepare returns an error) and are logged, not
// surfaced, by internal/auth.
type adminAuthConnector struct{}

func (adminAuthConnector) Connect(context.Context) (driver.Conn, error) { return adminAuthConn{}, nil }
func (adminAuthConnector) Driver() driver.Driver                        { return adminAuthDriverType{} }

type adminAuthDriverType struct{}

func (adminAuthDriverType) Open(string) (driver.Conn, error) { return adminAuthConn{}, nil }

type adminAuthConn struct{ noRowsConn }

func (adminAuthConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	normalized := strings.Join(strings.Fields(query), " ")
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	switch {
	case strings.Contains(normalized, "FROM api_keys k") && strings.Contains(normalized, "WHERE k.key_hash = $1"):
		hash := namedBytes(args, 0)
		if !bytesEqual(hash, db.HashAPIKey(fakeAdminAPIKey)) {
			return noRowsResult{}, nil
		}
		return &fakeRows{
			columns: []string{"id", "username", "is_admin", "created_at", "kind", "email", "disabled_at", "key_id"},
			values: [][]driver.Value{{
				fakeAdminUserID, fakeAdminUsername, true, createdAt, "person", "", nil, "fake-key-id",
			}},
		}, nil

	case strings.Contains(normalized, "FROM sessions s") && strings.Contains(normalized, "JOIN users u"):
		sessionID := namedString(args, 0)
		if sessionID != fakeAdminSessionID {
			return noRowsResult{}, nil
		}
		return &fakeRows{
			columns: []string{
				"s.id", "s.user_id", "s.created_at", "s.expires_at", "s.last_seen_at", "ip", "s.user_agent", "s.revoked_at",
				"u.id", "u.username", "u.is_admin", "u.created_at", "u.kind", "u.email", "u.disabled_at",
			},
			// expires_at and last_seen_at must be relative to the real clock:
			// GetValidSession compares them against time.Now(), not against
			// this fake's fixed createdAt (used only for display fields).
			values: [][]driver.Value{{
				fakeAdminSessionID, fakeAdminUserID, createdAt, time.Now().Add(time.Hour), time.Now(), "", "",
				nil, fakeAdminUserID, fakeAdminUsername, true, createdAt, "person", "", nil,
			}},
		}, nil
	}
	return noRowsResult{}, nil
}

// fakeRows is a minimal driver.Rows over a fixed set of values, shared by
// every canned-row fake connector in this package's tests.
type fakeRows struct {
	columns []string
	values  [][]driver.Value
	pos     int
}

func (r *fakeRows) Columns() []string { return r.columns }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.pos])
	r.pos++
	return nil
}

func TestRealAuthenticationPrecedesSkillVersionClassification(t *testing.T) {
	database := sql.OpenDB(adminAuthConnector{})
	t.Cleanup(func() { _ = database.Close() })

	skillMiddleware, err := SkillVersionMiddleware("0.8.1", MinimumSupportedSkillVersion)
	if err != nil {
		t.Fatalf("SkillVersionMiddleware: %v", err)
	}
	const adminKey = fakeAdminAPIKey
	signingKeys := []auth.SigningKey{{ID: "k1", Key: make([]byte, 32)}}
	validCookie, err := auth.SignSession(signingKeys, fakeAdminSessionID, fakeAdminUserID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}
	stack := auth.Middleware(database, signingKeys, time.Hour)(skillMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))

	tests := []struct {
		name       string
		apiKey     string
		client     string
		version    string
		withCookie bool
		wantStatus int
		wantHeader bool
	}{
		{name: "invalid header overrides valid cookie", apiKey: "invalid", withCookie: true, wantStatus: http.StatusUnauthorized},
		{name: "api classification does not rescue invalid header", apiKey: "invalid", client: "api", withCookie: true, wantStatus: http.StatusUnauthorized},
		{name: "control UI classification does not rescue invalid header", apiKey: "invalid", client: "control-ui", withCookie: true, wantStatus: http.StatusUnauthorized},
		{name: "valid API key reaches version guard", apiKey: adminKey, wantStatus: http.StatusBadRequest, wantHeader: true},
		{name: "valid supported skill reaches handler", apiKey: adminKey, version: "0.8.1", wantStatus: http.StatusNoContent, wantHeader: true},
		{name: "valid direct API reaches handler", apiKey: adminKey, client: "api", wantStatus: http.StatusNoContent, wantHeader: true},
		{name: "cookie-only browser bypasses compatibility guard", withCookie: true, wantStatus: http.StatusNoContent, wantHeader: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/sites", nil)
			if tt.apiKey != "" {
				req.Header.Set("X-API-Key", tt.apiKey)
			}
			if tt.client != "" {
				req.Header.Set(headerSimpleHostClient, tt.client)
			}
			if tt.version != "" {
				req.Header.Set(headerSkillVersion, tt.version)
			}
			if tt.withCookie {
				req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: validCookie})
			}
			recorder := httptest.NewRecorder()
			stack.ServeHTTP(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if got := recorder.Header().Get(headerLatestSkillVersion); (got != "") != tt.wantHeader {
				t.Fatalf("latest-version header = %q; want present=%t", got, tt.wantHeader)
			}
		})
	}
}

func TestAdminAPIRouteAppliesGuardAfterAdminAuthentication(t *testing.T) {
	database := sql.OpenDB(adminAuthConnector{})
	t.Cleanup(func() { _ = database.Close() })

	skillMiddleware, err := SkillVersionMiddleware("0.8.1", MinimumSupportedSkillVersion)
	if err != nil {
		t.Fatalf("SkillVersionMiddleware: %v", err)
	}
	const adminKey = fakeAdminAPIKey
	signingKeys := []auth.SigningKey{{ID: "k1", Key: make([]byte, 32)}}
	validCookie, err := auth.SignSession(signingKeys, fakeAdminSessionID, fakeAdminUserID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignSession: %v", err)
	}
	mux := http.NewServeMux()
	NewAdminHandler(database, "", "https://example.com", HostModel{}, CookiePolicy{}, signingKeys, time.Hour, nil).Register(
		mux,
		auth.Middleware(database, signingKeys, time.Hour),
		skillMiddleware,
	)

	tests := []struct {
		name         string
		apiKey       string
		client       string
		withCookie   bool
		wantStatus   int
		wantHeader   bool
		wantLocation string
	}{
		{name: "valid admin key requires skill version", apiKey: adminKey, wantStatus: http.StatusBadRequest, wantHeader: true},
		{name: "invalid header overrides admin cookie", apiKey: "invalid", client: "control-ui", withCookie: true, wantStatus: http.StatusUnauthorized},
		// Refused, and sent back to the dashboard rather than shown a raw JSON
		// body. The security property is that the handler never runs; 303 vs
		// 403 is only how the admin is told.
		{name: "cookie-only admin browser requires origin", withCookie: true, wantStatus: http.StatusSeeOther, wantLocation: "/admin?action_error=blocked"},
		{name: "declared direct API reaches handler", apiKey: adminKey, client: "api", wantStatus: http.StatusNotFound, wantHeader: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/admin/users/missing/disable", nil)
			if tt.apiKey != "" {
				req.Header.Set("X-API-Key", tt.apiKey)
			}
			if tt.client != "" {
				req.Header.Set(headerSimpleHostClient, tt.client)
			}
			if tt.withCookie {
				req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: validCookie})
			}
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if got := recorder.Header().Get(headerLatestSkillVersion); (got != "") != tt.wantHeader {
				t.Fatalf("latest-version header = %q; want present=%t", got, tt.wantHeader)
			}
			if got := recorder.Header().Get("Location"); got != tt.wantLocation {
				t.Fatalf("Location = %q, want %q", got, tt.wantLocation)
			}
		})
	}
}
