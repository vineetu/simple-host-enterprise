package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// Access levels (db.Access*): who can open a site. The owner (or a member of
// the owning team) sets any level but network directly; network is a
// request that admins approve or decline on /admin.

// requiredNetworkApprovals is how many different admins must approve a
// network-access request (NETWORK_ACCESS_APPROVALS): 1 unless set to 2.
func requiredNetworkApprovals(n int) int {
	if n == 2 {
		return 2
	}
	return 1
}

// WithNetworkAccessApprovals sets NETWORK_ACCESS_APPROVALS, which the
// owner's view of a pending request reports. Unset means 1.
func (h *SiteHandler) WithNetworkAccessApprovals(n int) *SiteHandler {
	h.networkApprovals = requiredNetworkApprovals(n)
	return h
}

// WithNetworkAccessApprovals sets NETWORK_ACCESS_APPROVALS: how many
// different admins, none of them the requester, must approve a request
// before the site opens to the network. Unset means 1.
func (h *AdminHandler) WithNetworkAccessApprovals(n int) *AdminHandler {
	h.networkApprovals = requiredNetworkApprovals(n)
	return h
}

type siteAccessRequest struct {
	Level  string `json:"level"`
	Reason string `json:"reason"`
}

const maxNetworkReasonRunes = 500

// setSiteAccess is POST /api/sites/{sitename}/access, on the caller's own
// namespace.
func (h *SiteHandler) setSiteAccess(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	h.setSiteAccessForTarget(w, r, selfTarget(user))
}

