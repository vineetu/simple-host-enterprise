package handler

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	neturl "net/url"
	"strings"

	"github.com/vsriram/simple-host/internal/audit"
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

// The host gate decides, per hostname, which routes the mux may answer.
// The base host is the control plane only: everything under
// /, /auth/*, /dashboard, /admin, /api/*, /mcp, /docs, /skills.zip passes
// through untouched, except that a path shaped like the site-facing API
// (state, and later assets) is refused outright — one mux serves every host,
// and /api/sites/{site}/state would otherwise match the base host too. An
// owner host ("<label>.<base>") serves that owner's index page, the session
// hand-off, and redirects from its pre-v1.3 site paths; a site's own host
// ("<owner label>--<site part>.<base>") serves that one site's content, its
// site-facing API, and the session hand-off.
// Anything else — an unrecognised host, a bare IP, a port-forward — answers
// only the Kubernetes probes.

type hostGate struct {
	hosts HostModel
	// files serves site files and carries the store client site files live
	// in (internal/storage's bucket store); resolveLabelHolder and
	// resolveSiteName read it from there.
	files       *SiteFiles
	signingKeys []auth.SigningKey
	// negCache backs VerifyHostedSession: hosted content checks a session's
	// signature and this 60-second snapshot instead of reading the sessions
	// table on every request.
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
	// writerAllowed is db.WriterAllowed by default, factored the same way
	// as the two fields above.
	writerAllowed func(r *http.Request, siteID, userID string) (bool, error)
	// networkOpen is db.NetworkOpen by default: whether an admin has
	// approved the site for anonymous visitors. Asked only when a request
	// carries no valid host session; nil means no site is.
	networkOpen func(r *http.Request, siteID string) (bool, error)
	// siteAPI serves the site-facing API (state and assets)
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
	// request, and only ever compared against the
	// addressed host's own origin (origin.go's expectedFor).
	originCheck func(http.Handler) http.Handler
	// touchHostSession is db.TouchSession by default (wired in NewHostGate),
	// called from requireHostSession on every successful hosted-content
	// auth; factored into a field for the same reason as
	// siteForServing/viewerAllowed/writerAllowed above.
	touchHostSession func(sessionID string)
	// legacyTeam is db.LegacyTeamName by default: which team a pre-v1.3
	// team address now belongs to. nil redirects nothing.
	legacyTeam func(r *http.Request, label string) (string, bool, error)
	// ownerIndex backs the root of an owner host (owner_index.go), wired in
	// NewHostGate from the database and factored into a field for the same
	// reason as siteForServing above.
	ownerIndex ownerIndexData
	// recordAccess writes one access_log row, reading SiteFiles' writer at
	// call time because it is attached after NewHostGate runs. A field for
	// the same reason as the funcs above: a test asserts what was logged
	// without a live Postgres behind the writer.
	recordAccess func(audit.AccessEvent)
	// audit records access_denied when a signed-in person is refused a
	// site (viewerAllowed or writerAllowed). nil records nothing.
	audit audit.Recorder
}

