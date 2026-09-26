package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// newHostGateTestHandler builds a mux that stands in for the real one: the
// real file handler, the real health routes, and recognisable stubs for the
// control-plane routes the gate must distinguish. State and assets are no
// longer mux routes at all: the gate dispatches those
// directly to a fake siteAPIRoutes (see testHostGate), never through this
// mux. It is wrapped in a gate whose siteForServing/viewerAllowed always
// admit an unrestricted site to any signed-in caller, which is what every
// test not specifically about restriction or refusal wants.
func newHostGateTestHandler(t *testing.T, store *testStore) http.Handler {
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
func testHostGate(t *testing.T, store *testStore, hosts HostModel) *hostGate {
	t.Helper()
	return &hostGate{
		hosts:       hosts,
		files:       NewSiteFiles(store.Store, nil, CookiePolicy{}, testSigningKeys, 0),
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

func writeGateSite(t *testing.T, store *testStore, user, site, body string) {
	t.Helper()
	store.publish(t, user, site, testSiteID(user, site), 1, map[string]string{
		"index.html":    body,
		"assets/app.js": "console.log(1)",
	})
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

// The site host every test below addresses: alice's site "my-site".
const aliceSiteHost = "my-site.alice.foo.example"

func TestHostGateRouting(t *testing.T) {
	store := newTestStore(t)
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
	// mux route on the base host at all any more, so these
	// simply find no route to answer them.
	for _, path := range []string{"/api/site/state", "/api/site/state/versioned", "/api/sites/my-site/state", "/api/sites/alice/my-site/state"} {
		response := gateRequest(handler, http.MethodGet, "foo.example", path)
		if response.Code != http.StatusNotFound {
			t.Errorf("base host %s: status = %d, want 404", path, response.Code)
		}
	}

	// Site host: an unauthenticated navigation to hosted content is sent
	// through the hand-off, never served directly.
	navigation := httptest.NewRequest(http.MethodGet, "/", nil)
	navigation.Host = aliceSiteHost
	navigation.Header.Set("Accept", "text/html")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, navigation)
	if response.Code != http.StatusFound {
		t.Fatalf("unauthenticated navigation: status = %d, want 302", response.Code)
	}
	if loc := response.Header().Get("Location"); !strings.HasPrefix(loc, "https://foo.example/auth/handoff?") {
		t.Fatalf("unauthenticated navigation redirected to %q", loc)
	}

	// Site host: authenticated, it serves, root-first (no site segment).
	response = gateAuthedRequest(t, handler, http.MethodGet, aliceSiteHost, "/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "alice-index") {
		t.Fatalf("authenticated site host: status = %d body %q", response.Code, response.Body.String())
	}
	response = gateAuthedRequest(t, handler, http.MethodGet, "x.bob.foo.example", "/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "bob-index") {
		t.Fatalf("authenticated site host: status = %d body %q", response.Code, response.Body.String())
	}
	// A 1-2 character owner label is a valid site host too.
	writeGateSite(t, store, "al", "s", "short-index")
	response = gateAuthedRequest(t, handler, http.MethodGet, "s.al.foo.example", "/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "short-index") {
		t.Fatalf("short owner label: status = %d body %q", response.Code, response.Body.String())
	}
	// An "xn--" label is never a site host: it reads as punycode.
	response = gateAuthedRequest(t, handler, http.MethodGet, "xn--my-site.foo.example", "/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("xn-- label: status = %d, want 404", response.Code)
	}
	// A non-navigation request with no session gets 401, not the hand-off.
	response = gateRequest(handler, http.MethodGet, aliceSiteHost, "/")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated non-navigation: status = %d, want 401", response.Code)
	}
	// A method other than GET/HEAD is refused before any auth check.
	response = gateRequest(handler, http.MethodPost, aliceSiteHost, "/")
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST hosted content: status = %d, want 405", response.Code)
	}
	// A path the site does not have, and a site host naming no site, are
	// both 404 with a session present.
	for _, test := range []struct{ host, path string }{
		{aliceSiteHost, "/api/anything"},
		{aliceSiteHost, "/does-not-exist/"},
		{"does-not-exist.alice.foo.example", "/"},
		{"my-site.nobody.foo.example", "/"},
	} {
		response = gateAuthedRequest(t, handler, http.MethodGet, test.host, test.path)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s%s: status = %d, want 404", test.host, test.path, response.Code)
		}
	}
	// Owner and site hosts: probes still answer with no session at all, and
	// every response carries the CORP and Cache-Control headers regardless
	// of the path (the latter was added later, a review finding — hosted
	// content never got Cache-Control at all).
	for _, host := range []string{"alice.foo.example", aliceSiteHost} {
		for _, path := range []string{"/healthz", "/readyz"} {
			response = gateRequest(handler, http.MethodGet, host, path)
			if response.Code != http.StatusOK {
				t.Errorf("%s%s: status = %d, want 200", host, path, response.Code)
			}
			if got := response.Header().Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
				t.Errorf("%s%s: CORP header = %q, want same-origin", host, path, got)
			}
			if got := response.Header().Get("Cache-Control"); got != "private, no-cache" {
				t.Errorf("%s%s: Cache-Control = %q, want %q", host, path, got, "private, no-cache")
			}
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

// The owner host serves no site content and no site API: every pre-v1.3
// address beneath it redirects to the site's own host, path and query kept,
// 301 for a read and 308 for anything else. The redirect is computed from
// the address alone — no site lookup, no auth — so it confirms nothing: a
// site that does not exist, a caller with no session, and a caller the site
// would refuse all get the same answer.
func TestHostGateOwnerHostRedirects(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	writeGateSite(t, store, "alice", "two words", "spaced-index")
	spaced := "https://" + siteHostPart("two words") + ".alice.foo.example"

	gate := testHostGate(t, store, testHostModel(t))
	lookups := 0
	gate.siteForServing = func(r *http.Request, owner, site string) (string, bool, error) {
		lookups++
		return "site-1", true, nil
	}
	gate.viewerAllowed = func(r *http.Request, siteID, userID string) (bool, error) {
		lookups++
		return false, nil
	}
	fake := &fakeSiteAPI{}
	gate.siteAPI = fake
	handler := gate.wrap(newHostGateTestMux())

	for _, test := range []struct {
		name, host, method, target string
		want                       int
		location                   string
	}{
		{"site root", "alice.foo.example", http.MethodGet, "/my-site/", http.StatusMovedPermanently, "https://my-site.alice.foo.example/"},
		{"bare site segment", "alice.foo.example", http.MethodGet, "/my-site", http.StatusMovedPermanently, "https://my-site.alice.foo.example/"},
		{"path and query kept", "alice.foo.example", http.MethodGet, "/my-site/a/b.html?x=1&y=two", http.StatusMovedPermanently, "https://my-site.alice.foo.example/a/b.html?x=1&y=two"},
		{"HEAD is a read", "alice.foo.example", http.MethodHead, "/my-site/a", http.StatusMovedPermanently, "https://my-site.alice.foo.example/a"},
		{"POST keeps its method", "alice.foo.example", http.MethodPost, "/my-site/form?z=1", http.StatusPermanentRedirect, "https://my-site.alice.foo.example/form?z=1"},
		{"asset serve path", "alice.foo.example", http.MethodGet, "/my-site/_assets/abc-1/photo.png", http.StatusMovedPermanently, "https://my-site.alice.foo.example/_assets/abc-1/photo.png"},
		{"state read keeps the full path", "alice.foo.example", http.MethodGet, "/api/sites/my-site/state?v=1", http.StatusMovedPermanently, "https://my-site.alice.foo.example/api/sites/my-site/state?v=1"},
		{"state write", "alice.foo.example", http.MethodPut, "/api/sites/my-site/state", http.StatusPermanentRedirect, "https://my-site.alice.foo.example/api/sites/my-site/state"},
		{"versioned state", "alice.foo.example", http.MethodGet, "/api/sites/my-site/state/versioned", http.StatusMovedPermanently, "https://my-site.alice.foo.example/api/sites/my-site/state/versioned"},
		{"asset delete", "alice.foo.example", http.MethodDelete, "/api/sites/my-site/assets/abc-1", http.StatusPermanentRedirect, "https://my-site.alice.foo.example/api/sites/my-site/assets/abc-1"},
		{"a site that does not exist redirects too", "alice.foo.example", http.MethodGet, "/nosuch/page", http.StatusMovedPermanently, "https://nosuch.alice.foo.example/page"},
		{"an owner that does not exist redirects too", "nobody.foo.example", http.MethodGet, "/x/", http.StatusMovedPermanently, "https://x.nobody.foo.example/"},
		{"reserved segment api is not a site", "alice.foo.example", http.MethodGet, "/api/collaboration/sites", http.StatusNotFound, ""},
		{"nameless API shape is not a site", "alice.foo.example", http.MethodGet, "/api/site/state", http.StatusNotFound, ""},
		{"reserved segment sites is not a site", "alice.foo.example", http.MethodGet, "/sites/anything", http.StatusNotFound, ""},
		{"a pre-v1.3 name with a space goes to its hashed part", "alice.foo.example", http.MethodGet, "/two%20words/p?q=1", http.StatusMovedPermanently, spaced + "/p?q=1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, authed := range []bool{false, true} {
				fake.calls = nil
				lookups = 0
				var response *httptest.ResponseRecorder
				if authed {
					response = gateAuthedRequest(t, handler, test.method, test.host, test.target)
				} else {
					response = gateRequest(handler, test.method, test.host, test.target)
				}
				if response.Code != test.want {
					t.Fatalf("authed=%t: status = %d, want %d (body %q)", authed, response.Code, test.want, response.Body.String())
				}
				if got := response.Header().Get("Location"); got != test.location {
					t.Fatalf("authed=%t: Location = %q, want %q", authed, got, test.location)
				}
				if lookups != 0 || len(fake.calls) != 0 {
					t.Fatalf("authed=%t: redirect consulted the site (lookups %d, API calls %+v)", authed, lookups, fake.calls)
				}
			}
		})
	}

	// The redirect target serves the pre-v1.3 name with a space.
	gate.viewerAllowed = func(r *http.Request, siteID, userID string) (bool, error) { return true, nil }
	response := gateAuthedRequest(t, handler, http.MethodGet, strings.TrimPrefix(spaced, "https://"), "/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "spaced-index") {
		t.Fatalf("hashed site host: status = %d body %q", response.Code, response.Body.String())
	}
}

