package handler

import (
	"context"
	"html"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// A browser opening a site host that has no site, or a site the signed-in
// person may not open, gets one page for both: it confirms nothing about
// whether the site exists, and says who they are signed in as and what to
// do. Scripts and agents keep the plain 404.

// switchAccountPath, on a site host, clears that host's session cookie and
// sends the person to the dashboard to sign out and in again as someone
// else. Reserved like /auth/session.
const switchAccountPath = "/auth/switch"

// noAccessCSP is the no-access page's policy: sandboxed (an opaque origin),
// inline styles only, never framed. noAccessPolicy adds img-src for the app
// origin alone, so the product icon can load and no other host can.
const noAccessCSP = "sandbox; default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'"

func noAccessPolicy(origin string) string {
	origin = iconOrigin(origin)
	if origin == "" {
		return noAccessCSP
	}
	return noAccessCSP + "; img-src " + origin
}

func newUserLabel(database db.Querier) func(ctx context.Context, userID string) string {
	return func(ctx context.Context, userID string) string {
		username, email, err := db.UserLabel(ctx, database, userID)
		if err != nil {
			log.Printf("host gate: label for %s: %v", userID, err)
			return ""
		}
		if email != "" && !strings.EqualFold(email, username) {
			return username + " (" + email + ")"
		}
		return username
	}
}

// noAccess writes the no-access page for a navigation and a plain 404 for
// anything else.
func (g *hostGate) noAccess(w http.ResponseWriter, r *http.Request, userID string) {
	if !wantsNavigation(r) {
		http.NotFound(w, r)
		return
	}
	name := ""
	if g.userLabel != nil && userID != "" {
		name = g.userLabel(r.Context(), userID)
	}
	// The page is byte-identical for a missing site, an unshared one and a
	// moved one, whatever host was asked for.
	body := renderNoAccess(name, g.hosts.origin)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// The page names the signed-in person. Sandboxed, it has an opaque
	// origin: on the owner-path fallback, where every site of the owner
	// shares one origin, another site's script that opened it cannot read
	// it. It runs no script and cannot be framed. img-src names only the
	// app origin, for the product icon.
	w.Header().Set("Content-Security-Policy", noAccessPolicy(g.hosts.origin))
	w.WriteHeader(http.StatusNotFound)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

// missingSite answers a site host that names no site. A navigation is
// treated exactly as one to a site the caller may not open: sign-in first
// (the hand-off), then the same page, so the two cannot be told apart.
func (g *hostGate) missingSite(w http.ResponseWriter, r *http.Request, requestHost string) {
	if !wantsNavigation(r) || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}
	userID, _, ok := g.requireHostSession(w, r, requestHost)
	if !ok {
		return
	}
	g.noAccess(w, r, userID)
}

// switchAccount clears this host's session cookie and sends the browser to
// the dashboard, which offers to sign out and come back here.
func (g *hostGate) switchAccount(w http.ResponseWriter, r *http.Request, requestHost string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookieName, Value: "", Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	back := url.URL{Scheme: g.hosts.scheme, Host: requestHost, Path: "/"}
	target := url.URL{
		Scheme:   g.hosts.scheme,
		Host:     g.hosts.BaseHost(),
		Path:     "/dashboard",
		RawQuery: url.Values{"switch": {back.String()}}.Encode(),
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

// switchTarget accepts only an https address on one of this server's owner
// or site hosts, returned without path or query (the site's root).
func switchTarget(hosts HostModel, raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.User != nil {
		return "", false
	}
	if kind, _ := hosts.Classify(parsed.Host); kind != hostOwner && kind != hostSite {
		return "", false
	}
	return "https://" + normalizeHost(parsed.Host) + "/", true
}

// signInCardCSS is the shared sign-in card, light only. Enterprise has no
// dark theme. Both pages keep it in a style element: the no-access policy
// allows style-src 'unsafe-inline' and default-src 'none' forbids a script
// and any other stylesheet.
const signInCardCSS = `*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:32px 16px;background:#f5f6f8;color:#16181d;font:16px/1.5 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
main{width:100%;max-width:400px;background:#fff;border:1px solid #d6d9e0;border-radius:14px;box-shadow:0 1px 2px rgba(22,24,29,.04),0 12px 32px rgba(22,24,29,.08);padding:28px 26px 22px}
.eyebrow{margin:0 0 6px;font-size:12px;font-weight:600;letter-spacing:.12em;line-height:1.4;text-transform:uppercase;color:#5d6370;overflow-wrap:anywhere}
h1{margin:0 0 8px;font-size:22px;font-weight:650;line-height:1.25;letter-spacing:-.015em;color:#16181d}
.lead{margin:0 0 18px;color:#4a505c;font-size:15.5px;line-height:1.5;overflow-wrap:anywhere}
.who{margin:0 0 16px;padding:10px 12px;border:1px solid #d6d9e0;border-radius:10px;font-size:14px;line-height:1.4;color:#16181d;overflow-wrap:anywhere}
.who strong{font-weight:650}
a.primary,a.secondary{display:flex;align-items:center;justify-content:center;width:100%;height:48px;border-radius:10px;font-weight:600;font-size:15.5px;line-height:1.2;text-align:center;text-decoration:none}
a.primary{background:#1b4ee0;color:#fff}
a.primary:hover{background:#163db3}
a.secondary{margin-top:10px;background:#fff;color:#16181d;border:1px solid #d6d9e0}
a.primary:focus-visible,a.secondary:focus-visible{outline:3px solid #b9caf7;outline-offset:2px}
.fine{margin:16px 0 0;font-size:13px;line-height:1.5;color:#5d6370}
`

// renderNoAccess is the one page for a missing site and a site the person
// may not open. It never says whether the site exists.
func renderNoAccess(name, origin string) string {
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
`)
	// This page is served on a site host or the owner-path fallback. Those
	// hosts do not serve the product icon, so the links name the app host.
	b.WriteString(absoluteProductIconLinks(origin))
	b.WriteString(`<meta name="robots" content="noindex,nofollow">
<title>No access</title>
<style>
`)
	b.WriteString(signInCardCSS)
	b.WriteString(`</style>
</head>
<body>
<main>
`)
	b.WriteString(`<h1>This site doesn't exist or isn't shared with you</h1>
`)
	if name != "" {
		b.WriteString(`<p class="who">You're signed in as <strong>`)
		b.WriteString(html.EscapeString(name))
		b.WriteString(`</strong>.</p>
`)
	}
	b.WriteString(`<p class="lead">Ask the person who sent you the link to share it with you.</p>
<a class="primary" href="` + switchAccountPath + `">Switch account</a>
</main>
</body>
</html>
`)
	return b.String()
}