// recordDenied writes one access_denied audit row for a signed-in caller
// the gate refused.
func (g *hostGate) recordDenied(r *http.Request, siteID, userID, reason string) {
	if g.audit == nil {
		return
	}
	g.audit.Record(r.Context(), audit.Event{
		ActorID: userID, Action: "access_denied", SiteID: siteID,
		Detail: r.Method + " " + r.Host + r.URL.Path, Extra: map[string]any{"reason": reason},
	})
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
		networkOpen: func(r *http.Request, siteID string) (bool, error) {
			return db.NetworkOpen(r.Context(), database, siteID)
		},
		touchHostSession: func(sessionID string) {
			if err := db.TouchSession(context.Background(), database, sessionID); err != nil {
				log.Printf("host gate: touch session %s: %v", sessionID, err)
			}
		},
		ownerIndex: newOwnerIndexData(database),
		legacyTeam: func(r *http.Request, label string) (string, bool, error) {
			return db.LegacyTeamName(r.Context(), database, label)
		},
		recordAccess: func(event audit.AccessEvent) {
			if files != nil {
				files.access.Enqueue(event)
			}
		},
		siteAPI:        siteAPI,
		authMiddleware: authMiddleware,
		audit:          siteAPIRecorder(siteAPI),
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
			// The site-facing API is not a mux route at all
			// any more — it is served directly by serveSiteHost below. But the base host's own mux
			// carries a catch-all "GET /" landing-page route (and every
			// other host-agnostic pattern), so a request shaped like the
			// site-facing API must still be refused explicitly here, or it
			// falls through to that catch-all instead of 404ing. This also
			// catches the older two-segment shape
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
		case hostSite:
			g.serveSiteHost(w, r, label, next)
		case hostLegacySite:
			g.serveLegacySiteHost(w, r, label, next)
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

// applyOwnerHostSecurity sets the headers required on every
// owner-host and site-host response, and reports whether the
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
	// Hosted content is authenticated (every viewer signed
	// in), so a shared cache must never keep a copy — "private, no-cache"
	// means "revalidate with the origin every time, and never on a shared
	// cache at all," not merely "vary by cookie." Set for every owner-host
	// and site-host response (hosted content, the site-facing
	// API, the hand-off), not hosted content alone: none of it belongs in
	// a cache another viewer could be served from.
	w.Header().Set("Cache-Control", "private, no-cache")
	// No service workers on hosted hosts: one registered at a site's root would intercept the navigation to /auth/session and read
	// a hand-off code before this server ever sees (and spends) it.
	if r.Header.Get("Service-Worker") != "" {
		return false
	}
	site := r.Header.Get("Sec-Fetch-Site")
	if site == "same-site" || site == "cross-site" {
		return r.Header.Get("Sec-Fetch-Dest") == "document"
	}
	return true
}

// serveOwnerHost is the exhaustive allow-list for an owner host: the
// person's or team's index page at "/", the session hand-off, and the
// pre-v1.3 addresses beneath it, "/<site>/..." and "/api/sites/<site>/...".
// Once the owner's certificate is ready those redirect to the site's own
// host, path and query kept, computed from the address alone (never from
// whether the site exists or who is asking, so the answer confirms
// nothing). Until then this host serves them itself (serveOwnerPath).
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
	if r.URL.Path == "/" {
		g.serveOwnerIndex(w, r, label, requestHost)
		return
	}

	escaped := r.URL.EscapedPath()
	var siteName, target string
	if _, siteFromPath, _, named, ok := parseSiteAPIPath(r.URL.Path); ok && named {
		// The site's own host accepts the named shape unchanged.
		siteName, target = siteFromPath, escaped
	} else {
		rest, _ := strings.CutPrefix(escaped, "/")
		segment, after, _ := strings.Cut(rest, "/")
		name, err := neturl.PathUnescape(segment)
		// "/api/..." and "/sites/..." were never site paths here.
		if err != nil || name == "" || reservedSiteNames[name] || !safepath.IsSegment(name) {
			http.NotFound(w, r)
			return
		}
		siteName, target = name, "/"+after
	}
	if current := g.currentOwnerLabel(r, label); current != label {
		// A pre-v1.3 team address: the same path on the team's own host,
		// which then serves or redirects it by its own readiness.
		g.redirectToHost(w, r, current, escaped)
		return
	}
	if g.hosts.OwnerReady(label) {
		host := siteHostPart(siteName) + "." + label
		if len(host)+1+len(g.hosts.BaseHost()) > maxHostLen {
			http.NotFound(w, r)
			return
		}
		g.redirectToHost(w, r, host, target)
		return
	}
	g.serveOwnerPath(w, r, label, requestHost)
}

