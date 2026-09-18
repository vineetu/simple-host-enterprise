package handler

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	neturl "net/url"
	"strings"

	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/safepath"
)

// siteAPIRoutes is the method set of *SiteAPIHandler the gate calls into,
// factored as an interface for the same reason siteForServing/viewerAllowed
// are factored into fields: a test builds a fake and exercises the gate's
// own auth/viewerAllowed/writerAllowed/Origin routing with no live Postgres
// and no real disk-backed asset store.
type siteAPIRoutes interface {
	GetState(w http.ResponseWriter, r *http.Request, call siteAPICall)
	PutState(w http.ResponseWriter, r *http.Request, call siteAPICall)
	GetStateVersioned(w http.ResponseWriter, r *http.Request, call siteAPICall)
	PutStateVersioned(w http.ResponseWriter, r *http.Request, call siteAPICall)
	CreateAsset(w http.ResponseWriter, r *http.Request, call siteAPICall)
	ListAssets(w http.ResponseWriter, r *http.Request, call siteAPICall)
	DeleteAsset(w http.ResponseWriter, r *http.Request, call siteAPICall, id string)
	ServeAsset(w http.ResponseWriter, r *http.Request, call siteAPICall, id string)
}

// The host gate decides, per hostname, which routes the mux may answer
// (design.md 7.1). The base host is the control plane only: everything under
// /, /auth/*, /dashboard, /admin, /api/*, /mcp, /docs, /skills.zip passes
// through untouched, except that a path shaped like the site-facing API
// (state, and later assets) is refused outright — one mux serves every host,
// and /api/sites/{site}/state would otherwise match the base host too. An
// owner host ("<label>.<base>") serves only that owner's hosted content, the
// site-facing API, and the session hand-off; a restricted site serves the
// same three things on its own flat label ("<owner label>--<site
// label>.<base>", design.md 5.2a) instead of at its owner's short path.
// Anything else — an unrecognised host, a bare IP, a port-forward — answers
// only the Kubernetes probes.

type hostGate struct {
	hosts HostModel
	// files serves site files and carries the only reference to disk storage;
	// resolveOwner and resolveRestrictedSiteName read it from there.
	files       *SiteFiles
	signingKeys []auth.SigningKey
	// negCache backs VerifyHostedSession: hosted content checks a session's
	// signature and this 60-second snapshot instead of reading the sessions
	// table on every request (design.md 5.1, 6.1).
	negCache *auth.NegativeSessionCache
	// handoff mints and redeems the hand-off's one-time codes; the gate
	// calls its unexported methods directly rather than mounting them as
	// ordinary mux routes, because which hostname is answering — the very
	// thing those methods need — is what the gate has just decided.
	handoff *HandoffHandler
	// siteForServing and viewerAllowed are db.SiteForServing and
	// db.ViewerAllowed by default (wired in NewHostGate); factored into
	// fields, the same shape search.go's recordSearchTelemetryFunc uses for
	// the same reason, so a test can supply a fake and exercise the gate's
	// own branching without a live Postgres — internal/db's own tests are
	// SQL-text assertions, never a live query, and there is no fake for
	// *sql.Row's concrete type to build a lighter-weight mock from.
	siteForServing func(r *http.Request, ownerUsername, siteName string) (siteID string, restricted bool, err error)
	viewerAllowed  func(r *http.Request, siteID, userID string) (bool, error)
	// writerAllowed is db.WriterAllowed by default (design.md 7.3's
	// writerAllowed rule), factored the same way as the two fields above.
	writerAllowed func(r *http.Request, siteID, userID string) (bool, error)
	// siteAPI serves the site-facing API (state and assets, design.md 7.3)
	// once this gate has resolved which site, authenticated the caller, and
	// checked viewerAllowed/writerAllowed.
	siteAPI siteAPIRoutes
	// authMiddleware authenticates a site-facing API request by session
	// cookie or X-API-Key — the same closure main.go already builds
	// (auth.Middleware) and wires into every other route; reused here
	// rather than re-implemented, so a revoked key or an expired session
	// behaves identically everywhere.
	authMiddleware func(http.Handler) http.Handler
	// originCheck is cookieOriginCheck(hosts, publicBaseURL): Origin is
	// required only for a session-cookie-authenticated, non-safe-method
	// request (design.md 7.3), and only ever compared against the
	// addressed host's own origin (origin.go's expectedFor).
	originCheck func(http.Handler) http.Handler
	// touchHostSession is db.TouchSession by default (wired in NewHostGate),
	// called from requireHostSession on every successful hosted-content
	// auth; factored into a field for the same reason as
	// siteForServing/viewerAllowed/writerAllowed above.
	touchHostSession func(sessionID string)
}

