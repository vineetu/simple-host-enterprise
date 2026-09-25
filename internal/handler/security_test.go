package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vsriram/simple-host/internal/auth"
)

func TestSecurityHeaders(t *testing.T) {
	for _, test := range []struct {
		name       string
		secureMode bool
		wantHSTS   string
	}{
		{name: "secure", secureMode: true, wantHSTS: hstsPolicy},
		{name: "local insecure"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}), test.secureMode, testHostModel(t)).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

			if got := response.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q", got)
			}
			if got := response.Header().Get("Content-Security-Policy"); got != contentSecurityPolicy {
				t.Fatalf("Content-Security-Policy = %q, want %q", got, contentSecurityPolicy)
			}
			// Phase 2 review finding: Referrer-Policy was never set on the
			// base host at all (design.md 7.4's control-plane bullet).
			if got := response.Header().Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
				t.Fatalf("Referrer-Policy = %q, want strict-origin-when-cross-origin", got)
			}
			if got := response.Header().Get("Strict-Transport-Security"); got != test.wantHSTS {
				t.Fatalf("Strict-Transport-Security = %q, want %q", got, test.wantHSTS)
			}
			if strings.Contains(response.Header().Get("Strict-Transport-Security"), "includeSubDomains") ||
				strings.Contains(response.Header().Get("Strict-Transport-Security"), "preload") {
				t.Fatal("HSTS enabled an unapproved domain-wide policy on an unrecognised host")
			}
		})
	}
}

// design.md 7.4: "Cache-Control: no-store on every authenticated page and
// API response" on the control plane. SecurityHeaders runs before any
// handler authenticates the request, so it reads "authenticated" as "the
// request carries a credential" (session cookie or X-API-Key) rather than
// waiting to learn whether that credential is valid — Phase 2 review
// finding: this used to be set ad hoc per handler (showcase.go, only when a
// search query was present), missing every other authenticated base-host
// route entirely.
func TestSecurityHeadersSetsNoStoreForAuthenticatedBaseHostRequests(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	for _, test := range []struct {
		name        string
		host        string
		withSession bool
		withAPIKey  bool
		wantNoStore bool
	}{
		{name: "base host, session cookie", host: "foo.example", withSession: true, wantNoStore: true},
		{name: "base host, X-API-Key", host: "foo.example", withAPIKey: true, wantNoStore: true},
		{name: "base host, no credential", host: "foo.example", wantNoStore: false},
		{name: "owner host, session cookie", host: "alice.foo.example", withSession: true, wantNoStore: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Host = test.host
			if test.withSession {
				request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "whatever-shape"})
			}
			if test.withAPIKey {
				request.Header.Set("X-API-Key", "whatever-shape")
			}
			response := httptest.NewRecorder()
			SecurityHeaders(next, true, testHostModel(t)).ServeHTTP(response, request)

			got := response.Header().Get("Cache-Control")
			if test.wantNoStore && got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
			if !test.wantNoStore && got == "no-store" {
				t.Fatalf("unauthenticated/non-base-host request got Cache-Control: no-store")
			}
		})
	}
}

// frame-ancestors protects screens this application renders. It must not
// reach hosted content on an owner or restricted-site host — people embed
// those pages in wikis and docs, and denying that is a product change
// nobody asked for — but it does apply to the base host in full (there is no
// more owner-host management preview to carve an exception for) and to any
// host this server does not recognise, since that is the safe default and
// costs nothing (the host gate, not this middleware, is what actually
// refuses those hosts). The base host also gets includeSubDomains on its
// HSTS header; an owner or restricted-site host does not repeat it.
func TestSecurityHeadersByHostKind(t *testing.T) {
	for _, test := range []struct {
		host        string
		wantCSP     bool
		wantSubDoms bool
	}{
		{host: "foo.example", wantCSP: true, wantSubDoms: true},
		{host: "alice.foo.example", wantCSP: false},
		{host: "alice--private.foo.example", wantCSP: false},
		{host: "10.0.0.5:8080", wantCSP: true},
	} {
		t.Run(test.host, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Host = test.host
			SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}), true, testHostModel(t)).ServeHTTP(response, request)

			got := response.Header().Get("Content-Security-Policy")
			if test.wantCSP && got != contentSecurityPolicy {
				t.Fatalf("%s: Content-Security-Policy = %q, want %q", test.host, got, contentSecurityPolicy)
			}
			if !test.wantCSP && got != "" {
				t.Fatalf("%s: hosted content carried Content-Security-Policy %q", test.host, got)
			}
			hsts := response.Header().Get("Strict-Transport-Security")
			if hasSubDoms := strings.Contains(hsts, "includeSubDomains"); hasSubDoms != test.wantSubDoms {
				t.Fatalf("%s: HSTS = %q, includeSubDomains = %t, want %t", test.host, hsts, hasSubDoms, test.wantSubDoms)
			}
		})
	}
}

func testHostModel(t *testing.T) HostModel {
	t.Helper()
	return newTestHostModel(t, "https://foo.example")
}

// newTestHostModel builds a HostModel for publicBaseURL, failing the test if
// the URL does not parse.
func newTestHostModel(t *testing.T, publicBaseURL string) HostModel {
	t.Helper()
	hosts, err := NewHostModel(publicBaseURL)
	if err != nil {
		t.Fatalf("NewHostModel(%q): %v", publicBaseURL, err)
	}
	return hosts
}

// Both cookies are __Host- prefixed, which a browser accepts only with
// Secure, Path "/" and no Domain.
func TestVisitCookieIsHostPrefixedAndPerSite(t *testing.T) {
	a, b := visitCookie("demo"), visitCookie("other")
	if !strings.HasPrefix(a.Name, "__Host-") || !a.Secure || !a.HttpOnly || a.Path != "/" || a.Domain != "" || a.MaxAge <= 0 {
		t.Fatalf("visit cookie = %+v", a)
	}
	if a.Name == b.Name {
		t.Fatal("two sites on one owner host share a visit cookie")
	}
}

func TestSearchSessionCookieIsHostPrefixed(t *testing.T) {
	for _, policy := range []CookiePolicy{{Secure: true}, {}} {
		c := policy.searchSession("anonymous-token")
		if c.Name != searchSessionCookieName || !strings.HasPrefix(c.Name, "__Host-") || c.Value != "anonymous-token" ||
			c.Path != "/" || !c.HttpOnly || !c.Secure || c.Domain != "" ||
			c.SameSite != http.SameSiteLaxMode || c.MaxAge != searchSessionCookieMaxAge {
			t.Fatalf("search session cookie = %+v", c)
		}
	}
}

func requireCookie(t *testing.T, response *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("response did not set %s", name)
	return nil
}
