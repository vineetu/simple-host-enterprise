package handler

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// AuditHandler serves two read routes: GET /api/audit (the
// action log) and GET /api/access (the visit log).
// Both are scoped identically in spirit — an admin sees everything, anyone
// else sees only their own namespace and the namespaces of teams they
// belong to — but audit_events and access_log disagree on how a namespace
// is named (owner_id, a uuid FK, on the former; owner_label, the plain host
// label, on the latter), so the two routes resolve scope differently; see
// each handler method's own comment.
type AuditHandler struct {
	database   *sql.DB
	reader     *audit.Reader
	limits     *AbuseLimits
	visibility string // "counts" (default), "owner" or "admin" — ACCESS_LOG_VISIBILITY
}

// NewAuditHandler constructs the handler. visibility is config.AuditConfig's
// AccessLogVisibility; an empty string is treated as "counts", the
// documented default, so a caller that forgets to pass it never shows an
// owner who visited.
func NewAuditHandler(database *sql.DB, reader *audit.Reader, visibility string, limits ...*AbuseLimits) *AuditHandler {
	if visibility == "" {
		visibility = "counts"
	}
	return &AuditHandler{database: database, reader: reader, limits: chooseAbuseLimits(limits), visibility: visibility}
}

func (h *AuditHandler) Register(mux *http.ServeMux, authMiddleware, skillVersionMiddleware func(http.Handler) http.Handler) {
	read := func(next http.Handler) http.Handler {
		return h.limitReadClient(authMiddleware(skillVersionMiddleware(next)))
	}
	mux.Handle("GET /api/audit", read(http.HandlerFunc(h.listAudit)))
	mux.Handle("GET /api/access", read(http.HandlerFunc(h.listAccess)))
}

