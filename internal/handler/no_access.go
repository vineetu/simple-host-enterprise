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
	body := renderNoAccess(name)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
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

func renderNoAccess(name string) string {
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex,nofollow">
<title>No access</title>
<style>
  body{margin:0;background:#fbfbfc;color:#16181d;font:16px/1.55 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
  main{max-width:560px;margin:0 auto;padding:72px 24px}
  h1{font-size:24px;margin:0 0 12px}
  p{margin:0 0 12px;color:#4a505c}
  a{color:#1b4ee0}
</style>
</head>
<body>
<main>
<h1>This site doesn't exist or isn't shared with you</h1>
`)
	if name != "" {
		b.WriteString(`<p>You're signed in as <strong>`)
		b.WriteString(html.EscapeString(name))
		b.WriteString(`</strong>.</p>
`)
	}
	b.WriteString(`<p>Ask the person who sent you the link to share it with you.</p>
<p><a href="` + switchAccountPath + `">Switch account</a></p>
</main>
</body>
</html>
`)
	return b.String()
}
