package handler

import (
	"archive/zip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/safepath"
)

type collaborationSiteResponse struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	OwnerUsername string `json:"owner_username"`
	// OwnerID is the namespace's immutable id. A project marker records it so
	// an agent can tell "the same namespace" from "a name that changed hands"
	// — a team deleted and its name re-registered resolves to a different id.
	OwnerID       string               `json:"owner_id"`
	AccessRole    db.CollaborationRole `json:"access_role"`
	ActiveVersion int                  `json:"active_version"`
	Public        bool                 `json:"public"`
	// PublicPath is the address the site should be handed out under. It was
	// the relative long path on the base host; after the subdomain cutover
	// it is the absolute short address on the owner's host. Clients use it
	// as returned rather than prefixing it with the server origin.
	PublicPath string    `json:"public_path"`
	URL        string    `json:"url"`
	ETag       string    `json:"etag"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	Analytics  analytics `json:"analytics"`
}

type collaborationVersionResponse struct {
	VersionNumber int       `json:"version_number"`
	Status        string    `json:"status"`
	UploadedBy    *string   `json:"uploaded_by,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type siteEditorResponse struct {
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
}

type editorCandidateResponse struct {
	Username      string `json:"username"`
	AlreadyEditor bool   `json:"already_editor"`
}

type grantEditorsRequest struct {
	Usernames []string `json:"usernames"`
}

type collaborationPreconditionResponse struct {
	Error         string `json:"error"`
	ActiveVersion int    `json:"active_version"`
	ETag          string `json:"etag"`
}

type archiveEntry struct {
	path string
	info fs.FileInfo
}

const archiveDownloadTimeout = 5 * time.Minute

func validatedCollaborationPath(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	ownerUsername := r.PathValue("owner")
	siteName := r.PathValue("sitename")
	if safepath.ValidateSegment(ownerUsername) != nil || safepath.ValidateSegment(siteName) != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid site path"})
		return "", "", false
	}
	return ownerUsername, siteName, true
}

func formatSiteETag(siteID string, activeVersion int) string {
	return fmt.Sprintf("\"site-%s-v%d\"", siteID, activeVersion)
}

func setSiteETag(w http.ResponseWriter, site db.Site) string {
	etag := formatSiteETag(site.ID, site.ActiveVersion)
	w.Header().Set("ETag", etag)
	return etag
}

func requireSitePrecondition(w http.ResponseWriter, r *http.Request, site db.Site, required bool) bool {
	current := setSiteETag(w, site)
	provided := strings.TrimSpace(r.Header.Get("If-Match"))
	if provided == "" {
		if !required {
			return true
		}
		writeJSON(w, http.StatusPreconditionRequired, collaborationPreconditionResponse{
			Error: "If-Match is required for this site", ActiveVersion: site.ActiveVersion, ETag: current,
		})
		return false
	}
	if provided != current {
		writeJSON(w, http.StatusPreconditionFailed, collaborationPreconditionResponse{
			Error: "site version changed", ActiveVersion: site.ActiveVersion, ETag: current,
		})
		return false
	}
	return true
}

func (h *SiteHandler) resolveCollaborationAccess(w http.ResponseWriter, r *http.Request, ownerUsername, siteName string) (db.SiteAccess, bool) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return db.SiteAccess{}, false
	}
	access, err := db.ResolveSiteAccess(r.Context(), h.database, user.ID, ownerUsername, siteName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
			return db.SiteAccess{}, false
		}
		log.Printf("resolve collaboration access %s/%s for actor %s: %v", ownerUsername, siteName, user.ID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return db.SiteAccess{}, false
	}
	if !validateStoredUsername(w, access.OwnerUsername) {
		return db.SiteAccess{}, false
	}
	return access, true
}