// NewHostGate returns middleware that decides, per hostname, which routes the
// wrapped mux may answer. Wire it directly around the mux in cmd/server/main.go.
func NewHostGate(hosts HostModel, files *SiteFiles, database *sql.DB, signingKeys []auth.SigningKey, negCache *auth.NegativeSessionCache, handoff *HandoffHandler, siteAPI *SiteAPIHandler, authMiddleware func(http.Handler) http.Handler, publicBaseURL string) func(http.Handler) http.Handler {
	gate := &hostGate{
		hosts:       hosts,
		files:       files,
		signingKeys: signingKeys,
		negCache:    negCache,
		handoff:     handoff,
		siteForServing: func(r *http.Request, ownerUsername, siteName string) (string, bool, error) {
			return db.SiteForServing(r.Context(), database, ownerUsername, siteName)
		},
		viewerAllowed: func(r *http.Request, siteID, userID string) (bool, error) {
			return db.ViewerAllowed(r.Context(), database, siteID, userID)
		},
		writerAllowed: func(r *http.Request, siteID, userID string) (bool, error) {
			return db.WriterAllowed(r.Context(), database, siteID, userID)
		},
		touchHostSession: func(sessionID string) {
			if err := db.TouchSession(context.Background(), database, sessionID); err != nil {
				log.Printf("host gate: touch session %s: %v", sessionID, err)
			}
		},
		siteAPI:        siteAPI,
		authMiddleware: authMiddleware,
		originCheck:    cookieOriginCheck(hosts, publicBaseURL),
	}
	return gate.wrap
}

// wrap is NewHostGate's middleware, factored onto *hostGate so a test can
// build a gate directly (with fake siteForServing/viewerAllowed funcs, to
// avoid needing a live Postgres — see the comment on those fields) and wrap
// a test mux with it the same way NewHostGate wraps the real one.
func (g *hostGate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind, label := g.hosts.Classify(r.Host)
		switch kind {
		case hostBase:
			// The site-facing API (design.md 7.3) is not a mux route at all
			// any more — it is served directly by serveOwnerHost and
			// serveRestrictedSiteHost below. But the base host's own mux
			// carries a catch-all "GET /" landing-page route (and every
			// other host-agnostic pattern), so a request shaped like the
			// site-facing API must still be refused explicitly here, or it
			// falls through to that catch-all instead of 404ing. This also
			// catches the pre-Phase-3 two-segment shape
			// (/api/sites/{user}/{site}/state[...]) a stale client or old
			// bookmark might still probe, even though nothing has served it
			// since attribution.go was deleted.
			if looksLikeSiteFacingAPIPath(r.URL.Path) {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		case hostOwner:
			g.serveOwnerHost(w, r, label, next)
		case hostRestrictedSite:
			g.serveRestrictedSiteHost(w, r, label, next)
		default:
			// The probes in deploy/base/deployment.yaml set no host, so
			// they arrive addressed to the Pod IP and must keep working.
			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				next.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}
	})
}