// serveOwnerPath serves "<owner>.<base>/<site>/..." and the named site API
// on the owner host while the owner's own certificate is not ready yet: the
// v1.2 shape, with the owner's sites sharing this origin until it is.
func (g *hostGate) serveOwnerPath(w http.ResponseWriter, r *http.Request, label, requestHost string) {
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
	rest, _ := strings.CutPrefix(r.URL.Path, "/")
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
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	if !hasSlash {
		redirectWithTrailingSlash(w, r)
		return
	}
	userID, sessionID, anonymous, ok := g.requireHostSessionOrNetwork(w, r, requestHost, siteID)
	if !ok {
		return
	}
	if !anonymous && !g.checkViewerAllowed(w, r, siteID, userID) {
		return
	}
	if assetID, _, ok := parseAssetServePath(afterSite); ok {
		g.siteAPI.ServeAsset(w, r, siteAPICall{Owner: owner, SiteName: sitename, SiteID: siteID, Restricted: restricted}, assetID)
		return
	}
	g.files.serveSite(w, r, owner, sitename, "/"+sitename, userID, sessionID)
}

// serveLegacySiteHost answers "<owner>--<site>.<base>", where v1.2 served a
// site shared with named people, by redirecting to that site's current
// address with the rest of the path and the query.
func (g *hostGate) serveLegacySiteHost(w http.ResponseWriter, r *http.Request, label string, next http.Handler) {
	if !applyOwnerHostSecurity(w, r) {
		http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
		return
	}
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		next.ServeHTTP(w, r)
		return
	}
	ownerPart, sitePart, ok := splitLegacySiteLabel(label)
	if !ok {
		http.NotFound(w, r)
		return
	}
	owner, ok := g.resolveLabelHolder(g.currentOwnerLabel(r, ownerPart))
	if !ok {
		http.NotFound(w, r)
		return
	}
	sitename, ok := g.resolveSiteName(owner, sitePart)
	if !ok {
		http.NotFound(w, r)
		return
	}
	location := g.hosts.SiteURL(owner, sitename)
	if location == "" {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	if suffix, nameless := strings.CutPrefix(rest, "api/site/"); nameless && !g.hosts.OwnerReady(ownerLabel(owner)) {
		// The owner path serves only the named API shape.
		location = g.hosts.OwnerOrigin(ownerLabel(owner)) + "/"
		rest = "api/sites/" + neturl.PathEscape(sitename) + "/" + suffix
	}
	location += rest
	if r.URL.RawQuery != "" {
		location += "?" + r.URL.RawQuery
	}
	code := http.StatusMovedPermanently
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		code = http.StatusPermanentRedirect
	}
	http.Redirect(w, r, location, code)
}

// currentOwnerLabel is label, unless it is a pre-v1.3 team address: no
// account holds it and "team-<label>" is a team (db.LegacyTeamName).
func (g *hostGate) currentOwnerLabel(r *http.Request, label string) string {
	if g.legacyTeam == nil {
		return label
	}
	team, ok, err := g.legacyTeam(r, label)
	if err != nil {
		log.Printf("host gate: legacy team lookup %q: %v", label, err)
		return label
	}
	if ok {
		return ownerLabel(team)
	}
	return label
}

// redirectToHost sends the request to hostLabel's host with escapedPath and
// the original query. A navigation or a read is a 301; anything else is a
// 308 so a client that follows it repeats the same method and body.
// hostLabel is always built by this server, never taken from the request.
func (g *hostGate) redirectToHost(w http.ResponseWriter, r *http.Request, hostLabel, escapedPath string) {
	location := g.hosts.OwnerOrigin(hostLabel) + escapedPath
	if r.URL.RawQuery != "" {
		location += "?" + r.URL.RawQuery
	}
	code := http.StatusMovedPermanently
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		code = http.StatusPermanentRedirect
	}
	http.Redirect(w, r, location, code)
}

