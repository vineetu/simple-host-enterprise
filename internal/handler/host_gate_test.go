package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/storage"
)

// newHostGateTestHandler builds a mux that stands in for the real one: the
// real file handler, the real health routes, and recognisable stubs for the
// control-plane routes the gate must distinguish. State and assets are no
// longer mux routes at all (design.md 7.3): the gate dispatches those
// directly to a fake siteAPIRoutes (see testHostGate), never through this
// mux. It is wrapped in a gate whose siteForServing/viewerAllowed always
// admit an unrestricted site to any signed-in caller, which is what every
// test not specifically about restriction or refusal wants.
func newHostGateTestHandler(t *testing.T, store *storage.DiskStorage) http.Handler {
	t.Helper()
	gate := testHostGate(t, store, testHostModel(t))
	return gate.wrap(newHostGateTestMux())
}

func newHostGateTestMux() *http.ServeMux {
	mux := http.NewServeMux()
	stub := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}
	}
	// The real readiness probe pings the database, which is nil here, so
	// both probes are stubs at the paths RegisterHealthRoutes registers.
	mux.HandleFunc("GET /healthz", stub("healthz-stub"))
	mux.HandleFunc("GET /readyz", stub("readyz-stub"))
	mux.HandleFunc("GET /api/sites", stub("sites-list-stub"))
	mux.HandleFunc("GET /api/me", stub("me-stub"))
	mux.HandleFunc("GET /admin", stub("admin-stub"))
	mux.HandleFunc("POST /mcp", stub("mcp-stub"))
	mux.HandleFunc("GET /skills.zip", stub("skills-stub"))
	mux.HandleFunc("GET /", stub("landing-stub"))
	return mux
}

// fakeSiteAPI records every call the gate dispatches to it, standing in for
// *SiteAPIHandler (which needs a live Postgres and a real disk-backed asset
// store for almost everything it does) so the gate's own routing — auth,
// viewerAllowed/writerAllowed, Origin, method dispatch, the via_site claim —
// can be tested with neither.
type fakeSiteAPI struct {
	calls []fakeSiteAPICall
}

type fakeSiteAPICall struct {
	method string
	call   siteAPICall
	id     string
}

func (f *fakeSiteAPI) record(method string, w http.ResponseWriter, call siteAPICall, id string) {
	f.calls = append(f.calls, fakeSiteAPICall{method: method, call: call, id: id})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(method))
}

func (f *fakeSiteAPI) GetState(w http.ResponseWriter, _ *http.Request, call siteAPICall) {
	f.record("GetState", w, call, "")
}
func (f *fakeSiteAPI) PutState(w http.ResponseWriter, _ *http.Request, call siteAPICall) {
	f.record("PutState", w, call, "")
}
func (f *fakeSiteAPI) GetStateVersioned(w http.ResponseWriter, _ *http.Request, call siteAPICall) {
	f.record("GetStateVersioned", w, call, "")
}
func (f *fakeSiteAPI) PutStateVersioned(w http.ResponseWriter, _ *http.Request, call siteAPICall) {
	f.record("PutStateVersioned", w, call, "")
}
func (f *fakeSiteAPI) CreateAsset(w http.ResponseWriter, _ *http.Request, call siteAPICall) {
	f.record("CreateAsset", w, call, "")
}
func (f *fakeSiteAPI) ListAssets(w http.ResponseWriter, _ *http.Request, call siteAPICall) {
	f.record("ListAssets", w, call, "")
}
func (f *fakeSiteAPI) DeleteAsset(w http.ResponseWriter, _ *http.Request, call siteAPICall, id string) {
	f.record("DeleteAsset", w, call, id)
}
func (f *fakeSiteAPI) ServeAsset(w http.ResponseWriter, _ *http.Request, call siteAPICall, id string) {
	f.record("ServeAsset", w, call, id)
}

// hostGateTestAuthUser is the one identity fakeAuthMiddleware and
// fakeAPIKeyMiddleware recognise, keyed by the exact cookie/header
// authenticatedGateRequest and apiKeyGateRequest set.
const (
	hostGateTestSessionID = "session-1"
	hostGateTestUserID    = "user-1"
	hostGateTestAPIKey    = "test-key-plaintext"
	hostGateTestKeyUserID = "key-user-1"
	hostGateTestKeyID     = "key-1"
)

