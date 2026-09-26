package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// readinessGate is a test gate whose host model reports only the owner
// labels in ready as having their own certificate.
func readinessGate(t *testing.T, store *testStore, ready ...string) (*hostGate, http.Handler) {
	t.Helper()
	set := map[string]bool{}
	for _, l := range ready {
		set[l] = true
	}
	hosts := testHostModel(t).WithOwnerReadiness(func(label string) bool { return set[label] })
	gate := testHostGate(t, store, hosts)
	return gate, gate.wrap(newHostGateTestMux())
}

// Until an owner's certificate is ready, the owner host serves that owner's
// sites itself at "/<site>/" (the v1.2 shape), for every access level:
// content, assets, the named site API, the trailing-slash redirect and the
// hand-off, all behind the same session and viewer checks as a site host.
func TestHostGateOwnerHostServesSitesUntilReady(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	for _, restricted := range []bool{false, true} {
		gate, handler := readinessGate(t, store)
		gate.siteForServing = func(r *http.Request, owner, site string) (string, bool, error) {
			return owner + "/" + site, restricted, nil
		}
		fake := &fakeSiteAPI{}
		gate.siteAPI = fake

		response := gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/my-site/")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "alice-index") {
			t.Fatalf("restricted=%t: owner-path content: status = %d body %q", restricted, response.Code, response.Body.String())
		}
		if got := response.Header().Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
			t.Errorf("restricted=%t: CORP = %q", restricted, got)
		}
		response = gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/my-site/assets/app.js")
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "console.log") {
			t.Fatalf("restricted=%t: owner-path file: status = %d body %q", restricted, response.Code, response.Body.String())
		}
		response = gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/my-site?x=1")
		if response.Code != http.StatusMovedPermanently || response.Header().Get("Location") != "/my-site/?x=1" {
			t.Fatalf("restricted=%t: bare segment: status = %d Location %q", restricted, response.Code, response.Header().Get("Location"))
		}
		response = gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/my-site/_assets/abc-1/photo.png")
		if response.Code != http.StatusOK || len(fake.calls) != 1 || fake.calls[0].method != "ServeAsset" || fake.calls[0].id != "abc-1" {
			t.Fatalf("restricted=%t: owner-path asset: status = %d calls %+v", restricted, response.Code, fake.calls)
		}
		fake.calls = nil
		response = gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/api/sites/my-site/state")
		if response.Code != http.StatusOK || len(fake.calls) != 1 || fake.calls[0].method != "GetState" ||
			fake.calls[0].call.SiteName != "my-site" || fake.calls[0].call.ViaSiteLabel != "alice" {
			t.Fatalf("restricted=%t: owner-path state: status = %d calls %+v", restricted, response.Code, fake.calls)
		}
		fake.calls = nil
		// The nameless shape names no site on a host that serves several.
		response = gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/api/site/state")
		if response.Code != http.StatusNotFound || len(fake.calls) != 0 {
			t.Fatalf("restricted=%t: nameless shape on owner host: status = %d calls %+v", restricted, response.Code, fake.calls)
		}

		// No session: a navigation takes the hand-off back to this owner
		// host, anything else is 401.
		navigation := httptest.NewRequest(http.MethodGet, "/my-site/", nil)
		navigation.Host = "alice.foo.example"
		navigation.Header.Set("Sec-Fetch-Dest", "document")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, navigation)
		if loc := recorder.Header().Get("Location"); recorder.Code != http.StatusFound ||
			!strings.HasPrefix(loc, "https://foo.example/auth/handoff?") || !strings.Contains(loc, "alice.foo.example%2Fmy-site%2F") {
			t.Fatalf("restricted=%t: unauthenticated navigation: status = %d Location %q", restricted, recorder.Code, loc)
		}
		if response := gateRequest(handler, http.MethodGet, "alice.foo.example", "/my-site/"); response.Code != http.StatusUnauthorized {
			t.Fatalf("restricted=%t: unauthenticated fetch: status = %d, want 401", restricted, response.Code)
		}
		// A cookie for one of alice's site hosts is not a cookie for her
		// owner host.
		request := authenticatedGateRequest(t, http.MethodGet, "my-site.alice.foo.example", "/my-site/")
		request.Host = "alice.foo.example"
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("restricted=%t: site-host cookie on owner host: status = %d, want 401", restricted, recorder.Code)
		}

		// Refused viewer, unknown site, reserved segments: 404.
		gate.viewerAllowed = func(r *http.Request, siteID, userID string) (bool, error) { return false, nil }
		for _, path := range []string{"/my-site/", "/api/sites/my-site/state", "/nosuch/", "/api/collaboration/sites", "/sites/x"} {
			if response := gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", path); response.Code != http.StatusNotFound {
				t.Errorf("restricted=%t: %s: status = %d, want 404", restricted, path, response.Code)
			}
		}
	}
}

