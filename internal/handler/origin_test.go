package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOriginCheckMiddleware(t *testing.T) {
	const publicBaseURL = "https://example.com"
	tests := []struct {
		name             string
		origin           string
		apiKey           bool
		emptyAPIKey      bool
		whitespaceAPIKey bool
		wantStatus       int
	}{
		{name: "allowed base origin", origin: "https://example.com", wantStatus: http.StatusNoContent},
		{name: "allowed default port and trailing dot", origin: "https://EXAMPLE.COM.:443", wantStatus: http.StatusNoContent},
		{name: "wrong origin", origin: "https://other.example.com", wantStatus: http.StatusForbidden},
		{name: "subdomain of base origin", origin: "https://attacker.example.com", wantStatus: http.StatusForbidden},
		{name: "different port", origin: "https://example.com:8443", wantStatus: http.StatusForbidden},
		{name: "absent Origin without API key", wantStatus: http.StatusForbidden},
		{name: "absent Origin with API key", apiKey: true, wantStatus: http.StatusNoContent},
		{name: "null origin", origin: "null", wantStatus: http.StatusForbidden},
		// A check that can be satisfied by sending an empty header is not a
		// check. Not browser-reachable — a custom header forces a preflight no
		// CORS policy answers — but the rule should not depend on that.
		{name: "empty API key does not bypass", emptyAPIKey: true, wantStatus: http.StatusForbidden},
		{name: "whitespace API key does not bypass", whitespaceAPIKey: true, wantStatus: http.StatusForbidden},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})
			request := httptest.NewRequest(http.MethodPost, "/api/admin/test", nil)
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.apiKey {
				request.Header.Set("X-API-Key", "client-key")
			}
			if test.emptyAPIKey {
				request.Header.Set("X-API-Key", "")
			}
			if test.whitespaceAPIKey {
				request.Header.Set("X-API-Key", "   ")
			}

			response := httptest.NewRecorder()
			originCheckMiddleware(newTestHostModel(t, publicBaseURL), publicBaseURL)(next).ServeHTTP(response, request)

			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if called != (test.wantStatus == http.StatusNoContent) {
				t.Fatalf("next called = %t, want %t", called, test.wantStatus == http.StatusNoContent)
			}
			if test.wantStatus == http.StatusForbidden {
				var got errorResponse
				if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
					t.Fatalf("decode rejection: %v", err)
				}
				if got.Error != "forbidden" {
					t.Fatalf("error = %q, want forbidden", got.Error)
				}
			}
		})
	}
}

// A base URL carrying the bare "/" path that config permits must still enable
// the check. Otherwise a trailing slash silently turns CSRF protection off.
func TestOriginCheckAcceptsBaseURLWithTrailingSlash(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	middleware := originCheckMiddleware(newTestHostModel(t, "https://example.com/"), "https://example.com/")

	for _, test := range []struct {
		origin string
		want   int
	}{
		{origin: "https://example.com", want: http.StatusNoContent},
		{origin: "https://attacker.example.com", want: http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/admin/test", nil)
		request.Header.Set("Origin", test.origin)
		response := httptest.NewRecorder()
		middleware(next).ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("origin %q status = %d, want %d", test.origin, response.Code, test.want)
		}
	}
}

func TestOriginCheckDoesNotAffectUntouchedRoute(t *testing.T) {
	mux := http.NewServeMux()
	called := false
	mux.HandleFunc("POST /api/sites/{username}/{sitename}/state", func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodPost, "/api/sites/alice/demo/state", nil)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || !called {
		t.Fatalf("untouched route status=%d called=%t, want 204 and called", response.Code, called)
	}
}

// The Origin demanded is the origin of the host the request was addressed to.
// A page on an owner host may post to its own host; it may not post to the
// base host, another owner host, or anything else, and no page anywhere else
// may post to an owner host.
func TestOriginCheckIsPerHost(t *testing.T) {
	const publicBaseURL = "https://foo.example"
	const base = "foo.example"
	const alice = "alice.foo.example"
	const unknown = "10.0.0.5:8080"

	tests := []struct {
		name   string
		host   string
		origin string
		apiKey bool
		want   int
	}{
		{name: "base host with base origin", host: base, origin: "https://foo.example", want: http.StatusNoContent},
		{name: "base host with an owner origin", host: base, origin: "https://alice.foo.example", want: http.StatusForbidden},
		{name: "owner host with its own origin", host: alice, origin: "https://alice.foo.example", want: http.StatusNoContent},
		{name: "owner host with the base origin", host: alice, origin: "https://foo.example", want: http.StatusForbidden},
		{name: "owner host with another owner's origin", host: alice, origin: "https://bob.foo.example", want: http.StatusForbidden},
		{name: "owner host normalises case and port", host: "ALICE.foo.example.:443", origin: "https://Alice.Foo.Example.:443", want: http.StatusNoContent},
		{name: "owner host with a foreign origin", host: alice, origin: "https://evil.example", want: http.StatusForbidden},
		{name: "base host without an origin", host: base, want: http.StatusForbidden},
		{name: "owner host without an origin", host: alice, want: http.StatusForbidden},
		{name: "base host with a null origin", host: base, origin: "null", want: http.StatusForbidden},
		{name: "owner host with a null origin", host: alice, origin: "null", want: http.StatusForbidden},
		{name: "base host with an API key", host: base, apiKey: true, want: http.StatusNoContent},
		{name: "owner host with an API key", host: alice, apiKey: true, want: http.StatusNoContent},
		// An unrecognised host expects the base origin, exactly as before.
		// The gate answers only the probes there, so this is belt and braces.
		{name: "unknown host with the base origin", host: unknown, origin: "https://foo.example", want: http.StatusNoContent},
		{name: "unknown host with an owner origin", host: unknown, origin: "https://alice.foo.example", want: http.StatusForbidden},
		// A label the host model refuses is not an owner host, so it gets the
		// base expectation and its own spelling is not accepted.
		{name: "nested label with its own origin", host: "a.alice.foo.example", origin: "https://a.alice.foo.example", want: http.StatusForbidden},
	}

	{
		middleware := originCheckMiddleware(newTestHostModel(t, publicBaseURL), publicBaseURL)
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
				request := httptest.NewRequest(http.MethodPost, "/api/sites/demo/visibility", nil)
				request.Host = test.host
				if test.origin != "" {
					request.Header.Set("Origin", test.origin)
				}
				if test.apiKey {
					request.Header.Set("X-API-Key", "client-key")
				}
				response := httptest.NewRecorder()
				middleware(next).ServeHTTP(response, request)
				if response.Code != test.want {
					t.Fatalf("host %q origin %q: status = %d, want %d", test.host, test.origin, response.Code, test.want)
				}
			})
		}
	}
}