// setSiteAccessForTarget sets a site's access level in the namespace named by
// target, or, for level "network", records a request for an admin to
// approve. See mutationTarget.
func (h *SiteHandler) setSiteAccessForTarget(w http.ResponseWriter, r *http.Request, target mutationTarget) {
	siteName, ok := validatedSiteName(w, r)
	if !ok {
		return
	}
	if !validateStoredUsername(w, target.OwnerUsername) {
		return
	}
	if decision := h.limits.allow(managementUserPolicy, target.ActorID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	var req siteAccessRequest
	if !decodeSmallJSON(w, r, &req) {
		return
	}
	if !db.ValidAccessLevel(req.Level) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "level must be one of only_me, specific, company, listed, network"})
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if req.Level == db.AccessNetwork {
		if reason == "" {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "a reason is required to request network access"})
			return
		}
		if utf8.RuneCountInString(reason) > maxNetworkReasonRunes {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "reason is too long (at most 500 characters)"})
			return
		}
	}

	unlock := h.mutations.lock(target.OwnerID, siteName)
	defer unlock()
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer audit.Rollback(tx)
	if err := db.LockSiteCollaboration(r.Context(), tx, target.OwnerID, siteName); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	site, err := db.GetSite(r.Context(), tx, target.OwnerID, siteName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	actorKind, keyID := auditActorKind(r.Context())
	event := audit.Event{
		ActorID: target.ActorID, ActorKind: actorKind, KeyID: keyID,
		OwnerID: target.OwnerID, SiteID: site.ID, RequestID: auditRequestID(r.Context()),
	}
	status := http.StatusOK
	var note string
	if req.Level == db.AccessNetwork {
		if err := db.RequestNetworkAccess(r.Context(), tx, site.ID, target.ActorID, reason); err != nil {
			if writeSiteRestricted(w, err) {
				return
			}
			if errors.Is(err, db.ErrAlreadyNetwork) {
				writeJSON(w, http.StatusConflict, errorResponse{Error: "the site is already open to the network", Code: "already_network"})
				return
			}
			log.Printf("request network access %s/%s: %v", target.OwnerUsername, siteName, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
		event.Action, event.Extra = "network_access_requested", map[string]any{"reason": reason}
		status = http.StatusAccepted
		note = "Network access requested. An admin must approve it; until then the site keeps its current access level."
		if requiredNetworkApprovals(h.networkApprovals) == 2 {
			note = "Network access requested. Two admins must approve it; until then the site keeps its current access level."
		}
	} else {
		previous, err := db.SetSiteAccess(r.Context(), tx, site.ID, req.Level)
		if err != nil {
			if writeSiteRestricted(w, err) {
				return
			}
			log.Printf("set access %s/%s: %v", target.OwnerUsername, siteName, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
		event.Action, event.Extra = "site_access", map[string]any{"from": previous, "to": req.Level}
		if previous == db.AccessNetwork {
			// Dropping off the network is its own audited fact: an approval
			// an admin gave has just been given back.
			reverted := event
			reverted.Action, reverted.Extra = "network_access_reverted", map[string]any{"to": req.Level}
			if err := h.audit.RecordTx(r.Context(), tx, reverted); err != nil {
				log.Printf("record audit for network_access_reverted %s/%s: %v", target.OwnerUsername, siteName, err)
				writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
				return
			}
		}
		note = accessLevelNote(req.Level)
	}
	if err := h.audit.RecordTx(r.Context(), tx, event); err != nil {
		log.Printf("record audit for %s %s/%s: %v", event.Action, target.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := audit.Commit(tx); err != nil {
		log.Printf("commit access %s/%s: %v", target.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	access, err := db.ResolveSiteAccess(r.Context(), h.database, target.ActorID, target.OwnerUsername, siteName)
	if err != nil {
		log.Printf("reload site after access change %s/%s: %v", target.OwnerUsername, siteName, err)
		writeJSON(w, status, map[string]string{"note": note})
		return
	}
	response := struct {
		collaborationSiteResponse
		Note string `json:"note"`
	}{h.collaborationSiteResponse(r, access.Site, access.OwnerUsername, access.Role, db.SiteAnalyticsSummary{}, nil), note}
	setSiteETag(w, access.Site)
	writeJSON(w, status, response)
}

// writeSiteRestricted answers a db.SiteRestrictedError: the site's owner or
// team tried to open up a site an admin restricted. It reports whether it
// wrote a response.
func writeSiteRestricted(w http.ResponseWriter, err error) bool {
	var restricted *db.SiteRestrictedError
	if !errors.As(err, &restricted) {
		return false
	}
	message := "An admin restricted this site to only you (or your team); only an admin can lift it."
	if restricted.Reason != "" {
		message = "An admin restricted this site to only you (or your team): " + restricted.Reason + ". Only an admin can lift it."
	}
	writeJSON(w, http.StatusConflict, struct {
		Error  string `json:"error"`
		Code   string `json:"code"`
		Reason string `json:"reason,omitempty"`
	}{message, "site_restricted_by_admin", restricted.Reason})
	return true
}

func accessLevelNote(level string) string {
	switch level {
	case db.AccessOnlyMe:
		return "Only you (for a team site, the team's members) can open the site."
	case db.AccessSpecific:
		return "Only the named viewers, plus you or the team, can open the site, at its own address."
	case db.AccessCompany:
		return "Anyone signed in with the link can open the site. It is not listed."
	case db.AccessListed:
		return "Anyone signed in can open the site, and it is listed in the showcase and search."
	}
	return ""
}

// --- Saved-data history ---

type stateHistoryResponse struct {
	ID        int64     `json:"id"`
	Version   int64     `json:"version"`
	WrittenBy string    `json:"written_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Bytes     int       `json:"bytes"`
	State     any       `json:"state,omitempty"`
}

func (h *SiteHandler) registerStateHistoryRoutes(mux *http.ServeMux, ownerMutation func(http.Handler) http.Handler, browserWrite func(http.Handler) http.Handler) {
	mux.Handle("GET /api/collaboration/sites/{owner}/{sitename}/state-versions", ownerMutation(http.HandlerFunc(h.listStateVersions)))
	mux.Handle("GET /api/collaboration/sites/{owner}/{sitename}/state-versions/{id}", ownerMutation(http.HandlerFunc(h.getStateVersion)))
	mux.Handle("POST /api/collaboration/sites/{owner}/{sitename}/state-versions/{id}/restore", browserWrite(ownerMutation(http.HandlerFunc(h.restoreStateVersion))))
}

// stateHistoryAccess resolves the site for the saved-data history routes:
// the owner or a member of the owning team.
func (h *SiteHandler) stateHistoryAccess(w http.ResponseWriter, r *http.Request) (db.SiteAccess, bool) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return db.SiteAccess{}, false
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok || !requireOwnerRole(w, access) {
		return db.SiteAccess{}, false
	}
	return access, true
}

func stateVersionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid state version id"})
		return 0, false
	}
	return id, true
}

func toStateHistoryResponse(e db.StateHistoryEntry) stateHistoryResponse {
	out := stateHistoryResponse{ID: e.ID, Version: e.StateVersion, WrittenBy: e.WrittenBy, CreatedAt: e.CreatedAt, Bytes: e.Bytes}
	if e.State != nil {
		out.State = e.State
	}
	return out
}

func (h *SiteHandler) listStateVersions(w http.ResponseWriter, r *http.Request) {
	access, ok := h.stateHistoryAccess(w, r)
	if !ok {
		return
	}
	entries, err := db.ListStateHistory(r.Context(), h.database, access.Site.ID)
	if err != nil {
		log.Printf("list state history %s/%s: %v", access.OwnerUsername, access.Site.Name, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	out := make([]stateHistoryResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, toStateHistoryResponse(e))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *SiteHandler) getStateVersion(w http.ResponseWriter, r *http.Request) {
	access, ok := h.stateHistoryAccess(w, r)
	if !ok {
		return
	}
	id, ok := stateVersionID(w, r)
	if !ok {
		return
	}
	entry, err := db.GetStateHistory(r.Context(), h.database, access.Site.ID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "state version not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, toStateHistoryResponse(entry))
}

func (h *SiteHandler) restoreStateVersion(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	access, ok := h.stateHistoryAccess(w, r)
	if !ok {
		return
	}
	id, ok := stateVersionID(w, r)
	if !ok {
		return
	}
	if decision := h.limits.allow(managementUserPolicy, user.ID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer audit.Rollback(tx)
	version, err := db.RestoreStateHistory(r.Context(), tx, access.Site.ID, id, user.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "state version not found"})
			return
		}
		log.Printf("restore state %s/%s #%d: %v", access.OwnerUsername, access.Site.Name, id, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	actorKind, keyID := auditActorKind(r.Context())
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: user.ID, ActorKind: actorKind, KeyID: keyID,
		Action: "state_restore", OwnerID: access.OwnerID, SiteID: access.Site.ID,
		RequestID: auditRequestID(r.Context()),
		Extra:     map[string]any{"history_id": id, "version": version},
	}); err != nil {
		log.Printf("record audit for state_restore %s/%s: %v", access.OwnerUsername, access.Site.Name, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := audit.Commit(tx); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, versionedStateSavedResponse{Version: version})
}

// --- Admin: network access requests ---

func (h *AdminHandler) registerAccessRequestRoutes(mux *http.ServeMux, adminAPI, dashboardCheck func(http.Handler) http.Handler) {
	mux.Handle("POST /api/admin/access-requests/{owner}/{sitename}/approve", dashboardCheck(adminAPI(http.HandlerFunc(h.approveAccessRequest))))
	mux.Handle("POST /api/admin/access-requests/{owner}/{sitename}/decline", dashboardCheck(adminAPI(http.HandlerFunc(h.declineAccessRequest))))
	mux.Handle("POST /api/admin/access-requests/{owner}/{sitename}/revoke", dashboardCheck(adminAPI(http.HandlerFunc(h.revokeNetworkAccess))))
	mux.Handle("POST /api/admin/sites/{owner}/{sitename}/restrict", dashboardCheck(adminAPI(http.HandlerFunc(h.restrictSite))))
	mux.Handle("POST /api/admin/sites/{owner}/{sitename}/unrestrict", dashboardCheck(adminAPI(http.HandlerFunc(h.unrestrictSite))))
}

// adminNote reads an admin's one-line note ("reason") from a dashboard
// form or a JSON body, whitespace collapsed to one line, at most 500
// characters. required refuses an empty one. On false the response has
// been written.
func (h *AdminHandler) adminNote(w http.ResponseWriter, r *http.Request, required bool) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, smallJSONBodyBytes)
	var note string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			h.respondAdmin(w, r, http.StatusBadRequest, "invalid request body")
			return "", false
		}
		note = body.Reason
	} else {
		if err := r.ParseForm(); err != nil {
			h.respondAdmin(w, r, http.StatusBadRequest, "invalid request body")
			return "", false
		}
		note = r.PostForm.Get("reason")
	}
	note = strings.Join(strings.Fields(note), " ")
	if required && note == "" {
		h.respondAdmin(w, r, http.StatusBadRequest, "a reason is required")
		return "", false
	}
	if utf8.RuneCountInString(note) > maxNetworkReasonRunes {
		h.respondAdmin(w, r, http.StatusBadRequest, "reason is too long (at most 500 characters)")
		return "", false
	}
	return note, true
}

func (h *AdminHandler) approveAccessRequest(w http.ResponseWriter, r *http.Request) {
	h.decideNetworkAccess(w, r, "network_access_approved")
}

func (h *AdminHandler) declineAccessRequest(w http.ResponseWriter, r *http.Request) {
	h.decideNetworkAccess(w, r, "network_access_declined")
}

func (h *AdminHandler) revokeNetworkAccess(w http.ResponseWriter, r *http.Request) {
	h.decideNetworkAccess(w, r, "network_access_reverted")
}

// decideNetworkAccess approves or declines a pending request, or takes an
// approved site back off the network (to company, where a shared link keeps
// working for signed-in people). Each writes its audit row in the same
// transaction. With NETWORK_ACCESS_APPROVALS=2 an approval that is not yet
// the second is recorded and audited as network_access_approval_added; the
// one that opens the site is network_access_approved. The requester can
// never approve their own request.
func (h *AdminHandler) decideNetworkAccess(w http.ResponseWriter, r *http.Request, action string) {
	actor := auth.GetUser(r.Context())
	required := requiredNetworkApprovals(h.networkApprovals)
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	// Declining and revoking take an optional note the owner is shown.
	var note string
	if action != "network_access_approved" {
		if note, ok = h.adminNote(w, r, false); !ok {
			return
		}
	}
	owner, err := db.GetUserByUsername(r.Context(), h.database, ownerUsername)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			h.respondAdmin(w, r, http.StatusNotFound, "site not found")
			return
		}
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	defer audit.Rollback(tx)
	if err := db.LockSiteCollaboration(r.Context(), tx, owner.ID, siteName); err != nil {
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	site, err := db.GetSite(r.Context(), tx, owner.ID, siteName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			h.respondAdmin(w, r, http.StatusNotFound, "site not found")
			return
		}
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	extra := map[string]any{}
	switch action {
	case "network_access_approved":
		var approval db.NetworkApproval
		approval, err = db.ApproveNetworkAccess(r.Context(), tx, site.ID, actor.ID, required)
		if err == nil {
			extra["approval"], extra["required"] = approval.Approvals, required
			switch {
			case approval.Approved:
				extra["from"] = approval.Previous
			case approval.Duplicate:
				// Nothing changed: this admin's approval is already counted.
				h.respondAdmin(w, r, http.StatusOK, fmt.Sprintf("already approved (%d of %d): %s/%s", approval.Approvals, required, ownerUsername, siteName))
				return
			default:
				action = "network_access_approval_added"
			}
		}
	case "network_access_declined":
		err = db.DeclineNetworkAccess(r.Context(), tx, site.ID, note)
	case "network_access_reverted":
		var open bool
		open, err = db.NetworkOpen(r.Context(), tx, site.ID)
		if err == nil && !open {
			h.respondAdmin(w, r, http.StatusConflict, "the site is not open to the network")
			return
		}
		if err == nil {
			_, err = db.SetSiteAccess(r.Context(), tx, site.ID, db.AccessCompany)
			extra["to"] = db.AccessCompany
		}
		if err == nil {
			err = db.RecordNetworkRevoked(r.Context(), tx, site.ID, note)
		}
	}
	if note != "" {
		extra["reason"] = note
	}
	if err != nil {
		if errors.Is(err, db.ErrNoPendingRequest) {
			h.respondAdmin(w, r, http.StatusConflict, "no pending request for this site")
			return
		}
		if errors.Is(err, db.ErrSelfApproval) {
			h.respondAdmin(w, r, http.StatusForbidden, "you made this request; another admin must approve it")
			return
		}
		log.Printf("admin: %s %s/%s: %v", action, ownerUsername, siteName, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	actorKind, keyID := auditActorKind(r.Context())
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: actor.ID, ActorKind: actorKind, KeyID: keyID,
		Action: action, OwnerID: owner.ID, SiteID: site.ID,
		RequestID: auditRequestID(r.Context()), Extra: extra,
	}); err != nil {
		log.Printf("admin: record audit for %s %s/%s: %v", action, ownerUsername, siteName, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	if err := audit.Commit(tx); err != nil {
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	status := strings.TrimPrefix(action, "network_access_")
	if action == "network_access_approval_added" {
		status = fmt.Sprintf("approval %d of %d", extra["approval"], required)
	}
	h.respondAdmin(w, r, http.StatusOK, fmt.Sprintf("%s: %s/%s", status, ownerUsername, siteName))
}

func (h *AdminHandler) restrictSite(w http.ResponseWriter, r *http.Request) {
	h.setSiteRestriction(w, r, true)
}

func (h *AdminHandler) unrestrictSite(w http.ResponseWriter, r *http.Request) {
	h.setSiteRestriction(w, r, false)
}

// setSiteRestriction is an admin's take-down of any site, and its undo.
// Restricting sets the site to only_me (the owner, or the owning team's
// members) with a required one-line reason the owner sees on their
// dashboard. It is sticky: until an admin unrestricts it, the owner or team
// cannot raise the level, ask for network access or add viewers
// (db.SiteRestrictedError). Unrestricting restores the level the
// site had (company for a site that was open to the network). Each is
// audited in the same transaction: site_restricted, site_restriction_lifted.
func (h *AdminHandler) setSiteRestriction(w http.ResponseWriter, r *http.Request, restrict bool) {
	actor := auth.GetUser(r.Context())
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	var reason string
	if restrict {
		if reason, ok = h.adminNote(w, r, true); !ok {
			return
		}
	}
	owner, err := db.GetUserByUsername(r.Context(), h.database, ownerUsername)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			h.respondAdmin(w, r, http.StatusNotFound, "site not found")
			return
		}
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	defer audit.Rollback(tx)
	if err := db.LockSiteCollaboration(r.Context(), tx, owner.ID, siteName); err != nil {
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	site, err := db.GetSite(r.Context(), tx, owner.ID, siteName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			h.respondAdmin(w, r, http.StatusNotFound, "site not found")
			return
		}
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	action, extra := "site_restricted", map[string]any{}
	if restrict {
		var previous string
		previous, err = db.RestrictSite(r.Context(), tx, site.ID, reason)
		extra["from"], extra["reason"] = previous, reason
	} else {
		action = "site_restriction_lifted"
		var level string
		level, err = db.LiftSiteRestriction(r.Context(), tx, site.ID)
		extra["to"] = level
	}
	if err != nil {
		if errors.Is(err, db.ErrNotRestricted) {
			h.respondAdmin(w, r, http.StatusConflict, "the site is not restricted by an admin")
			return
		}
		log.Printf("admin: %s %s/%s: %v", action, ownerUsername, siteName, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	actorKind, keyID := auditActorKind(r.Context())
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: actor.ID, ActorKind: actorKind, KeyID: keyID,
		Action: action, OwnerID: owner.ID, SiteID: site.ID,
		RequestID: auditRequestID(r.Context()), Extra: extra,
	}); err != nil {
		log.Printf("admin: record audit for %s %s/%s: %v", action, ownerUsername, siteName, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	if err := audit.Commit(tx); err != nil {
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	refreshSiteManifest(r.Context(), h.database, h.store, site.ID)
	status := "restricted"
	if !restrict {
		status = "restriction lifted"
	}
	h.respondAdmin(w, r, http.StatusOK, fmt.Sprintf("%s: %s/%s", status, ownerUsername, siteName))
}

// accessLevelLabel is a level's short name on the admin page.
func accessLevelLabel(level string) string {
	switch level {
	case db.AccessOnlyMe:
		return "only me"
	case db.AccessSpecific:
		return "specific people"
	case db.AccessCompany:
		return "company"
	case db.AccessListed:
		return "listed"
	case db.AccessNetwork:
		return "network"
	}
	return level
}

// adminNotePrompt is a form's onsubmit: ask for a note (Cancel aborts, an
// empty note is allowed) and put it in the form's hidden reason field.
func adminNotePrompt(question string) string {
	quoted, _ := json.Marshal(question)
	return "var n = prompt(" + string(quoted) + ", ''); if (n === null) return false; this.reason.value = n; return true;"
}

// renderAccessRequests is the admin page's "Access requests" section: every
// pending request to open a site to the network, with approve and decline,
// and every site already open, with revoke. Nothing is rendered when there
// is neither. A pending request shows who has approved it so far when two
// approvals are required; viewer (the admin looking) gets no approve button
// on a request they made or have already approved.
func (h *AdminHandler) renderAccessRequests(r *http.Request, b *strings.Builder, viewer *db.User) {
	required := requiredNetworkApprovals(h.networkApprovals)
	entries, err := db.ListNetworkAccess(r.Context(), h.database)
	if err != nil {
		log.Printf("admin: list access requests: %v", err)
		return
	}
	if len(entries) == 0 {
		return
	}
	b.WriteString(`<section id="access-requests" class="overview"><div class="overview-card"><h2 class="section-title">Access requests</h2>
<p class="login-copy">Owners asking to open a site to anyone on the network, with no sign-in. Anonymous visitors can read its pages and saved data but cannot change anything.</p>
<div class="rank-list" role="region" aria-label="Access requests">`)
	for _, e := range entries {
		base := "/api/admin/access-requests/" + url.PathEscape(e.Owner) + "/" + url.PathEscape(e.SiteName)
		link := h.hosts.SiteURL(e.Owner, e.SiteName)
		var detail, actions string
		if e.RequestedAt != nil {
			detail = fmt.Sprintf("requested by %s · now %s · %s", html.EscapeString(e.RequestedBy), html.EscapeString(accessLevelLabel(e.Access)), localTimeHTML(*e.RequestedAt, "datetime"))
			approvedByViewer := false
			if required > 1 {
				names := make([]string, 0, len(e.ApprovedBy))
				for _, a := range e.ApprovedBy {
					names = append(names, html.EscapeString(a.Name))
					approvedByViewer = approvedByViewer || (viewer != nil && a.AdminID == viewer.ID)
				}
				progress := fmt.Sprintf("%d of %d approvals", len(e.ApprovedBy), required)
				if len(names) > 0 {
					progress += ": " + strings.Join(names, ", ")
				}
				detail += " · " + progress
			}
			approve := fmt.Sprintf(`<form method="POST" action="%s/approve"><button type="submit" class="btn-reset">Approve</button></form>`, html.EscapeString(base))
			switch {
			case viewer != nil && e.RequestedByID == viewer.ID:
				approve = `<span class="rank-sub">your request</span>`
			case approvedByViewer:
				approve = `<span class="rank-sub">you approved</span>`
			}
			actions = approve + fmt.Sprintf(`<form method="POST" action="%s/decline" onsubmit="%s"><input type="hidden" name="reason"><button type="submit" class="btn-reject">Decline</button></form>`,
				html.EscapeString(base), html.EscapeString(adminNotePrompt("Decline this request? Add a note for the owner (optional):")))
		} else {
			detail = "open to the network"
			actions = fmt.Sprintf(`<form method="POST" action="%s/revoke" onsubmit="%s"><input type="hidden" name="reason"><button type="submit" class="btn-reject">Revoke</button></form>`,
				html.EscapeString(base), html.EscapeString(adminNotePrompt("Take this site off the network? Signed-in people keep access. Add a note for the owner (optional):")))
		}
		fmt.Fprintf(b, `<div class="rank-row"><span class="rank-name"><a href="%s" target="_blank" rel="noopener">%s/%s</a> <span class="rank-sub">%s</span>`,
			html.EscapeString(link), html.EscapeString(e.Owner), html.EscapeString(e.SiteName), detail)
		if e.Reason != "" {
			fmt.Fprintf(b, `<span class="rank-sub">“%s”</span>`, html.EscapeString(e.Reason))
		}
		fmt.Fprintf(b, `</span><span class="rank-metric">%s</span></div>`, actions)
	}
	b.WriteString(`</div></div></section>`)
}