// The owner host's root is still the owner's index, behind the host
// session: unauthenticated it is a 401 (or the hand-off for a navigation),
// never a redirect or a listing.
func TestHostGateOwnerHostRootServesIndexBehindSession(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	gate := testHostGate(t, store, testHostModel(t))
	gate.ownerIndex = func(_ context.Context, owner string) (string, []db.Site, map[string]bool, error) {
		return "owner-user", nil, nil, nil
	}
	handler := gate.wrap(newHostGateTestMux())

	response := gateRequest(handler, http.MethodGet, "alice.foo.example", "/")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated owner index: status = %d, want 401", response.Code)
	}
	navigation := httptest.NewRequest(http.MethodGet, "/", nil)
	navigation.Host = "alice.foo.example"
	navigation.Header.Set("Sec-Fetch-Dest", "document")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, navigation)
	if recorder.Code != http.StatusFound || !strings.HasPrefix(recorder.Header().Get("Location"), "https://foo.example/auth/handoff?") {
		t.Fatalf("unauthenticated owner index navigation: status = %d Location %q", recorder.Code, recorder.Header().Get("Location"))
	}
	response = gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/")
	if response.Code != http.StatusOK || response.Header().Get("Location") != "" {
		t.Fatalf("authenticated owner index: status = %d Location %q", response.Code, response.Header().Get("Location"))
	}
}