// applyOwnerHostSecurity sets the headers design.md 7.4 requires on every
// owner-host and restricted-site-host response, and reports whether the
// request may proceed at all: a Sec-Fetch-Site of same-site or cross-site is
// refused unless Sec-Fetch-Dest is document, so a navigation from a
// colleague's link (including the hand-off's own redirects) still works
// while a sibling-origin subresource load (<script src>, <img>, a
// credentialed fetch) is refused even by a browser that ignores the CORP
// header. A request with no Sec-Fetch-* headers at all (old browsers, curl
// with a key) is let through to the session and key checks instead.
func applyOwnerHostSecurity(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	// design.md 7.4: hosted content is authenticated (every viewer signed
	// in), so a shared cache must never keep a copy — "private, no-cache"
	// means "revalidate with the origin every time, and never on a shared
	// cache at all," not merely "vary by cookie." Set for every owner-host
	// and restricted-site-host response (hosted content, the site-facing
	// API, the hand-off), not hosted content alone: none of it belongs in
	// a cache another viewer could be served from.
	w.Header().Set("Cache-Control", "private, no-cache")
	site := r.Header.Get("Sec-Fetch-Site")
	if site == "same-site" || site == "cross-site" {
		return r.Header.Get("Sec-Fetch-Dest") == "document"
	}
	return true
}

