package handler

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/reqlog"
)

// ownerIndexData is what the root of an owner host needs from the database:
// who the label belongs to, every site they own, and which of those are
// restricted. Wired in NewHostGate; a field on hostGate for the same reason
// siteForServing is one — a test supplies a fake instead of a live Postgres.
type ownerIndexData func(ctx context.Context, owner string) (ownerUserID string, sites []db.Site, restricted map[string]bool, err error)

func newOwnerIndexData(database *sql.DB) ownerIndexData {
	return func(ctx context.Context, owner string) (string, []db.Site, map[string]bool, error) {
		user, err := db.GetUserByUsername(ctx, database, owner)
		if err != nil {
			return "", nil, nil, err
		}
		sites, err := db.ListSitesByUsername(ctx, database, owner)
		if err != nil {
			return "", nil, nil, err
		}
		restricted, err := db.ListRestrictedSiteIDs(ctx, database)
		if err != nil {
			return "", nil, nil, err
		}
		return user.ID, sites, restricted, nil
	}
}

// ownerIndexEntry is one row of the page.
type ownerIndexEntry struct {
	name      string
	url       string
	updatedAt time.Time
	// shared marks a site everyone signed in can reach. Only the owner sees
	// the distinction, because only the owner is shown anything that is not
	// shared.
	shared bool
	// restricted marks a site that lives on its own host and is readable by
	// a named list. Only ever shown to the owner.
	restricted bool
}

// ownerIndexVisible decides what a caller may see on an owner host's root.
//
// The owner sees everything they have published. Everyone else sees only the
// sites the owner marked public, and never a restricted one: a restricted
// site's whole point is that its existence is not discoverable from anywhere
// except its own host, which checks the viewer list. A site
// with no active version is listed for nobody — there is nothing to link to.
func ownerIndexVisible(owner string, sites []db.Site, restricted map[string]bool, isOwner bool, hosts HostModel) []ownerIndexEntry {
	entries := make([]ownerIndexEntry, 0, len(sites))
	for _, s := range sites {
		if s.ActiveVersion <= 0 {
			continue
		}
		isRestricted := restricted[s.ID]
		if !isOwner && (isRestricted || !s.Public) {
			continue
		}
		entries = append(entries, ownerIndexEntry{
			name:       s.Name,
			url:        hosts.SiteURL(owner, s.Name, isRestricted),
			updatedAt:  s.UpdatedAt,
			shared:     s.Public,
			restricted: isRestricted,
		})
	}
	// Most recently updated first: a portfolio is read newest-first, and the
	// owner's own most recent work is what they came to find.
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].updatedAt.Equal(entries[j].updatedAt) {
			return entries[i].updatedAt.After(entries[j].updatedAt)
		}
		return entries[i].name < entries[j].name
	})
	return entries
}

// serveOwnerIndex renders the root of an owner host: everything that person
// has published, under their own name. Reached only from serveOwnerHost, and
// behind the same host session every other byte that host serves requires.
func (g *hostGate) serveOwnerIndex(w http.ResponseWriter, r *http.Request, label, requestHost string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	// The session is checked before the label is resolved, so an
	// unauthenticated caller gets the same answer for every label and cannot
	// enumerate who has an account by asking which subdomains exist.
	userID, sessionID, ok := g.requireHostSession(w, r, requestHost)
	if !ok {
		return
	}
	// Every response past the session check is logged, the same way every
	// hosted-content response is (see serve.go). This page
	// discloses one person's site list to a named caller, so it is exactly
	// the kind of access the audit trail exists to record — including the
	// refusals, which is why status and bytes are captured and written on
	// the way out rather than only on success.
	status := http.StatusOK
	var written int64
	defer func() { g.recordOwnerIndexVisit(r, label, userID, sessionID, status, written) }()

	owner, ok := g.resolveOwnerLabel(label)
	if !ok {
		status = http.StatusNotFound
		http.NotFound(w, r)
		return
	}
	if g.ownerIndex == nil {
		status = http.StatusNotFound
		http.NotFound(w, r)
		return
	}
	ownerUserID, sites, restricted, err := g.ownerIndex(r.Context(), owner)
	if err != nil {
		log.Printf("host gate: owner index %q: %v", owner, err)
		status = http.StatusInternalServerError
		http.Error(w, "failed to load sites", http.StatusInternalServerError)
		return
	}
	entries := ownerIndexVisible(owner, sites, restricted, ownerUserID == userID, g.hosts)

	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	body := renderOwnerIndex(owner, entries, ownerUserID == userID)
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusOK)
	written = int64(len(body))
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write([]byte(body)); err != nil {
		log.Printf("host gate: write owner index %q: %v", owner, err)
	}
}