// fakeAuthMiddleware stands in for auth.Middleware without a live Postgres:
// it accepts the exact session cookie authenticatedGateRequest signs (real
// signature verification, via auth.VerifySessionCookie — only the
// "session still valid in the database" step is skipped) or the exact
// X-API-Key apiKeyGateRequest sets, and 401s anything else. This is the
// same fake-at-the-seam approach as siteForServing/viewerAllowed above: the
// gate's own routing is what is under test, not auth.Middleware itself
// (which has its own tests).
func fakeAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := r.Header.Get("X-API-Key"); key != "" {
			if key != hostGateTestAPIKey {
				writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
				return
			}
			user := &db.User{ID: hostGateTestKeyUserID}
			ctx := auth.ContextWithTestAuth(r.Context(), user, "", hostGateTestKeyID)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		cookie, err := r.Cookie(auth.SessionCookieName)
		if err != nil || cookie.Value == "" {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
			return
		}
		verified, err := auth.VerifySessionCookie(testSigningKeys, cookie.Value)
		if err != nil || !strings.EqualFold(verified.Host, auth.ExpectedSessionHost(r.Context())) {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
			return
		}
		user := &db.User{ID: verified.UserID}
		ctx := auth.ContextWithTestAuth(r.Context(), user, verified.SessionID, "")
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// testHostGate builds a *hostGate directly, bypassing NewHostGate's database
// wiring: siteForServing, viewerAllowed, and writerAllowed are supplied as
// fakes, since internal/db's own tests are SQL-text assertions and there is
// no fake for *sql.Row's concrete type to build a lighter-weight mock from
// (see the comment on hostGate's fields). By default every site is
// unrestricted, writerAllowed matches viewerAllowed (state_write_mode
// "anyone"), and every signed-in caller is an allowed viewer. siteAPI is a
// *fakeSiteAPI so a test can inspect exactly what the gate decided to call.
func testHostGate(t *testing.T, store *storage.DiskStorage, hosts HostModel) *hostGate {
	t.Helper()
	return &hostGate{
		hosts:       hosts,
		files:       NewSiteFiles(store, nil, CookiePolicy{}, testSigningKeys, 0),
		signingKeys: testSigningKeys,
		handoff:     NewHandoffHandler(nil, testSigningKeys, hosts, nil),
		siteForServing: func(r *http.Request, ownerUsername, siteName string) (string, bool, error) {
			return ownerUsername + "/" + siteName, false, nil
		},
		viewerAllowed: func(r *http.Request, siteID, userID string) (bool, error) {
			return true, nil
		},
		writerAllowed: func(r *http.Request, siteID, userID string) (bool, error) {
			return true, nil
		},
		touchHostSession: func(sessionID string) {},
		siteAPI:          &fakeSiteAPI{},
		authMiddleware:   fakeAuthMiddleware,
		originCheck:      cookieOriginCheck(hosts, "https://foo.example"),
	}
}

var testSigningKeys = []auth.SigningKey{{ID: "k1", Key: bytes.Repeat([]byte{7}, 32)}}

// authenticatedGateRequest builds a request carrying a valid host session
// cookie, signed the way the hand-off mints one, so it reaches past
// requireHostSession straight to viewerAllowed and serving.
func authenticatedGateRequest(t *testing.T, method, host, target string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request.Host = host
	cookieValue, err := auth.SignHostSession(testSigningKeys, "session-1", "user-1", host, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignHostSession: %v", err)
	}
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: cookieValue})
	return request
}

func writeGateSite(t *testing.T, store *storage.DiskStorage, user, site, body string) {
	t.Helper()
	if err := store.WriteFiles(user, site, 1, map[string][]byte{
		"index.html":    []byte(body),
		"assets/app.js": []byte("console.log(1)"),
	}); err != nil {
		t.Fatalf("WriteFiles(%s/%s): %v", user, site, err)
	}
	if err := store.SetCurrentVersion(user, site, 1); err != nil {
		t.Fatalf("SetCurrentVersion(%s/%s): %v", user, site, err)
	}
}

func gateRequest(handler http.Handler, method, host, target string) *httptest.ResponseRecorder {
	return gateRequestFrom(handler, method, host, target, "")
}

// gateRequestFrom is gateRequest with a Referer, for the state routes whose
// caller is named by the page that sends them.
func gateRequestFrom(handler http.Handler, method, host, target, referer string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	request.Host = host
	if referer != "" {
		request.Header.Set("Referer", referer)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// gateAuthedRequest serves an authenticated request (see
// authenticatedGateRequest) through handler and returns the recorded
// response.
func gateAuthedRequest(t *testing.T, handler http.Handler, method, host, target string) *httptest.ResponseRecorder {
	t.Helper()
	request := authenticatedGateRequest(t, method, host, target)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestHostGateRouting(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	writeGateSite(t, store, "bob", "x", "bob-index")
	handler := newHostGateTestHandler(t, store)

	// Base host: control plane passes through untouched.
	for _, path := range []string{"/", "/admin", "/api/sites", "/api/me", "/skills.zip"} {
		response := gateRequest(handler, http.MethodGet, "foo.example", path)
		if response.Code != http.StatusOK {
			t.Errorf("base host %s: status = %d, want 200", path, response.Code)
		}
	}
	// Base host: the site-facing API shape is refused outright — it is not a
	// mux route on the base host at all any more (design.md 7.3), so these
	// simply find no route to answer them.
	for _, path := range []string{"/api/site/state", "/api/site/state/versioned", "/api/sites/my-site/state", "/api/sites/alice/my-site/state"} {
		response := gateRequest(handler, http.MethodGet, "foo.example", path)
		if response.Code != http.StatusNotFound {
			t.Errorf("base host %s: status = %d, want 404", path, response.Code)
		}
	}

	// Owner host: an unauthenticated navigation to hosted content is sent
	// through the hand-off, never served directly.
	navigation := httptest.NewRequest(http.MethodGet, "/my-site/", nil)
	navigation.Host = "alice.foo.example"
	navigation.Header.Set("Accept", "text/html")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, navigation)
	if response.Code != http.StatusFound {
		t.Fatalf("unauthenticated navigation: status = %d, want 302", response.Code)
	}
	if loc := response.Header().Get("Location"); !strings.HasPrefix(loc, "https://foo.example/auth/handoff?") {
		t.Fatalf("unauthenticated navigation redirected to %q", loc)
	}

	// Owner host: authenticated, it serves.
	response = gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/my-site/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "alice-index") {
		t.Fatalf("authenticated owner host: status = %d body %q", response.Code, response.Body.String())
	}
	response = gateAuthedRequest(t, handler, http.MethodGet, "bob.foo.example", "/x/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "bob-index") {
		t.Fatalf("authenticated owner host: status = %d body %q", response.Code, response.Body.String())
	}
	// A bare short path (no trailing slash) redirects, unauthenticated or not.
	response = gateRequest(handler, http.MethodGet, "alice.foo.example", "/my-site")
	if response.Code != http.StatusMovedPermanently {
		t.Fatalf("bare short path: status = %d, want 301", response.Code)
	}
	// A non-navigation request with no session gets 401, not the hand-off.
	response = gateRequest(handler, http.MethodGet, "alice.foo.example", "/my-site/")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated non-navigation: status = %d, want 401", response.Code)
	}
	// A method other than GET/HEAD is refused before any auth check.
	response = gateRequest(handler, http.MethodPost, "alice.foo.example", "/my-site/")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST short path: status = %d, want 405", response.Code)
	}
	// A reserved short-path prefix and an unknown site are both 404, whether
	// or not a session is present.
	for _, path := range []string{"/api/anything", "/sites/anything", "/does-not-exist/"} {
		response = gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", path)
		if response.Code != http.StatusNotFound {
			t.Errorf("owner host %s: status = %d, want 404", path, response.Code)
		}
	}
	// Owner host: probes still answer with no session at all, and every
	// owner-host response carries the CORP and Cache-Control headers
	// regardless of the path (design.md 7.4; the latter is a Phase 2
	// review finding — hosted content never got Cache-Control at all).
	for _, path := range []string{"/healthz", "/readyz"} {
		response = gateRequest(handler, http.MethodGet, "alice.foo.example", path)
		if response.Code != http.StatusOK {
			t.Errorf("owner host %s: status = %d, want 200", path, response.Code)
		}
		if got := response.Header().Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
			t.Errorf("owner host %s: CORP header = %q, want same-origin", path, got)
		}
		if got := response.Header().Get("Cache-Control"); got != "private, no-cache" {
			t.Errorf("owner host %s: Cache-Control = %q, want %q", path, got, "private, no-cache")
		}
	}

	// Unrecognised host: probes only.
	response = gateRequest(handler, http.MethodGet, "unknown.example", "/healthz")
	if response.Code != http.StatusOK {
		t.Fatalf("unrecognised host healthz: status = %d", response.Code)
	}
	response = gateRequest(handler, http.MethodGet, "unknown.example", "/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("unrecognised host: status = %d, want 404", response.Code)
	}
}