// serveOwnerHost is the exhaustive allow-list for an owner host.
func (g *hostGate) serveOwnerHost(w http.ResponseWriter, r *http.Request, label string, next http.Handler) {
	if !applyOwnerHostSecurity(w, r) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		next.ServeHTTP(w, r)
		return
	}
	requestHost := label + "." + g.hosts.BaseHost()
	if r.URL.Path == "/auth/session" {
		g.handoff.redeemHandoffSession(w, r, requestHost)
		return
	}

	// The site-facing API (design.md 7.3): the site is named by the {site}
	// path segment, never a Referer. Only the named shape is honoured on an
	// owner host — the nameless convenience shape (/api/site/...) would be
	// ambiguous the moment an owner has more than one site, with nothing left
	// to disambiguate it now that Referer parsing is gone; that shape is
	// reserved for a restricted site's own host below, which serves exactly
	// one site and so has no such ambiguity.
	if route, siteFromPath, assetID, named, ok := parseSiteAPIPath(r.URL.Path); ok {
		if !named {
			http.NotFound(w, r)
			return
		}
		owner, ok := g.resolveOwner(label, siteFromPath)
		if !ok {
			http.NotFound(w, r)
			return
		}
		g.serveSiteAPI(w, r, route, owner, siteFromPath, assetID, label)
		return
	}

	// Hosted content: the short path /{sitename} and /{sitename}/...
	rest, ok := strings.CutPrefix(r.URL.Path, "/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	sitename, afterSite, hasSlash := strings.Cut(rest, "/")
	if sitename == "" || reservedSiteNames[sitename] || !safepath.IsSegment(sitename) {
		http.NotFound(w, r)
		return
	}
	owner, ok := g.resolveOwner(label, sitename)
	if !ok {
		http.NotFound(w, r)
		return
	}
	siteID, restricted, err := g.siteForServing(r, owner, sitename)
	if err != nil {
		if err != db.ErrSiteNotFound {
			log.Printf("host gate: resolve site %s/%s for serving: %v", owner, sitename, err)
		}
		http.NotFound(w, r)
		return
	}
	if restricted {
		// This site's address moved to its own host the moment it gained a
		// first viewer (design.md 5.2a); the owner-host short path no
		// longer serves it, the same way a deleted site does not.
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	if !hasSlash {
		redirectWithTrailingSlash(w, r)
		return
	}
	userID, sessionID, ok := g.requireHostSession(w, r, requestHost)
	if !ok {
		return
	}
	if !g.checkViewerAllowed(w, r, siteID, userID) {
		return
	}
	// GET /{site}/_assets/{id}[/{name}] (design.md 7.3): a top-level
	// "_assets" entry can never exist in a real upload (tarball refuses it),
	// so this interception is always unambiguous. Reuses every check above
	// (owner and site resolution, restriction, session, viewerAllowed) —
	// assets are viewerAllowed just like hosted content.
	if assetID, _, ok := parseAssetServePath(afterSite); ok {
		g.siteAPI.ServeAsset(w, r, siteAPICall{Owner: owner, SiteName: sitename, SiteID: siteID, Restricted: restricted}, assetID)
		return
	}
	g.files.serveSite(w, r, owner, sitename, "/"+sitename, userID, sessionID)
}

// serveRestrictedSiteHost is the exhaustive allow-list for a restricted
// site's own host, "<owner label>--<site label>.<base>" (design.md 5.2a).
// The whole host is dedicated to one site, so there is no "/<site>/"
// segment: the request path is the site's own file path directly.
func (g *hostGate) serveRestrictedSiteHost(w http.ResponseWriter, r *http.Request, label string, next http.Handler) {
	if !applyOwnerHostSecurity(w, r) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		next.ServeHTTP(w, r)
		return
	}
	requestHost := label + "." + g.hosts.BaseHost()
	if r.URL.Path == "/auth/session" {
		g.handoff.redeemHandoffSession(w, r, requestHost)
		return
	}

	ownerLabelPart, siteLabelPart, ok := SplitRestrictedSiteLabel(label)
	if !ok {
		http.NotFound(w, r)
		return
	}
	owner, ok := g.resolveLabelHolder(ownerLabelPart)
	if !ok {
		http.NotFound(w, r)
		return
	}
	sitename, ok := g.resolveRestrictedSiteName(owner, siteLabelPart)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// The site-facing API (design.md 7.3): a restricted site's own host
	// serves exactly one site, so the nameless convenience shape
	// (/api/site/...) is unambiguous here — this is the one place it is
	// honoured. The named shape (/api/sites/{site}/...) is accepted too, but
	// only when it names this exact site: a mismatch is not this route, not
	// a hint to try somewhere else.
	if route, siteFromPath, assetID, named, ok := parseSiteAPIPath(r.URL.Path); ok {
		if named && siteFromPath != sitename {
			http.NotFound(w, r)
			return
		}
		g.serveSiteAPI(w, r, route, owner, sitename, assetID, label)
		return
	}

	siteID, restricted, err := g.siteForServing(r, owner, sitename)
	if err != nil {
		if err != db.ErrSiteNotFound {
			log.Printf("host gate: resolve restricted site %s/%s for serving: %v", owner, sitename, err)
		}
		http.NotFound(w, r)
		return
	}
	if !restricted {
		// This address exists only for a site that currently has viewers;
		// an unrestricted site is not reachable here even if the labels
		// happen to match (design.md 5.2a).
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	userID, sessionID, ok := g.requireHostSession(w, r, requestHost)
	if !ok {
		return
	}
	if !g.checkViewerAllowed(w, r, siteID, userID) {
		return
	}
	// GET /_assets/{id}[/{name}] (design.md 7.3): root-served, matching how
	// hosted content itself has no site segment on this host kind.
	if assetID, _, ok := parseAssetServePath(strings.TrimPrefix(r.URL.Path, "/")); ok {
		g.siteAPI.ServeAsset(w, r, siteAPICall{Owner: owner, SiteName: sitename, SiteID: siteID, Restricted: restricted}, assetID)
		return
	}
	g.files.serveSite(w, r, owner, sitename, "", userID, sessionID)
}

// requireHostSession authenticates a hosted-content request from its
// __Host-sh_session cookie alone, with no database read beyond the
// negative-cache refresh loop (design.md 5.1, 6.1) — except for
// touchHostSession, a fire-and-forget, self-throttled last_seen_at write
// (db.TouchSession already matches zero rows outside its own five-minute
// window, design.md 6.1: "at most once per five minutes from hosted
// content") on every successful hosted-content auth. Without this, an
// actively-browsing viewer's session goes idle and is killed by
// SESSION_IDLE even while they keep viewing, because nothing on the
// hosted-content path ever told the row they were still there — Phase 2
// review finding. Missing or invalid, a navigation is sent through the
// hand-off (beginHandoff writes the response itself); anything else — a
// script's fetch, an asset request, an agent with no cookie at all — gets
// 401, so it fails fast rather than following a redirect chain built for a
// browser.
func (g *hostGate) requireHostSession(w http.ResponseWriter, r *http.Request, requestHost string) (userID, sessionID string, ok bool) {
	c, err := r.Cookie(auth.SessionCookieName)
	if err == nil && c.Value != "" {
		if uid, sid, valid := auth.VerifyHostedSession(g.signingKeys, g.negCache, c.Value, requestHost); valid {
			g.touchHostSession(sid)
			return uid, sid, true
		}
	}
	if wantsNavigation(r) {
		g.handoff.beginHandoff(w, r, requestHost)
		return "", "", false
	}
	writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
	return "", "", false
}

// viewerAllowed applies design.md 7.2's rule and writes a 404 (never 403: a
// restricted site's existence is not confirmed to somebody it refuses) when
// it does not hold.
func (g *hostGate) checkViewerAllowed(w http.ResponseWriter, r *http.Request, siteID, userID string) bool {
	allowed, err := g.viewerAllowed(r, siteID, userID)
	if err != nil {
		log.Printf("host gate: viewer allowed check for site %s: %v", siteID, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return false
	}
	if !allowed {
		http.NotFound(w, r)
		return false
	}
	return true
}

// siteAPIRouteKind is which of design.md 7.3's site-facing API routes a
// path names. assetsCollection covers both GET (list) and POST (create) on
// /assets; the method is decided by serveSiteAPI, same as GET/PUT on state.
type siteAPIRouteKind int

const (
	siteAPIState siteAPIRouteKind = iota
	siteAPIStateVersioned
	siteAPIAssetsCollection
	siteAPIAssetItem
)

// looksLikeSiteFacingAPIPath is parseSiteAPIPath plus the pre-Phase-3
// two-segment shape (/api/sites/{user}/{site}/state[/versioned], names
// ignored) that attribution.go used to attribute from Referer. Nothing
// serves that shape any more; this exists only so the base host's own
// catch-all routes do not accidentally answer for it (see wrap's hostBase
// case) — a defense a stale client or bookmark still benefits from.
func looksLikeSiteFacingAPIPath(p string) bool {
	if _, _, _, _, ok := parseSiteAPIPath(p); ok {
		return true
	}
	rest, ok := strings.CutPrefix(p, "/api/sites/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 3 || parts[0] == "" || parts[2] != "state" {
		return false
	}
	return len(parts) == 3 || (len(parts) == 4 && parts[3] == "versioned")
}

// parseSiteAPIPath recognises every site-facing API path shape (design.md
// 7.3), state and assets alike, in both the named form
// (/api/sites/{site}/...) and the nameless convenience form (/api/site/...,
// honoured only on a restricted site's own host — see serveRestrictedSiteHost).
// It does not recognise the asset *serve* route
// (/{site}/_assets/{id}[/{name}]), which lives under the hosted-content path
// shape instead and is handled by parseAssetServePath at the point hosted
// content is already being resolved.
func parseSiteAPIPath(p string) (kind siteAPIRouteKind, site, assetID string, named, ok bool) {
	if rest, isNameless := strings.CutPrefix(p, "/api/site/"); isNameless {
		if k, id, ok := parseSiteAPISuffix(rest); ok {
			return k, "", id, false, true
		}
		return 0, "", "", false, false
	}
	rest, ok := strings.CutPrefix(p, "/api/sites/")
	if !ok {
		return 0, "", "", false, false
	}
	siteSeg, suffix, hasSuffix := strings.Cut(rest, "/")
	if !hasSuffix || siteSeg == "" || !safepath.IsSegment(siteSeg) {
		return 0, "", "", false, false
	}
	if k, id, ok := parseSiteAPISuffix(suffix); ok {
		return k, siteSeg, id, true, true
	}
	return 0, "", "", false, false
}

// parseSiteAPISuffix parses everything after the site name (or, for the
// nameless form, after "/api/site/"): "state", "state/versioned", "assets",
// or "assets/{id}".
func parseSiteAPISuffix(suffix string) (kind siteAPIRouteKind, assetID string, ok bool) {
	switch suffix {
	case "state":
		return siteAPIState, "", true
	case "state/versioned":
		return siteAPIStateVersioned, "", true
	case "assets":
		return siteAPIAssetsCollection, "", true
	}
	if id, ok := strings.CutPrefix(suffix, "assets/"); ok && id != "" && safepath.IsSegment(id) {
		return siteAPIAssetItem, id, true
	}
	return 0, "", false
}

// parseAssetServePath recognises design.md 7.3's asset-serving path once the
// site (or, on a restricted host, nothing) has already been stripped off:
// "_assets/{id}" or "_assets/{id}/{name}". name is display-only — id alone
// resolves the file — so an invalid name does not fail the parse.
func parseAssetServePath(p string) (id, name string, ok bool) {
	rest, ok := strings.CutPrefix(p, "_assets/")
	if !ok || rest == "" {
		return "", "", false
	}
	id, name, _ = strings.Cut(rest, "/")
	if id == "" || !safepath.IsSegment(id) {
		return "", "", false
	}
	return id, name, true
}

// serveSiteAPI is the shared dispatcher for both host kinds once the owner
// and site name are resolved: it looks up the site, checks Origin on a
// non-safe method, authenticates (session or X-API-Key, via the same
// authMiddleware every other route uses), checks viewerAllowed (every
// route) and writerAllowed (every route but a plain read), builds the
// via_site claim, and calls the matching SiteAPIHandler method.
func (g *hostGate) serveSiteAPI(w http.ResponseWriter, r *http.Request, kind siteAPIRouteKind, owner, siteName, assetID, label string) {
	siteID, restricted, err := g.siteForServing(r, owner, siteName)
	if err != nil {
		if err != db.ErrSiteNotFound {
			log.Printf("host gate: resolve site %s/%s for site API: %v", owner, siteName, err)
		}
		http.NotFound(w, r)
		return
	}

	method := r.Method
	needsWrite := method == http.MethodPut || method == http.MethodPost || method == http.MethodDelete
	safeMethod := method == http.MethodGet || method == http.MethodHead
	if !safeMethod {
		if ok := g.checkSiteAPIOrigin(w, r); !ok {
			return
		}
	}

	user, _, keyID, ok := g.authenticateSiteAPI(w, r)
	if !ok {
		return
	}
	if !g.checkSiteAccess(w, r, siteID, user.ID, needsWrite) {
		return
	}

	actorKind := "person"
	if keyID != "" {
		actorKind = "key"
	}
	call := siteAPICall{
		Owner: owner, SiteName: siteName, SiteID: siteID, Restricted: restricted,
		ActorUserID: user.ID, ActorKind: actorKind, KeyID: keyID,
		ViaSiteLabel: label, ViaSiteName: siteName,
		ViaSiteObserved: viaSiteObserved(r, siteName, restricted, label+"."+g.hosts.BaseHost()),
	}

	switch kind {
	case siteAPIState:
		switch method {
		case http.MethodGet:
			g.siteAPI.GetState(w, r, call)
		case http.MethodPut:
			g.siteAPI.PutState(w, r, call)
		default:
			methodNotAllowed(w, "GET, PUT")
		}
	case siteAPIStateVersioned:
		switch method {
		case http.MethodGet:
			g.siteAPI.GetStateVersioned(w, r, call)
		case http.MethodPut:
			g.siteAPI.PutStateVersioned(w, r, call)
		default:
			methodNotAllowed(w, "GET, PUT")
		}
	case siteAPIAssetsCollection:
		switch method {
		case http.MethodGet:
			g.siteAPI.ListAssets(w, r, call)
		case http.MethodPost:
			g.siteAPI.CreateAsset(w, r, call)
		default:
			methodNotAllowed(w, "GET, POST")
		}
	case siteAPIAssetItem:
		if method == http.MethodDelete {
			g.siteAPI.DeleteAsset(w, r, call, assetID)
		} else {
			methodNotAllowed(w, "DELETE")
		}
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
}

// authenticateSiteAPI runs the shared authMiddleware (session cookie or
// X-API-Key — the same closure every other route authenticates through) and
// captures what it decided, without duplicating its logic. ok is false only
// when authMiddleware already wrote a 401 and called next itself.
func (g *hostGate) authenticateSiteAPI(w http.ResponseWriter, r *http.Request) (user *db.User, sessionID, keyID string, ok bool) {
	g.authMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, ar *http.Request) {
		user = auth.GetUser(ar.Context())
		sessionID = auth.SessionID(ar.Context())
		keyID = auth.APIKeyID(ar.Context())
		ok = true
	})).ServeHTTP(w, r)
	return user, sessionID, keyID, ok
}