// A pre-v1.3 team address ("sales") now belongs to "team-sales": the owner
// host redirects its root and every old site path to the same path on the
// team's owner host (which then serves or redirects by its own readiness),
// and a v1.2 "sales--deck" address redirects to the team's site, path and
// query kept.
func TestHostGateLegacyTeamRedirects(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "team-sales", "deck", "deck-index")
	gate := testHostGate(t, store, testHostModel(t))
	gate.legacyTeam = func(r *http.Request, label string) (string, bool, error) {
		switch label {
		case "sales":
			return "team-sales", true, nil
		case "broken":
			return "", false, errors.New("database down")
		}
		return "", false, nil
	}
	gate.ownerIndex = func(_ context.Context, owner string) (string, []db.Site, map[string]bool, error) {
		return "owner-user", nil, nil, nil
	}
	handler := gate.wrap(newHostGateTestMux())

	for _, test := range []struct {
		name, host, method, target string
		want                       int
		location                   string
	}{
		{"owner host site path", "sales.foo.example", http.MethodGet, "/deck/p?q=1", http.StatusMovedPermanently, "https://team-sales.foo.example/deck/p?q=1"},
		{"owner host site API", "sales.foo.example", http.MethodPut, "/api/sites/deck/state", http.StatusPermanentRedirect, "https://team-sales.foo.example/api/sites/deck/state"},
		{"owner host root", "sales.foo.example", http.MethodGet, "/", http.StatusMovedPermanently, "https://team-sales.foo.example/"},
		{"legacy site host", "sales--deck.foo.example", http.MethodGet, "/p?q=1", http.StatusMovedPermanently, "https://deck.team-sales.foo.example/p?q=1"},
		{"legacy site host write", "sales--deck.foo.example", http.MethodPost, "/api/site/assets", http.StatusPermanentRedirect, "https://deck.team-sales.foo.example/api/site/assets"},
		{"not a team: owner host keeps its label", "nobody.foo.example", http.MethodGet, "/deck/", http.StatusMovedPermanently, "https://deck.nobody.foo.example/"},
		{"not a team: legacy site host is 404", "nobody--deck.foo.example", http.MethodGet, "/", http.StatusNotFound, ""},
		{"lookup error: legacy site host is 404", "broken--deck.foo.example", http.MethodGet, "/", http.StatusNotFound, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := gateAuthedRequest(t, handler, test.method, test.host, test.target)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d (body %q)", response.Code, test.want, response.Body.String())
			}
			if got := response.Header().Get("Location"); got != test.location {
				t.Fatalf("Location = %q, want %q", got, test.location)
			}
		})
	}

	// The owner index checks the session before it resolves anything, so
	// the legacy redirect of "/" confirms nothing to an anonymous caller.
	response := gateRequest(handler, http.MethodGet, "sales.foo.example", "/")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated legacy owner root: status = %d, want 401", response.Code)
	}
	// The legacy site host is not session-gated before the redirect: an anonymous
	// caller is sent on too.
	response = gateRequest(handler, http.MethodGet, "sales--deck.foo.example", "/")
	if response.Code != http.StatusMovedPermanently || response.Header().Get("Location") != "https://deck.team-sales.foo.example/" {
		t.Fatalf("unauthenticated legacy site host: status = %d Location %q", response.Code, response.Header().Get("Location"))
	}
	// And the target serves.
	response = gateAuthedRequest(t, handler, http.MethodGet, "deck.team-sales.foo.example", "/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "deck-index") {
		t.Fatalf("team site host: status = %d body %q", response.Code, response.Body.String())
	}
}

// A session cookie minted for one host (by the hand-off) authenticates the
// site-facing API only on that host: not on the owner's own host, not on a
// sibling site of the same owner, not on another owner's site, and a
// base-host cookie (no host binding) not on any site host.
func TestHostGateSiteAPISessionBoundToHost(t *testing.T) {
	store := newTestStore(t)
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
		{"own host", aliceSiteHost, http.StatusOK},
		{"the owner's host", "alice.foo.example", http.StatusUnauthorized},
		{"a sibling site of the same owner", "other.alice.foo.example", http.StatusUnauthorized},
		{"another owner's site", "my-site.mallory.foo.example", http.StatusUnauthorized},
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
			request := httptest.NewRequest(http.MethodGet, "/api/site/state", nil)
			request.Host = aliceSiteHost
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

// Two sites of the same owner are two origins: a session cookie the hand-off
// minted for a.alice is refused on b.alice, for content and for the API,
// and a page on a.alice cannot write b.alice's state with b.alice's own
// cookie either (Origin mismatch).
func TestHostGateSitesOfOneOwnerAreSeparateOrigins(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "a", "a-index")
	writeGateSite(t, store, "alice", "b", "b-index")
	gate := testHostGate(t, store, testHostModel(t))
	fake := &fakeSiteAPI{}
	gate.siteAPI = fake
	handler := gate.wrap(newHostGateTestMux())

	cookieA, err := auth.SignHostSession(testSigningKeys, "session-1", "user-1", "a.alice.foo.example", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	serve := func(method, host, target string, headers map[string]string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, target, nil)
		request.Host = host
		request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: cookieA})
		for k, v := range headers {
			request.Header.Set(k, v)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	if response := serve(http.MethodGet, "a.alice.foo.example", "/", nil); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "a-index") {
		t.Fatalf("a's cookie on a: status = %d body %q", response.Code, response.Body.String())
	}
	if response := serve(http.MethodGet, "b.alice.foo.example", "/", nil); response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), "b-index") {
		t.Fatalf("a's cookie on b: status = %d, want 401 (body %q)", response.Code, response.Body.String())
	}
	// A navigation with a's cookie on b is sent through b's own hand-off,
	// not served.
	if response := serve(http.MethodGet, "b.alice.foo.example", "/", map[string]string{"Sec-Fetch-Dest": "document"}); response.Code != http.StatusFound {
		t.Fatalf("a's cookie navigating b: status = %d, want 302", response.Code)
	}
	fake.calls = nil
	if response := serve(http.MethodGet, "b.alice.foo.example", "/api/site/state", nil); response.Code != http.StatusUnauthorized || len(fake.calls) != 0 {
		t.Fatalf("a's cookie on b's API: status = %d, calls %+v", response.Code, fake.calls)
	}

	// b's own cookie, but the write comes from a page on a.
	request := authenticatedGateRequest(t, http.MethodPut, "b.alice.foo.example", "/api/site/state")
	request.Header.Set("Origin", "https://a.alice.foo.example")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || len(fake.calls) != 0 {
		t.Fatalf("cross-site write from a to b: status = %d, calls %+v", response.Code, fake.calls)
	}
}