// A restricted site (viewerAllowed's siteForServing reports restricted=true)
// no longer serves on its owner's short path: the address moved.
func TestHostGateRestrictedSiteLeavesOwnerHost(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "private", "private-index")
	gate := testHostGate(t, store, testHostModel(t))
	gate.siteForServing = func(r *http.Request, owner, site string) (string, bool, error) {
		return "site-1", true, nil
	}
	handler := gate.wrap(newHostGateTestMux())

	response := gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/private/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("restricted site on owner host: status = %d, want 404", response.Code)
	}
}

// The site-facing API follows the same rule as hosted content: a restricted
// site's state and assets are never answered on its owner's shared host,
// and an unrestricted site's are never answered on a restricted-site host.
func TestHostGateSiteAPIRestrictedOnlyOnItsOwnHost(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "private", "private-index")
	for _, test := range []struct {
		name       string
		restricted bool
		host, path string
		want       int
	}{
		{"restricted site on owner host", true, "alice.foo.example", "/api/sites/private/state", http.StatusNotFound},
		{"restricted site assets on owner host", true, "alice.foo.example", "/api/sites/private/assets", http.StatusNotFound},
		{"restricted site on its own host", true, "alice--private.foo.example", "/api/site/state", http.StatusOK},
		{"unrestricted site on a restricted host", false, "alice--private.foo.example", "/api/site/state", http.StatusNotFound},
		{"unrestricted site on owner host", false, "alice.foo.example", "/api/sites/private/state", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := testHostGate(t, store, testHostModel(t))
			gate.siteForServing = func(r *http.Request, owner, site string) (string, bool, error) {
				return "site-1", test.restricted, nil
			}
			response := gateAuthedRequest(t, gate.wrap(newHostGateTestMux()), http.MethodGet, test.host, test.path)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

// A session cookie minted for one host (by the hand-off) authenticates the
// site-facing API only on that host: not on another owner's host, and a
// base-host cookie (no host binding) not on any owner host.
func TestHostGateSiteAPISessionBoundToHost(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	gate := testHostGate(t, store, testHostModel(t))
	fake := &fakeSiteAPI{}
	gate.siteAPI = fake
	handler := gate.wrap(newHostGateTestMux())

	for _, test := range []struct {
		name       string
		cookieHost string
		want       int
	}{
		{"own host", "alice.foo.example", http.StatusOK},
		{"another owner's host", "mallory.foo.example", http.StatusUnauthorized},
		{"base host cookie", "", http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake.calls = nil
			var value string
			var err error
			if test.cookieHost == "" {
				value, err = auth.SignSession(testSigningKeys, "session-1", "user-1", time.Now().Add(time.Hour))
			} else {
				value, err = auth.SignHostSession(testSigningKeys, "session-1", "user-1", test.cookieHost, time.Now().Add(time.Hour))
			}
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "/api/sites/my-site/state", nil)
			request.Host = "alice.foo.example"
			request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: value})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
			if test.want != http.StatusOK && len(fake.calls) != 0 {
				t.Fatalf("site API reached with a cookie for %q", test.cookieHost)
			}
		})
	}
}

// A site that fails viewerAllowed is 404, not 403: its existence must not be
// confirmed to somebody it refuses (design.md 7.2).
func TestHostGateViewerNotAllowedIs404(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "private", "private-index")
	gate := testHostGate(t, store, testHostModel(t))
	gate.viewerAllowed = func(r *http.Request, siteID, userID string) (bool, error) { return false, nil }
	handler := gate.wrap(newHostGateTestMux())

	response := gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/private/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("viewer not allowed: status = %d, want 404", response.Code)
	}
}

