package handler

import (
	"database/sql"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/sitetype"
	"github.com/vsriram/simple-host/internal/storage"
)

type AdminHandler struct {
	database      *sql.DB
	publicBaseURL string
	// hosts decides which address the dashboard links a site under.
	hosts       HostModel
	cookies     CookiePolicy
	limits      *AbuseLimits
	signingKeys []auth.SigningKey
	sessionIdle time.Duration
	audit       audit.Recorder
	// store sizes the storage ranking. Optional: nil shows no sizes.
	store *storage.Store
	// siteTypes enables the classification backfill endpoint. Optional.
	siteTypes *sitetype.Worker
	// auditReader backs GET /api/admin/export (design.md 8.3). Optional:
	// nil leaves the endpoint returning 503, the same shape siteTypes uses
	// for its own optional endpoint.
	auditReader *audit.Reader
}

// WithAuditReader attaches the reader GET /api/admin/export streams from.
// Optional: nil leaves the endpoint returning 503.
func (h *AdminHandler) WithAuditReader(reader *audit.Reader) *AdminHandler {
	h.auditReader = reader
	return h
}

func NewAdminHandler(
	database *sql.DB,
	publicBaseURL string,
	hosts HostModel,
	cookies CookiePolicy,
	signingKeys []auth.SigningKey,
	sessionIdle time.Duration,
	recorder audit.Recorder,
	limits ...*AbuseLimits,
) *AdminHandler {
	if recorder == nil {
		recorder = audit.NoOp{}
	}
	return &AdminHandler{
		database:      database,
		publicBaseURL: publicBaseURL,
		hosts:         hosts,
		cookies:       cookies,
		signingKeys:   signingKeys,
		sessionIdle:   sessionIdle,
		audit:         recorder,
		limits:        chooseAbuseLimits(limits),
	}
}

// optionalUser resolves the session cookie without requiring one, the same
// way DashboardHandler does (dashboard.go) — the admin dashboard renders a
// sign-in prompt rather than a bare 401 when there is none.
func (h *AdminHandler) optionalUser(r *http.Request) *db.User {
	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	verified, err := auth.VerifyBaseSessionCookie(h.signingKeys, c.Value)
	if err != nil {
		return nil
	}
	withUser, err := db.GetValidSession(r.Context(), h.database, verified.SessionID, h.sessionIdle)
	if err != nil || withUser.Session.UserID != verified.UserID {
		return nil
	}
	user := withUser.User
	return &user
}

// WithStore attaches the site store the storage ranking is measured from.
func (h *AdminHandler) WithStore(store *storage.Store) *AdminHandler {
	h.store = store
	return h
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func localTimeHTML(t time.Time, format string) string {
	utc := t.UTC()
	fallback := "2006-01-02 15:04 UTC"
	if format == "date" {
		fallback = "2006-01-02"
	}
	return fmt.Sprintf(`<time data-local-time="%s" datetime="%s">%s</time>`,
		html.EscapeString(format),
		html.EscapeString(utc.Format(time.RFC3339)),
		html.EscapeString(utc.Format(fallback)),
	)
}

// Register wires admin routes. The browser login flow (GET /admin, POST
// /admin/login, POST /admin/logout) reads the admin_session cookie inline
// — it deliberately doesn't go through authMiddleware because the
// unauth case is "render the login form," not 401.
//
// The /api/admin/* endpoints DO go through authMiddleware + RequireAdmin
// so they're protected the same way for both header-auth (skill/curl)
// and cookie-auth (browser submitting forms from /admin).
func (h *AdminHandler) Register(mux *http.ServeMux, authMiddleware, skillVersionMiddleware func(http.Handler) http.Handler) {
	// GET /admin reads the session cookie itself (h.optionalUser) and renders
	// a sign-in prompt, a "not an admin" page, or the dashboard — the same
	// three-way branch the site listing page uses for viewer roles. It does
	// not go through authMiddleware because a signed-out visitor must see a
	// sign-in prompt, not a bare 401.
	mux.HandleFunc("GET /admin", h.dashboard)

	// Every one of these is submitted by a form on the dashboard, so a refusal
	// renders a page. The default JSON body would be shown to the admin as raw
	// text with no way back.
	dashboardCheck := originCheckMiddleware(h.hosts, h.publicBaseURL, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin?action_error=blocked", http.StatusSeeOther)
	})

	// Admin API endpoints — auth, admin authorization, then the client-version
	// compatibility guard. A browser session only: an API key is for CI, and
	// a leaked one must not carry an admin's powers with it.
	adminAPI := func(next http.Handler) http.Handler {
		return h.limitAdminClient(
			authMiddleware(requireSessionAuth(h.requireAdmin(skillVersionMiddleware(h.limitAdminIdentity(next))))),
		)
	}
	mux.Handle("POST /api/admin/users/{username}/disable", dashboardCheck(adminAPI(http.HandlerFunc(h.disableUser))))
	mux.Handle("POST /api/admin/users/{username}/enable", dashboardCheck(adminAPI(http.HandlerFunc(h.enableUser))))
	mux.Handle("POST /api/admin/classify-sites", dashboardCheck(adminAPI(http.HandlerFunc(h.classifySites))))
	mux.Handle("GET /api/admin/export", adminAPI(http.HandlerFunc(h.exportAuditOrAccess)))
	h.registerAccessRequestRoutes(mux, adminAPI, dashboardCheck)
}

// requireAdmin is auth.RequireAdmin plus an access_denied audit row when a
// signed-in non-admin is refused.
func (h *AdminHandler) requireAdmin(next http.Handler) http.Handler {
	admin := auth.RequireAdmin(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user := auth.GetUser(r.Context()); user != nil && !user.IsAdmin {
			h.audit.Record(r.Context(), audit.Event{
				ActorID: user.ID, Action: "access_denied",
				Detail: r.Method + " " + r.URL.Path, Extra: map[string]any{"reason": "not_an_admin"},
			})
		}
		admin.ServeHTTP(w, r)
	})
}