// The owner origin is rebuilt from the configured base URL, so a base URL
// carrying a port produces an owner origin carrying the same port, and an
// origin without it is refused.
func TestOriginCheckOwnerHostKeepsConfiguredPort(t *testing.T) {
	const publicBaseURL = "http://localhost:8080"
	middleware := originCheckMiddleware(newTestHostModel(t, publicBaseURL), publicBaseURL)
	for _, test := range []struct {
		origin string
		want   int
	}{
		{origin: "http://alice.localhost:8080", want: http.StatusNoContent},
		{origin: "http://alice.localhost", want: http.StatusForbidden},
		{origin: "https://alice.localhost:8080", want: http.StatusForbidden},
	} {
		next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
		request := httptest.NewRequest(http.MethodPost, "/api/sites/demo/visibility", nil)
		request.Host = "alice.localhost:8080"
		request.Header.Set("Origin", test.origin)
		response := httptest.NewRecorder()
		middleware(next).ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("origin %q: status = %d, want %d", test.origin, response.Code, test.want)
		}
	}
}

// A forged Host header cannot nominate its own expected origin: the check
// only ever rebuilds "<label>.<base>" from the configured base URL, so a Host
// the model does not recognise falls back to the base expectation.
func TestOriginCheckNeverEchoesTheHostHeader(t *testing.T) {
	const publicBaseURL = "https://foo.example"
	middleware := originCheckMiddleware(newTestHostModel(t, publicBaseURL), publicBaseURL)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	for _, host := range []string{"evil.example", "foo.example.evil.example", "alice.foo.example.evil.example"} {
		request := httptest.NewRequest(http.MethodPost, "/api/sites/demo/visibility", nil)
		request.Host = host
		request.Header.Set("Origin", "https://"+host)
		response := httptest.NewRecorder()
		middleware(next).ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("host %q: status = %d, want 403", host, response.Code)
		}
	}
}

// Three rules the check relies on that nothing else asserts.
func TestOriginCheckRefusesMalformedAndDuplicateOrigins(t *testing.T) {
	const publicBaseURL = "https://foo.example"
	middleware := originCheckMiddleware(newTestHostModel(t, publicBaseURL), publicBaseURL)

	// A request carrying two Origin headers is malformed, and there is no
	// sensible way to pick which one to trust. Both spellings are refused
	// even when one of them is the right origin.
	t.Run("duplicate Origin headers", func(t *testing.T) {
		for _, pair := range [][2]string{
			{"https://foo.example", "https://evil.example"},
			{"https://evil.example", "https://foo.example"},
			{"https://foo.example", "https://foo.example"},
		} {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			request := httptest.NewRequest(http.MethodPost, "/api/sites/demo/visibility", nil)
			request.Host = "foo.example"
			request.Header.Add("Origin", pair[0])
			request.Header.Add("Origin", pair[1])
			response := httptest.NewRecorder()
			middleware(next).ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Errorf("origins %v: status = %d, want 403", pair, response.Code)
			}
		}
	})

	// parseRequestOrigin refuses anything that is not a bare origin, not just
	// the "null" literal: a path, a query, a fragment, credentials, an opaque
	// URL, a missing scheme or a missing host all fail.
	t.Run("malformed origins", func(t *testing.T) {
		for _, raw := range []string{
			"",
			"null",
			"https://foo.example/",
			"https://foo.example/path",
			"https://foo.example?a=b",
			"https://foo.example#f",
			"https://user:pass@foo.example",
			"foo.example",
			"https://",
			"mailto:someone@foo.example",
			"   ",
		} {
			if got, ok := parseRequestOrigin(raw); ok {
				t.Errorf("parseRequestOrigin(%q) = %+v, true; want false", raw, got)
			}
		}
		// And the shapes that must still parse.
		for _, raw := range []string{"https://foo.example", "http://localhost:8080", "https://FOO.EXAMPLE.:443"} {
			if _, ok := parseRequestOrigin(raw); !ok {
				t.Errorf("parseRequestOrigin(%q) = false, want true", raw)
			}
		}
	})
}