// The restricted-site host serves only a site that is actually restricted,
// resolved by folding each of the owner's site names to a label and finding
// the one match, the same disk-only, fail-closed-on-collision pattern
// resolveOwner uses for the owner label itself.
func TestHostGateServesRestrictedSiteHost(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "private", "private-index")
	gate := testHostGate(t, store, testHostModel(t))
	gate.siteForServing = func(r *http.Request, owner, site string) (string, bool, error) {
		return "site-1", true, nil
	}
	handler := gate.wrap(newHostGateTestMux())

	response := gateAuthedRequest(t, handler, http.MethodGet, "alice--private.foo.example", "/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "private-index") {
		t.Fatalf("restricted site host: status = %d body %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
		t.Errorf("CORP header = %q, want same-origin", got)
	}

	// The site exists but siteForServing now reports it unrestricted: the
	// "--" address must not serve it either, so the two addresses can never
	// both work for the same site at once.
	gate.siteForServing = func(r *http.Request, owner, site string) (string, bool, error) {
		return "site-1", false, nil
	}
	handler = gate.wrap(newHostGateTestMux())
	response = gateAuthedRequest(t, handler, http.MethodGet, "alice--private.foo.example", "/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("unrestricted site on its restricted address: status = %d, want 404", response.Code)
	}

	// An unknown label resolves to no site at all.
	response = gateAuthedRequest(t, handler, http.MethodGet, "alice--nosuch.foo.example", "/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown restricted site: status = %d, want 404", response.Code)
	}
}

// A host session cookie is bound to the host it was minted for (design.md
// 6.1's hardening over "same signed payload" — see
// docs/security-review.md's deviation record): a cookie
// alice's host handed out must not authenticate a request on bob's host,
// even though both are ordinary owner hosts under the same base domain and
// the underlying session row is shared on purpose.
func TestHostGateSessionCookieBoundToItsOwnHost(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	writeGateSite(t, store, "bob", "x", "bob-index")
	handler := newHostGateTestHandler(t, store)

	aliceCookie, err := auth.SignHostSession(testSigningKeys, "session-1", "user-1", "alice.foo.example", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignHostSession: %v", err)
	}

	// Presented back on alice's own host: serves.
	request := httptest.NewRequest(http.MethodGet, "/my-site/", nil)
	request.Host = "alice.foo.example"
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: aliceCookie})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("alice's cookie on alice's host: status = %d, want 200", response.Code)
	}

	// Presented on bob's host: refused. A non-navigation request with an
	// invalid cookie gets 401 (the same as no cookie at all), not access to
	// bob's site.
	request = httptest.NewRequest(http.MethodGet, "/x/", nil)
	request.Host = "bob.foo.example"
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: aliceCookie})
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("alice's cookie on bob's host: status = %d, want 401 (body %q)", response.Code, response.Body.String())
	}
}

// Phase 2 review finding: hosted-content auth never touched last_seen_at,
// so an actively-browsing viewer's session went idle and was killed by
// SESSION_IDLE even while they kept viewing (design.md 6.1: "at most once
// per five minutes from hosted content"). requireHostSession must call
// touchHostSession on every successful auth. touchHostSession's own
// signature (no return value) is what makes "never block serving on its
// error" structural rather than a runtime check: NewHostGate's default
// implementation (db.TouchSession) only ever logs a failure, and there is
// nothing here for a caller to propagate even if it wanted to.
func TestHostGateTouchesSessionOnSuccessfulHostedAuth(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	gate := testHostGate(t, store, testHostModel(t))
	var touched []string
	gate.touchHostSession = func(sessionID string) { touched = append(touched, sessionID) }
	handler := gate.wrap(newHostGateTestMux())

	response := gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/my-site/")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if len(touched) != 1 || touched[0] != hostGateTestSessionID {
		t.Fatalf("touchHostSession calls = %v, want exactly one call with %q", touched, hostGateTestSessionID)
	}
}