// checkSiteAPIOrigin runs cookieOriginCheck, which enforces Origin only when
// a session cookie is present (an X-API-Key caller is never a browser page)
// and compares against the addressed host's own origin (origin.go's
// expectedFor, which treats an owner host and a restricted site's own host
// identically). false means the check already wrote the 403.
func (g *hostGate) checkSiteAPIOrigin(w http.ResponseWriter, r *http.Request) bool {
	ok := false
	g.originCheck(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ok = true })).ServeHTTP(w, r)
	return ok
}

// checkSiteAccess applies design.md 7.2's viewerAllowed to every route (a
// site a caller may not even view does not confirm its own existence, so a
// failure here is 404) and, for a route that writes, design.md 7.3's
// writerAllowed on top (a caller who may view but not write gets 403: the
// site's existence is already established by the successful view check).
func (g *hostGate) checkSiteAccess(w http.ResponseWriter, r *http.Request, siteID, userID string, needsWrite bool) bool {
	allowed, err := g.viewerAllowed(r, siteID, userID)
	if err != nil {
		log.Printf("host gate: viewer allowed check for site %s: %v", siteID, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return false
	}
	if !allowed {
		http.NotFound(w, r)
		return false
	}
	if !needsWrite {
		return true
	}
	writable, err := g.writerAllowed(r, siteID, userID)
	if err != nil {
		log.Printf("host gate: writer allowed check for site %s: %v", siteID, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return false
	}
	if !writable {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "forbidden"})
		return false
	}
	return true
}