// A site that fails viewerAllowed is 404, not 403: its existence must not be
// confirmed to somebody it refuses.
func TestHostGateViewerNotAllowedIs404(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "private", "private-index")
	gate := testHostGate(t, store, testHostModel(t))
	gate.viewerAllowed = func(r *http.Request, siteID, userID string) (bool, error) { return false, nil }
	handler := gate.wrap(newHostGateTestMux())

	response := gateAuthedRequest(t, handler, http.MethodGet, "private.alice.foo.example", "/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("viewer not allowed: status = %d, want 404", response.Code)
	}
}

// Every site is served on its own host whatever its access level: the
// restricted flag no longer decides which host answers.
func TestHostGateServesEverySiteOnItsOwnHost(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "private", "private-index")
	for _, restricted := range []bool{true, false} {
		gate := testHostGate(t, store, testHostModel(t))
		gate.siteForServing = func(r *http.Request, owner, site string) (string, bool, error) {
			return "site-1", restricted, nil
		}
		handler := gate.wrap(newHostGateTestMux())

		response := gateAuthedRequest(t, handler, http.MethodGet, "private.alice.foo.example", "/")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "private-index") {
			t.Fatalf("restricted=%t: status = %d body %q", restricted, response.Code, response.Body.String())
		}
		if got := response.Header().Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
			t.Errorf("restricted=%t: CORP header = %q, want same-origin", restricted, got)
		}

		// An unknown label resolves to no site at all.
		response = gateAuthedRequest(t, handler, http.MethodGet, "nosuch.alice.foo.example", "/")
		if response.Code != http.StatusNotFound {
			t.Fatalf("restricted=%t: unknown site: status = %d, want 404", restricted, response.Code)
		}
	}
}

// A network-open site is readable anonymously on its own host (content and
// state read); a write, or ?signin, still needs a session.
func TestHostGateNetworkOpenSiteOnItsOwnHost(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	gate := testHostGate(t, store, testHostModel(t))
	gate.networkOpen = func(r *http.Request, siteID string) (bool, error) { return true, nil }
	fake := &fakeSiteAPI{}
	gate.siteAPI = fake
	handler := gate.wrap(newHostGateTestMux())

	response := gateRequest(handler, http.MethodGet, aliceSiteHost, "/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "alice-index") {
		t.Fatalf("anonymous read of a network-open site: status = %d body %q", response.Code, response.Body.String())
	}
	response = gateRequest(handler, http.MethodGet, aliceSiteHost, "/api/site/state")
	if response.Code != http.StatusOK || len(fake.calls) != 1 || fake.calls[0].call.ActorKind != "anonymous" {
		t.Fatalf("anonymous state read: status = %d calls %+v", response.Code, fake.calls)
	}
	fake.calls = nil
	response = gateRequest(handler, http.MethodPut, aliceSiteHost, "/api/site/state")
	if response.Code != http.StatusUnauthorized || len(fake.calls) != 0 {
		t.Fatalf("anonymous state write: status = %d calls %+v", response.Code, fake.calls)
	}
	response = gateRequest(handler, http.MethodGet, aliceSiteHost, "/?signin")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("?signin without a session: status = %d, want 401", response.Code)
	}
}

// A host session cookie is bound to the host it was minted for (a
// hardening over "same signed payload" — see
// docs/security-review.md's deviation record): a cookie
// alice's site handed out must not authenticate a request on bob's site,
// even though both are site hosts under the same base domain and the
// underlying session row is shared on purpose.
func TestHostGateSessionCookieBoundToItsOwnHost(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	writeGateSite(t, store, "bob", "x", "bob-index")
	handler := newHostGateTestHandler(t, store)

	aliceCookie, err := auth.SignHostSession(testSigningKeys, "session-1", "user-1", aliceSiteHost, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SignHostSession: %v", err)
	}

	// Presented back on alice's own site host: serves.
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Host = aliceSiteHost
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: aliceCookie})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("alice's cookie on alice's site: status = %d, want 200", response.Code)
	}

	// Presented on bob's site host: refused. A non-navigation request with
	// an invalid cookie gets 401 (the same as no cookie at all), not access
	// to bob's site.
	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.Host = "x.bob.foo.example"
	request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: aliceCookie})
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("alice's cookie on bob's site: status = %d, want 401 (body %q)", response.Code, response.Body.String())
	}
}

// A review finding: hosted-content auth never touched last_seen_at,
// so an actively-browsing viewer's session went idle and was killed by
// SESSION_IDLE even while they kept viewing (touchHostSession runs at most
// once per five minutes from hosted content). requireHostSession must call
// touchHostSession on every successful auth. touchHostSession's own
// signature (no return value) is what makes "never block serving on its
// error" structural rather than a runtime check: NewHostGate's default
// implementation (db.TouchSession) only ever logs a failure, and there is
// nothing here for a caller to propagate even if it wanted to.
func TestHostGateTouchesSessionOnSuccessfulHostedAuth(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	gate := testHostGate(t, store, testHostModel(t))
	var touched []string
	gate.touchHostSession = func(sessionID string) { touched = append(touched, sessionID) }
	handler := gate.wrap(newHostGateTestMux())

	response := gateAuthedRequest(t, handler, http.MethodGet, aliceSiteHost, "/")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if len(touched) != 1 || touched[0] != hostGateTestSessionID {
		t.Fatalf("touchHostSession calls = %v, want exactly one call with %q", touched, hostGateTestSessionID)
	}
}