// requireOwnerRole gates the editor-management routes. It admits the site's
// owner and any member of the team that owns it: a team has one role, so a
// member is trusted with the team's sites exactly as an owner is trusted with
// their own. An editor grant never manages other editors.
//
// All four editor gates must move together — listSiteEditors,
// searchEditorCandidates, the grant/revoke preliminary check (all three
// through here) and the in-transaction recheck in mutateSiteEditors. Widening
// only the ones that write leaves the share dialog half-open: a member could
// grant an editor and then get a 403 listing or searching them.
func requireOwnerRole(w http.ResponseWriter, access db.SiteAccess) bool {
	if access.Role != db.CollaborationRoleOwner && access.Role != db.CollaborationRoleMember {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: "only the site owner or a member of the owning team can manage editors"})
		return false
	}
	return true
}

// collaborationSiteResponse reports a site's address on its own owner's
// host, restricted or not (design.md 5.2a): the owner passed in is the
// site's own owner, so a site shared with somebody else is still reported
// under its owner's host.
func (h *SiteHandler) collaborationSiteResponse(r *http.Request, site db.Site, ownerUsername string, role db.CollaborationRole, summary db.SiteAnalyticsSummary, downloads map[string]db.FileDownloadStat) collaborationSiteResponse {
	base := toSiteResponse(site, h.siteURL(r.Context(), ownerUsername, site.Name, site.ID), "", summary, downloads)
	return collaborationSiteResponse{
		ID:            site.ID,
		Name:          site.Name,
		OwnerUsername: ownerUsername,
		OwnerID:       site.UserID,
		AccessRole:    role,
		ActiveVersion: site.ActiveVersion,
		Public:        site.Public,
		PublicPath:    base.URL,
		URL:           base.URL,
		ETag:          formatSiteETag(site.ID, site.ActiveVersion),
		CreatedAt:     site.CreatedAt,
		UpdatedAt:     site.UpdatedAt,
		Analytics:     base.Analytics,
	}
}