func (h *AuditHandler) limitReadClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decision := h.limits.allow(managementClientPolicy, clientLimitKey(r)); !decision.Allowed {
			writeRateLimit(w, decision)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// callerNamespaceScope resolves the caller's own namespace and every team
// they belong to: their own user id, plus the id of every team
// db.ListTeamsForUser reports. Used by /api/audit (a list of owner_id
// values ANDed into the query) and, by way of the labels it derives, by
// /api/access's authorization check.
func (h *AuditHandler) callerNamespaceScope(r *http.Request, user *db.User) ([]string, error) {
	scope := []string{user.ID}
	teams, err := db.ListTeamsForUser(r.Context(), h.database, user.ID)
	if err != nil {
		return nil, err
	}
	for _, t := range teams {
		scope = append(scope, t.ID)
	}
	return scope, nil
}

// callerNamespaceLabels is callerNamespaceScope's own-labels form, for
// /api/access's owner_label comparison: the caller's own host label, plus
// the label of every team they belong to.
func (h *AuditHandler) callerNamespaceLabels(r *http.Request, user *db.User) ([]string, error) {
	labels := []string{ownerLabel(user.Username)}
	teams, err := db.ListTeamsForUser(r.Context(), h.database, user.ID)
	if err != nil {
		return nil, err
	}
	for _, t := range teams {
		labels = append(labels, ownerLabel(t.Username))
	}
	return labels, nil
}

// adminView reports whether the caller gets the company-wide view of the
// audit and access logs: an admin, signed in with a browser session. An API
// key (any scope) or an AI app's connector token held by an admin sees only
// what any other person sees, their own namespace and their teams', the same
// rule as the admin API, so a leaked CI key never reads who viewed what
// across the company.
func adminView(r *http.Request, user *db.User) bool {
	return user.IsAdmin && auth.SessionID(r.Context()) != ""
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// auditEventResponse is one row of GET /api/audit's JSON body. Field names
// match the audit_events column names, not Go convention, since this is
// consumed by the dashboard's own JavaScript and by an agent reading the
// API directly.
type auditEventResponse struct {
	ID              int64          `json:"id"`
	At              time.Time      `json:"at"`
	RequestID       string         `json:"request_id,omitempty"`
	ActorID         string         `json:"actor_id,omitempty"`
	ActorKind       string         `json:"actor_kind"`
	KeyID           string         `json:"key_id,omitempty"`
	Action          string         `json:"action"`
	OwnerID         string         `json:"owner_id,omitempty"`
	SiteID          string         `json:"site_id,omitempty"`
	TeamID          string         `json:"team_id,omitempty"`
	ViaSiteLabel    string         `json:"via_site_label,omitempty"`
	ViaSiteName     string         `json:"via_site_name,omitempty"`
	ViaSiteObserved bool           `json:"via_site_observed"`
	IP              string         `json:"ip,omitempty"`
	UserAgent       string         `json:"user_agent,omitempty"`
	Detail          map[string]any `json:"detail,omitempty"`
	// ActorName, OwnerName and SiteName are the names behind the ids, so a
	// reader sees people and sites rather than uuids. ActorName is given to
	// an owner or team member for changes to the site: made by themselves,
	// a member of the team, or anyone who saved its data (never who opened
	// it, or was refused); for the rest, ActorID, KeyID, IP and UserAgent
	// are left out too. An admin always sees everything.
	ActorName string `json:"actor_name,omitempty"`
	OwnerName string `json:"owner_name,omitempty"`
	SiteName  string `json:"site_name,omitempty"`
}

func toAuditEventResponse(e db.AuditEvent) auditEventResponse {
	return auditEventResponse{
		ID: e.ID, At: e.At, RequestID: e.RequestID, ActorID: e.ActorID, ActorKind: e.ActorKind,
		KeyID: e.KeyID, Action: e.Action, OwnerID: e.OwnerID, SiteID: e.SiteID, TeamID: e.TeamID,
		ViaSiteLabel: e.ViaSiteLabel, ViaSiteName: e.ViaSiteName, ViaSiteObserved: e.ViaSiteObserved,
		IP: e.IP, UserAgent: e.UserAgent, Detail: e.Detail,
	}
}

type auditListResponse struct {
	Events     []auditEventResponse `json:"events"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

// listAudit answers GET /api/audit: scope is the caller's own
// namespace and teams unless they are an admin, in which case any owner may
// be named. owner/actor are usernames (or team names, which live in the
// same users table — see internal/db/teams.go), resolved to ids here so
// the caller never has to know audit_events stores uuids; site is a name
// resolved against the resolved owner, since site names are unique only
// per owner. An owner/actor/site name that does not resolve is not an
// error — it is a filter nothing can match, so the response is an empty
// page rather than a 404 (a filter is not an address).
func (h *AuditHandler) listAudit(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}

	admin := adminView(r, user)
	query := audit.AuditQuery{Admin: admin, Cursor: r.URL.Query().Get("cursor"), Action: r.URL.Query().Get("action")}
	if !admin {
		scope, err := h.callerNamespaceScope(r, user)
		if err != nil {
			log.Printf("audit: resolve namespace scope for %s: %v", user.Username, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
		query.OwnerScope = scope
	}

	nothing, bad, err := resolveAuditFilters(r, h.database, &query)
	if err != nil {
		log.Printf("audit: resolve filters for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if bad != "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: bad})
		return
	}
	if !nothing && !admin && query.Actor != "" && query.Actor != user.ID {
		// Someone who is not an admin filters by a person only for
		// themselves or a fellow member of one of their teams: never "did
		// this colleague open (or try to open) my site".
		members, err := db.TeamMemberIDs(r.Context(), h.database, query.OwnerScope)
		if err != nil {
			log.Printf("audit: team members for %s: %v", user.Username, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
		nothing = true
		for _, team := range members {
			if team[query.Actor] {
				nothing = false
			}
		}
	}
	if nothing {
		writeJSON(w, http.StatusOK, auditListResponse{})
		return
	}

	page, err := h.reader.ListAuditEvents(r.Context(), query)
	if err != nil {
		log.Printf("audit: list audit events for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	out, err := namedAuditEvents(r, h.database, page.Events, user, admin)
	if err != nil {
		log.Printf("audit: name audit events for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, auditListResponse{Events: out, NextCursor: page.NextCursor})
}

// resolveAuditFilters fills query's owner, site and actor from the request's
// names (usernames, team names, a site name under the owner), and its from
// and to. nothing is true when a name resolves to nobody: a filter nothing
// can match, answered with an empty page rather than an error. bad is a
// message for a 400.
func resolveAuditFilters(r *http.Request, database *sql.DB, query *audit.AuditQuery) (nothing bool, bad string, err error) {
	params := r.URL.Query()
	query.Action = params.Get("action")
	var ownerID string
	if raw := params.Get("owner"); raw != "" {
		owner, err := db.GetUserByUsername(r.Context(), database, raw)
		if errors.Is(err, sql.ErrNoRows) {
			return true, "", nil
		}
		if err != nil {
			return false, "", err
		}
		ownerID = owner.ID
		query.Owner = ownerID
	}
	if raw := params.Get("site"); raw != "" {
		if ownerID == "" {
			return false, "site filter requires owner", nil
		}
		id, err := db.SiteIDForAuditFilter(r.Context(), database, ownerID, raw)
		if errors.Is(err, sql.ErrNoRows) {
			return true, "", nil
		}
		if err != nil {
			return false, "", err
		}
		query.Site = id
	}
	if raw := params.Get("actor"); raw != "" {
		actor, err := db.GetUserByUsername(r.Context(), database, raw)
		if errors.Is(err, sql.ErrNoRows) {
			return true, "", nil
		}
		if err != nil {
			return false, "", err
		}
		query.Actor = actor.ID
	}
	for _, name := range []string{"from", "to"} {
		raw := params.Get(name)
		if raw == "" {
			continue
		}
		t, perr := parseAuditTime(raw, name == "to")
		if perr != nil {
			return false, name + " must be an RFC 3339 timestamp or a YYYY-MM-DD date", nil
		}
		if name == "from" {
			query.From = t
		} else {
			query.To = t
		}
	}
	return false, "", nil
}

// namedAuditEvents is a page of events for the JSON body, with the names
// behind their ids. For someone who is not in the admin view, an actor is
// named only when it is the caller, or a member of the team whose namespace
// the event is in: the change events of the people who share the site, not
// the visitors who opened or wrote it.
func namedAuditEvents(r *http.Request, database *sql.DB, events []db.AuditEvent, caller *db.User, admin bool) ([]auditEventResponse, error) {
	var userIDs, siteIDs []string
	for _, e := range events {
		for _, id := range []string{e.ActorID, e.OwnerID} {
			if id != "" {
				userIDs = append(userIDs, id)
			}
		}
		if e.SiteID != "" {
			siteIDs = append(siteIDs, e.SiteID)
		}
	}
	users, sites, err := db.AuditNames(r.Context(), database, userIDs, siteIDs)
	if err != nil {
		return nil, err
	}
	var members map[string]map[string]bool
	if !admin {
		var teamIDs []string
		for _, e := range events {
			if e.OwnerID != "" && e.OwnerID != caller.ID {
				teamIDs = append(teamIDs, e.OwnerID)
			}
		}
		if members, err = db.TeamMemberIDs(r.Context(), database, teamIDs); err != nil {
			return nil, err
		}
	}
	out := make([]auditEventResponse, 0, len(events))
	for _, e := range events {
		row := toAuditEventResponse(e)
		row.OwnerName = users[e.OwnerID]
		if site, ok := sites[e.SiteID]; ok && site.OwnerID == e.OwnerID {
			row.SiteName = site.Name
		}
		if admin || actorNamedToOwner(e, caller.ID, members) {
			row.ActorName = users[e.ActorID]
			if !admin && e.ActorID != caller.ID && !members[e.OwnerID][e.ActorID] {
				// Named as the author of a saved-data change, like "written
				// by", but where they were and on what device is theirs.
				row.IP, row.UserAgent = "", ""
			}
		} else {
			// Nor anything that would tell the same visitor apart across
			// rows.
			row.ActorID, row.KeyID, row.IP, row.UserAgent = "", "", "", ""
		}
		out = append(out, row)
	}
	return out, nil
}

// actorNamedToOwner reports whether an owner or team member may see who
// made this change: themselves, a member of the team that owns it, or
// anyone who saved the site's data (state_write). The authors of changes to
// your own site are visible, the same as "written by" on each saved-data
// version; mere visitors are not: a refused visit (access_denied) is a
// view, never a change.
func actorNamedToOwner(e db.AuditEvent, callerID string, members map[string]map[string]bool) bool {
	if e.ActorID == "" || e.Action == "access_denied" {
		return false
	}
	return e.ActorID == callerID || members[e.OwnerID][e.ActorID] || e.Action == "state_write"
}

// accessLogEntryResponse is one row of GET /api/access's JSON body.
type accessLogEntryResponse struct {
	ID         int64     `json:"id"`
	At         time.Time `json:"at"`
	UserID     string    `json:"user_id,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	OwnerLabel string    `json:"owner_label"`
	SiteName   string    `json:"site_name"`
	Path       string    `json:"path"`
	Method     string    `json:"method"`
	Status     int       `json:"status"`
	Bytes      int64     `json:"bytes"`
	IP         string    `json:"ip,omitempty"`
	UserAgent  string    `json:"user_agent,omitempty"`
	ClientKind string    `json:"client_kind"`
	// ReferrerDomain is the linking page's host only, never its URL.
	ReferrerDomain string `json:"referrer_domain,omitempty"`
}

func toAccessLogEntryResponse(e db.AccessLogEntry) accessLogEntryResponse {
	return accessLogEntryResponse{
		ID: e.ID, At: e.At, UserID: e.UserID, SessionID: e.SessionID, OwnerLabel: e.OwnerLabel,
		SiteName: e.SiteName, Path: e.Path, Method: e.Method, Status: e.Status, Bytes: e.Bytes,
		IP: e.IP, UserAgent: e.UserAgent, ClientKind: e.ClientKind, ReferrerDomain: e.ReferrerDomain,
	}
}

type accessListResponse struct {
	Entries    []accessLogEntryResponse `json:"entries"`
	NextCursor string                   `json:"next_cursor,omitempty"`
}

// listAccess answers GET /api/access. Unlike /api/audit,
// access_log's owner/site columns are plain labels/names, not ids, so no
// database lookup is needed to filter by them — but that also means
// authorization has to be checked here rather than left to a WHERE clause:
// a non-admin must name an owner whose label is their own or one of their
// teams'. ACCESS_LOG_VISIBILITY=admin refuses every non-admin outright,
// before any of that; the default, counts, answers a non-admin with
// aggregates only (listAccessCounts), as does summary=counts under owner.
func (h *AuditHandler) listAccess(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}

	owner := r.URL.Query().Get("owner")
	site := r.URL.Query().Get("site")

	admin := adminView(r, user)
	query := audit.AccessQuery{Admin: admin, Owner: owner, Site: site, Cursor: r.URL.Query().Get("cursor")}
	if !admin {
		if h.visibility == "admin" {
			writeJSON(w, http.StatusForbidden, errorResponse{Error: "forbidden"})
			return
		}
		if owner == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "owner is required"})
			return
		}
		labels, err := h.callerNamespaceLabels(r, user)
		if err != nil {
			log.Printf("audit: resolve namespace labels for %s: %v", user.Username, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
		if !containsString(labels, owner) {
			writeJSON(w, http.StatusForbidden, errorResponse{Error: "forbidden"})
			return
		}
		// summary=counts asks for the aggregate under "owner" visibility
		// too, so a caller that wants numbers (site_activity) gets the same
		// shape whichever visibility the server runs with.
		if h.visibility == "counts" || r.URL.Query().Get("summary") == "counts" {
			h.listAccessCounts(w, r, owner, site)
			return
		}
	} else if owner != "" && r.URL.Query().Get("summary") == "counts" {
		// An admin asking for the aggregate (the dashboard's top pages and
		// referrers) gets it too.
		h.listAccessCounts(w, r, owner, site)
		return
	}

	from, ok := parseAuditTimeParam(w, r, "from")
	if !ok {
		return
	}
	to, ok := parseAuditTimeParam(w, r, "to")
	if !ok {
		return
	}
	query.From, query.To = from, to
	if owner != "" {
		labels, err := db.OwnerLabelHistory(r.Context(), h.database, owner)
		if err != nil {
			log.Printf("audit: label history for %s: %v", owner, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
		query.Aliases = labels[1:]
	}

	page, err := h.reader.ListAccess(r.Context(), query)
	if err != nil {
		log.Printf("audit: list access log for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	out := make([]accessLogEntryResponse, 0, len(page.Entries))
	for _, e := range page.Entries {
		out = append(out, toAccessLogEntryResponse(e))
	}
	writeJSON(w, http.StatusOK, accessListResponse{Entries: out, NextCursor: page.NextCursor})
}

type accessDayCountResponse struct {
	Day           string `json:"day"`
	Views         int64  `json:"views"`
	UniqueViewers int64  `json:"unique_viewers"`
}

type accessCountsResponse struct {
	From          time.Time                `json:"from"`
	To            time.Time                `json:"to"`
	UniqueViewers int64                    `json:"unique_viewers"`
	Days          []accessDayCountResponse `json:"days"`
	// TopPages and TopReferrers count people's page views by path and by
	// referring domain (domain only), most first, at most 10 each.
	TopPages     []accessPageCountResponse     `json:"top_pages"`
	TopReferrers []accessReferrerCountResponse `json:"top_referrers"`
}

type accessPageCountResponse struct {
	Path  string `json:"path"`
	Views int64  `json:"views"`
}

type accessReferrerCountResponse struct {
	Domain string `json:"domain"`
	Views  int64  `json:"views"`
}

// listAccessCounts is GET /api/access for a non-admin under the default
// ACCESS_LOG_VISIBILITY=counts: views per day and how many distinct
// signed-in people viewed, over from..to (default the last 30 days). Who
// they were is for admins only.
func (h *AuditHandler) listAccessCounts(w http.ResponseWriter, r *http.Request, owner, site string) {
	from, ok := parseAuditTimeParam(w, r, "from")
	if !ok {
		return
	}
	to, ok := parseAuditTimeParam(w, r, "to")
	if !ok {
		return
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	if from.IsZero() {
		from = to.AddDate(0, 0, -30)
	}
	// A person an admin renamed keeps their history: it is recorded under
	// the labels they had then.
	labels, err := db.OwnerLabelHistory(r.Context(), h.database, owner)
	if err != nil {
		log.Printf("audit: label history for %s: %v", owner, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	counts, err := db.ListAccessCounts(r.Context(), h.database, labels, site, from, to)
	if err != nil {
		log.Printf("audit: access counts for %s/%s: %v", owner, site, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	out := accessCountsResponse{From: from, To: to, UniqueViewers: counts.UniqueViewers, Days: make([]accessDayCountResponse, 0, len(counts.Days)),
		TopPages: make([]accessPageCountResponse, 0, len(counts.TopPages)), TopReferrers: make([]accessReferrerCountResponse, 0, len(counts.TopReferrers))}
	for _, d := range counts.Days {
		out.Days = append(out.Days, accessDayCountResponse{Day: d.Day.Format("2006-01-02"), Views: d.Views, UniqueViewers: d.UniqueViewers})
	}
	for _, p := range counts.TopPages {
		out.TopPages = append(out.TopPages, accessPageCountResponse{Path: p.Key, Views: p.Views})
	}
	for _, ref := range counts.TopReferrers {
		out.TopReferrers = append(out.TopReferrers, accessReferrerCountResponse{Domain: ref.Key, Views: ref.Views})
	}
	writeJSON(w, http.StatusOK, out)
}

// parseAuditTimeParam parses an RFC3339 query parameter, writing a 400 and
// returning ok=false on a malformed (non-empty) value. An absent parameter
// is the zero time and ok=true, meaning "no restriction" to both
// AuditQuery.From/To and AccessQuery.From/To.
func parseAuditTimeParam(w http.ResponseWriter, r *http.Request, name string) (time.Time, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return time.Time{}, true
	}
	parsed, err := parseAuditTime(raw, name == "to")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: name + " must be an RFC 3339 timestamp or a YYYY-MM-DD date"})
		return time.Time{}, false
	}
	return parsed, true
}

// parseAuditTime reads an RFC 3339 timestamp or a YYYY-MM-DD date (UTC). A
// date as the end of a range (endOfDay) covers that whole day, since both
// bounds are inclusive.
func parseAuditTime(raw string, endOfDay bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	day, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return time.Time{}, err
	}
	if endOfDay {
		return day.Add(24*time.Hour - time.Nanosecond), nil
	}
	return day, nil
}