// recordOwnerIndexVisit writes one access_log row for a visit to an owner
// host's root. SiteName is empty because this page is the host itself rather
// than any one of its sites, which is also how a reader tells the two apart.
func (g *hostGate) recordOwnerIndexVisit(r *http.Request, label, userID, sessionID string, status int, written int64) {
	if g.recordAccess == nil {
		return
	}
	g.recordAccess(audit.AccessEvent{
		At:         time.Now().UTC(),
		UserID:     userID,
		SessionID:  sessionID,
		OwnerLabel: label,
		SiteName:   "",
		Path:       r.URL.Path,
		Method:     r.Method,
		Status:     status,
		Bytes:      written,
		IP:         reqlog.ClientIP(r),
		UserAgent:  r.UserAgent(),
		ClientKind: classifyClient(r).String(),
	})
}

// resolveOwnerLabel maps a host label to the single user who owns it, reading
// the same user list resolveOwner does. It refuses an ambiguous label for the
// same reason: two users whose names normalise to one label must not have one
// of them silently chosen for the other.
func (g *hostGate) resolveOwnerLabel(label string) (string, bool) {
	users, err := g.files.store.ListUsers()
	if err != nil {
		log.Printf("host gate: list users for label %q: %v", label, err)
		return "", false
	}
	var holders []string
	for _, user := range users {
		if g.hosts.OwnsLabel(label, user) {
			holders = append(holders, user)
		}
	}
	switch len(holders) {
	case 1:
		return holders[0], true
	case 0:
		return "", false
	default:
		log.Printf("host gate: label %q resolves to %d users %v; refusing to serve an index for any of them", label, len(holders), holders)
		return "", false
	}
}

// renderOwnerIndex writes the page. Every style is inline: an owner host
// serves that owner's sites and nothing else, and adding a stylesheet path to
// it would widen the host's allow-list for decoration.
func renderOwnerIndex(owner string, entries []ownerIndexEntry, isOwner bool) string {
	var b strings.Builder
	b.WriteString(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex,nofollow">
<title>`)
	b.WriteString(html.EscapeString(owner))
	b.WriteString(`</title>
<style>
  :root{--ink:#16181d;--ink-soft:#4a505c;--ink-muted:#737b8a;--line:#e3e6ec;--bg:#fbfbfc;--accent:#1b4ee0}
  *{box-sizing:border-box}
  body{margin:0;background:var(--bg);color:var(--ink);
       font:16px/1.55 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
  main{max-width:720px;margin:0 auto;padding:56px 24px 80px}
  h1{font-size:28px;line-height:1.2;margin:0 0 4px;letter-spacing:-0.01em}
  .sub{color:var(--ink-muted);font-size:14px;margin:0 0 36px}
  ul{list-style:none;margin:0;padding:0;border-top:1px solid var(--line)}
  li{border-bottom:1px solid var(--line)}
  a.site{display:flex;align-items:baseline;gap:12px;padding:16px 4px;
         text-decoration:none;color:inherit}
  a.site:hover .name{color:var(--accent);text-decoration:underline}
  a.site:focus-visible{outline:3px solid #b9caf7;outline-offset:2px}
  .name{font-weight:600;font-size:17px}
  .when{margin-left:auto;color:var(--ink-muted);font-size:13px;white-space:nowrap}
  .tag{font-size:11px;letter-spacing:.06em;text-transform:uppercase;
       color:var(--ink-soft);border:1px solid var(--line);border-radius:999px;
       padding:2px 8px;background:#fff}
  .empty{color:var(--ink-soft);padding:28px 0}
</style>
</head>
<body>
<main>
<h1>`)
	b.WriteString(html.EscapeString(owner))
	b.WriteString("</h1>\n<p class=\"sub\">")
	if isOwner {
		b.WriteString("Everything you have published.")
	} else {
		b.WriteString("Shared work by ")
		b.WriteString(html.EscapeString(owner))
		b.WriteString(".")
	}
	b.WriteString("</p>\n")

	if len(entries) == 0 {
		if isOwner {
			b.WriteString(`<p class="empty">Nothing published yet. Ask your agent to deploy a site and it will appear here.</p>`)
		} else {
			b.WriteString(`<p class="empty">Nothing shared yet.</p>`)
		}
	} else {
		b.WriteString("<ul>\n")
		for _, e := range entries {
			b.WriteString(`<li><a class="site" href="`)
			b.WriteString(html.EscapeString(e.url))
			b.WriteString(`"><span class="name">`)
			b.WriteString(html.EscapeString(e.name))
			b.WriteString(`</span>`)
			if isOwner {
				switch {
				case e.restricted:
					b.WriteString(`<span class="tag">Named viewers</span>`)
				case e.shared:
					b.WriteString(`<span class="tag">Shared</span>`)
				default:
					b.WriteString(`<span class="tag">Private</span>`)
				}
			}
			b.WriteString(`<span class="when">`)
			b.WriteString(html.EscapeString(e.updatedAt.UTC().Format("2 Jan 2006")))
			b.WriteString("</span></a></li>\n")
		}
		b.WriteString("</ul>\n")
	}
	b.WriteString("</main>\n</body>\n</html>\n")
	return b.String()
}