func (h *AdminHandler) limitAdminClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decision := h.limits.allow(adminClientPolicy, clientLimitKey(r)); !decision.Allowed {
			writeRateLimit(w, decision)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *AdminHandler) limitAdminIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := auth.GetUser(r.Context())
		if user == nil {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
			return
		}
		if decision := h.limits.allow(adminIdentityPolicy, user.ID); !decision.Allowed {
			writeRateLimit(w, decision)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// dashboard renders the login form if the admin_session cookie is missing
// or stale; renders the full dashboard if the cookie is valid.
func (h *AdminHandler) dashboard(w http.ResponseWriter, r *http.Request) {
	user := h.optionalUser(r)
	if user == nil {
		writeAdminSignInPrompt(w, "")
		return
	}
	if !user.IsAdmin {
		writeAdminSignInPrompt(w, user.Username)
		return
	}
	analyticsDays := analyticsDaysForRequest(r, true)
	usersMetric := parseRankMetric(r.URL.Query().Get("users"), userMetrics)
	sitesMetric := parseRankMetric(r.URL.Query().Get("sites"), siteMetrics)
	hosts := h.hosts

	users, err := db.ListAllUsers(r.Context(), h.database)
	if err != nil {
		http.Error(w, "failed to load users", http.StatusInternalServerError)
		return
	}
	sites, err := db.ListAllSites(r.Context(), h.database)
	if err != nil {
		http.Error(w, "failed to load sites", http.StatusInternalServerError)
		return
	}
	// design.md 5.2a: a restricted site's own address is
	// "<owner>--<site>.<base>", not "<owner>.<base>/<site>/"; every link this
	// admin page renders for a site needs to know which one applies.
	restrictedSiteIDs, err := db.ListRestrictedSiteIDs(r.Context(), h.database)
	if err != nil {
		log.Printf("admin restricted sites: %v", err)
		restrictedSiteIDs = map[string]bool{}
	}
	analyticsBySiteID, err := db.ListSiteAnalyticsSummaries(r.Context(), h.database, analyticsDays)
	if err != nil {
		log.Printf("admin analytics: %v", err)
		analyticsBySiteID = make(map[string]db.SiteAnalyticsSummary)
	}
	// Ranking inputs. Each is optional: a failure degrades one metric's numbers
	// to zero rather than taking the whole dashboard down.
	usageBySiteID := measureSiteStorage(r.Context(), h.store)

	sitesByUser := make(map[string][]db.Site, len(users))
	for _, s := range sites {
		sitesByUser[s.UserID] = append(sitesByUser[s.UserID], s)
	}

	var todayPageviews, todayVisits, rangePageviews, rangeVisits, rangeBotViews int64
	for _, summary := range analyticsBySiteID {
		todayPageviews += summary.TodayPageviews
		todayVisits += summary.TodayVisits
		rangePageviews += summary.Last7Pageviews
		rangeVisits += summary.Last7Visits
		rangeBotViews += summary.BotPageviews
	}

	sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })

	// Per-user aggregates for the overview sections + selected-range sort data.
	// All derived from data already loaded above — no extra queries.
	usernameByID := make(map[string]string, len(users))
	for _, u := range users {
		usernameByID[u.ID] = u.Username
	}

	type userStat struct {
		username   string
		siteCount  int
		viewsRange int64
		latest     time.Time
	}
	statByUsername := make(map[string]userStat, len(users))
	userRanks := make([]userRank, 0, len(users))
	siteRanks := make([]siteRank, 0, len(sites))
	for _, u := range users {
		st := userStat{username: u.Username, siteCount: len(sitesByUser[u.ID])}
		rank := userRank{
			username:  u.Username,
			siteCount: len(sitesByUser[u.ID]),
		}
		for _, s := range sitesByUser[u.ID] {
			st.viewsRange += analyticsBySiteID[s.ID].Last7Pageviews
			if s.UpdatedAt.After(st.latest) {
				st.latest = s.UpdatedAt
			}

			usage := usageBySiteID.site(s.ID, s.ActiveVersion)
			rank.views += analyticsBySiteID[s.ID].Last7Pageviews
			rank.totalBytes += usage.totalBytes
			rank.liveBytes += usage.liveBytes

			siteRanks = append(siteRanks, siteRank{
				name:       s.Name,
				owner:      u.Username,
				views:      analyticsBySiteID[s.ID].Last7Pageviews,
				totalBytes: usage.totalBytes,
				liveBytes:  usage.liveBytes,
				updatedAt:  s.UpdatedAt,
			})
		}
		statByUsername[u.Username] = st
		userRanks = append(userRanks, rank)
	}
	sortUserRanks(userRanks, usersMetric)
	sortSiteRanks(siteRanks, sitesMetric)

	// Every site, flat. Ordered newest-created first because that is the
	// default view; the "recently updated sites" mode re-sorts these same rows
	// client-side off the epoch attributes each row carries, so the server
	// renders one list rather than two copies of every site.
	flatSites := make([]db.Site, len(sites))
	copy(flatSites, sites)
	sort.Slice(flatSites, func(i, j int) bool {
		return flatSites[i].CreatedAt.After(flatSites[j].CreatedAt)
	})

	// Newest registrations. There is no separate "recently updated" card any
	// more: the "Updated" tab on the Top sites card ranks the whole set.
	newestUsers := make([]newUser, 0, len(users))
	for _, u := range users {
		newestUsers = append(newestUsers, newUser{
			username:  u.Username,
			joined:    u.CreatedAt,
			siteCount: len(sitesByUser[u.ID]),
			disabled:  u.DisabledAt != nil,
			team:      u.IsTeam(),
		})
	}
	sort.Slice(newestUsers, func(i, j int) bool {
		if !newestUsers[i].joined.Equal(newestUsers[j].joined) {
			return newestUsers[i].joined.After(newestUsers[j].joined)
		}
		return newestUsers[i].username < newestUsers[j].username
	})

	// State-backend usage across all sites. Recorded on first read/write of
	// each variant since the feature deployed — not retroactive.
	var stateSites []db.Site
	var versionedCount, simpleCount int
	for _, s := range sites {
		if s.UsesVersionedState || s.UsesState {
			stateSites = append(stateSites, s)
		}
		if s.UsesVersionedState {
			versionedCount++
		}
		if s.UsesState {
			simpleCount++
		}
	}
	sort.Slice(stateSites, func(i, j int) bool {
		return stateSites[i].UpdatedAt.After(stateSites[j].UpdatedAt)
	})

	var b strings.Builder
	b.WriteString(adminHeadHTML)
	fmt.Fprintf(&b, `<header class="bar">
  <div class="mast">Simple Host<span class="dot">.</span> <span class="kicker">admin</span></div>
  <div class="stats"><b>%d</b> %s <span class="sep" aria-hidden="true"></span> <b>%d</b> %s <span class="sep" aria-hidden="true"></span> <b>%s</b> views today <span class="sep" aria-hidden="true"></span> <b>%s</b> visits today <span class="sep" aria-hidden="true"></span> <b>%s</b> views %s <span class="sep" aria-hidden="true"></span> <b>%s</b> visits %s <span class="sep" aria-hidden="true"></span> <b>%s</b> agent views %s%s</div>
  <nav class="dash-nav"><a href="/dashboard">My keys</a> <span class="sep" aria-hidden="true"></span> <a href="#admin-activity">Activity &amp; visitors</a></nav>
  <form method="POST" action="/auth/logout" class="logout-form"><button type="submit" class="btn-logout">Sign out</button></form>
</header>
<main>%s`,
		len(users), pluralize(len(users), "user", "users"),
		len(sites), pluralize(len(sites), "site", "sites"),
		formatCount(todayPageviews),
		formatCount(todayVisits),
		formatCount(rangePageviews),
		analyticsRangeCompact(analyticsDays),
		formatCount(rangeVisits),
		analyticsRangeCompact(analyticsDays),
		formatCount(rangeBotViews),
		analyticsRangeCompact(analyticsDays),
		storageStatsHTML(usageBySiteID),
		adminActionNoticeHTML(r),
	)
	renderAnalyticsRangeSelectorWith(&b, "/admin", analyticsDays, map[string]string{
		"users": string(usersMetric),
		"sites": string(sitesMetric),
	})

	h.renderAccessRequests(r, &b)

	if len(users) == 0 {
		b.WriteString(`<div class="empty">No users yet.</div>`)
	}

	// Overview: top users by selected-range views and most recently updated sites.
	if len(users) > 0 {
		b.WriteString(`<section class="overview">`)

		renderUserRankingCard(&b, hosts, userRanks, usersMetric, sitesMetric, analyticsDays)
		renderSiteRankingCard(&b, hosts, siteRanks, sitesMetric, usersMetric, analyticsDays)

		renderNewUsersCard(&b, hosts, newestUsers, len(users))

		// State backend usage. Full width and outside the ranking grid: this is
		// the one card that lists every matching site rather than a top ten, so
		// it needs the room to lay them out in columns instead of hiding all but
		// six behind a scrollbar.
		b.WriteString(`</section><section class="overview">`)
		fmt.Fprintf(&b, `<div class="overview-card"><h2 class="section-title">State backend <span class="card-count">%d versioned · %d simple</span></h2><div class="rank-list state-rank-list" role="region" aria-label="State backend sites">`,
			versionedCount, simpleCount)
		if len(stateSites) == 0 {
			b.WriteString(`<div class="rank-empty">No sites have used the state backend yet.</div>`)
		}
		for _, s := range stateSites {
			owner := usernameByID[s.UserID]
			publicPath := hosts.SiteURL(owner, s.Name, restrictedSiteIDs[s.ID])
			fmt.Fprintf(&b, `<div class="rank-row">
  <span class="rank-name"><a href="%s" target="_blank" rel="noopener">%s</a> <span class="rank-sub">%s</span></span>
  <span class="rank-metric">%s</span>
</div>`,
				html.EscapeString(publicPath),
				html.EscapeString(s.Name),
				html.EscapeString(owner),
				stateBackendHTML(s),
			)
		}
		b.WriteString(`</div></div>`)

		b.WriteString(`</section>`)

		// Every owner's audit trail and access log (design.md 8.3), plus the
		// export button — the admin-wide equivalent of the per-site
		// Activity/Visitors tabs the dashboard's own site panel renders
		// (dashboard.go). Fetch-driven against the same routes, admin-scoped
		// (no owner filter, so this sees every namespace) rather than
		// server-rendered, so it stays cheap on a dashboard load that
		// already assembles a lot of HTML above.
		fmt.Fprintf(&b, `<section id="admin-activity" class="overview">
  <div class="overview-card"><h2 class="section-title">Activity <span class="card-count" id="admin-activity-count"></span></h2>
    <p class="login-copy">Every audited action, across every owner. <a href="/api/admin/export?kind=audit&amp;format=csv" download>Export CSV</a> · <a href="/api/admin/export?kind=audit&amp;format=jsonl" download>Export JSONL</a></p>
    <div id="admin-activity-list" class="rank-list" role="region" aria-label="Audit log"></div>
  </div>
  <div class="overview-card"><h2 class="section-title">Visitors <span class="card-count" id="admin-visitor-count"></span></h2>
    <p class="login-copy">Every recorded view, across every owner. <a href="/api/admin/export?kind=access&amp;format=csv" download>Export CSV</a> · <a href="/api/admin/export?kind=access&amp;format=jsonl" download>Export JSONL</a></p>
    <div id="admin-visitor-list" class="rank-list" role="region" aria-label="Access log"></div>
  </div>
</section>`)

		// Filter + sort toolbar. Both operate client-side on the user blocks below.
		fmt.Fprintf(&b, `<div class="toolbar">
  <input type="search" id="filter" class="filter-input" placeholder="Filter users or sites…" autocomplete="off">
  <label class="sort-label">Sort
    <select id="sort" class="sort-select">
      <option value="recent-sites" selected>Recently created sites</option>
      <option value="latest-sites">Recently updated sites</option>
      <option value="username">Username A–Z</option>
      <option value="sitecount">Most sites</option>
      <option value="views">Most views (%s)</option>
      <option value="latest">Recently updated</option>
    </select>
  </label>
</div>
<div id="userlist" hidden>`, analyticsRangeCompact(analyticsDays))
	}

	for _, u := range users {
		userSites := sitesByUser[u.ID]
		st := statByUsername[u.Username]
		// Searchable haystack: username + all site names, lowercased.
		haystack := strings.ToLower(u.Username)
		for _, s := range userSites {
			haystack += " " + strings.ToLower(s.Name)
		}
		var latestUnix int64
		if !st.latest.IsZero() {
			latestUnix = st.latest.Unix()
		}
		writeUserBlockHeader(&b, hosts, u, len(userSites), haystack, st.viewsRange, latestUnix)

		if len(userSites) == 0 {
			b.WriteString(`<div class="empty-user">No sites yet.</div></section>`)
			continue
		}

		sort.Slice(userSites, func(i, j int) bool { return userSites[i].Name < userSites[j].Name })

		b.WriteString(`<div class="sites">`)
		for _, s := range userSites {
			writeSiteRow(&b, hosts, s, u.Username, analyticsBySiteID[s.ID], analyticsDays, false, restrictedSiteIDs[s.ID])
		}
		b.WriteString(`</div></section>`)
	}

	if len(users) > 0 {
		b.WriteString(`</div>`) // #userlist

		// The same sites again, flat. Rendered rather than re-sorted from the
		// grouped list, because a site-first order cuts across the user blocks
		// the other sorts operate on. Visible at load: this is the default.
		b.WriteString(`<div id="sitelist" class="sites flat-sites">`)
		for _, s := range flatSites {
			writeSiteRow(&b, hosts, s, usernameByID[s.UserID], analyticsBySiteID[s.ID], analyticsDays, true, restrictedSiteIDs[s.ID])
		}
		b.WriteString(`</div>`)
	}

	b.WriteString(`</main>`)
	b.WriteString(adminListScript)
	b.WriteString(adminActivityScript)
	b.WriteString(`</body></html>`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// writeUserBlockHeader opens one user block: the section the filter and the
// sort boxes read, the name, and the header actions beside it.
//
// The person and the team differ here and nowhere else in the list. A team is
// a namespace rather than somebody: it is labelled as one, carries how many
// people are in it, and offers no offboarding control — there is no sign-in
// to disable.
func writeUserBlockHeader(b *strings.Builder, hosts HostModel, u db.User, siteCount int, haystack string, viewsRange, latestUnix int64) {
	nameChips := ""
	actions := fmt.Sprintf(`<a class="btn-view" href="%s" target="_blank" rel="noopener">View details</a>`,
		html.EscapeString(hosts.OwnerPageURL(u.Username)))
	if u.IsTeam() {
		nameChips = fmt.Sprintf(` <span class="chip">team</span> <span class="chip chip-muted">%s</span>`,
			html.EscapeString(pluralize(u.MemberCount, "1 member", fmt.Sprintf("%d members", u.MemberCount))))
	} else if u.DisabledAt != nil {
		nameChips = ` <span class="chip chip-warn">disabled</span>`
		actions += fmt.Sprintf(`<form method="POST" action="/api/admin/users/%s/enable" onsubmit="return confirm('Re-enable %s? They will be able to sign in again.');"><button type="submit" class="btn-reset">Enable</button></form>`,
			html.EscapeString(u.Username),
			html.EscapeString(u.Username),
		)
	} else {
		actions += fmt.Sprintf(`<form method="POST" action="/api/admin/users/%s/disable" onsubmit="return confirm('Disable %s? Their sessions and API keys are revoked immediately and they cannot sign in again until re-enabled. Their sites keep serving.');"><button type="submit" class="btn-reset">Disable</button></form>`,
			html.EscapeString(u.Username),
			html.EscapeString(u.Username),
		)
	}
	fmt.Fprintf(b, `<section class="user-block" data-search="%s" data-username="%s" data-sitecount="%d" data-views="%d" data-latest="%d">
  <div class="user-header">
    <div>
      <h2 class="username display">%s%s</h2>
      <div class="user-meta">Joined %s · %s</div>
    </div>
    <div class="user-header-actions">%s</div>
  </div>`,
		html.EscapeString(haystack),
		html.EscapeString(u.Username),
		siteCount,
		viewsRange,
		latestUnix,
		html.EscapeString(u.Username),
		nameChips,
		localTimeHTML(u.CreatedAt, "date"),
		pluralize(siteCount, "1 site", fmt.Sprintf("%d sites", siteCount)),
		actions,
	)
}

// writeSiteRow emits one row of a site list. The grouped list under each user
// leaves the owner off — the heading above it already says who that is — while
// the flat list shows it, and carries the haystack the filter box searches.
func writeSiteRow(b *strings.Builder, hosts HostModel, site db.Site, owner string, summary db.SiteAnalyticsSummary, analyticsDays int, showOwner, restricted bool) {
	visibility := fmt.Sprintf(`<span class="chip chip-muted">%s</span>`, html.EscapeString(accessLevelLabel(site.Access)))
	if site.Access == db.AccessListed || site.Access == db.AccessNetwork {
		visibility = fmt.Sprintf(`<span class="chip">%s</span>`, html.EscapeString(accessLevelLabel(site.Access)))
	}
	attrs := ""
	ownerLine := ""
	timeCell := localTimeHTML(site.UpdatedAt, "date")
	if showOwner {
		attrs = fmt.Sprintf(` data-search="%s" data-created="%d" data-updated="%d"`,
			html.EscapeString(strings.ToLower(owner+" "+site.Name)),
			site.CreatedAt.Unix(), site.UpdatedAt.Unix())
		ownerLine = fmt.Sprintf(`<span class="site-owner">%s</span>`, html.EscapeString(owner))
		// The flat list sorts by creation or by update depending on the mode.
		// Both dates ship and CSS picks one: a list ordered by creation but
		// dated by last update reads as though the sort is broken.
		timeCell = fmt.Sprintf(`<span class="t-created">%s</span><span class="t-updated">%s</span>`,
			localTimeHTML(site.CreatedAt, "date"), localTimeHTML(site.UpdatedAt, "date"))
	}
	fmt.Fprintf(b, `<div class="site"%s>
  <div class="site-name"><a href="%s" target="_blank" rel="noopener">%s</a>%s</div>
  <div class="site-version"><span class="chip">v%d</span></div>
  <div class="site-visibility">%s</div>
  <div class="site-backend">%s</div>
  <div class="site-traffic"><b>%s</b> views / <b>%s</b> visits today <span>%s views %s</span></div>
  <div class="site-time">%s</div>
	</div>`,
		attrs,
		html.EscapeString(hosts.SiteURL(owner, site.Name, restricted)),
		html.EscapeString(site.Name),
		ownerLine,
		site.ActiveVersion,
		visibility,
		stateBackendHTML(site),
		formatCount(summary.TodayPageviews),
		formatCount(summary.TodayVisits),
		formatCount(summary.Last7Pageviews),
		analyticsRangeCompact(analyticsDays),
		timeCell,
	)
}

// disableUser is the offboarding action (design.md 6.4): sessions and keys
// revoked in the same transaction as the flag, sign-in refused from then on,
// sites left untouched.
func (h *AdminHandler) disableUser(w http.ResponseWriter, r *http.Request) {
	h.setUserDisabled(w, r, true, "admin_disable_user")
}

func (h *AdminHandler) enableUser(w http.ResponseWriter, r *http.Request) {
	h.setUserDisabled(w, r, false, "admin_enable_user")
}

func (h *AdminHandler) setUserDisabled(w http.ResponseWriter, r *http.Request, disabled bool, action string) {
	username := r.PathValue("username")
	target, err := db.GetUserByUsername(r.Context(), h.database, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			h.respondAdmin(w, r, http.StatusNotFound, "user not found")
			return
		}
		log.Printf("admin: resolve user %q: %v", username, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	if err := db.SetUserDisabled(r.Context(), h.database, target.ID, disabled); err != nil {
		if errors.Is(err, db.ErrLastAdmin) {
			h.respondAdmin(w, r, http.StatusConflict, "cannot disable the last enabled admin")
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			h.respondAdmin(w, r, http.StatusNotFound, "user not found or is a team")
			return
		}
		log.Printf("admin: %s %q: %v", action, username, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	actor := auth.GetUser(r.Context())
	actorID := ""
	if actor != nil {
		actorID = actor.ID
	}
	actorKind, keyID := auditActorKind(r.Context())
	h.audit.Record(r.Context(), audit.Event{
		ActorID: actorID, ActorKind: actorKind, KeyID: keyID,
		Action: action, SubjectID: target.ID, Detail: username,
		RequestID: auditRequestID(r.Context()),
	})
	verb := "disabled"
	if !disabled {
		verb = "enabled"
	}
	h.respondAdmin(w, r, http.StatusOK, "user "+verb)
}

func (h *AdminHandler) respondAdmin(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		if status >= 400 {
			http.Error(w, msg, status)
			return
		}
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	writeJSON(w, status, map[string]string{"status": msg})
}

// adminActionNoticeHTML renders a banner when a dashboard action was refused
// by the origin check, so the admin sees why nothing happened instead of a
// button that silently did nothing.
func adminActionNoticeHTML(r *http.Request) string {
	if r.URL.Query().Get("action_error") != "blocked" {
		return ""
	}
	return `
<section class="roadmap-block"><span class="roadmap-tag">Blocked</span>
  <span class="roadmap-text">That action was refused because the request didn't arrive from this dashboard. Nothing changed. Open /admin directly rather than through a link or frame, then try again.</span>
</section>`
}

// writeAdminSignInPrompt renders the page shown instead of the dashboard
// when the visitor is signed out (username == "") or signed in but not an
// admin. There is no key-paste form any more: identity is OIDC sign-in
// (design.md 6.1, 6.2), so the only action here is a link to it.
func writeAdminSignInPrompt(w http.ResponseWriter, username string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	body := adminHeadHTML + `<header class="bar"><div class="mast">Simple Host<span class="dot">.</span> <span class="kicker">admin</span></div></header>
<main>
<section class="login-block">
  <h2 class="section-title">Admin access is restricted</h2>`
	if username == "" {
		body += `<p class="login-copy">Sign in with an admin account to see this page.</p>
  <p><a class="btn-login" href="/auth/login?to=%2Fadmin">Sign in</a></p>`
	} else {
		body += `<p class="login-copy">Signed in as ` + html.EscapeString(username) + `, which is not an admin account. Ask a platform admin to add your email to ADMIN_EMAILS, or sign in with a different account.</p>
  <p><a class="btn-login" href="/dashboard">Go to your dashboard</a></p>`
	}
	body += `</section>
</main></body></html>`
	_, _ = w.Write([]byte(body))
}

func pluralize(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func storageStatsHTML(usage siteStorage) string {
	if usage.bySite == nil {
		return ""
	}
	return fmt.Sprintf(` <span class="sep" aria-hidden="true"></span> <b>%s</b> stored`, formatBytes(usage.totalBytes))
}

// stateBackendHTML renders chips showing which state-backend variant(s) a site
// has used. A site can use both (versioned + simple); usage is recorded on
// first read/write of each variant, so this reflects real traffic since the
// feature deployed — not a retroactive guess. Sites that have never touched
// the backend render nothing.
func stateBackendHTML(s db.Site) string {
	var chips string
	if s.UsesVersionedState {
		chips += `<span class="chip">versioned</span>`
	}
	if s.UsesState {
		chips += `<span class="chip chip-muted">simple</span>`
	}
	return chips
}

const adminHeadHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Simple Host · Admin</title>
<link rel="stylesheet" href="/ps-blue.css">
<style>
  /* Page-specific layout overrides only — tokens/components live in ps-blue.css. */
  body { min-height: 100vh; }
  header.bar{
    position:sticky;top:0;z-index:10;
    display:flex;align-items:baseline;justify-content:space-between;
    padding:24px 56px 16px;
    border-bottom:1px solid var(--surface-line);
    gap:32px;
    background:var(--canvas);
  }
  .mast{
    font-family:var(--font-sans);font-weight:700;
    font-size:26px;color:var(--ink);line-height:1;letter-spacing:-0.03em;
  }
  .mast .dot{color:var(--ps-blue-800);}
  .mast .kicker{
    font-family:var(--font-mono);font-weight:600;font-size:12px;
    color:var(--ink-muted);letter-spacing:0.18em;text-transform:uppercase;
    margin-left:10px;
  }
  .stats{
    font-family:var(--font-mono);font-weight:500;
    font-size:13px;color:var(--ink-muted);letter-spacing:0.04em;
    font-variant-numeric:tabular-nums;
    flex:1;
  }
  .stats b{color:var(--ink);font-weight:600;font-variant-numeric:tabular-nums}
  .stats .sep{
    display:inline-block;width:10px;height:10px;margin:0 12px;
    border-radius:3px;background:var(--ps-blue-800);transform:rotate(45deg);
    vertical-align:middle;
  }
  .logout-form{margin:0}
  .btn-logout{
    font-family:var(--font-mono);font-weight:600;font-size:11px;
    letter-spacing:0.12em;text-transform:uppercase;
    padding:6px 12px;border-radius:6px;
    background:transparent;color:var(--ink-muted);
    border:1px solid var(--surface-line);cursor:pointer;
  }
  .btn-logout:hover{color:var(--ink);border-color:var(--ink-muted)}
  /* 960px is a reading width. This page's job is comparison — two rankings
     side by side, and per-user site tables — so it gets a dashboard width
     instead. Below 1280px it simply uses what is available. */
  main{
    padding:32px 56px 64px;
    display:grid;gap:36px;
    max-width:1280px;margin:0 auto;
  }
  .section-title{
    font-family:var(--font-sans);font-weight:700;font-size:18px;
    color:var(--ink);margin:0 0 16px;letter-spacing:-0.01em;
  }
  /* 420px is the width at which a ranking row fits a name, a value and a full
     tab strip without wrapping. Below that the cards stack instead. */
  /* 340px, not 420px: with three ranking cards a 420px floor leaves the third
     one alone on its own row next to an empty half. */
  .overview{display:grid;grid-template-columns:repeat(auto-fit,minmax(340px,1fr));gap:24px}
  .overview-card{
    border:1px solid var(--surface-line);border-radius:10px;
    padding:20px 24px;background:var(--canvas);
  }
  .overview-card .section-title{margin-bottom:12px}
  .card-count{
    font-family:var(--font-mono);font-weight:500;font-size:11px;
    color:var(--ink-muted);letter-spacing:0.04em;margin-left:8px;
    font-variant-numeric:tabular-nums;
  }
  .rank-empty{
    font-family:var(--font-mono);font-size:12px;color:var(--ink-muted);
    letter-spacing:0.04em;padding:7px 0;
  }
  /* Metric tabs. Plain links, so re-sorting works without JavaScript and each
     view is a URL you can bookmark or paste to someone. */
  /* One line always. Wrapping made the two cards' headers different heights,
     so their rows stopped lining up with each other. */
  .rank-tabs{
    display:flex;gap:4px;margin:0 0 12px;
    border-bottom:1px solid var(--surface-line);padding-bottom:10px;
    overflow-x:auto;scrollbar-width:thin;
  }
  .rank-tab{
    font-family:var(--font-mono);font-size:11px;font-weight:600;
    letter-spacing:0.06em;text-transform:uppercase;text-decoration:none;
    color:var(--ink-muted);padding:4px 9px;border-radius:6px;
    border:1px solid transparent;white-space:nowrap;
  }
  .rank-tab:hover{color:var(--ink);background:var(--surface-line)}
  .rank-tab.is-active{
    color:var(--ps-blue-800);border-color:var(--surface-line);
    background:var(--canvas-raised,transparent);
  }
  .reissue-block{border-left:3px solid var(--surface-line)}
  .btn-dismiss{
    font-family:var(--font-mono);font-size:11px;font-weight:600;letter-spacing:0.1em;
    text-transform:uppercase;padding:7px 14px;border-radius:6px;cursor:pointer;
    background:transparent;color:var(--ink-muted);border:1px solid var(--surface-line);
  }
  .btn-dismiss:hover{color:var(--ink);border-color:var(--ink-muted)}
  .rank-metric .chip{font-size:10px;letter-spacing:0.1em;padding:2px 7px}
  .rank-metric .chip+.chip{margin-left:4px}
  .rank-list{display:grid;gap:2px}
  /* Every state site is listed, so the list flows into as many columns as the
     card is wide. It used to be a 280px scroll box, which showed six of thirty
     rows and hid the rest behind a scrollbar nobody found. */
  .state-rank-list{
    grid-template-columns:repeat(auto-fill,minmax(340px,1fr));
    column-gap:28px;
  }
  /* Uniform borders: with more than one column, "first child" is only the top
     of the first column, which would leave the other columns starting flush. */
  .state-rank-list .rank-row:first-child{border-top:1px solid var(--surface-line)}
  /* Grid, not flex: the value needs its own column so numbers line up down the
     card. minmax(0,1fr) is what lets the name column shrink and ellipsize —
     a plain 1fr refuses to go below its content and overflows the card. */
  .rank-row{
    display:grid;grid-template-columns:minmax(0,1fr) auto;
    align-items:baseline;column-gap:16px;
    padding:9px 0;border-top:1px solid var(--surface-line);
  }
  .rank-row--ranked{grid-template-columns:auto minmax(0,1fr) auto}
  .rank-row:first-child{border-top:none}
  .rank-num{
    font-family:var(--font-mono);font-weight:700;font-size:12px;
    color:var(--ink-muted);min-width:18px;font-variant-numeric:tabular-nums;
  }
  /* Identity column: name on one line, context beneath it. */
  .rank-id{min-width:0;display:flex;flex-direction:column;gap:2px}
  .rank-name{
    font-family:var(--font-sans);font-size:15px;font-weight:600;
    color:var(--ink);letter-spacing:-0.005em;
    min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;
  }
  .rank-id .rank-sub{
    overflow:hidden;text-overflow:ellipsis;white-space:nowrap;
  }
  /* The single number this row is ranked by. Never wraps, never shrinks. */
  .rank-value{
    font-family:var(--font-mono);font-weight:600;font-size:13px;
    color:var(--ink);letter-spacing:0.02em;
    font-variant-numeric:tabular-nums;white-space:nowrap;text-align:right;
  }
  .rank-value .rank-unit{font-weight:500;color:var(--ink-muted);font-size:11px;margin-left:3px}
  .rank-name a{color:var(--ink);text-decoration:none}
  .rank-name a:hover{color:var(--ps-blue-800)}
  .rank-sub{font-family:var(--font-mono);font-weight:500;font-size:11px;color:var(--ink-muted);letter-spacing:0.04em}
  .rank-metric{
    font-family:var(--font-mono);font-weight:500;font-size:12px;
    color:var(--ink-muted);letter-spacing:0.04em;
    font-variant-numeric:tabular-nums;white-space:nowrap;text-align:right;
  }
  .rank-metric b{color:var(--ink);font-weight:600}
  .toolbar{
    display:flex;align-items:center;gap:16px;flex-wrap:wrap;
    margin-bottom:-8px;
  }
  .analytics-range{
    display:flex;align-items:center;gap:10px;justify-self:end;
    font-family:var(--font-mono);font-weight:600;font-size:11px;
    letter-spacing:0.12em;text-transform:uppercase;color:var(--ink-muted);
  }
  .analytics-range select,.analytics-range button{
    padding:8px 12px;border-radius:8px;border:1px solid var(--surface-line);
    background:var(--canvas);font:inherit;color:var(--ink);cursor:pointer;
  }
  .analytics-range select:focus,.analytics-range button:focus{
    outline:none;border-color:var(--ps-blue-800);box-shadow:0 0 0 3px rgba(0,48,135,0.15);
  }
  .analytics-range button{color:var(--ps-blue-800)}
  .filter-input{
    flex:1;min-width:220px;padding:10px 14px;border-radius:8px;
    border:1px solid var(--surface-line);background:var(--canvas);
    font-family:var(--font-mono);font-size:14px;color:var(--ink);
  }
  .filter-input:focus{outline:none;border-color:var(--ps-blue-800);box-shadow:0 0 0 3px rgba(0,48,135,0.15)}
  .sort-label{
    font-family:var(--font-mono);font-weight:600;font-size:11px;
    letter-spacing:0.12em;text-transform:uppercase;color:var(--ink-muted);
    display:flex;align-items:center;gap:8px;
  }
  .sort-select{
    padding:8px 12px;border-radius:8px;border:1px solid var(--surface-line);
    background:var(--canvas);font-family:var(--font-mono);font-size:13px;
    color:var(--ink);cursor:pointer;text-transform:none;letter-spacing:0;
  }
  .sort-select:focus{outline:none;border-color:var(--ps-blue-800)}
  .roadmap-block{
    display:flex;align-items:flex-start;gap:14px;
    padding:14px 18px;
    border:1px solid #ffd9a8;background:#fff5e6;
    border-radius:10px;margin-top:-12px;
  }
  .roadmap-tag{
    flex-shrink:0;
    font-family:var(--font-mono);font-weight:700;font-size:10px;
    letter-spacing:0.18em;text-transform:uppercase;color:#a84300;
    background:#ffe0b8;padding:5px 10px;border-radius:5px;line-height:1;
    margin-top:2px;
  }
  .roadmap-text{
    font-family:var(--font-sans);font-size:13px;line-height:1.55;
    color:#5d2c00;
  }
  .pending-block{
    border:1px solid var(--ps-blue-200);background:var(--ps-blue-50,#eff3ff);
    border-radius:10px;padding:20px 24px;
  }
  .pending-list{display:grid;gap:12px}
  .pending-row{
    display:flex;align-items:center;justify-content:space-between;gap:16px;
    padding:10px 14px;background:var(--canvas);border-radius:8px;
  }
  .pending-meta{display:flex;align-items:center;gap:14px;flex-wrap:wrap;font-family:var(--font-mono);font-size:13px}
  .pending-username{font-weight:700;color:var(--ink)}
  .pending-email{color:var(--ink-muted)}
  .pending-age{color:var(--ink-muted);font-size:12px}
  .pending-actions{display:flex;gap:8px}
  .pending-actions form{margin:0}
  .btn-approve,.btn-reject,.btn-reset,.btn-login{
    font-family:var(--font-mono);font-weight:600;font-size:11px;
    letter-spacing:0.12em;text-transform:uppercase;
    padding:7px 14px;border-radius:6px;cursor:pointer;border:1px solid;
  }
  .btn-approve{background:var(--ps-blue-800);color:white;border-color:var(--ps-blue-800)}
  .btn-approve:hover{background:var(--ps-blue-900)}
  .btn-reject{background:transparent;color:var(--ink-muted);border-color:var(--surface-line)}
  .btn-reject:hover{color:var(--ink);border-color:var(--ink-muted)}
  .user-header-actions{display:flex;gap:8px;align-items:center}
  .btn-view{padding:6px 12px;border:1px solid var(--line,#d8dee9);border-radius:6px;background:transparent;color:inherit;font:500 12px/1 Inter,system-ui,sans-serif;text-decoration:none;white-space:nowrap}
  .btn-view:hover{border-color:var(--accent,#2563eb);color:var(--accent,#2563eb)}
  .btn-reset{background:transparent;color:var(--ink-muted);border-color:var(--surface-line)}
  .btn-reset:hover{color:var(--ps-blue-800);border-color:var(--ps-blue-800)}
  .btn-login{background:var(--ps-blue-800);color:white;border-color:var(--ps-blue-800);padding:10px 20px;font-size:13px}
  .btn-login:hover{background:var(--ps-blue-900)}
  .user-block{
    border-bottom:1px solid var(--surface-line);
    padding-bottom:28px;
  }
  .user-block:last-child{border-bottom:none;padding-bottom:0}
  .user-header{display:flex;align-items:flex-start;justify-content:space-between;gap:16px;margin-bottom:16px}
  .user-header form{margin:0}
  .username{
    font-size:28px;line-height:1.0;
    color:var(--ps-blue-800);margin:0 0 4px;
  }
  .user-meta{
    font-family:var(--font-mono);font-weight:500;font-size:12px;
    color:var(--ink-muted);letter-spacing:0.08em;
    text-transform:uppercase;
  }
  .chip-warn{background:#fff5e6;color:#a84300;border-color:#ffd9a8}
  .chip-muted{background:transparent;color:var(--ink-muted);border-color:var(--surface-line)}
  .sites{display:grid;gap:0}
  /* Each row is its own grid, so its columns are sized from its own content and
     nothing lines up down the list. Subgrid hands every row the list's columns
     instead. Without support the rows keep their own template below, which is
     the behaviour this page has always had. */
  @supports (grid-template-columns: subgrid) {
    .sites{grid-template-columns:minmax(200px,2.5fr) repeat(5,auto)}
    .sites .site{grid-column:1/-1;grid-template-columns:subgrid}
  }
  .site{
    display:grid;grid-template-columns:minmax(200px,2.5fr) auto auto auto auto auto;align-items:baseline;
    gap:14px;padding:14px 0;
    border-top:1px solid var(--surface-line);
    border-radius:var(--radius-md);
    transition:background-color 150ms ease, box-shadow 150ms ease;
  }
  .site:first-child{border-top:none}
  .site:hover{background:var(--ps-blue-100);box-shadow:var(--shadow-xs)}
  .site-name{font-family:var(--font-sans);font-size:18px;font-weight:600;letter-spacing:-0.005em;min-width:0}
  /* Only the flat list needs to say who owns a site; inside a user block the
     heading above already does. */
  .site-owner{
    display:block;font-family:var(--font-mono);font-weight:500;font-size:12px;
    color:var(--ink-muted);letter-spacing:0.04em;margin-top:2px;
  }
  .site-name a{color:var(--ink);text-decoration:none;transition:color 150ms ease;overflow-wrap:anywhere}
  .site-name a:hover{color:var(--ps-blue-800);text-decoration:none}
  .site-version,.site-visibility,.site-backend{font-variant-numeric:tabular-nums}
  .site-version .chip,.site-visibility .chip,.site-backend .chip{font-size:11px;letter-spacing:0.12em;padding:3px 8px}
  .site-backend .chip+.chip{margin-left:4px}
  .site-traffic{
    font-family:var(--font-mono);font-weight:500;font-size:12px;
    color:var(--ink-muted);letter-spacing:0.04em;font-variant-numeric:tabular-nums;
    white-space:nowrap;
  }
  .site-traffic b{color:var(--ink);font-weight:600}
  .site-traffic span{margin-left:8px;color:var(--ink-soft)}
  .site-time{font-family:var(--font-mono);font-weight:500;font-size:12px;color:var(--ink-muted);letter-spacing:0.04em;font-variant-numeric:tabular-nums;white-space:nowrap;text-align:right}
  /* Flat list shows the date it is currently sorted by, nothing else. */
  .flat-sites .t-updated{display:none}
  .flat-sites.show-updated .t-created{display:none}
  .flat-sites.show-updated .t-updated{display:inline}
  .empty-user{
    font-family:var(--font-sans);font-size:15px;font-weight:500;
    color:var(--ink-muted);padding:6px 0;
  }
  .empty{
    text-align:center;padding:80px 0;
    font-family:var(--font-sans);font-size:20px;font-weight:500;
    color:var(--ink-muted);
  }
  .login-block{
    max-width:440px;margin:80px auto 0;
    padding:32px 36px;
    border:1px solid var(--surface-line);border-radius:12px;
    background:var(--canvas);
  }
  .login-copy{
    font-family:var(--font-sans);font-size:15px;color:var(--ink-soft);
    line-height:1.55;margin:0 0 12px;
  }
  .login-copy a{color:var(--ps-blue-800);text-decoration:none;font-weight:600}
  .login-copy a:hover{text-decoration:underline}
  .login-hint{font-family:var(--font-mono);font-size:12px;letter-spacing:0.08em;text-transform:uppercase}
  .login-error{color:#a84300}
  .login-form{display:flex;gap:8px;margin-top:16px}
  .login-form input{
    flex:1;padding:10px 14px;border-radius:8px;
    border:1px solid var(--surface-line);background:var(--canvas);
    font-family:var(--font-mono);font-size:14px;color:var(--ink);
  }
  .login-form input:focus{outline:none;border-color:var(--ps-blue-800);box-shadow:0 0 0 3px rgba(0,48,135,0.15)}
  @media (max-width: 780px) {
    header.bar{padding:18px 20px;display:block}
    .stats{margin-top:10px}
    main{padding:28px 20px 48px}
    /* .sites .site, not .site: the subgrid rule above is the more specific
       selector and would otherwise keep six columns on a phone. */
    .sites .site{grid-template-columns:1fr;gap:6px;padding:16px 0}
    .sites{grid-template-columns:1fr}
    .site-traffic{white-space:normal}
    .pending-row{flex-direction:column;align-items:flex-start}
    .overview{grid-template-columns:1fr}
    .toolbar{flex-direction:column;align-items:stretch}
    .analytics-range{justify-self:start;flex-wrap:wrap}
  }
</style>
</head>
<body>
`

// adminListScript is kept whole and separate so it can be exercised in a DOM
// harness (testdata/admin_list_test.js) rather than only eyeballed in a page.
const adminListScript = `<script>
(() => {
  const filter = document.getElementById('filter');
  const sortSel = document.getElementById('sort');
  const list = document.getElementById('userlist');
  const flat = document.getElementById('sitelist');
  if (list) {
    const blocks = Array.from(list.querySelectorAll('.user-block'));
    const rows = flat ? Array.from(flat.querySelectorAll('.site')) : [];
    // Two of the sorts reorder user blocks. The rest show a different list
    // entirely: sites on their own, ordered by the timestamp named here.
    const FLAT_SORTS = { 'recent-sites': 'data-created', 'latest-sites': 'data-updated' };
    const flatMode = () => sortSel && Object.prototype.hasOwnProperty.call(FLAT_SORTS, sortSel.value);
    const applyFilter = () => {
      const q = (filter.value || '').trim().toLowerCase();
      const show = (el) => {
        const hay = el.getAttribute('data-search') || '';
        el.style.display = (!q || hay.indexOf(q) !== -1) ? '' : 'none';
      };
      blocks.forEach(show);
      rows.forEach(show);
    };
    const num = (el, attr) => parseInt(el.getAttribute(attr) || '0', 10);
    const applySort = () => {
      const mode = sortSel.value;
      const isFlat = flatMode();
      if (flat) {
        list.hidden = isFlat;
        flat.hidden = !isFlat;
        if (isFlat) {
          flat.classList.toggle('show-updated', mode === 'latest-sites');
          const attr = FLAT_SORTS[mode];
          rows.slice()
            .sort((a, b) => num(b, attr) - num(a, attr))
            .forEach((el) => flat.appendChild(el));
        }
      }
      if (isFlat) return;
      const sorted = blocks.slice().sort((a, b) => {
        switch (mode) {
          case 'sitecount': return num(b, 'data-sitecount') - num(a, 'data-sitecount');
          case 'views': return num(b, 'data-views') - num(a, 'data-views');
          case 'latest': return num(b, 'data-latest') - num(a, 'data-latest');
          default: return (a.getAttribute('data-username') || '').localeCompare(b.getAttribute('data-username') || '');
        }
      });
      sorted.forEach((el) => list.appendChild(el));
    };
    if (filter) filter.addEventListener('input', applyFilter);
    if (sortSel) sortSel.addEventListener('change', () => { applySort(); applyFilter(); });
    // Run once at load. The default is a flat sort, and a browser restoring a
    // previously selected value on reload would otherwise leave the page
    // showing a list that does not match the dropdown.
    if (sortSel) applySort();
  }

  const formatters = {
    date: new Intl.DateTimeFormat(undefined, { year: 'numeric', month: '2-digit', day: '2-digit' }),
    datetime: new Intl.DateTimeFormat(undefined, {
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      timeZoneName: 'short',
    }),
  };
  document.querySelectorAll('time[data-local-time]').forEach((el) => {
    const date = new Date(el.dateTime);
    if (Number.isNaN(date.getTime())) return;
    const formatter = formatters[el.dataset.localTime] || formatters.datetime;
    el.textContent = formatter.format(date);
    el.title = el.dateTime;
  });
})();
</script>`

// adminActivityScript fills the "admin-activity"/"admin-visitor" cards
// added to the admin dashboard for design.md 8.3's admin-wide read of
// audit_events and access_log: no owner filter is passed, so a caller
// admin.go has already confirmed is an admin (IsAdmin) sees every
// namespace's rows, one page (100 rows) of each, newest first — the export
// links next to each list are how an admin gets more than that.
const adminActivityScript = `<script>
(function(){
  var activityList = document.getElementById('admin-activity-list');
  var visitorList = document.getElementById('admin-visitor-list');
  if (!activityList && !visitorList) return;
  var activityCount = document.getElementById('admin-activity-count');
  var visitorCount = document.getElementById('admin-visitor-count');

  function esc(s) { var d = document.createElement('div'); d.textContent = s == null ? '' : String(s); return d.innerHTML; }

  function load(list, countEl, url, render) {
    fetch(url, {credentials: 'same-origin'})
      .then(function(r){ return r.json(); })
      .then(function(body){
        var rows = render.rows(body);
        list.innerHTML = '';
        if (countEl) countEl.textContent = rows.length ? '(' + rows.length + (body.next_cursor ? '+' : '') + ')' : '';
        if (!rows.length) { list.innerHTML = '<div class="rank-empty">Nothing recorded yet.</div>'; return; }
        rows.forEach(function(row){
          var el = document.createElement('div');
          el.className = 'rank-row';
          el.innerHTML = render.row(row);
          list.appendChild(el);
        });
      })
      .catch(function(){ list.innerHTML = '<div class="rank-empty">Could not load.</div>'; });
  }

  if (activityList) {
    load(activityList, activityCount, '/api/audit', {
      rows: function(body){ return (body && body.events) || []; },
      row: function(e){
        return '<span class="rank-name">' + esc(e.action) +
          ' <span class="rank-sub">' + esc(e.owner_id || '') + (e.site_id ? ' · ' + esc(e.site_id) : '') +
          ' · ' + esc(new Date(e.at).toLocaleString()) + '</span></span>';
      },
    });
  }
  if (visitorList) {
    load(visitorList, visitorCount, '/api/access', {
      rows: function(body){ return (body && body.entries) || []; },
      row: function(e){
        return '<span class="rank-name">' + esc(e.owner_label) + '/' + esc(e.site_name) +
          ' <span class="rank-sub">' + esc(e.method) + ' ' + esc(e.path) + ' · ' + esc(e.status) +
          ' · ' + esc(new Date(e.at).toLocaleString()) + '</span></span>';
      },
    });
  }
})();
</script>`
