package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vsriram/simple-host/internal/auth"
)

// The management API accepts the browser session cookie, an ambient credential
// that any same-origin page (and, after the subdomain split, any same-site
// page) can replay. The Origin check must therefore sit outside authentication
// on every cookie-reachable write, and nowhere else.
func TestManagementWritesRequireOriginOnCookiePath(t *testing.T) {
	const publicBaseURL = "https://example.com"

	// Stands in for auth.Middleware: records whether the request got past the
	// Origin check. A refused request must never reach it, because the real
	// middleware's cookie branch is a database lookup.
	authReached := false
	fakeAuth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			authReached = true
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		})
	}
	identity := func(next http.Handler) http.Handler { return next }

	handler := NewSiteHandler(nil, nil, publicBaseURL, newTestHostModel(t, publicBaseURL))
	mux := http.NewServeMux()
	handler.Register(mux, fakeAuth, identity)

	type credential int
	const (
		cookie credential = iota
		apiKey
		none
		adminCookie
	)
	tests := []struct {
		name       string
		method     string
		target     string
		credential credential
		origin     string
		wantReject bool
	}{
		{name: "cookie with foreign origin", method: http.MethodPost, target: "/api/sites/x/rollback", credential: cookie, origin: "https://evil.example", wantReject: true},
		{name: "cookie with base origin", method: http.MethodPost, target: "/api/sites/x/rollback", credential: cookie, origin: publicBaseURL},
		{name: "api key without origin", method: http.MethodPost, target: "/api/sites/x/rollback", credential: apiKey},
		{name: "cookie without origin", method: http.MethodPost, target: "/api/sites/x/rollback", credential: cookie, wantReject: true},
		{name: "list with cookie and foreign origin", method: http.MethodGet, target: "/api/sites", credential: cookie, origin: "https://evil.example"},
		// No credential at all: nothing ambient to forge, so the request must
		// reach authentication and get the 401 a keyless client expects.
		{name: "no credential without origin", method: http.MethodPost, target: "/api/sites/x/rollback", credential: none},
		{name: "admin cookie without origin", method: http.MethodPost, target: "/api/sites/x/rollback", credential: adminCookie, wantReject: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authReached = false
			request := httptest.NewRequest(test.method, test.target, nil)
			switch test.credential {
			case cookie, adminCookie:
				// Both cases exercise the same cookie mechanism now: there
				// is one browser session cookie (design.md 6.1), not a
				// separate admin cookie.
				request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session-value"})
			case apiKey:
				request.Header.Set("X-API-Key", "user-key")
			}
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}

			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)

			if test.wantReject {
				if response.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", response.Code)
				}
				var got errorResponse
				if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
					t.Fatalf("decode rejection: %v", err)
				}
				if got.Error != "forbidden" {
					t.Fatalf("error = %q, want forbidden", got.Error)
				}
				if authReached {
					t.Fatal("rejected request reached authentication")
				}
				return
			}
			if response.Code == http.StatusForbidden {
				t.Fatalf("status = 403; request should have passed the Origin check")
			}
			if !authReached {
				t.Fatal("request did not reach authentication")
			}
		})
	}
}

