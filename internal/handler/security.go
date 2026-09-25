package handler

import (
	"net/http"

	"github.com/vsriram/simple-host/internal/auth"
)

const (
	hstsPolicy                = "max-age=31536000"
	hstsPolicyBase            = hstsPolicy + "; includeSubDomains"
	contentSecurityPolicy     = "frame-ancestors 'none'"
	searchSessionCookieName   = "__Host-simple_host_search_session"
	searchSessionCookieMaxAge = 180 * 24 * 60 * 60
)

// CookiePolicy is trusted deployment configuration. It must not be inferred
// from request headers because the application listener is behind a layer-4
// load balancer and those headers are client controlled.
type CookiePolicy struct {
	Secure bool
}

// SecurityHeaders applies headers owned by the application, split by host
// kind rather than by path (design.md 7.1, 7.4): with the owner-host
// management preview gone, an owner or restricted-site host serves nothing
// but hosted content, the site-facing API, and the session hand-off, none of
// which is this application's own control-plane UI, so frame-ancestors never
// applies there — people embed hosted pages in wikis and docs, and denying
// that is a product change nobody asked for. Every other host — the base
// host and anything this server does not recognise — keeps the policy, the
// safe default: an unrecognised host answers only the probes anyway (the
// host gate, not this middleware, is what enforces that), so there is
// nothing to lose by defending it too.
//
// HSTS is emitted on every host in explicit secure mode; the base host adds
// includeSubDomains, since design.md 7.4 treats it as dedicated to this
// application (an owner or restricted-site host is a subdomain of it, so
// that instruction already covers them without repeating it there).
//
// The owner-host-specific headers CORP and the Sec-Fetch-Site refusal
// (design.md 7.4) are applied by the host gate itself, not here: they are
// part of deciding whether the gate answers a request at all, not a
// blanket header pass.
func SecurityHeaders(next http.Handler, secureMode bool, hosts HostModel) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// design.md 7.4: "Control plane: as today (nosniff, frame-ancestors
		// 'none', HSTS), plus Referrer-Policy: strict-origin-when-cross-origin."
		// Set unconditionally, for every host this middleware sees: an owner
		// or restricted-site host response gets the identical value again
		// from applyOwnerHostSecurity downstream (host_gate.go), and the base
		// host — which nothing else in this chain ever touched — finally
		// gets it at all. Phase 2 review finding: it was never set on the
		// base host.
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		kind, _ := hosts.Classify(r.Host)
		hostedContent := kind == hostOwner || kind == hostRestrictedSite
		if !hostedContent {
			w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		}
		// design.md 7.4: "Cache-Control: no-store on every authenticated
		// page and API response" on the control plane. This middleware runs
		// before any handler authenticates the request, so "authenticated"
		// is read here as "the request itself carries a credential" (a
		// session cookie or X-API-Key) rather than "the credential turned
		// out valid" — a request whose credential is rejected still gets
		// no-store, which is the safer direction to round on. Phase 2
		// review finding: this used to be set ad hoc per handler (e.g.
		// showcase.go, only when a search query was present); doing it once
		// here covers every base-host route uniformly, including ones that
		// never set it themselves.
		if kind == hostBase && requestCarriesCredential(r) {
			w.Header().Set("Cache-Control", "no-store")
		}
		if secureMode {
			if kind == hostBase {
				w.Header().Set("Strict-Transport-Security", hstsPolicyBase)
			} else {
				w.Header().Set("Strict-Transport-Security", hstsPolicy)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requestCarriesCredential reports whether r presents either credential
// shape auth.Middleware accepts (a session cookie or an X-API-Key header),
// without validating it — SecurityHeaders runs before authentication, so
// this is a proxy for "this request is trying to be authenticated," not a
// verdict on whether it succeeds.
func requestCarriesCredential(r *http.Request) bool {
	if r.Header.Get("X-API-Key") != "" {
		return true
	}
	c, err := r.Cookie(auth.SessionCookieName)
	return err == nil && c.Value != ""
}

func (p CookiePolicy) searchSession(value string) *http.Cookie {
	return &http.Cookie{
		Name:     searchSessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true, // required by the __Host- prefix
		SameSite: http.SameSiteLaxMode,
		MaxAge:   searchSessionCookieMaxAge,
	}
}