// A sibling-origin subresource load (Sec-Fetch-Site same-site
// or cross-site, Sec-Fetch-Dest anything but document) is refused even
// though the request otherwise carries no credential problem; a navigation
// (Sec-Fetch-Dest document) is let through to the ordinary checks. The owner
// host applies the same rule before it redirects.
func TestHostGateRefusesSiblingOriginSubresourceLoads(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	handler := newHostGateTestHandler(t, store)

	for _, test := range []struct {
		name, host, path string
		site, dest       string
		wantStatus       int
	}{
		{name: "same-site script load", host: aliceSiteHost, path: "/", site: "same-site", dest: "script", wantStatus: http.StatusForbidden},
		{name: "cross-site image load", host: aliceSiteHost, path: "/", site: "cross-site", dest: "image", wantStatus: http.StatusForbidden},
		{name: "same-site navigation still works", host: aliceSiteHost, path: "/", site: "same-site", dest: "document", wantStatus: http.StatusOK},
		{name: "no Sec-Fetch headers at all (curl, an old browser)", host: aliceSiteHost, path: "/", wantStatus: http.StatusOK},
		{name: "same-origin fetch", host: aliceSiteHost, path: "/", site: "same-origin", dest: "empty", wantStatus: http.StatusOK},
		{name: "same-site script load via the owner host", host: "alice.foo.example", path: "/my-site/assets/app.js", site: "same-site", dest: "script", wantStatus: http.StatusForbidden},
		{name: "same-site navigation via the owner host redirects", host: "alice.foo.example", path: "/my-site/", site: "same-site", dest: "document", wantStatus: http.StatusMovedPermanently},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := authenticatedGateRequest(t, http.MethodGet, test.host, test.path)
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
// nonce cookie on the site host itself and redirects to <base>/auth/handoff
// naming this exact URL, with n = sha256(nonce), base64url.
func TestHostGateBeginHandoff(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	handler := newHostGateTestHandler(t, store)

	request := httptest.NewRequest(http.MethodGet, "/?q=1", nil)
	request.Host = aliceSiteHost
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
	wantTo := "https%3A%2F%2Fmy-site.alice.foo.example%2F%3Fq%3D1"
	if !strings.Contains(loc, "to="+wantTo) || !strings.Contains(loc, "n="+wantN) {
		t.Fatalf("Location = %q, want to=%s and n=%s", loc, wantTo, wantN)
	}
	if !strings.HasPrefix(loc, "https://foo.example/auth/handoff?") {
		t.Fatalf("Location = %q, want the base host's /auth/handoff", loc)
	}
}

// Two accounts whose usernames fold to one owner label: the site host's
// owner part names neither, for any of their sites (the collision is per
// label, not per site), and the refusal is logged.
func TestHostGateRefusesCollidingLabels(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice.b", "my-site", "dotted")
	writeGateSite(t, store, "alice-b", "my-site", "dashed")
	writeGateSite(t, store, "alice-b", "only-dashed", "only-dashed-index")
	handler := newHostGateTestHandler(t, store)

	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	for _, host := range []string{"my-site.alice-b.foo.example", "only-dashed.alice-b.foo.example"} {
		response := gateAuthedRequest(t, handler, http.MethodGet, host, "/")
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404 (body %q)", host, response.Code, response.Body.String())
		}
		if body := response.Body.String(); strings.Contains(body, "dotted") || strings.Contains(body, "dashed") {
			t.Fatalf("%s: colliding label served a site: %q", host, body)
		}
		response = gateAuthedRequest(t, handler, http.MethodGet, host, "/api/site/state")
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s API: status = %d, want 404", host, response.Code)
		}
	}
	logged := logs.String()
	if !strings.Contains(logged, `"alice-b"`) || !strings.Contains(logged, "alice.b") || !strings.Contains(logged, "alice-b") {
		t.Fatalf("collision not logged with label and candidates: %q", logged)
	}
}

// Two pre-v1.3 sites of one owner whose names fold to the same host part:
// the one whose name is exactly the part keeps the address; with no exact
// match neither is served, and the refusal is logged.
func TestHostGateCollidingSitePartsFailClosed(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "exact")
	writeGateSite(t, store, "alice", "My-Site", "folded")
	writeGateSite(t, store, "alice", "Other.Site", "fold-one")
	writeGateSite(t, store, "alice", "other.site", "fold-two")
	handler := newHostGateTestHandler(t, store)

	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	response := gateAuthedRequest(t, handler, http.MethodGet, aliceSiteHost, "/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "exact") {
		t.Fatalf("exact name: status = %d body %q", response.Code, response.Body.String())
	}
	response = gateAuthedRequest(t, handler, http.MethodGet, "other-site.alice.foo.example", "/")
	if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "fold-") {
		t.Fatalf("ambiguous fold: status = %d body %q", response.Code, response.Body.String())
	}
	if logged := logs.String(); !strings.Contains(logged, `"other-site"`) || !strings.Contains(logged, "Other.Site") || !strings.Contains(logged, "other.site") {
		t.Fatalf("site part collision not logged with part and candidates: %q", logged)
	}
}