// serveSiteHost is the exhaustive allow-list for a site's own host,
// "<site part>.<owner label>.<base>". The whole host is dedicated to one
// site, so there is no "/<site>/" segment: the request path is the site's
// own file path directly.
func (g *hostGate) serveSiteHost(w http.ResponseWriter, r *http.Request, label string, next http.Handler) {
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

	sitePart, ownerLabelPart, ok := SplitSiteLabel(label)
	if !ok {
		http.NotFound(w, r)
		return
	}
	owner, ok := g.resolveLabelHolder(ownerLabelPart)
	if !ok {
		http.NotFound(w, r)
		return
	}
	sitename, ok := g.resolveSiteName(owner, sitePart)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// The site-facing API: this host serves exactly one site, so the
	// nameless convenience shape (/api/site/...) is unambiguous. The named
	// shape (/api/sites/{site}/...) is accepted too, but only when it names
	// this exact site: a mismatch is not this route, not a hint to try
	// somewhere else.
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
			log.Printf("host gate: resolve site %s/%s for serving: %v", owner, sitename, err)
		}
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	userID, sessionID, anonymous, ok := g.requireHostSessionOrNetwork(w, r, requestHost, siteID)
	if !ok {
		return
	}
	if !anonymous && !g.checkViewerAllowed(w, r, siteID, userID) {
		return
	}
	// GET /_assets/{id}[/{name}]: root-served, matching how hosted content
	// itself has no site segment on this host. A top-level "_assets" entry
	// can never exist in a real upload (tarball refuses it), so this
	// interception is always unambiguous.
	if assetID, _, ok := parseAssetServePath(strings.TrimPrefix(r.URL.Path, "/")); ok {
		g.siteAPI.ServeAsset(w, r, siteAPICall{Owner: owner, SiteName: sitename, SiteID: siteID, Restricted: restricted}, assetID)
		return
	}
	g.files.serveSite(w, r, owner, sitename, "", userID, sessionID)
}

// requireHostSession authenticates a hosted-content request from its
// __Host-sh_session cookie alone, with no database read beyond the
// negative-cache refresh loop — except for touchHostSession, a
// fire-and-forget, self-throttled last_seen_at write (db.TouchSession
// already matches zero rows outside its own five-minute window: at most
// once per five minutes from hosted content) on every successful
// hosted-content auth. Without this, an actively-browsing viewer's session
// goes idle and is killed by SESSION_IDLE even while they keep viewing,
// because nothing on the hosted-content path ever told the row they were
// still there — a review finding. Missing or invalid, a navigation is sent
// through the
// hand-off (beginHandoff writes the response itself); anything else — a
// script's fetch, an asset request, an agent with no cookie at all — gets
// 401, so it fails fast rather than following a redirect chain built for a
// browser.
func (g *hostGate) requireHostSession(w http.ResponseWriter, r *http.Request, requestHost string) (userID, sessionID string, ok bool) {
	if uid, sid, valid := g.validHostSession(r, requestHost); valid {
		return uid, sid, true
	}
	if wantsNavigation(r) {
		g.handoff.beginHandoff(w, r, requestHost)
		return "", "", false
	}
	writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
	return "", "", false
}

// validHostSession verifies the request's host session cookie, if any, and
// touches it when valid.
func (g *hostGate) validHostSession(r *http.Request, requestHost string) (userID, sessionID string, ok bool) {
	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil || c.Value == "" {
		return "", "", false
	}
	uid, sid, valid := auth.VerifyHostedSession(g.signingKeys, g.negCache, c.Value, requestHost)
	if !valid {
		return "", "", false
	}
	g.touchHostSession(sid)
	return uid, sid, true
}