// Readiness is per owner: a ready owner's site paths redirect to the site's
// own host, while a not-ready owner's are served where they are.
func TestHostGateOwnerHostRedirectsOnlyWhenReady(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	writeGateSite(t, store, "bob", "x", "bob-index")
	_, handler := readinessGate(t, store, "alice")

	response := gateAuthedRequest(t, handler, http.MethodGet, "alice.foo.example", "/my-site/p?q=1")
	if response.Code != http.StatusMovedPermanently || response.Header().Get("Location") != "https://my-site.alice.foo.example/p?q=1" {
		t.Fatalf("ready owner: status = %d Location %q", response.Code, response.Header().Get("Location"))
	}
	response = gateAuthedRequest(t, handler, http.MethodPut, "alice.foo.example", "/api/sites/my-site/state")
	if response.Code != http.StatusPermanentRedirect || response.Header().Get("Location") != "https://my-site.alice.foo.example/api/sites/my-site/state" {
		t.Fatalf("ready owner write: status = %d Location %q", response.Code, response.Header().Get("Location"))
	}
	response = gateAuthedRequest(t, handler, http.MethodGet, "bob.foo.example", "/x/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "bob-index") {
		t.Fatalf("not-ready owner: status = %d body %q", response.Code, response.Body.String())
	}
	// A not-ready owner's site host still serves: readiness decides which
	// address is handed out and redirected to, not whether a host answers.
	response = gateAuthedRequest(t, handler, http.MethodGet, "x.bob.foo.example", "/")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "bob-index") {
		t.Fatalf("not-ready owner's site host: status = %d body %q", response.Code, response.Body.String())
	}
}

// The v1.2 "<owner>--<site>" address redirects to the site's current
// address, which is the owner-host path until the owner is ready and the
// site's own host after. It never serves, and an unknown owner or site is
// 404.
func TestHostGateLegacySiteHostRedirectsByReadiness(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	writeGateSite(t, store, "alice", "two words", "spaced-index")
	for _, test := range []struct {
		name, method, host, target string
		ready                      bool
		want                       int
		location                   string
	}{
		{"not ready: owner path", http.MethodGet, "alice--my-site.foo.example", "/a/b?q=1", false, http.StatusMovedPermanently, "https://alice.foo.example/my-site/a/b?q=1"},
		{"ready: site host", http.MethodGet, "alice--my-site.foo.example", "/a/b?q=1", true, http.StatusMovedPermanently, "https://my-site.alice.foo.example/a/b?q=1"},
		{"not ready: write keeps its method", http.MethodPost, "alice--my-site.foo.example", "/form", false, http.StatusPermanentRedirect, "https://alice.foo.example/my-site/form"},
		{"ready: write keeps its method", http.MethodPut, "alice--my-site.foo.example", "/api/site/state", true, http.StatusPermanentRedirect, "https://my-site.alice.foo.example/api/site/state"},
		{"ready: hashed part", http.MethodGet, "alice--" + siteHostPart("two words") + ".foo.example", "/", true, http.StatusMovedPermanently, "https://" + siteHostPart("two words") + ".alice.foo.example/"},
		{"not ready: hashed part", http.MethodGet, "alice--" + siteHostPart("two words") + ".foo.example", "/", false, http.StatusMovedPermanently, "https://alice.foo.example/two%20words/"},
		{"unknown site", http.MethodGet, "alice--nosuch.foo.example", "/", true, http.StatusNotFound, ""},
		{"unknown owner", http.MethodGet, "nobody--my-site.foo.example", "/", true, http.StatusNotFound, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var ready []string
			if test.ready {
				ready = []string{"alice"}
			}
			_, handler := readinessGate(t, store, ready...)
			for _, authed := range []bool{false, true} {
				request := httptest.NewRequest(test.method, test.target, nil)
				if authed {
					request = authenticatedGateRequest(t, test.method, test.host, test.target)
				}
				request.Host = test.host
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != test.want || response.Header().Get("Location") != test.location {
					t.Fatalf("authed=%t: status = %d Location %q, want %d %q", authed, response.Code, response.Header().Get("Location"), test.want, test.location)
				}
				if strings.Contains(response.Body.String(), "-index") {
					t.Fatalf("authed=%t: legacy host served content: %q", authed, response.Body.String())
				}
			}
		})
	}
}

// Hosts the model does not recognise answer only the probes: three labels
// under the base, and punycode labels in any position.
func TestHostGateUnknownHostShapes(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "my-site", "alice-index")
	handler := newHostGateTestHandler(t, store)
	for _, host := range []string{
		"a.b.c.foo.example",
		"x.my-site.alice.foo.example",
		"xn--alice.foo.example",
		"xn--my-site.alice.foo.example",
		"my-site.xn--alice.foo.example",
		"xn--alice--my-site.foo.example",
	} {
		response := gateAuthedRequest(t, handler, http.MethodGet, host, "/")
		if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "alice-index") {
			t.Errorf("%s: status = %d body %q, want 404", host, response.Code, response.Body.String())
		}
		if response := gateRequest(handler, http.MethodGet, host, "/healthz"); response.Code != http.StatusOK {
			t.Errorf("%s /healthz: status = %d, want 200", host, response.Code)
		}
	}
}
