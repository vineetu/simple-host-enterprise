package handler

import (
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/vsriram/simple-host/internal/auth"
)

type requestOrigin struct {
	scheme string
	host   string
	port   string
}

// originRejector renders the refusal. Routes reached by a browser form pass
// one that returns a page, because the default JSON body would be displayed
// raw — and on a sign-in form "forbidden" reads as "your key is wrong", which
// sends someone to ask for a replacement key they do not need.
type originRejector func(http.ResponseWriter, *http.Request)

// originCheckMiddleware protects browser-authenticated mutations while
// allowing clients that identify themselves with an API key to omit Origin.
//
// The Origin demanded is the origin of the host the request was addressed to,
// not the base origin for every host: a request to the base host must carry
// the base origin, and a request to an owner host "<label>.<base>" must carry
// that owner host's own origin. The expected origin is rebuilt from the
// configured public base URL's scheme and port via HostModel, so the only
// thing the request supplies is which of the server's own hostnames it was
// sent to; the Origin header is never echoed back as its own expectation.
//
// This is not a weakening. A cross-origin post is still refused everywhere:
// a page on the base host still cannot post to an owner host, a page on one
// owner host still cannot post to another, and a page anywhere else still
// cannot post to any of them. What it additionally permits is a management
// page posting to the host it is itself served from.
func originCheckMiddleware(hosts HostModel, publicBaseURL string, reject ...originRejector) func(http.Handler) http.Handler {
	refuse := func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "forbidden"})
	}
	if len(reject) > 0 && reject[0] != nil {
		refuse = reject[0]
	}
	// Config permits a bare "/" path on the base URL; an Origin header never
	// carries one. Trim it here so a valid configuration always parses,
	// rather than making a trailing slash silently disable the check.
	expected, expectedOK := parseRequestOrigin(strings.TrimSuffix(publicBaseURL, "/"))
	if !expectedOK {
		// Every protected route would 403 forever, including the admin login
		// page, and the only symptom would be a forbidden response with no
		// hint as to why. Say so once, loudly, at startup. This branch fails
		// open; what makes it unreachable in production is
		// config.validatePublicBaseURL, which refuses anything but a bare
		// origin before the server starts.
		log.Printf("origin check DISABLED FOR SAFETY: public base URL %q is not a bare origin", publicBaseURL)
	}
	// expectedFor names the origin this request had to come from. An owner
	// host, or a restricted site's own host, expects its own origin — both
	// classify to a label OwnerOrigin builds the right authority from
	// unchanged (a restricted-site label already carries "owner--site"); the
	// base host and anything else expect the configured base origin, exactly
	// as before. An owner origin that will not parse refuses rather than
	// falling open: the base URL is validated at startup and the label
	// passed Classify, so it cannot happen in practice, and if it somehow
	// does the safe answer is to demand nothing less.
	expectedFor := func(r *http.Request) (requestOrigin, bool) {
		if kind, label := hosts.Classify(r.Host); kind == hostOwner || kind == hostRestrictedSite {
			return parseRequestOrigin(hosts.OwnerOrigin(label))
		}
		return expected, true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !expectedOK {
				next.ServeHTTP(w, r)
				return
			}
			// A present but empty header is not a client identifying itself.
			// Not reachable from a browser — a custom header forces a preflight
			// that no CORS policy answers — but a check that can be satisfied
			// by sending nothing is not a check.
			if strings.TrimSpace(r.Header.Get("X-API-Key")) != "" {
				next.ServeHTTP(w, r)
				return
			}

			// Exactly one Origin, or none: a request carrying two is malformed
			// and there is no sensible way to pick which one to trust.
			origins := r.Header.Values("Origin")
			allowed := len(origins) == 1
			if allowed {
				want, wantOK := expectedFor(r)
				seen, seenOK := parseRequestOrigin(origins[0])
				allowed = wantOK && seenOK && seen == want
			}
			if allowed {
				next.ServeHTTP(w, r)
				return
			}

			log.Printf("origin check rejected route=%s origin=%q", r.URL.Path, r.Header.Get("Origin"))
			refuse(w, r)
		})
	}
}

// cookieOriginCheck is originCheckMiddleware for API routes that a browser
// session cookie can authenticate. It demands an Origin only when a session
// cookie is actually present: with no cookie there is nothing ambient for a
// cross-site page to forge, and refusing such a request here would turn the
// 401 a credential-less client expects (and the "ask for the API key" hint
// built on it) into a misleading 403. The plain form routes keep the
// unconditional check, because sign-in and login set a cookie rather than
// consume one and must still be protected from a cross-site submission.
func cookieOriginCheck(hosts HostModel, publicBaseURL string) func(http.Handler) http.Handler {
	check := originCheckMiddleware(hosts, publicBaseURL)
	return func(next http.Handler) http.Handler {
		checked := check(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := r.Cookie(auth.SessionCookieName); err != nil {
				next.ServeHTTP(w, r)
				return
			}
			checked.ServeHTTP(w, r)
		})
	}
}

func parseRequestOrigin(raw string) (requestOrigin, bool) {
	if raw == "" || raw == "null" {
		return requestOrigin{}, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return requestOrigin{}, false
	}

	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return requestOrigin{}, false
	}

	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == defaultOriginPort(scheme) {
		port = ""
	}
	return requestOrigin{scheme: scheme, host: host, port: port}, true
}

func defaultOriginPort(scheme string) string {
	switch scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}
