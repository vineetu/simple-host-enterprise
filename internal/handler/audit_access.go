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

	query := audit.AuditQuery{Admin: user.IsAdmin, Cursor: r.URL.Query().Get("cursor"), Action: r.URL.Query().Get("action")}
	if !user.IsAdmin {
		scope, err := h.callerNamespaceScope(r, user)
		if err != nil {
			log.Printf("audit: resolve namespace scope for %s: %v", user.Username, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
		query.OwnerScope = scope
	}

	var ownerID string
	if raw := r.URL.Query().Get("owner"); raw != "" {
		owner, err := db.GetUserByUsername(r.Context(), h.database, raw)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				log.Printf("audit: resolve owner %q: %v", raw, err)
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
				return
			}
			writeJSON(w, http.StatusOK, auditListResponse{})
			return
		}
		ownerID = owner.ID
		query.Owner = ownerID
	}

	if raw := r.URL.Query().Get("site"); raw != "" {
		if ownerID == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "site filter requires owner"})
			return
		}
		site, err := db.GetSite(r.Context(), h.database, ownerID, raw)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				log.Printf("audit: resolve site %s/%s: %v", raw, ownerID, err)
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
				return
			}
			writeJSON(w, http.StatusOK, auditListResponse{})
			return
		}
		query.Site = site.ID
	}

	if raw := r.URL.Query().Get("actor"); raw != "" {
		actor, err := db.GetUserByUsername(r.Context(), h.database, raw)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				log.Printf("audit: resolve actor %q: %v", raw, err)
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
				return
			}
			writeJSON(w, http.StatusOK, auditListResponse{})
			return
		}
		query.Actor = actor.ID
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

	page, err := h.reader.ListAuditEvents(r.Context(), query)
	if err != nil {
		log.Printf("audit: list audit events for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	out := make([]auditEventResponse, 0, len(page.Events))
	for _, e := range page.Events {
		out = append(out, toAuditEventResponse(e))
	}
	writeJSON(w, http.StatusOK, auditListResponse{Events: out, NextCursor: page.NextCursor})
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
}

func toAccessLogEntryResponse(e db.AccessLogEntry) accessLogEntryResponse {
	return accessLogEntryResponse{
		ID: e.ID, At: e.At, UserID: e.UserID, SessionID: e.SessionID, OwnerLabel: e.OwnerLabel,
		SiteName: e.SiteName, Path: e.Path, Method: e.Method, Status: e.Status, Bytes: e.Bytes,
		IP: e.IP, UserAgent: e.UserAgent, ClientKind: e.ClientKind,
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
// aggregates only (listAccessCounts).
func (h *AuditHandler) listAccess(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}

	owner := r.URL.Query().Get("owner")
	site := r.URL.Query().Get("site")

	query := audit.AccessQuery{Admin: user.IsAdmin, Owner: owner, Site: site, Cursor: r.URL.Query().Get("cursor")}
	if !user.IsAdmin {
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
		if h.visibility == "counts" {
			h.listAccessCounts(w, r, owner, site)
			return
		}
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
	counts, err := db.ListAccessCounts(r.Context(), h.database, owner, site, from, to)
	if err != nil {
		log.Printf("audit: access counts for %s/%s: %v", owner, site, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	out := accessCountsResponse{From: from, To: to, UniqueViewers: counts.UniqueViewers, Days: make([]accessDayCountResponse, 0, len(counts.Days))}
	for _, d := range counts.Days {
		out.Days = append(out.Days, accessDayCountResponse{Day: d.Day.Format("2006-01-02"), Views: d.Views, UniqueViewers: d.UniqueViewers})
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
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: name + " must be an RFC 3339 timestamp"})
		return time.Time{}, false
	}
	return parsed, true
}