// viaSiteObserved corroborates the via_site claim (design.md 7.3's audit
// paragraph) where the browser allows it: Sec-Fetch-Dest must be "empty" (a
// fetch, not a navigation — a hosted page's own script call looks like
// this; a top-level page load does not), and, when a Referer is present,
// its first path segment must agree with the claimed site name. Most
// callers send neither header, so false is the common case and is not
// itself a sign of anything wrong — see the field's own doc comment.
// viaSiteObserved corroborates the via_site claim two different ways
// depending on host kind, because a restricted site is root-served
// (design.md 5.2a) and so has no "/<site>/" path segment for its own pages
// to carry at all — comparing the Referer's first path segment against the
// site name there would always fail, silently marking every legitimate
// restricted-site write as unobserved (review finding). For a restricted
// site, ownHost (the site's own "<owner>--<site>.<base>" host) is the one
// thing that names "this exact site" unambiguously, so the Referer's own
// host is compared against it instead. An owner host, where several sites
// share one origin, keeps the path-segment rule: the host alone cannot
// distinguish which of an owner's sites a page belongs to.
func viaSiteObserved(r *http.Request, claimedSite string, restricted bool, ownHost string) bool {
	if r.Header.Get("Sec-Fetch-Dest") != "empty" {
		return false
	}
	referer := r.Header.Get("Referer")
	if referer == "" {
		return false
	}
	parsed, err := neturl.Parse(referer)
	if err != nil {
		return false
	}
	if restricted {
		return normalizeHost(parsed.Host) == normalizeHost(ownHost)
	}
	segment, _, _ := strings.Cut(strings.TrimPrefix(parsed.EscapedPath(), "/"), "/")
	name, err := neturl.PathUnescape(segment)
	if err != nil {
		return false
	}
	return name == claimedSite
}

