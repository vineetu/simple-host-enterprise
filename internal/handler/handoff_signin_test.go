package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/vsriram/simple-host/internal/auth"
)

// A signed-out colleague who opens a shared link reaches <base>/auth/handoff
// with no base session: a browser navigation is sent to sign in and back to
// the same hand-off URL; a script, or anything carrying a key, still gets
// the 401 JSON.
func TestHandoffSignedOutNavigationGoesToSignIn(t *testing.T) {
	mux := http.NewServeMux()
	NewHandoffHandler(nil, testSigningKeys, HostModel{}, nil).Register(mux, auth.Middleware(nil, testSigningKeys, 0))
	const handoffURL = "/auth/handoff?to=https%3A%2F%2Fmy-site.alice.foo.example%2F%3Fq%3D1&n=abc"

	cases := []struct {
		name     string
		headers  map[string]string
		wantCode int
	}{
		{"navigation by fetch metadata", map[string]string{"Sec-Fetch-Dest": "document"}, http.StatusFound},
		{"navigation by Accept", map[string]string{"Accept": "text/html,application/xhtml+xml"}, http.StatusFound},
		{"script fetch", map[string]string{"Sec-Fetch-Dest": "empty", "Accept": "*/*"}, http.StatusUnauthorized},
		{"plain client", map[string]string{"Accept": "application/json"}, http.StatusUnauthorized},
		{"bearer token", map[string]string{"Accept": "text/html", "Authorization": "Bearer x"}, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, handoffURL, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == http.StatusUnauthorized {
				if !strings.Contains(rec.Body.String(), `"error"`) {
					t.Fatalf("401 body = %q, want the JSON error", rec.Body.String())
				}
				return
			}
			loc := rec.Header().Get("Location")
			if loc != "/auth/login?to="+url.QueryEscape(handoffURL) {
				t.Fatalf("Location = %q, want sign-in returning to the hand-off", loc)
			}
			if strings.Contains(rec.Body.String(), "unauthorized") || strings.Contains(rec.Header().Get("Content-Type"), "json") {
				t.Fatalf("redirect carried the JSON error: %q %q", rec.Header().Get("Content-Type"), rec.Body.String())
			}
			// The return path survives /auth/login's own rule.
			if sanitizeRedirectPath(handoffURL) != handoffURL {
				t.Fatalf("sanitizeRedirectPath refused the hand-off URL")
			}
		})
	}
}