// design.md 7.4: a sibling-origin subresource load (Sec-Fetch-Site same-site
// or cross-site, Sec-Fetch-Dest anything but document) is refused even
// though the request otherwise carries no credential problem; a navigation
// (Sec-Fetch-Dest document) is let through to the ordinary checks.
func TestHostGateRefusesSiblingOriginSubresourceLoads(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	handler := newHostGateTestHandler(t, store)

	for _, test := range []struct {
		name       string
		site, dest string
		wantStatus int
	}{
		{name: "same-site script load", site: "same-site", dest: "script", wantStatus: http.StatusForbidden},
		{name: "cross-site image load", site: "cross-site", dest: "image", wantStatus: http.StatusForbidden},
		{name: "same-site navigation still works", site: "same-site", dest: "document", wantStatus: http.StatusOK},
		{name: "no Sec-Fetch headers at all (curl, an old browser)", wantStatus: http.StatusOK},
		{name: "same-origin fetch", site: "same-origin", dest: "empty", wantStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := authenticatedGateRequest(t, http.MethodGet, "alice.foo.example", "/my-site/")
			if test.site != "" {
				request.Header.Set("Sec-Fetch-Site", test.site)
			}
			if test.dest != "" {
				request.Header.Set("Sec-Fetch-Dest", test.dest)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

// beginHandoff (an unauthenticated navigation to hosted content) sets the
// nonce cookie on the owner host itself and redirects to <base>/auth/handoff
// naming this exact URL, with n = sha256(nonce), base64url.
func TestHostGateBeginHandoff(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	handler := newHostGateTestHandler(t, store)

	request := httptest.NewRequest(http.MethodGet, "/my-site/?q=1", nil)
	request.Host = "alice.foo.example"
	request.Header.Set("Sec-Fetch-Dest", "document")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", response.Code)
	}

	var nonce string
	for _, c := range response.Result().Cookies() {
		if c.Name == handoffNonceCookie {
			nonce = c.Value
		}
	}
	if nonce == "" {
		t.Fatalf("no %s cookie set; headers %v", handoffNonceCookie, response.Header())
	}
	wantHash := sha256.Sum256([]byte(nonce))
	wantN := base64.RawURLEncoding.EncodeToString(wantHash[:])
	loc := response.Header().Get("Location")
	wantTo := "https%3A%2F%2Falice.foo.example%2Fmy-site%2F%3Fq%3D1"
	if !strings.Contains(loc, "to="+wantTo) || !strings.Contains(loc, "n="+wantN) {
		t.Fatalf("Location = %q, want to=%s and n=%s", loc, wantTo, wantN)
	}
	if !strings.HasPrefix(loc, "https://foo.example/auth/handoff?") {
		t.Fatalf("Location = %q, want the base host's /auth/handoff", loc)
	}
}

func TestHostGateRefusesCollidingLabels(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice.b", "my-site", "dotted")
	writeGateSite(t, store, "alice-b", "my-site", "dashed")
	writeGateSite(t, store, "alice-b", "only-dashed", "only-dashed-index")
	handler := newHostGateTestHandler(t, store)

	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	response := gateAuthedRequest(t, handler, http.MethodGet, "alice-b.foo.example", "/my-site/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("colliding label: status = %d, want 404 (body %q)", response.Code, response.Body.String())
	}
	if body := response.Body.String(); strings.Contains(body, "dotted") || strings.Contains(body, "dashed") {
		t.Fatalf("colliding label served a site: %q", body)
	}
	logged := logs.String()
	if !strings.Contains(logged, `"alice-b"`) || !strings.Contains(logged, "alice.b") || !strings.Contains(logged, "alice-b") {
		t.Fatalf("collision not logged with label and candidates: %q", logged)
	}

	// Redirects fail closed too: the bare short path must not 301.
	response = gateRequest(handler, http.MethodGet, "alice-b.foo.example", "/my-site")
	if response.Code != http.StatusNotFound {
		t.Fatalf("colliding label bare path: status = %d, want 404", response.Code)
	}

	// A site only one of them holds still resolves, because the collision is
	// per site: the other candidate has no current version of it.
	response = gateAuthedRequest(t, handler, http.MethodGet, "alice-b.foo.example", "/only-dashed/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "only-dashed-index") {
		t.Fatalf("uncontested site: status = %d body %q", response.Code, response.Body.String())
	}
}

// A candidate whose current version cannot be read must fail the whole
// resolution closed, not be skipped: otherwise the readable user on the same
// label would be handed the name. The quarantine latch is the one way to make
// CurrentVersion return an error other than "does not exist".
func TestHostGateUnreadableCandidateFailsClosed(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice.b", "my-site", "dotted")
	writeGateSite(t, store, "alice-b", "my-site", "dashed")
	if err := store.QuarantineCurrentServing("alice-b", "my-site"); err != nil {
		t.Fatal(err)
	}
	handler := newHostGateTestHandler(t, store)

	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	response := gateAuthedRequest(t, handler, http.MethodGet, "alice-b.foo.example", "/my-site/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("unreadable candidate: status = %d, want 404 (body %q)", response.Code, response.Body.String())
	}
	if body := response.Body.String(); strings.Contains(body, "dotted") || strings.Contains(body, "dashed") {
		t.Fatalf("unreadable candidate served a site: %q", body)
	}
	logged := logs.String()
	for _, want := range []string{`"alice-b"`, `"my-site"`, "unreadable"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("unreadable candidate not logged with %s: %q", want, logged)
		}
	}
	if strings.Count(logged, "unreadable") != 1 {
		t.Fatalf("expected exactly one unreadable log line: %q", logged)
	}
}

// A dotted username resolves on its dashed label through the short path.
func TestHostGateResolvesDottedUsername(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "Alice.Smith", "demo", "dotted-demo")
	handler := newHostGateTestHandler(t, store)

	response := gateAuthedRequest(t, handler, http.MethodGet, "alice-smith.foo.example", "/demo/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "dotted-demo") {
		t.Fatalf("status = %d body %q", response.Code, response.Body.String())
	}
	// A site with no current version is not served, even though the
	// directory exists.
	if err := store.WriteFiles("Alice.Smith", "draft", 1, map[string][]byte{"index.html": []byte("draft")}); err != nil {
		t.Fatal(err)
	}
	response = gateAuthedRequest(t, handler, http.MethodGet, "alice-smith.foo.example", "/draft/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("draft without current: status = %d", response.Code)
	}
}

func TestHostGateShortPathServesNoDirectoryListing(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	handler := newHostGateTestHandler(t, store)

	response := gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/my-site/assets/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if strings.Contains(response.Body.String(), "app.js") {
		t.Fatal("directory listing exposed app.js on the short path")
	}
}

func TestParseSiteAPIPath(t *testing.T) {
	for _, test := range []struct {
		path      string
		wantKind  siteAPIRouteKind
		wantSite  string
		wantID    string
		wantNamed bool
		wantOK    bool
	}{
		{path: "/api/sites/demo/state", wantKind: siteAPIState, wantSite: "demo", wantNamed: true, wantOK: true},
		{path: "/api/sites/demo/state/versioned", wantKind: siteAPIStateVersioned, wantSite: "demo", wantNamed: true, wantOK: true},
		{path: "/api/sites/demo/assets", wantKind: siteAPIAssetsCollection, wantSite: "demo", wantNamed: true, wantOK: true},
		{path: "/api/sites/demo/assets/abc-123", wantKind: siteAPIAssetItem, wantSite: "demo", wantID: "abc-123", wantNamed: true, wantOK: true},
		{path: "/api/site/state", wantKind: siteAPIState, wantOK: true},
		{path: "/api/site/state/versioned", wantKind: siteAPIStateVersioned, wantOK: true},
		{path: "/api/site/assets", wantKind: siteAPIAssetsCollection, wantOK: true},
		{path: "/api/site/assets/abc-123", wantKind: siteAPIAssetItem, wantID: "abc-123", wantOK: true},
		// Not this shape at all.
		{path: "/api/sites/demo", wantOK: false},
		{path: "/api/sites/demo/versions", wantOK: false},
		{path: "/api/sites/demo/state/", wantOK: false},
		{path: "/api/sites/demo/assets/", wantOK: false},
		{path: "/api/sites//state", wantOK: false},
		{path: "/api/sites/demo/assets/abc/extra", wantOK: false},
		{path: "/api/site/", wantOK: false},
		{path: "/api/sites", wantOK: false},
		{path: "/api/me", wantOK: false},
		// An unsafe site segment is refused, not merely ignored. This
		// function sees r.URL.Path, which net/http has already
		// percent-decoded, so the unsafe form to test is the decoded one.
		{path: "/api/sites/../state", wantOK: false},
	} {
		t.Run(test.path, func(t *testing.T) {
			kind, site, id, named, ok := parseSiteAPIPath(test.path)
			if ok != test.wantOK {
				t.Fatalf("parseSiteAPIPath(%q) ok = %t, want %t", test.path, ok, test.wantOK)
			}
			if !ok {
				return
			}
			if kind != test.wantKind || site != test.wantSite || id != test.wantID || named != test.wantNamed {
				t.Fatalf("parseSiteAPIPath(%q) = %v, %q, %q, %t; want %v, %q, %q, %t",
					test.path, kind, site, id, named, test.wantKind, test.wantSite, test.wantID, test.wantNamed)
			}
		})
	}
}

func TestParseAssetServePath(t *testing.T) {
	for _, test := range []struct {
		path     string
		wantID   string
		wantName string
		wantOK   bool
	}{
		{path: "_assets/abc-123", wantID: "abc-123", wantOK: true},
		{path: "_assets/abc-123/photo.png", wantID: "abc-123", wantName: "photo.png", wantOK: true},
		{path: "_assets/", wantOK: false},
		{path: "_assets", wantOK: false},
		{path: "other/path", wantOK: false},
		{path: "", wantOK: false},
	} {
		id, name, ok := parseAssetServePath(test.path)
		if ok != test.wantOK || id != test.wantID || name != test.wantName {
			t.Errorf("parseAssetServePath(%q) = %q, %q, %t; want %q, %q, %t", test.path, id, name, ok, test.wantID, test.wantName, test.wantOK)
		}
	}
}

// Review finding: a restricted site is root-served (design.md 5.2a), so its
// own pages carry no "/<site>/" path segment for the owner-host rule to
// find — comparing the Referer's first path segment against the site name
// there always fails, silently marking every legitimate restricted-site
// write as unobserved. A restricted site instead corroborates by the
// Referer's own host equalling the site's own "<owner>--<site>.<base>" host;
// an owner host keeps the path-segment rule, since several sites share one
// origin there and the host alone cannot tell them apart.
func TestViaSiteObserved(t *testing.T) {
	for _, test := range []struct {
		name       string
		dest       string
		referer    string
		claimed    string
		restricted bool
		ownHost    string
		want       bool
	}{
		{name: "owner host, matching path segment", dest: "empty", referer: "https://alice.foo.example/my-site/page", claimed: "my-site", want: true},
		{name: "owner host, different site's path segment", dest: "empty", referer: "https://alice.foo.example/other-site/", claimed: "my-site", want: false},
		{name: "owner host, restricted flag ignored when false", dest: "empty", referer: "https://alice.foo.example/my-site/", claimed: "my-site", restricted: false, ownHost: "alice--my-site.foo.example", want: true},
		{name: "restricted host, matching host", dest: "empty", referer: "https://alice--my-site.foo.example/page", claimed: "my-site", restricted: true, ownHost: "alice--my-site.foo.example", want: true},
		{name: "restricted host, path segment would have matched but host does not", dest: "empty", referer: "https://evil.example/my-site/", claimed: "my-site", restricted: true, ownHost: "alice--my-site.foo.example", want: false},
		{name: "restricted host, case-insensitive host match", dest: "empty", referer: "https://ALICE--MY-SITE.foo.example/page", claimed: "my-site", restricted: true, ownHost: "alice--my-site.foo.example", want: true},
		{name: "not a fetch (navigation)", dest: "document", referer: "https://alice--my-site.foo.example/page", claimed: "my-site", restricted: true, ownHost: "alice--my-site.foo.example", want: false},
		{name: "no Sec-Fetch-Dest at all", referer: "https://alice--my-site.foo.example/page", claimed: "my-site", restricted: true, ownHost: "alice--my-site.foo.example", want: false},
		{name: "no Referer", dest: "empty", claimed: "my-site", restricted: true, ownHost: "alice--my-site.foo.example", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if test.dest != "" {
				request.Header.Set("Sec-Fetch-Dest", test.dest)
			}
			if test.referer != "" {
				request.Header.Set("Referer", test.referer)
			}
			if got := viaSiteObserved(request, test.claimed, test.restricted, test.ownHost); got != test.want {
				t.Errorf("viaSiteObserved(...) = %t, want %t", got, test.want)
			}
		})
	}
}