// resolveLabelHolder finds the single username whose label is label, using
// only the user directories on disk. It is resolveOwner without the per-site
// filter, for the restricted-site host's owner part, which names no site
// until resolveRestrictedSiteName runs; the reasons for reading the disk
// rather than the database, and for failing closed on a collision, are the
// ones documented on resolveOwner.
func (g *hostGate) resolveLabelHolder(label string) (string, bool) {
	users, err := g.files.diskStorage.ListUsers()
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
		log.Printf("host gate: label %q resolves to %d users %v; refusing to serve any of them", label, len(holders), holders)
		return "", false
	}
}

// resolveRestrictedSiteName finds the single site under owner whose DNS
// label (siteLabel) is siteLabelPart, using only the site directories on
// disk — the same disk-only, fail-closed-on-collision pattern resolveOwner
// uses for the owner label itself. It also requires a current version to
// exist, so a site with no live content is not addressable here either.
func (g *hostGate) resolveRestrictedSiteName(owner, siteLabelPart string) (string, bool) {
	names, err := g.files.diskStorage.ListSites(owner)
	if err != nil {
		log.Printf("host gate: list sites for %q: %v", owner, err)
		return "", false
	}
	var matches []string
	for _, name := range names {
		if siteLabel(name) != siteLabelPart {
			continue
		}
		_, ok, err := g.files.diskStorage.CurrentVersion(owner, name)
		if err != nil {
			log.Printf("host gate: owner %q site %q: current version unreadable, refusing to resolve: %v", owner, name, err)
			return "", false
		}
		if ok {
			matches = append(matches, name)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], true
	case 0:
		return "", false
	default:
		log.Printf("host gate: owner %q site label %q resolves to %d sites %v; refusing to serve any of them", owner, siteLabelPart, len(matches), matches)
		return "", false
	}
}

