package handler

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/safepath"
)

// Viewer management: the named viewers of a site at the "specific" access
// level. Granting a viewer also moves the site to that level. Removing the last
// viewer leaves the level alone, so the site narrows to its owner rather
// than silently widening. Owner-role gate, in-transaction recheck,
// bounded-batch grant.

// siteViewerResponse is one viewer. A person named by email who has not
// signed in yet is Pending, with the address as Username (what removing
// them takes).
type siteViewerResponse struct {
	Username string `json:"username"`
	Kind     string `json:"kind"`
	Pending  bool   `json:"pending,omitempty"`
}

func siteViewerResponses(viewers []db.SiteViewer) []siteViewerResponse {
	response := make([]siteViewerResponse, 0, len(viewers))
	for _, v := range viewers {
		response = append(response, siteViewerResponse{Username: v.Username, Kind: v.Kind, Pending: v.Pending})
	}
	return response
}

type viewerCandidateResponse struct {
	Username      string `json:"username"`
	Kind          string `json:"kind"`
	AlreadyViewer bool   `json:"already_viewer"`
}

type grantViewersRequest struct {
	Usernames []string `json:"usernames"`
}

// registerViewerRoutes is called from (*SiteHandler).Register.
func (h *SiteHandler) registerViewerRoutes(mux *http.ServeMux, ownerMutation func(http.Handler) http.Handler, browserWrite func(http.Handler) http.Handler) {
	mux.Handle("GET /api/collaboration/sites/{owner}/{sitename}/viewers", ownerMutation(http.HandlerFunc(h.listSiteViewers)))
	mux.Handle("POST /api/collaboration/sites/{owner}/{sitename}/viewers", browserWrite(ownerMutation(http.HandlerFunc(h.grantSiteViewers))))
	mux.Handle("DELETE /api/collaboration/sites/{owner}/{sitename}/viewers/{username}", browserWrite(ownerMutation(http.HandlerFunc(h.revokeSiteViewer))))
	mux.Handle("GET /api/collaboration/sites/{owner}/{sitename}/viewer-candidates", ownerMutation(http.HandlerFunc(h.searchViewerCandidates)))
}

func (h *SiteHandler) listSiteViewers(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok || !requireOwnerRole(w, access) {
		return
	}
	viewers, err := db.ListSiteViewers(r.Context(), h.database, access.Site.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, siteViewerResponses(viewers))
}

func (h *SiteHandler) searchViewerCandidates(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok || !requireOwnerRole(w, access) {
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 20 {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "limit must be between 1 and 20"})
			return
		}
		limit = parsed
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) > 100 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "search query is too long"})
		return
	}
	candidates, err := db.SearchViewerCandidates(r.Context(), h.database, access.Site.ID, query, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	response := make([]viewerCandidateResponse, 0, len(candidates))
	for _, c := range candidates {
		response = append(response, viewerCandidateResponse{Username: c.Username, Kind: c.Kind, AlreadyViewer: c.AlreadyViewer})
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *SiteHandler) grantSiteViewers(w http.ResponseWriter, r *http.Request) {
	h.mutateSiteViewers(w, r, true)
}

func (h *SiteHandler) revokeSiteViewer(w http.ResponseWriter, r *http.Request) {
	h.mutateSiteViewers(w, r, false)
}

func (h *SiteHandler) mutateSiteViewers(w http.ResponseWriter, r *http.Request, grant bool) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	if decision := h.limits.allow(managementUserPolicy, user.ID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	preliminary, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok || !requireOwnerRole(w, preliminary) {
		return
	}

	var usernames []string
	if grant {
		var request grantViewersRequest
		if !decodeSmallJSON(w, r, &request) {
			return
		}
		if len(request.Usernames) == 0 {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "at least one username is required"})
			return
		}
		if len(request.Usernames) > db.MaxSiteViewers {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "too many usernames"})
			return
		}
		if refusal := checkGrantEmails(request.Usernames, h.allowedEmailDomains); refusal != "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: refusal})
			return
		}
		usernames = request.Usernames
	} else {
		username := strings.ToLower(strings.TrimSpace(r.PathValue("username")))
		if safepath.ValidateSegment(username) != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid viewer username"})
			return
		}
		usernames = []string{username}
	}

	unlock := h.mutations.lock(preliminary.OwnerID, siteName)
	defer unlock()
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer audit.Rollback(tx)
	if err := db.LockSiteCollaboration(r.Context(), tx, preliminary.OwnerID, siteName); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	locked, err := db.ResolveSiteAccess(r.Context(), tx, user.ID, ownerUsername, siteName)
	if err != nil || locked.Site.ID != preliminary.Site.ID ||
		(locked.Role != db.CollaborationRoleOwner && locked.Role != db.CollaborationRoleMember) {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("recheck viewer-management access for %s/%s: %v", ownerUsername, siteName, err)
		}
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
		return
	}

	var viewers []db.SiteViewer
	var pending []string
	if grant {
		viewers, err = db.GrantSiteViewers(r.Context(), tx, locked.OwnerID, siteName, locked.Site.ID, &user.ID, usernames)
		requested := map[string]bool{}
		for _, name := range usernames {
			requested[strings.ToLower(strings.TrimSpace(name))] = true
		}
		for _, v := range viewers {
			if v.Pending && requested[v.Username] {
				pending = append(pending, v.Username)
			}
		}
	} else {
		var removed, wasPending bool
		removed, wasPending, err = db.RevokeSiteViewer(r.Context(), tx, locked.OwnerID, siteName, locked.Site.ID, usernames[0])
		if wasPending {
			pending = usernames
		}
		if err == nil && !removed {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "viewer not found"})
			return
		}
		if err == nil {
			viewers, err = db.ListSiteViewers(r.Context(), tx, locked.Site.ID)
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, db.ErrUserNotFound):
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "one or more usernames do not exist"})
		case errors.Is(err, db.ErrAmbiguousEmail):
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "more than one account has that email; use the username"})
		case errors.Is(err, db.ErrViewerLimit):
			writeJSON(w, http.StatusConflict, errorResponse{Error: "a site can have at most 50 listed viewers"})
		case errors.Is(err, sql.ErrNoRows):
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
		default:
			log.Printf("mutate viewers for %s/%s: %v", ownerUsername, siteName, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		}
		return
	}
	action := "viewer_revoke"
	if grant {
		action = "viewer_grant"
	}
	extra := map[string]any{"usernames": usernames}
	if len(pending) > 0 {
		// Named by email, not signed in yet: a pending grant.
		extra["pending"] = pending
	}
	actorKind, keyID := auditActorKind(r.Context())
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: user.ID, ActorKind: actorKind, KeyID: keyID,
		Action: action, OwnerID: locked.OwnerID, SiteID: locked.Site.ID,
		RequestID: auditRequestID(r.Context()),
		Extra:     extra,
	}); err != nil {
		log.Printf("record audit for %s %s/%s: %v", action, ownerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := audit.Commit(tx); err != nil {
		log.Printf("commit viewer mutation for %s/%s: %v", ownerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if !grant {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, siteViewerResponses(viewers))
}

// WithAllowedEmailDomains sets ALLOWED_EMAIL_DOMAINS: a viewer named by
// email must be at one of them, the same rule sign-in applies. Empty means
// any domain.
func (h *SiteHandler) WithAllowedEmailDomains(domains []string) *SiteHandler {
	h.allowedEmailDomains = domains
	return h
}