// A candidate whose current version cannot be read must fail the whole
// resolution closed, not be skipped: otherwise the readable site folding to
// the same host part would be handed the address.
func TestHostGateUnreadableCandidateFailsClosed(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "exact")
	writeGateSite(t, store, "alice", "My.Site", "folded")
	store.index.fail("alice", "my-site")
	handler := newHostGateTestHandler(t, store)

	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	response := gateAuthedRequest(t, handler, http.MethodGet, aliceSiteHost, "/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("unreadable candidate: status = %d, want 404 (body %q)", response.Code, response.Body.String())
	}
	if body := response.Body.String(); strings.Contains(body, "exact") || strings.Contains(body, "folded") {
		t.Fatalf("unreadable candidate served a site: %q", body)
	}
	logged := logs.String()
	for _, want := range []string{`"alice"`, `"my-site"`, "unreadable"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("unreadable candidate not logged with %s: %q", want, logged)
		}
	}
	if strings.Count(logged, "unreadable") != 1 {
		t.Fatalf("expected exactly one unreadable log line: %q", logged)
	}
}

// A dotted username resolves on its dashed label as a site host's owner
// part.
func TestHostGateResolvesDottedUsername(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "Alice.Smith", "demo", "dotted-demo")
	handler := newHostGateTestHandler(t, store)

	response := gateAuthedRequest(t, handler, http.MethodGet, "demo.alice-smith.foo.example", "/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "dotted-demo") {
		t.Fatalf("status = %d body %q", response.Code, response.Body.String())
	}
	// An uploaded version no site row makes live is not served.
	if _, err := store.PutVersion(context.Background(), testSiteID("Alice.Smith", "draft"), 1, map[string][]byte{"index.html": []byte("draft")}); err != nil {
		t.Fatal(err)
	}
	response = gateAuthedRequest(t, handler, http.MethodGet, "draft.alice-smith.foo.example", "/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("draft without current: status = %d", response.Code)
	}
}

// A site created before v1.3 whose name is not a label ("two words") is
// served at its hashed host part, and only there.
func TestHostGateServesSpacedNameAtHashedPart(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "two words", "spaced-index")
	handler := newHostGateTestHandler(t, store)

	part := siteHostPart("two words")
	if !strings.HasPrefix(part, "two-words-") || len(part) != len("two-words-")+siteHashLen {
		t.Fatalf("siteHostPart(alice, two words) = %q", part)
	}
	response := gateAuthedRequest(t, handler, http.MethodGet, part+".alice.foo.example", "/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "spaced-index") {
		t.Fatalf("hashed part: status = %d body %q", response.Code, response.Body.String())
	}
	response = gateAuthedRequest(t, handler, http.MethodGet, "two-words.alice.foo.example", "/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("unhashed fold: status = %d, want 404", response.Code)
	}
}