// requireHostSessionOrNetwork is requireHostSession, except that a request
// with no valid host session to a site an admin has opened to the network is
// let through as anonymous (anonymous true, no user). A signed-in person who
// wants their own rights on such a site adds ?signin to the address, which
// takes the usual hand-off.
func (g *hostGate) requireHostSessionOrNetwork(w http.ResponseWriter, r *http.Request, requestHost, siteID string) (userID, sessionID string, anonymous, ok bool) {
	if uid, sid, valid := g.validHostSession(r, requestHost); valid {
		return uid, sid, false, true
	}
	if _, signin := r.URL.Query()["signin"]; !signin && g.networkOpen != nil {
		open, err := g.networkOpen(r, siteID)
		if err != nil {
			log.Printf("host gate: network access check for site %s: %v", siteID, err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return "", "", false, false
		}
		if open {
			return "", "", true, true
		}
	}
	uid, sid, ok := g.requireHostSession(w, r, requestHost)
	return uid, sid, false, ok
}

// viewerAllowed applies its own rule and writes a 404 (never 403: a
// site's existence is not confirmed to somebody it refuses) when
// it does not hold.
func (g *hostGate) checkViewerAllowed(w http.ResponseWriter, r *http.Request, siteID, userID string) bool {
	allowed, err := g.viewerAllowed(r, siteID, userID)
	if err != nil {
		log.Printf("host gate: viewer allowed check for site %s: %v", siteID, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return false
	}
	if !allowed {
		g.recordDenied(r, siteID, userID, "not_a_viewer")
		http.NotFound(w, r)
		return false
	}
	return true
}

func siteAPIRecorder(h *SiteAPIHandler) audit.Recorder {
	if h == nil {
		return nil
	}
	return h.audit
}

// siteAPIRouteKind is which of the site-facing API routes a
// path names. assetsCollection covers both GET (list) and POST (create) on
// /assets; the method is decided by serveSiteAPI, same as GET/PUT on state.
type siteAPIRouteKind int

const (
	siteAPIState siteAPIRouteKind = iota
	siteAPIStateVersioned
	siteAPIAssetsCollection
	siteAPIAssetItem
)

// looksLikeSiteFacingAPIPath is parseSiteAPIPath plus the older
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

// parseSiteAPIPath recognises every site-facing API path shape, state and
// assets alike, in both the named form
// (/api/sites/{site}/...) and the nameless convenience form (/api/site/...,
// the site's own host serves exactly one site — see serveSiteHost).
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

// parseAssetServePath recognises the asset-serving path once the
// leading "/" has been stripped off:
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

// serveSiteAPI is the dispatcher once a site's own host has resolved the
// owner and site name: it looks up the site, checks Origin on a non-safe
// method, authenticates (session or X-API-Key, via the same authMiddleware
// every other route uses), checks viewerAllowed (every route) and
// writerAllowed (every route but a plain read), builds the via_site claim,
// and calls the matching SiteAPIHandler method.
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

	// An anonymous read of a network-open site's saved data: no key or
	// token, no valid host session, a GET of state. Writes, asset routes and anything
	// carrying a credential take the normal path below, so an anonymous
	// visitor can read what the page reads and change nothing.
	_, hasBearer := auth.BearerToken(r)
	if safeMethod && (kind == siteAPIState || kind == siteAPIStateVersioned) && r.Header.Get("X-API-Key") == "" && !hasBearer && g.networkOpen != nil {
		if _, _, valid := g.validHostSession(r, label+"."+g.hosts.BaseHost()); !valid {
			open, err := g.networkOpen(r, siteID)
			if err != nil {
				log.Printf("host gate: network access check for site %s: %v", siteID, err)
				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				return
			}
			if open {
				call := siteAPICall{Owner: owner, SiteName: siteName, SiteID: siteID, Restricted: restricted, ActorKind: "anonymous"}
				if kind == siteAPIState {
					g.siteAPI.GetState(w, r, call)
				} else {
					g.siteAPI.GetStateVersioned(w, r, call)
				}
				return
			}
		}
	}

	// WithSiteAPI puts this request in the API-key scope table
	// (auth.SiteAPIPattern): the mux never matched it, so it has no pattern.
	user, _, keyID, ok := g.authenticateSiteAPI(w, r.WithContext(auth.WithSiteAPI(auth.WithExpectedSessionHost(r.Context(), label+"."+g.hosts.BaseHost()))))
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
		ViaSiteObserved: viaSiteObserved(r, label+"."+g.hosts.BaseHost()),
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
// expectedFor, which treats an owner host and a site's own host
// identically). false means the check already wrote the 403.
func (g *hostGate) checkSiteAPIOrigin(w http.ResponseWriter, r *http.Request) bool {
	ok := false
	g.originCheck(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ok = true })).ServeHTTP(w, r)
	return ok
}