// resolveOwner finds the username whose label is label and who has a current
// version of sitename, using only the site directory tree. The database is
// deliberately not consulted for this: static serving survives a database
// outage today and must keep doing so. Whether the site is restricted, and
// whether the caller may view it, are separate checks made afterward against
// the database (SiteForServing, viewerAllowed) — a new, accepted dependency
// this phase's authenticated-viewing requirement introduces.
//
// Exactly one candidate is the owner. Two accounts sharing a label is a
// collision the registration guard is supposed to prevent; serving either
// would let one owner's page answer for another's, so that fails closed.
//
// A candidate whose current version cannot be read fails the whole resolution
// closed too, rather than being skipped. Two users sharing a label is already
// a 404, and an unreadable candidate must not silently hand the name to the
// other one: skipping it would serve the readable user's site under a label
// the unreadable user may equally own.
//
// This is a readdir of the base path per short-path request. With ~108 user
// directories that is cheap; a cache keyed on label is the place to look if
// it ever shows up in profiles.
func (g *hostGate) resolveOwner(label, sitename string) (string, bool) {
	disk := g.files.diskStorage
	users, err := disk.ListUsers()
	if err != nil {
		log.Printf("host gate: list users for label %q: %v", label, err)
		return "", false
	}
	var holders []string
	for _, user := range users {
		if !g.hosts.OwnsLabel(label, user) {
			continue
		}
		// CurrentVersion reports a missing site or missing current link as
		// (0, false, nil); any error is something other than "does not exist".
		_, ok, err := disk.CurrentVersion(user, sitename)
		if err != nil {
			log.Printf("host gate: label %q user %q site %q: current version unreadable, refusing to resolve: %v", label, user, sitename, err)
			return "", false
		}
		if !ok {
			continue
		}
		holders = append(holders, user)
	}
	switch len(holders) {
	case 1:
		return holders[0], true
	case 0:
		return "", false
	default:
		log.Printf("host gate: label %q resolves to %d users %v for site %q; refusing to serve any of them", label, len(holders), holders, sitename)
		return "", false
	}
}