// apiKeyGateRequest builds a request carrying the one X-API-Key
// fakeAuthMiddleware recognises.
func apiKeyGateRequest(method, host, target string) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	request.Host = host
	request.Header.Set("X-API-Key", hostGateTestAPIKey)
	return request
}

// The route auth matrix (session, key, none), on both an owner host and a
// restricted site's own host (design.md 7.3's explicit ask: "restricted
// sites get working state routes on their own host").
func TestHostGateSiteAPIAuthMatrix(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")

	for _, host := range []string{"alice.foo.example", "alice--my-site.foo.example"} {
		t.Run(host, func(t *testing.T) {
			path := "/api/sites/my-site/state"
			if host != "alice.foo.example" {
				// The restricted host's own convenience shape names no site.
				path = "/api/site/state"
			}
			gate := testHostGate(t, store, testHostModel(t))
			gate.siteForServing = func(r *http.Request, owner, site string) (string, bool, error) {
				return "site-1", host != "alice.foo.example", nil
			}
			fake := &fakeSiteAPI{}
			gate.siteAPI = fake
			handler := gate.wrap(newHostGateTestMux())

			// No credential at all: 401, never reaching the site API.
			response := gateRequest(handler, http.MethodGet, host, path)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("no credential: status = %d, want 401 (body %q)", response.Code, response.Body.String())
			}

			// A session cookie: dispatched as a person.
			response = gateAuthedRequest(t, handler, http.MethodGet, host, path)
			if response.Code != http.StatusOK || len(fake.calls) != 1 {
				t.Fatalf("session request: status = %d, calls = %d", response.Code, len(fake.calls))
			}
			if got := fake.calls[0]; got.call.ActorUserID != hostGateTestUserID || got.call.ActorKind != "person" || got.call.KeyID != "" {
				t.Fatalf("session call = %+v", got.call)
			}

			// An X-API-Key: dispatched as a key.
			fake.calls = nil
			keyRequest := apiKeyGateRequest(http.MethodGet, host, path)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, keyRequest)
			if recorder.Code != http.StatusOK || len(fake.calls) != 1 {
				t.Fatalf("key request: status = %d, calls = %d (body %q)", recorder.Code, len(fake.calls), recorder.Body.String())
			}
			if got := fake.calls[0]; got.call.ActorUserID != hostGateTestKeyUserID || got.call.ActorKind != "key" || got.call.KeyID != hostGateTestKeyID {
				t.Fatalf("key call = %+v", got.call)
			}

			// A garbage key: 401.
			fake.calls = nil
			badKeyRequest := httptest.NewRequest(http.MethodGet, path, nil)
			badKeyRequest.Host = host
			badKeyRequest.Header.Set("X-API-Key", "not-the-right-key")
			recorder = httptest.NewRecorder()
			handler.ServeHTTP(recorder, badKeyRequest)
			if recorder.Code != http.StatusUnauthorized || len(fake.calls) != 0 {
				t.Fatalf("bad key: status = %d, calls = %d", recorder.Code, len(fake.calls))
			}
		})
	}
}