// Every cookie-reachable write is wrapped; every read is not. A route that
// drifts out of the wrapped set reopens the gap silently, so enumerate them.
func TestManagementRouteOriginCoverage(t *testing.T) {
	const publicBaseURL = "https://example.com"
	identity := func(next http.Handler) http.Handler { return next }
	accept := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})
	}
	handler := NewSiteHandler(nil, nil, publicBaseURL, newTestHostModel(t, publicBaseURL))
	mux := http.NewServeMux()
	handler.Register(mux, accept, identity)

	routes := []struct {
		method  string
		target  string
		guarded bool
	}{
		{http.MethodPost, "/api/sites/demo", true},
		{http.MethodPut, "/api/sites/demo", true},
		{http.MethodDelete, "/api/sites/demo", true},
		{http.MethodPost, "/api/sites/demo/rollback", true},
		{http.MethodPost, "/api/sites/demo/access", true},
		{http.MethodPut, "/api/collaboration/sites/alice/demo", true},
		{http.MethodPost, "/api/collaboration/sites/alice/demo/rollback", true},
		{http.MethodPost, "/api/collaboration/sites/alice/demo/access", true},
		{http.MethodPost, "/api/collaboration/sites/alice/demo/state-versions/1/restore", true},
		{http.MethodGet, "/api/sites", false},
		{http.MethodGet, "/api/sites/demo/versions", false},
		{http.MethodGet, "/api/collaboration/sites", false},
		{http.MethodGet, "/api/collaboration/sites/alice/demo", false},
		{http.MethodGet, "/api/collaboration/sites/alice/demo/versions", false},
		{http.MethodGet, "/api/collaboration/sites/alice/demo/versions/1/archive", false},
		{http.MethodGet, "/api/collaboration/sites/alice/demo/state-versions", false},
		{http.MethodGet, "/api/collaboration/sites/alice/demo/viewer-candidates", false},
	}
	for _, route := range routes {
		request := httptest.NewRequest(route.method, route.target, nil)
		request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session-value"})
		request.Header.Set("Origin", "https://evil.example")
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)

		if route.guarded && response.Code != http.StatusForbidden {
			t.Errorf("%s %s status = %d, want 403 from the Origin check", route.method, route.target, response.Code)
		}
		if !route.guarded && response.Code != http.StatusTeapot {
			t.Errorf("%s %s status = %d, want to pass through to authentication", route.method, route.target, response.Code)
		}
	}
}

// On an owner host the same guarded writes demand that owner host's own
// origin. The base origin is refused there, and an owner origin is refused on
// the base host, so every route moves with the host rather than accepting
// both. The reads stay unwrapped on both hosts.
func TestManagementRouteOriginCoverageOnOwnerHost(t *testing.T) {
	const publicBaseURL = "https://example.com"
	const ownerHost = "alice.example.com"
	identity := func(next http.Handler) http.Handler { return next }
	accept := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		})
	}
	handler := NewSiteHandler(nil, nil, publicBaseURL, newTestHostModel(t, publicBaseURL))
	mux := http.NewServeMux()
	handler.Register(mux, accept, identity)

	routes := []struct {
		method  string
		target  string
		guarded bool
	}{
		{http.MethodPost, "/api/sites/demo", true},
		{http.MethodPut, "/api/sites/demo", true},
		{http.MethodDelete, "/api/sites/demo", true},
		{http.MethodPost, "/api/sites/demo/rollback", true},
		{http.MethodPost, "/api/sites/demo/access", true},
		{http.MethodPut, "/api/collaboration/sites/alice/demo", true},
		{http.MethodPost, "/api/collaboration/sites/alice/demo/rollback", true},
		{http.MethodPost, "/api/collaboration/sites/alice/demo/access", true},
		{http.MethodPost, "/api/collaboration/sites/alice/demo/state-versions/1/restore", true},
		{http.MethodGet, "/api/sites", false},
		{http.MethodGet, "/api/sites/demo/versions", false},
		{http.MethodGet, "/api/collaboration/sites/alice/demo/state-versions", false},
		{http.MethodGet, "/api/collaboration/sites/alice/demo/viewer-candidates", false},
	}

	request := func(method, target, host, origin string) int {
		r := httptest.NewRequest(method, target, nil)
		r.Host = host
		r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session-value"})
		r.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, r)
		return response.Code
	}

	for _, route := range routes {
		// The owner host's own origin is the one that passes there.
		if got := request(route.method, route.target, ownerHost, "https://"+ownerHost); got != http.StatusTeapot {
			t.Errorf("%s %s on %s with its own origin: status = %d, want to pass through to authentication", route.method, route.target, ownerHost, got)
		}
		// The base origin does not, and neither does another owner's.
		for _, origin := range []string{publicBaseURL, "https://bob.example.com", "https://evil.example"} {
			got := request(route.method, route.target, ownerHost, origin)
			want := http.StatusTeapot
			if route.guarded {
				want = http.StatusForbidden
			}
			if got != want {
				t.Errorf("%s %s on %s with origin %s: status = %d, want %d", route.method, route.target, ownerHost, origin, got, want)
			}
		}
		// And the owner origin is not accepted on the base host.
		got := request(route.method, route.target, "example.com", "https://"+ownerHost)
		want := http.StatusTeapot
		if route.guarded {
			want = http.StatusForbidden
		}
		if got != want {
			t.Errorf("%s %s on the base host with an owner origin: status = %d, want %d", route.method, route.target, got, want)
		}
	}
}