// checkSiteAccess applies the view check (viewerAllowed) to every route (a
// site a caller may not even view does not confirm its own existence, so a
// failure here is 404) and, for a route that writes, the write check
// (writerAllowed) on top (a caller who may view but not write gets 403: the
// site's existence is already established by the successful view check).
func (g *hostGate) checkSiteAccess(w http.ResponseWriter, r *http.Request, siteID, userID string, needsWrite bool) bool {
	allowed, err := g.viewerAllowed(r, siteID, userID)
	if err != nil {
		log.Printf("host gate: viewer allowed check for site %s: %v", siteID, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return false
	}
	if !allowed {
		g.recordDenied(r, siteID, userID, "not_a_viewer")
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
		g.recordDenied(r, siteID, userID, "not_a_writer")
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "forbidden"})
		return false
	}
	return true
}

// viaSiteObserved corroborates the via_site claim where the browser allows
// it: Sec-Fetch-Dest must be "empty" (a fetch, not a navigation — a hosted
// page's own script call looks like this; a top-level page load does not),
// and the Referer's host must be this site's own host, the one thing that
// names "this exact site". Most callers send neither header, so false is the
// common case and is not itself a sign of anything wrong.
func viaSiteObserved(r *http.Request, ownHost string) bool {
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
	return normalizeHost(parsed.Host) == normalizeHost(ownHost)
}

// resolveLabelHolder finds the single username whose label is label, among
// the owners of at least one site. It is resolveOwner without the per-site
// filter, for the site host's owner part, which names no site until
// resolveSiteName runs; two accounts sharing a label is a collision
// the registration guard is supposed to prevent, and serving either would
// let one owner's page answer for another's, so that fails closed.
func (g *hostGate) resolveLabelHolder(label string) (string, bool) {
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
		log.Printf("host gate: label %q resolves to %d users %v; refusing to serve any of them", label, len(holders), holders)
		return "", false
	}
}

// resolveSiteName finds the site under owner whose host part
// (siteHostPart) is sitePart. It also requires a current version to exist,
// so a site with no live content is not addressable. Two sites can share a
// part only when both predate v1.3 and their names differ by case or by '.'
// against '-'; the one whose name is exactly the part keeps the address
// whatever the others' state, and otherwise neither is served (fail
// closed, logged).
func (g *hostGate) resolveSiteName(owner, sitePart string) (string, bool) {
	names, err := g.files.store.ListSites(owner)
	if err != nil {
		log.Printf("host gate: list sites for %q: %v", owner, err)
		return "", false
	}
	var matches []string
	for _, name := range names {
		if siteHostPart(name) != sitePart {
			continue
		}
		_, ok, err := g.files.store.CurrentVersion(owner, name)
		if err != nil {
			log.Printf("host gate: owner %q site %q: current version unreadable, refusing to resolve: %v", owner, name, err)
			return "", false
		}
		if !ok {
			continue
		}
		if name == sitePart {
			return name, true
		}
		matches = append(matches, name)
	}
	switch len(matches) {
	case 1:
		return matches[0], true
	case 0:
		return "", false
	default:
		log.Printf("host gate: owner %q site part %q resolves to %d sites %v; refusing to serve any of them", owner, sitePart, len(matches), matches)
		return "", false
	}
}

// resolveOwner finds the username whose label is label and who has a current
// version of sitename, from the site store's index (the database). Whether
// the site is restricted, and whether the caller may view it, are separate
// checks made afterward (SiteForServing, viewerAllowed).
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
func (g *hostGate) resolveOwner(label, sitename string) (string, bool) {
	store := g.files.store
	users, err := store.ListUsers()
	if err != nil {
		log.Printf("host gate: list users for label %q: %v", label, err)
		return "", false
	}
	var holders []string
	for _, user := range users {
		if !g.hosts.OwnsLabel(label, user) {
			continue
		}
		// CurrentVersion reports a missing site as
		// (0, false, nil); any error is something other than "does not exist".
		_, ok, err := store.CurrentVersion(user, sitename)
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