// writerAllowed gates every write route on top of viewerAllowed: a caller
// who may view but not write gets 403 (existence already established), and
// a caller who may not even view gets 404 either way.
func TestHostGateSiteAPIWriterAllowedTable(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")

	for _, test := range []struct {
		name          string
		viewerAllowed bool
		writerAllowed bool
		method        string
		wantStatus    int
	}{
		{name: "read, viewer allowed", viewerAllowed: true, writerAllowed: false, method: http.MethodGet, wantStatus: http.StatusOK},
		{name: "read, viewer refused", viewerAllowed: false, writerAllowed: false, method: http.MethodGet, wantStatus: http.StatusNotFound},
		{name: "write, both allowed", viewerAllowed: true, writerAllowed: true, method: http.MethodPut, wantStatus: http.StatusOK},
		{name: "write, viewer allowed but not writer", viewerAllowed: true, writerAllowed: false, method: http.MethodPut, wantStatus: http.StatusForbidden},
		{name: "write, neither allowed", viewerAllowed: false, writerAllowed: false, method: http.MethodPut, wantStatus: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := testHostGate(t, store, testHostModel(t))
			gate.viewerAllowed = func(r *http.Request, siteID, userID string) (bool, error) { return test.viewerAllowed, nil }
			gate.writerAllowed = func(r *http.Request, siteID, userID string) (bool, error) { return test.writerAllowed, nil }
			handler := gate.wrap(newHostGateTestMux())

			request := authenticatedGateRequest(t, test.method, "alice.foo.example", "/api/sites/my-site/state")
			if test.method != http.MethodGet {
				request.Header.Set("Origin", "https://alice.foo.example")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

// Origin is required only on a non-safe method, and only when the caller is
// session-authenticated (design.md 7.3); it must equal the addressed host's
// own origin, not the base origin, on both an owner host and a restricted
// site's own host (origin.go's expectedFor).
func TestHostGateSiteAPIOriginCheckedOnNonSafeMethodsOnly(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")

	for _, test := range []struct {
		name       string
		host       string
		path       string
		method     string
		origin     string
		useKey     bool
		wantStatus int
	}{
		{name: "GET needs no Origin", host: "alice.foo.example", path: "/api/sites/my-site/state", method: http.MethodGet, wantStatus: http.StatusOK},
		{name: "PUT with the right Origin", host: "alice.foo.example", path: "/api/sites/my-site/state", method: http.MethodPut, origin: "https://alice.foo.example", wantStatus: http.StatusOK},
		{name: "PUT with the base Origin is wrong", host: "alice.foo.example", path: "/api/sites/my-site/state", method: http.MethodPut, origin: "https://foo.example", wantStatus: http.StatusForbidden},
		{name: "PUT with no Origin at all", host: "alice.foo.example", path: "/api/sites/my-site/state", method: http.MethodPut, wantStatus: http.StatusForbidden},
		{name: "restricted host PUT needs its own origin", host: "alice--my-site.foo.example", path: "/api/site/state", method: http.MethodPut, origin: "https://alice--my-site.foo.example", wantStatus: http.StatusOK},
		{name: "restricted host PUT with the owner's plain origin is wrong", host: "alice--my-site.foo.example", path: "/api/site/state", method: http.MethodPut, origin: "https://alice.foo.example", wantStatus: http.StatusForbidden},
		{name: "an X-API-Key skips Origin entirely", host: "alice.foo.example", path: "/api/sites/my-site/state", method: http.MethodPut, useKey: true, wantStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := testHostGate(t, store, testHostModel(t))
			gate.siteForServing = func(r *http.Request, owner, site string) (string, bool, error) {
				return "site-1", strings.Contains(test.host, "--"), nil
			}
			handler := gate.wrap(newHostGateTestMux())

			var request *http.Request
			if test.useKey {
				request = apiKeyGateRequest(test.method, test.host, test.path)
			} else {
				request = authenticatedGateRequest(t, test.method, test.host, test.path)
			}
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
}

// Method dispatch: GET/PUT on state and state/versioned, GET/POST on
// assets, DELETE on assets/{id}; anything else on the same path is 405.
func TestHostGateSiteAPIMethodDispatch(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	gate := testHostGate(t, store, testHostModel(t))
	fake := &fakeSiteAPI{}
	gate.siteAPI = fake
	handler := gate.wrap(newHostGateTestMux())

	for _, test := range []struct {
		method, path, wantCall string
		wantStatus             int
	}{
		{method: http.MethodGet, path: "/api/sites/my-site/state", wantCall: "GetState", wantStatus: http.StatusOK},
		{method: http.MethodGet, path: "/api/sites/my-site/state/versioned", wantCall: "GetStateVersioned", wantStatus: http.StatusOK},
		{method: http.MethodGet, path: "/api/sites/my-site/assets", wantCall: "ListAssets", wantStatus: http.StatusOK},
		{method: http.MethodPost, path: "/api/sites/my-site/assets", wantCall: "CreateAsset", wantStatus: http.StatusOK},
		{method: http.MethodDelete, path: "/api/sites/my-site/assets/abc-1", wantCall: "DeleteAsset", wantStatus: http.StatusOK},
		{method: http.MethodDelete, path: "/api/sites/my-site/state", wantStatus: http.StatusMethodNotAllowed},
		{method: http.MethodPost, path: "/api/sites/my-site/state", wantStatus: http.StatusMethodNotAllowed},
		{method: http.MethodDelete, path: "/api/sites/my-site/assets", wantStatus: http.StatusMethodNotAllowed},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			fake.calls = nil
			request := authenticatedGateRequest(t, test.method, "alice.foo.example", test.path)
			if test.method != http.MethodGet {
				request.Header.Set("Origin", "https://alice.foo.example")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", response.Code, test.wantStatus, response.Body.String())
			}
			if test.wantCall == "" {
				return
			}
			if len(fake.calls) != 1 || fake.calls[0].method != test.wantCall {
				t.Fatalf("calls = %+v, want exactly one %s", fake.calls, test.wantCall)
			}
			if test.wantCall == "DeleteAsset" && fake.calls[0].id != "abc-1" {
				t.Fatalf("DeleteAsset id = %q, want abc-1", fake.calls[0].id)
			}
		})
	}
}

// The named shape is not honoured on an owner host with no way to
// disambiguate it, but is honoured on a restricted site's own host, which
// serves exactly one site — and only when it names that exact site.
func TestHostGateNamelessShapeOnlyOnRestrictedHost(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")

	ownerGate := testHostGate(t, store, testHostModel(t))
	ownerHandler := ownerGate.wrap(newHostGateTestMux())
	response := gateAuthedRequest(t, ownerHandler, http.MethodGet, "alice.foo.example", "/api/site/state")
	if response.Code != http.StatusNotFound {
		t.Fatalf("nameless shape on an owner host: status = %d, want 404", response.Code)
	}

	restrictedGate := testHostGate(t, store, testHostModel(t))
	restrictedGate.siteForServing = func(r *http.Request, owner, site string) (string, bool, error) {
		return "site-1", true, nil
	}
	restrictedHandler := restrictedGate.wrap(newHostGateTestMux())
	response = gateAuthedRequest(t, restrictedHandler, http.MethodGet, "alice--my-site.foo.example", "/api/site/state")
	if response.Code != http.StatusOK {
		t.Fatalf("nameless shape on its own restricted host: status = %d, want 200", response.Code)
	}
	// The named shape naming a different site than this host resolves to is
	// refused outright, not treated as a hint to look elsewhere.
	response = gateAuthedRequest(t, restrictedHandler, http.MethodGet, "alice--my-site.foo.example", "/api/sites/some-other-site/state")
	if response.Code != http.StatusNotFound {
		t.Fatalf("mismatched named shape on a restricted host: status = %d, want 404", response.Code)
	}
	// The named shape naming the right site works too.
	response = gateAuthedRequest(t, restrictedHandler, http.MethodGet, "alice--my-site.foo.example", "/api/sites/my-site/state")
	if response.Code != http.StatusOK {
		t.Fatalf("matching named shape on a restricted host: status = %d, want 200", response.Code)
	}
}

// GET /{site}/_assets/{id} on an owner host, and GET /_assets/{id} on a
// restricted site's own host, reuse hosted content's own session and
// viewerAllowed checks and dispatch to ServeAsset.
func TestHostGateAssetServeRoute(t *testing.T) {
	store, _ := newServeTestStorage(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")

	t.Run("owner host", func(t *testing.T) {
		gate := testHostGate(t, store, testHostModel(t))
		fake := &fakeSiteAPI{}
		gate.siteAPI = fake
		handler := gate.wrap(newHostGateTestMux())

		response := gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/my-site/_assets/abc-1/photo.png")
		if response.Code != http.StatusOK || len(fake.calls) != 1 || fake.calls[0].method != "ServeAsset" || fake.calls[0].id != "abc-1" {
			t.Fatalf("status = %d, calls = %+v", response.Code, fake.calls)
		}
		// Unauthenticated, it goes through the same hand-off as hosted content.
		fake.calls = nil
		response = gateRequest(handler, http.MethodGet, "alice.foo.example", "/my-site/_assets/abc-1")
		if response.Code != http.StatusUnauthorized || len(fake.calls) != 0 {
			t.Fatalf("unauthenticated asset fetch: status = %d, calls = %d", response.Code, len(fake.calls))
		}
	})

	t.Run("restricted site host", func(t *testing.T) {
		gate := testHostGate(t, store, testHostModel(t))
		gate.siteForServing = func(r *http.Request, owner, site string) (string, bool, error) {
			return "site-1", true, nil
		}
		fake := &fakeSiteAPI{}
		gate.siteAPI = fake
		handler := gate.wrap(newHostGateTestMux())

		response := gateAuthedRequest(t, handler, http.MethodGet, "alice--my-site.foo.example", "/_assets/abc-1")
		if response.Code != http.StatusOK || len(fake.calls) != 1 || fake.calls[0].method != "ServeAsset" || fake.calls[0].id != "abc-1" {
			t.Fatalf("status = %d, calls = %+v", response.Code, fake.calls)
		}
	})
}