func TestHostGateSiteHostServesNoDirectoryListing(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	handler := newHostGateTestHandler(t, store)

	response := gateAuthedRequest(t, handler, http.MethodGet, aliceSiteHost, "/assets/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if strings.Contains(response.Body.String(), "app.js") {
		t.Fatal("directory listing exposed app.js on the site host")
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

// Every site is root-served on its own host, so the via_site claim is
// corroborated by the Referer's own host equalling the site's host.
func TestViaSiteObserved(t *testing.T) {
	const own = "my-site.alice.foo.example"
	for _, test := range []struct {
		name    string
		dest    string
		referer string
		want    bool
	}{
		{name: "matching host", dest: "empty", referer: "https://my-site.alice.foo.example/page", want: true},
		{name: "case-insensitive host match", dest: "empty", referer: "https://MY-SITE.ALICE.foo.example/page", want: true},
		{name: "owner host is another origin", dest: "empty", referer: "https://alice.foo.example/my-site/", want: false},
		{name: "sibling site of the same owner", dest: "empty", referer: "https://other.alice.foo.example/", want: false},
		{name: "foreign host", dest: "empty", referer: "https://evil.example/my-site/", want: false},
		{name: "not a fetch (navigation)", dest: "document", referer: "https://my-site.alice.foo.example/page", want: false},
		{name: "no Sec-Fetch-Dest at all", referer: "https://my-site.alice.foo.example/page", want: false},
		{name: "no Referer", dest: "empty", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if test.dest != "" {
				request.Header.Set("Sec-Fetch-Dest", test.dest)
			}
			if test.referer != "" {
				request.Header.Set("Referer", test.referer)
			}
			if got := viaSiteObserved(request, own); got != test.want {
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

// The route auth matrix (session, key, none) on a site's own host, in both
// the nameless and the named shape, whatever the site's access level.
func TestHostGateSiteAPIAuthMatrix(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")

	for _, restricted := range []bool{false, true} {
		for _, path := range []string{"/api/site/state", "/api/sites/my-site/state"} {
			t.Run(fmt.Sprintf("restricted=%t %s", restricted, path), func(t *testing.T) {
				gate := testHostGate(t, store, testHostModel(t))
				gate.siteForServing = func(r *http.Request, owner, site string) (string, bool, error) {
					return "site-1", restricted, nil
				}
				fake := &fakeSiteAPI{}
				gate.siteAPI = fake
				handler := gate.wrap(newHostGateTestMux())

				// No credential at all: 401, never reaching the site API.
				response := gateRequest(handler, http.MethodGet, aliceSiteHost, path)
				if response.Code != http.StatusUnauthorized || len(fake.calls) != 0 {
					t.Fatalf("no credential: status = %d, want 401 (body %q)", response.Code, response.Body.String())
				}

				// A session cookie: dispatched as a person.
				response = gateAuthedRequest(t, handler, http.MethodGet, aliceSiteHost, path)
				if response.Code != http.StatusOK || len(fake.calls) != 1 {
					t.Fatalf("session request: status = %d, calls = %d", response.Code, len(fake.calls))
				}
				if got := fake.calls[0]; got.call.ActorUserID != hostGateTestUserID || got.call.ActorKind != "person" || got.call.KeyID != "" ||
					got.call.Owner != "alice" || got.call.SiteName != "my-site" || got.call.Restricted != restricted || got.call.ViaSiteLabel != "my-site.alice" {
					t.Fatalf("session call = %+v", got.call)
				}

				// An X-API-Key: dispatched as a key.
				fake.calls = nil
				keyRequest := apiKeyGateRequest(http.MethodGet, aliceSiteHost, path)
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
				badKeyRequest.Host = aliceSiteHost
				badKeyRequest.Header.Set("X-API-Key", "not-the-right-key")
				recorder = httptest.NewRecorder()
				handler.ServeHTTP(recorder, badKeyRequest)
				if recorder.Code != http.StatusUnauthorized || len(fake.calls) != 0 {
					t.Fatalf("bad key: status = %d, calls = %d", recorder.Code, len(fake.calls))
				}
			})
		}
	}
}

// writerAllowed gates every write route on top of viewerAllowed: a caller
// who may view but not write gets 403 (existence already established), and
// a caller who may not even view gets 404 either way.
func TestHostGateSiteAPIWriterAllowedTable(t *testing.T) {
	store := newTestStore(t)
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

			request := authenticatedGateRequest(t, test.method, aliceSiteHost, "/api/site/state")
			if test.method != http.MethodGet {
				request.Header.Set("Origin", "https://"+aliceSiteHost)
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
// session-authenticated; it must equal the addressed site host's own origin
// — not the base origin, not the owner's host, not a sibling site's host
// (origin.go's expectedFor).
func TestHostGateSiteAPIOriginCheckedOnNonSafeMethodsOnly(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")

	for _, test := range []struct {
		name       string
		path       string
		method     string
		origin     string
		useKey     bool
		wantStatus int
	}{
		{name: "GET needs no Origin", path: "/api/site/state", method: http.MethodGet, wantStatus: http.StatusOK},
		{name: "PUT with the site's own Origin", path: "/api/site/state", method: http.MethodPut, origin: "https://" + aliceSiteHost, wantStatus: http.StatusOK},
		{name: "PUT via the named shape with the site's own Origin", path: "/api/sites/my-site/state", method: http.MethodPut, origin: "https://" + aliceSiteHost, wantStatus: http.StatusOK},
		{name: "PUT with the base Origin is wrong", path: "/api/site/state", method: http.MethodPut, origin: "https://foo.example", wantStatus: http.StatusForbidden},
		{name: "PUT with the owner host's Origin is wrong", path: "/api/site/state", method: http.MethodPut, origin: "https://alice.foo.example", wantStatus: http.StatusForbidden},
		{name: "PUT with a sibling site's Origin is wrong", path: "/api/site/state", method: http.MethodPut, origin: "https://other.alice.foo.example", wantStatus: http.StatusForbidden},
		{name: "PUT with no Origin at all", path: "/api/site/state", method: http.MethodPut, wantStatus: http.StatusForbidden},
		{name: "an X-API-Key skips Origin entirely", path: "/api/site/state", method: http.MethodPut, useKey: true, wantStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			gate := testHostGate(t, store, testHostModel(t))
			handler := gate.wrap(newHostGateTestMux())

			var request *http.Request
			if test.useKey {
				request = apiKeyGateRequest(test.method, aliceSiteHost, test.path)
			} else {
				request = authenticatedGateRequest(t, test.method, aliceSiteHost, test.path)
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
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	gate := testHostGate(t, store, testHostModel(t))
	fake := &fakeSiteAPI{}
	gate.siteAPI = fake
	handler := gate.wrap(newHostGateTestMux())

	for _, test := range []struct {
		method, path, wantCall string
		wantStatus             int
	}{
		{method: http.MethodGet, path: "/api/site/state", wantCall: "GetState", wantStatus: http.StatusOK},
		{method: http.MethodPut, path: "/api/site/state", wantCall: "PutState", wantStatus: http.StatusOK},
		{method: http.MethodGet, path: "/api/site/state/versioned", wantCall: "GetStateVersioned", wantStatus: http.StatusOK},
		{method: http.MethodPut, path: "/api/site/state/versioned", wantCall: "PutStateVersioned", wantStatus: http.StatusOK},
		{method: http.MethodGet, path: "/api/site/assets", wantCall: "ListAssets", wantStatus: http.StatusOK},
		{method: http.MethodPost, path: "/api/site/assets", wantCall: "CreateAsset", wantStatus: http.StatusOK},
		{method: http.MethodDelete, path: "/api/site/assets/abc-1", wantCall: "DeleteAsset", wantStatus: http.StatusOK},
		{method: http.MethodGet, path: "/api/sites/my-site/state", wantCall: "GetState", wantStatus: http.StatusOK},
		{method: http.MethodDelete, path: "/api/site/state", wantStatus: http.StatusMethodNotAllowed},
		{method: http.MethodPost, path: "/api/site/state", wantStatus: http.StatusMethodNotAllowed},
		{method: http.MethodDelete, path: "/api/site/assets", wantStatus: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/api/site/assets/abc-1", wantStatus: http.StatusMethodNotAllowed},
	} {
		t.Run(test.method+" "+test.path, func(t *testing.T) {
			fake.calls = nil
			request := authenticatedGateRequest(t, test.method, aliceSiteHost, test.path)
			if test.method != http.MethodGet {
				request.Header.Set("Origin", "https://"+aliceSiteHost)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %q)", response.Code, test.wantStatus, response.Body.String())
			}
			if test.wantCall == "" {
				if len(fake.calls) != 0 {
					t.Fatalf("calls = %+v, want none", fake.calls)
				}
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

// Every site host serves exactly one site, so the nameless shape works on
// every one of them, whatever the site's access level; the named shape is
// honoured too, but only when it names that exact site.
func TestHostGateNamelessShapeOnEverySiteHost(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	writeGateSite(t, store, "alice", "other", "other-index")

	for _, restricted := range []bool{false, true} {
		gate := testHostGate(t, store, testHostModel(t))
		gate.siteForServing = func(r *http.Request, owner, site string) (string, bool, error) {
			return owner + "/" + site, restricted, nil
		}
		fake := &fakeSiteAPI{}
		gate.siteAPI = fake
		handler := gate.wrap(newHostGateTestMux())

		for _, test := range []struct{ host, site string }{
			{aliceSiteHost, "my-site"},
			{"other.alice.foo.example", "other"},
		} {
			fake.calls = nil
			response := gateAuthedRequest(t, handler, http.MethodGet, test.host, "/api/site/state")
			if response.Code != http.StatusOK || len(fake.calls) != 1 || fake.calls[0].call.SiteName != test.site {
				t.Fatalf("restricted=%t %s nameless: status = %d, calls %+v", restricted, test.host, response.Code, fake.calls)
			}
			// The named shape naming a different site than this host
			// resolves to is refused outright, not treated as a hint to look
			// elsewhere — even when that other site exists.
			fake.calls = nil
			for _, other := range []string{"some-other-site", "other", "my-site"} {
				if other == test.site {
					continue
				}
				response = gateAuthedRequest(t, handler, http.MethodGet, test.host, "/api/sites/"+other+"/state")
				if response.Code != http.StatusNotFound || len(fake.calls) != 0 {
					t.Fatalf("restricted=%t %s naming %s: status = %d, calls %+v", restricted, test.host, other, response.Code, fake.calls)
				}
			}
			// The named shape naming the right site works too.
			response = gateAuthedRequest(t, handler, http.MethodGet, test.host, "/api/sites/"+test.site+"/state")
			if response.Code != http.StatusOK || len(fake.calls) != 1 || fake.calls[0].call.SiteName != test.site {
				t.Fatalf("restricted=%t %s named: status = %d, calls %+v", restricted, test.host, response.Code, fake.calls)
			}
		}
	}
}

// GET /_assets/{id}[/{name}] on a site's own host reuses hosted content's
// own session and viewerAllowed checks and dispatches to ServeAsset,
// whatever the site's access level.
func TestHostGateAssetServeRoute(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")

	for _, restricted := range []bool{false, true} {
		t.Run(fmt.Sprintf("restricted=%t", restricted), func(t *testing.T) {
			gate := testHostGate(t, store, testHostModel(t))
			gate.siteForServing = func(r *http.Request, owner, site string) (string, bool, error) {
				return "site-1", restricted, nil
			}
			fake := &fakeSiteAPI{}
			gate.siteAPI = fake
			handler := gate.wrap(newHostGateTestMux())

			response := gateAuthedRequest(t, handler, http.MethodGet, aliceSiteHost, "/_assets/abc-1/photo.png")
			if response.Code != http.StatusOK || len(fake.calls) != 1 || fake.calls[0].method != "ServeAsset" || fake.calls[0].id != "abc-1" {
				t.Fatalf("status = %d, calls = %+v", response.Code, fake.calls)
			}
			if got := fake.calls[0].call; got.SiteName != "my-site" || got.Restricted != restricted {
				t.Fatalf("ServeAsset call = %+v", got)
			}
			// Unauthenticated, it goes through the same checks as hosted
			// content: 401 for a non-navigation.
			fake.calls = nil
			response = gateRequest(handler, http.MethodGet, aliceSiteHost, "/_assets/abc-1")
			if response.Code != http.StatusUnauthorized || len(fake.calls) != 0 {
				t.Fatalf("unauthenticated asset fetch: status = %d, calls = %d", response.Code, len(fake.calls))
			}
			// A refused viewer gets 404 and no asset.
			gate.viewerAllowed = func(r *http.Request, siteID, userID string) (bool, error) { return false, nil }
			response = gateAuthedRequest(t, handler, http.MethodGet, aliceSiteHost, "/_assets/abc-1")
			if response.Code != http.StatusNotFound || len(fake.calls) != 0 {
				t.Fatalf("refused viewer asset fetch: status = %d, calls = %d", response.Code, len(fake.calls))
			}
		})
	}
}

type recordingRecorder struct{ events []audit.Event }

func (r *recordingRecorder) Record(_ context.Context, e audit.Event) { r.events = append(r.events, e) }
func (r *recordingRecorder) RecordTx(ctx context.Context, _ *sql.Tx, e audit.Event) error {
	r.Record(ctx, e)
	return nil
}

func TestHostGateRecordsAccessDenied(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	gate := testHostGate(t, store, testHostModel(t))
	recorder := &recordingRecorder{}
	gate.audit = recorder
	gate.viewerAllowed = func(r *http.Request, siteID, userID string) (bool, error) { return false, nil }
	handler := gate.wrap(newHostGateTestMux())

	gateAuthedRequest(t, handler, http.MethodGet, aliceSiteHost, "/")
	gateAuthedRequest(t, handler, http.MethodGet, aliceSiteHost, "/api/site/state")
	if len(recorder.events) != 2 {
		t.Fatalf("recorded %d events, want 2", len(recorder.events))
	}
	for _, e := range recorder.events {
		if e.Action != "access_denied" || e.ActorID != hostGateTestUserID || e.Extra["reason"] != "not_a_viewer" || !strings.Contains(e.Detail, aliceSiteHost) {
			t.Fatalf("event = %+v", e)
		}
	}

	// A signed-in viewer refused a write records not_a_writer.
	recorder.events = nil
	gate.viewerAllowed = func(r *http.Request, siteID, userID string) (bool, error) { return true, nil }
	gate.writerAllowed = func(r *http.Request, siteID, userID string) (bool, error) { return false, nil }
	request := authenticatedGateRequest(t, http.MethodPut, aliceSiteHost, "/api/site/state")
	request.Header.Set("Origin", "https://"+aliceSiteHost)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if len(recorder.events) != 1 || recorder.events[0].Extra["reason"] != "not_a_writer" {
		t.Fatalf("events = %+v, want one not_a_writer", recorder.events)
	}
}