func (h *SiteHandler) listCollaborationSites(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	if decision := h.limits.allow(managementUserPolicy, user.ID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	sites, err := db.ListAccessibleSites(r.Context(), h.database, user.ID)
	if err != nil {
		log.Printf("list collaboration sites for actor %s: %v", user.ID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	siteIDs := make([]string, 0, len(sites))
	for _, accessible := range sites {
		siteIDs = append(siteIDs, accessible.Site.ID)
	}
	summaries, err := db.ListSiteAnalyticsSummariesForSites(r.Context(), h.database, 7, siteIDs)
	if err != nil {
		log.Printf("list collaboration analytics for actor %s: %v", user.ID, err)
		summaries = make(map[string]db.SiteAnalyticsSummary)
	}
	downloads, err := db.ListSiteFileDownloadSummaries(r.Context(), h.database, 7, siteIDs)
	if err != nil {
		log.Printf("list collaboration downloads for actor %s: %v", user.ID, err)
		downloads = make(map[string]map[string]db.FileDownloadStat)
	}
	response := make([]collaborationSiteResponse, 0, len(sites))
	for _, accessible := range sites {
		if !validateStoredUsername(w, accessible.OwnerUsername) {
			return
		}
		response = append(response, h.collaborationSiteResponse(
			r,
			accessible.Site,
			accessible.OwnerUsername,
			accessible.AccessRole,
			summaries[accessible.Site.ID],
			downloads[accessible.Site.ID],
		))
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *SiteHandler) getCollaborationSite(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok {
		return
	}
	summaries, err := db.ListSiteAnalyticsSummariesForSites(r.Context(), h.database, 7, []string{access.Site.ID})
	if err != nil {
		log.Printf("get collaboration analytics for %s/%s: %v", ownerUsername, siteName, err)
		summaries = make(map[string]db.SiteAnalyticsSummary)
	}
	downloads, err := db.ListSiteFileDownloadSummaries(r.Context(), h.database, 7, []string{access.Site.ID})
	if err != nil {
		log.Printf("get collaboration downloads for %s/%s: %v", ownerUsername, siteName, err)
		downloads = make(map[string]map[string]db.FileDownloadStat)
	}
	setSiteETag(w, access.Site)
	writeJSON(w, http.StatusOK, h.collaborationSiteResponse(
		r, access.Site, access.OwnerUsername, access.Role, summaries[access.Site.ID], downloads[access.Site.ID],
	))
}

func (h *SiteHandler) listCollaborationVersions(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok {
		return
	}
	versions, err := db.ListVersions(r.Context(), h.database, access.Site.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	response := make([]collaborationVersionResponse, 0, len(versions))
	for _, version := range versions {
		response = append(response, collaborationVersionResponse{
			VersionNumber: version.VersionNumber,
			Status:        version.Status,
			UploadedBy:    version.UploaderUsername,
			CreatedAt:     version.CreatedAt,
		})
	}
	setSiteETag(w, access.Site)
	writeJSON(w, http.StatusOK, response)
}

func (h *SiteHandler) updateCollaborationSite(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	actor := auth.GetUser(r.Context())
	if actor == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	if decision := h.limits.allow(managementUserPolicy, actor.ID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	preliminary, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok {
		return
	}
	files, err := h.readAndValidateFiles(w, r, siteName)
	if err != nil {
		return
	}

	unlock := h.mutations.lock(preliminary.OwnerID, siteName)
	defer unlock()
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer tx.Rollback()
	if err := db.LockSiteCollaboration(r.Context(), tx, preliminary.OwnerID, siteName); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	access, err := db.ResolveSiteAccess(r.Context(), tx, actor.ID, ownerUsername, siteName)
	if err != nil || access.Site.ID != preliminary.Site.ID {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("recheck collaboration update access %s/%s: %v", ownerUsername, siteName, err)
		}
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
		return
	}
	if !requireSitePrecondition(w, r, access.Site, true) {
		return
	}
	previousVersion := access.Site.ActiveVersion
	maxVersion, err := db.GetMaxVersionNumber(r.Context(), tx, access.Site.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	versionNumber := maxVersion + 1
	versionPrefix := siteVersionPrefix(access.OwnerUsername, siteName, versionNumber)
	version, err := db.CreateVersion(r.Context(), tx, access.Site.ID, versionNumber, versionPrefix, &actor.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	if _, err := h.store.PutVersion(r.Context(), access.Site.ID, versionNumber, files); err != nil {
		log.Printf("upload collaboration files for %s/%s v%d actor=%s: %v", access.OwnerUsername, siteName, versionNumber, actor.ID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	keepObject := false
	defer func() {
		if !keepObject {
			h.discardVersion(access.Site.ID, versionNumber)
		}
	}()
	if err := db.ActivateVersion(r.Context(), tx, version.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := db.UpdateSiteActiveVersion(r.Context(), tx, access.Site.ID, versionNumber); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := db.EnqueueSiteSearch(r.Context(), tx, access.Site.ID, db.SiteSearchReconcile); err != nil {
		log.Printf("enqueue collaboration search reconcile for %s/%s: %v", access.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	actorKind, keyID := auditActorKind(r.Context())
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: actor.ID, ActorKind: actorKind, KeyID: keyID,
		Action: "site_update", OwnerID: access.OwnerID, SiteID: access.Site.ID,
		RequestID: auditRequestID(r.Context()),
		Extra:     map[string]any{"version": versionNumber, "previous_version": previousVersion},
	}); err != nil {
		log.Printf("record audit for site_update %s/%s: %v", access.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if commitErr := tx.Commit(); commitErr != nil {
		result := reconcileSiteCommit(h.database, access.OwnerID, siteName, siteCommitExpectation{
			applied:    existingSiteCommitSnapshot(access.Site.ID, versionNumber),
			rolledBack: existingSiteCommitSnapshot(access.Site.ID, previousVersion),
		})
		logSiteCommitOutcome("collaboration_update", access.OwnerID, siteName, previousVersion, versionNumber, commitErr, result)
		keepObject = result.outcome != siteCommitRolledBack
		if result.outcome != siteCommitApplied {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
	}
	keepObject = true
	access.Site.ActiveVersion = versionNumber
	access.Site.UpdatedAt = time.Now().UTC()
	if err := h.cleanupOldVersions(r.Context(), access.OwnerID, access.OwnerUsername, siteName, access.Site.ID); err != nil {
		log.Printf("cleanup collaboration versions for %s/%s: %v", access.OwnerUsername, siteName, err)
	}
	setSiteETag(w, access.Site)
	writeJSON(w, http.StatusOK, h.collaborationSiteResponse(
		r, access.Site, access.OwnerUsername, access.Role, db.SiteAnalyticsSummary{}, nil,
	))
}

func (h *SiteHandler) rollbackCollaborationSite(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	actor := auth.GetUser(r.Context())
	if actor == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	if decision := h.limits.allow(managementUserPolicy, actor.ID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	var request rollbackSiteRequest
	if !decodeSmallJSON(w, r, &request) {
		return
	}
	if request.Version < 1 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "version must be at least 1"})
		return
	}
	preliminary, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok {
		return
	}
	unlock := h.mutations.lock(preliminary.OwnerID, siteName)
	defer unlock()
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer tx.Rollback()
	if err := db.LockSiteCollaboration(r.Context(), tx, preliminary.OwnerID, siteName); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	access, err := db.ResolveSiteAccess(r.Context(), tx, actor.ID, ownerUsername, siteName)
	if err != nil || access.Site.ID != preliminary.Site.ID {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("recheck collaboration rollback access %s/%s: %v", ownerUsername, siteName, err)
		}
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
		return
	}
	if !requireSitePrecondition(w, r, access.Site, true) {
		return
	}
	previousVersion := access.Site.ActiveVersion
	versions, err := db.ListVersions(r.Context(), tx, access.Site.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if !versionExists(versions, request.Version) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "version not found"})
		return
	}

	if err := db.UpdateSiteActiveVersion(r.Context(), tx, access.Site.ID, request.Version); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := db.EnqueueSiteSearch(r.Context(), tx, access.Site.ID, db.SiteSearchReconcile); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	actorKind, keyID := auditActorKind(r.Context())
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: actor.ID, ActorKind: actorKind, KeyID: keyID,
		Action: "site_rollback", OwnerID: access.OwnerID, SiteID: access.Site.ID,
		RequestID: auditRequestID(r.Context()),
		Extra:     map[string]any{"version": request.Version, "previous_version": previousVersion},
	}); err != nil {
		log.Printf("record audit for site_rollback %s/%s: %v", access.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if commitErr := tx.Commit(); commitErr != nil {
		result := reconcileSiteCommit(h.database, access.OwnerID, siteName, siteCommitExpectation{
			applied:    existingSiteCommitSnapshot(access.Site.ID, request.Version),
			rolledBack: existingSiteCommitSnapshot(access.Site.ID, previousVersion),
		})
		logSiteCommitOutcome("collaboration_rollback", access.OwnerID, siteName, previousVersion, request.Version, commitErr, result)
		if result.outcome != siteCommitApplied {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
	}
	access.Site.ActiveVersion = request.Version
	access.Site.UpdatedAt = time.Now().UTC()
	setSiteETag(w, access.Site)
	writeJSON(w, http.StatusOK, h.collaborationSiteResponse(
		r, access.Site, access.OwnerUsername, access.Role, db.SiteAnalyticsSummary{}, nil,
	))
}

func (h *SiteHandler) listSiteEditors(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok || !requireOwnerRole(w, access) {
		return
	}
	editors, err := db.ListSiteEditors(r.Context(), h.database, access.Site.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	response := make([]siteEditorResponse, 0, len(editors))
	for _, editor := range editors {
		response = append(response, siteEditorResponse{Username: editor.Username, CreatedAt: editor.CreatedAt})
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *SiteHandler) searchEditorCandidates(w http.ResponseWriter, r *http.Request) {
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
	candidates, err := db.SearchEditorCandidates(r.Context(), h.database, access.Site.ID, query, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	response := make([]editorCandidateResponse, 0, len(candidates))
	for _, candidate := range candidates {
		response = append(response, editorCandidateResponse{
			Username: candidate.Username, AlreadyEditor: candidate.AlreadyEditor,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *SiteHandler) grantSiteEditors(w http.ResponseWriter, r *http.Request) {
	h.mutateSiteEditors(w, r, true)
}

func (h *SiteHandler) revokeSiteEditor(w http.ResponseWriter, r *http.Request) {
	h.mutateSiteEditors(w, r, false)
}

func (h *SiteHandler) mutateSiteEditors(w http.ResponseWriter, r *http.Request, grant bool) {
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
		var request grantEditorsRequest
		if !decodeSmallJSON(w, r, &request) {
			return
		}
		if len(request.Usernames) == 0 {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "at least one username is required"})
			return
		}
		if len(request.Usernames) > db.MaxSiteEditors {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "too many usernames"})
			return
		}
		usernames = request.Usernames
	} else {
		username := strings.ToLower(strings.TrimSpace(r.PathValue("username")))
		if safepath.ValidateSegment(username) != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid editor username"})
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
	defer tx.Rollback()
	if err := db.LockSiteCollaboration(r.Context(), tx, preliminary.OwnerID, siteName); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	// The fourth editor gate. It must admit exactly what requireOwnerRole
	// admits, or the preliminary check and this one disagree and a member
	// gets a 404 after passing the door.
	locked, err := db.ResolveSiteAccess(r.Context(), tx, user.ID, ownerUsername, siteName)
	if err != nil || locked.Site.ID != preliminary.Site.ID ||
		(locked.Role != db.CollaborationRoleOwner && locked.Role != db.CollaborationRoleMember) {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("recheck editor-management access for %s/%s: %v", ownerUsername, siteName, err)
		}
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
		return
	}

	var editors []db.SiteEditor
	if grant {
		editors, err = db.GrantSiteEditors(r.Context(), tx, locked.OwnerID, siteName, locked.Site.ID, &user.ID, usernames)
	} else {
		var removed bool
		removed, err = db.RevokeSiteEditor(r.Context(), tx, locked.OwnerID, siteName, locked.Site.ID, usernames[0])
		if err == nil && !removed {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "editor not found"})
			return
		}
		if err == nil {
			editors, err = db.ListSiteEditors(r.Context(), tx, locked.Site.ID)
		}
	}
	if err != nil {
		switch {
		case errors.Is(err, db.ErrEditorNotFound):
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "one or more usernames do not exist"})
		case errors.Is(err, db.ErrOwnerCannotBeEditor):
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "the owner cannot be added as an editor"})
		case errors.Is(err, db.ErrEditorLimit):
			writeJSON(w, http.StatusConflict, errorResponse{Error: "a site can have at most 50 editors"})
		case errors.Is(err, sql.ErrNoRows):
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
		default:
			log.Printf("mutate editors for %s/%s: %v", ownerUsername, siteName, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		}
		return
	}
	action := "editor_revoke"
	if grant {
		action = "editor_grant"
	}
	actorKind, keyID := auditActorKind(r.Context())
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: user.ID, ActorKind: actorKind, KeyID: keyID,
		Action: action, OwnerID: locked.OwnerID, SiteID: locked.Site.ID,
		RequestID: auditRequestID(r.Context()),
		Extra:     map[string]any{"usernames": usernames},
	}); err != nil {
		log.Printf("record audit for %s %s/%s: %v", action, ownerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("commit editor mutation for %s/%s: %v", ownerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if !grant {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	response := make([]siteEditorResponse, 0, len(editors))
	for _, editor := range editors {
		response = append(response, siteEditorResponse{Username: editor.Username, CreatedAt: editor.CreatedAt})
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *SiteHandler) downloadCollaborationVersion(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok {
		return
	}
	versionNumber, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || versionNumber < 1 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid version"})
		return
	}
	releaseSlot, acquired := h.limits.acquireArchiveDownload()
	if !acquired {
		writeConcurrencyRateLimit(w)
		return
	}
	defer releaseSlot()

	// Archive admission participates in the same canonical namespace ordering
	// as revoke. Recheck access after both locks are acquired and establish an
	// exact storage pin before releasing them. A request admitted first may
	// finish, but once revoke returns no later archive can pass this boundary.
	unlock := h.mutations.lock(access.OwnerID, siteName)
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer tx.Rollback()
	if err := db.LockSiteCollaboration(r.Context(), tx, access.OwnerID, siteName); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	lockedAccess, err := db.ResolveSiteAccess(r.Context(), tx, access.ActorID, ownerUsername, siteName)
	if err != nil || lockedAccess.Site.ID != access.Site.ID {
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("recheck collaboration archive access %s/%s: %v", ownerUsername, siteName, err)
		}
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
		return
	}
	versions, err := db.ListVersions(r.Context(), tx, lockedAccess.Site.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if !versionExists(versions, versionNumber) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "version not found"})
		return
	}

	lease, err := h.store.OpenVersion(r.Context(), lockedAccess.Site.ID, versionNumber)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "version not found"})
			return
		}
		log.Printf("lease collaboration archive %s/%s v%d: %v", ownerUsername, siteName, versionNumber, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer lease.Close()
	if err := tx.Commit(); err != nil {
		log.Printf("commit collaboration archive admission %s/%s v%d: %v", ownerUsername, siteName, versionNumber, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	// The successful final access check, retained-version validation, and exact
	// storage pin form archive admission. Release namespace ordering before any
	// descriptor walk or response I/O; an admitted stream may finish after a
	// later revoke, while a revoke that won this boundary denied admission above.
	unlock()
	locked = false

	deadline := time.Now().Add(archiveDownloadTimeout)
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(deadline); err != nil {
		log.Printf("set collaboration archive write deadline %s/%s v%d: %v", ownerUsername, siteName, versionNumber, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	streamContext, cancelStream := context.WithDeadline(r.Context(), deadline)
	defer cancelStream()

	entries, err := collectArchiveEntries(streamContext, lease.FS())
	if err != nil {
		log.Printf("inspect collaboration archive %s/%s v%d: %v", ownerUsername, siteName, versionNumber, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": fmt.Sprintf("%s-%s-v%d.zip", ownerUsername, siteName, versionNumber),
	}))
	w.WriteHeader(http.StatusOK)

	zipWriter := zip.NewWriter(w)
	if err := streamArchiveEntries(streamContext, zipWriter, lease.FS(), entries); err != nil {
		log.Printf("stream collaboration archive %s/%s v%d: %v", ownerUsername, siteName, versionNumber, err)
		_ = zipWriter.Close()
		return
	}
	if err := zipWriter.Close(); err != nil {
		log.Printf("finish collaboration archive %s/%s v%d: %v", ownerUsername, siteName, versionNumber, err)
	}
}

func collectArchiveEntries(ctx context.Context, root fs.FS) ([]archiveEntry, error) {
	if root == nil {
		return nil, errors.New("archive root is nil")
	}
	entries := make([]archiveEntry, 0)
	err := fs.WalkDir(root, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported archive entry %q", path)
		}
		entries = append(entries, archiveEntry{path: path, info: info})
		return nil
	})
	return entries, err
}

func streamArchiveEntries(ctx context.Context, destination *zip.Writer, root fs.FS, entries []archiveEntry) error {
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := zip.FileInfoHeader(entry.info)
		if err != nil {
			return err
		}
		header.Name = entry.path
		if entry.info.IsDir() {
			header.Name += "/"
		} else {
			header.Method = zip.Deflate
		}
		writer, err := destination.CreateHeader(header)
		if err != nil {
			return err
		}
		if entry.info.IsDir() {
			continue
		}
		file, err := root.Open(entry.path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(writer, &contextReader{ctx: ctx, reader: file})
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

// requireOwnerOrMember gates the owner-qualified routes that act on a
// namespace rather than on one shared site: create, delete, and listing.
// An editor grant is per-site and says nothing about the namespace, so an
// editor may update the site they were given and nothing else — they cannot
// create a sibling, delete it, or change whether it is listed.
func requireOwnerOrMember(w http.ResponseWriter, role db.CollaborationRole) bool {
	if role != db.CollaborationRoleOwner && role != db.CollaborationRoleMember {
		writeJSON(w, http.StatusForbidden, errorResponse{
			Error: "only the owner or a member of the owning team can do that",
			Code:  "no_access",
		})
		return false
	}
	return true
}

// createCollaborationSite creates a site in a named namespace: the caller's
// own, or a team they belong to. It is the only one of the three that cannot
// go through ResolveSiteAccess, because that resolver starts from a sites row
// and the site does not exist yet — see db.ResolveNamespaceAccess.
func (h *SiteHandler) createCollaborationSite(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	access, err := db.ResolveNamespaceAccess(r.Context(), h.database, user.ID, ownerUsername)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("resolve namespace %q for %s: %v", ownerUsername, user.Username, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
		// Not found rather than forbidden, and deliberately the same answer
		// for "no such namespace" and "not yours": whether a name exists is
		// not something a stranger needs told.
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "namespace not found", Code: "not_found"})
		return
	}
	if !requireOwnerOrMember(w, access.Role) {
		return
	}
	h.createSiteForTarget(w, r, mutationTarget{
		ActorID:       user.ID,
		ActorUsername: user.Username,
		OwnerID:       access.OwnerID,
		OwnerUsername: access.OwnerUsername,
	}, siteName)
}

// deleteCollaborationSite deletes a site from a namespace the caller owns or
// belongs to. Members can delete: a member is trusted with the team's sites
// the way an owner is trusted with their own, and the agent confirms the full
// address first, as it does for owners today.
func (h *SiteHandler) deleteCollaborationSite(w http.ResponseWriter, r *http.Request) {
	target, ok := h.namespaceTarget(w, r)
	if !ok {
		return
	}
	h.deleteSiteForTarget(w, r, target)
}

// setCollaborationSiteVisibility changes whether a team's site is listed.
func (h *SiteHandler) setCollaborationSiteVisibility(w http.ResponseWriter, r *http.Request) {
	target, ok := h.namespaceTarget(w, r)
	if !ok {
		return
	}
	h.setSiteVisibilityForTarget(w, r, target)
}

// namespaceTarget resolves an owner-qualified path to the namespace the
// mutation lands in, having checked the caller may act on the whole namespace
// rather than on one shared site. The site must already exist, so this goes
// through ResolveSiteAccess rather than ResolveNamespaceAccess.
func (h *SiteHandler) namespaceTarget(w http.ResponseWriter, r *http.Request) (mutationTarget, bool) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return mutationTarget{}, false
	}
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return mutationTarget{}, false
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok {
		return mutationTarget{}, false
	}
	if !requireOwnerOrMember(w, access.Role) {
		return mutationTarget{}, false
	}
	return mutationTarget{
		ActorID:       user.ID,
		ActorUsername: user.Username,
		OwnerID:       access.OwnerID,
		OwnerUsername: access.OwnerUsername,
	}, true
}
