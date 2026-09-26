package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/safepath"
	"github.com/vsriram/simple-host/internal/scan"
	"github.com/vsriram/simple-host/internal/storage"
	"github.com/vsriram/simple-host/internal/tarball"
)

const maxSiteArchiveSize = 100 << 20
const maxSiteStateSize = 1 << 20

type SiteHandler struct {
	database      *sql.DB
	store         *storage.Store
	publicBaseURL string
	// hosts decides which address a site is reported under; see HostModel.
	hosts     HostModel
	mutations siteMutationLocks
	limits    *AbuseLimits
	// audit defaults to audit.NoOp{} (see WithAudit); its only caller today
	// is the dashboard's asset-delete route (assets_admin.go), the one
	// mutation this handler owns that requires an audit row
	// for. WithAudit exists rather than a constructor parameter so every
	// existing NewSiteHandler(...) call site (production and test alike)
	// keeps compiling unchanged, the same shape AdminHandler.WithStore
	// already uses.
	audit audit.Recorder
	// networkApprovals is NETWORK_ACCESS_APPROVALS (see access.go).
	networkApprovals int
	// quota and scanner are the upload limits (upload_limits.go): per-owner
	// quotas with version retention, and the optional malware scan (nil
	// when CLAMD_ADDR is unset).
	quota   UploadQuota
	scanner scan.Scanner
}

// WithUploadLimits sets the per-owner quota and the malware scanner (nil for
// none) and returns h for chaining.
func (h *SiteHandler) WithUploadLimits(quota UploadQuota, scanner scan.Scanner) *SiteHandler {
	h.quota = quota
	h.scanner = scanner
	return h
}

// WithAudit attaches an audit recorder and returns h for chaining. Called
// once from main.go, alongside the other handlers' own WithAudit-shaped
// wiring; a handler nobody calls this on keeps auditing through
// audit.NoOp{} (set in NewSiteHandler), which still logs at info level.
func (h *SiteHandler) WithAudit(recorder audit.Recorder) *SiteHandler {
	if recorder != nil {
		h.audit = recorder
	}
	return h
}

type siteResponse struct {
	ID            string    `json:"id"`
	UserID        string    `json:"user_id"`
	Name          string    `json:"name"`
	ActiveVersion int       `json:"active_version"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	URL           string    `json:"url,omitempty"`
	Note          string    `json:"note,omitempty"`
	Analytics     analytics `json:"analytics"`
}

type analytics struct {
	TodayPageviews int64                       `json:"today_pageviews"`
	TodayVisits    int64                       `json:"today_visits"`
	Last7Pageviews int64                       `json:"last_7_pageviews"`
	Last7Visits    int64                       `json:"last_7_visits"`
	FileDownloads  map[string]fileDownloadStat `json:"file_downloads,omitempty"`
}

type fileDownloadStat struct {
	Total int64 `json:"total"`
	Last7 int64 `json:"last_7"`
}

type versionResponse struct {
	ID            string    `json:"id"`
	VersionNumber int       `json:"version_number"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

type rollbackSiteRequest struct {
	Version int `json:"version"`
}

type versionedStateResponse struct {
	Version int64           `json:"version"`
	State   json.RawMessage `json:"state"`
}

type versionedStatePutRequest struct {
	Version *int64          `json:"version"`
	State   json.RawMessage `json:"state"`
}

type versionedStateSavedResponse struct {
	Version int64 `json:"version"`
}

type versionedStateConflictResponse struct {
	Error   string          `json:"error"`
	Version int64           `json:"version"`
	State   json.RawMessage `json:"state"`
}

func NewSiteHandler(database *sql.DB, store *storage.Store, publicBaseURL string, hosts HostModel, limits ...*AbuseLimits) *SiteHandler {
	return &SiteHandler{
		database:      database,
		store:         store,
		publicBaseURL: strings.TrimRight(publicBaseURL, "/"),
		hosts:         hosts,
		limits:        chooseAbuseLimits(limits),
		audit:         audit.NoOp{},
	}
}

// discardVersion deletes an uploaded version whose transaction is known not
// to have committed. Best effort, and deliberately not on the request's
// context: a leftover object is unreferenced and harmless, and is replaced
// if the same version number is allocated again.
func (h *SiteHandler) discardVersion(siteID string, version int) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := h.store.DeleteVersionObject(ctx, siteID, version); err != nil {
		log.Printf("discard uncommitted version %s v%d: %v", siteID, version, err)
	}
}

// stateUsageMarkerKey is shared with SiteAPIHandler.markStateUsage
// (site_api.go), which now owns every state route and so
// owns the fire-and-forget uses_state/uses_versioned_state marker too;
// SiteHandler itself no longer reads or writes state at all.
func stateUsageMarkerKey(siteID string, versioned bool) string {
	if versioned {
		return siteID + "/versioned"
	}
	return siteID + "/simple"
}

// siteURL is the absolute address reported for a site in deploy and rollback
// responses: the short address on the owner's own host, or on the
// restricted site's own host once it has any viewers.
func (h *SiteHandler) siteURL(ctx context.Context, username, siteName, siteID string) string {
	restricted, err := db.IsSiteRestricted(ctx, h.database, siteID)
	if err != nil {
		log.Printf("site url: check restriction for %s/%s: %v", username, siteName, err)
	}
	return h.hosts.SiteURL(username, siteName, restricted)
}

func (h *SiteHandler) Register(mux *http.ServeMux, authMiddleware, skillVersionMiddleware func(http.Handler) http.Handler) {
	// State and assets moved to the site-facing API: they are
	// no longer mux routes at all. The host gate resolves the site from the
	// owner/restricted-site host and the request path directly (never a
	// Referer, which this package no longer reads anywhere) and dispatches
	// straight to SiteAPIHandler — see host_gate.go's serveSiteAPI.

	// Skill-called routes — authenticate before applying the client-version
	// compatibility guard so invalid credentials retain their 401 response.
	//
	// The single-site routes are owner-scoped: they filter by the caller's
	// user_id (a UUID). RequireRealUser is a pass-through today (the
	// ADMIN_API_KEY-backed synthetic admin — a fixed, non-UUID id
	// with no users row — is gone; every principal auth.Middleware produces
	// is a real users row with a real UUID) and is kept only so this call
	// site needs no change. Admin capability now lives entirely in
	// users.is_admin, checked per handler (listSites below, /admin,
	// /api/admin/*), never in a separate credential.
	// listSites intentionally skips RequireRealUser: it has its own IsAdmin
	// branch that returns all sites (god-mode listing is unambiguous).
	owner := func(next http.Handler) http.Handler {
		return authMiddleware(skillVersionMiddleware(auth.RequireRealUser(next)))
	}
	ownerMutation := func(next http.Handler) http.Handler {
		return h.limitManagementClient(owner(next))
	}
	ownerUpload := func(next http.Handler) http.Handler {
		return ownerMutation(h.limitUploadConcurrency(next))
	}
	// Owner-qualified routes name the namespace explicitly. The version
	// middleware is streaming-safe, so archive downloads use the same guard
	// without buffering.
	collaborationArchive := func(next http.Handler) http.Handler {
		return h.limitManagementClient(authMiddleware(skillVersionMiddleware(auth.RequireRealUser(next))))
	}
	// The auth middleware also accepts the browser session cookie, which is an
	// ambient credential: any page that shares it (a hosted site on the same
	// origin today, a per-owner subdomain that is same-site with the base
	// domain later) can drive a write through the viewer's browser. The check
	// runs outermost, before authentication, so a refused request touches no
	// database. It passes anything carrying X-API-Key, so agents and the CLI
	// are untouched — only the cookie path gains the Origin requirement. Reads
	// stay unwrapped: they leak nothing to a cross-origin page that cannot read
	// the response, and the state routes are not cookie-authenticated at all.
	browserWrite := cookieOriginCheck(h.hosts, h.publicBaseURL)
	mux.Handle("POST /api/sites/{sitename}", browserWrite(ownerUpload(http.HandlerFunc(h.createSite))))
	mux.Handle("PUT /api/sites/{sitename}", browserWrite(ownerUpload(http.HandlerFunc(h.updateSite))))
	mux.Handle("DELETE /api/sites/{sitename}", browserWrite(ownerMutation(http.HandlerFunc(h.deleteSite))))
	mux.Handle("POST /api/sites/{sitename}/rollback", browserWrite(ownerMutation(http.HandlerFunc(h.rollbackSite))))
	mux.Handle("POST /api/sites/{sitename}/access", browserWrite(ownerMutation(http.HandlerFunc(h.setSiteAccess))))
	mux.Handle("GET /api/sites/{sitename}/versions", owner(http.HandlerFunc(h.listVersions)))
	mux.Handle("GET /api/sites", h.limitManagementClient(authMiddleware(skillVersionMiddleware(http.HandlerFunc(h.listSites)))))
	mux.Handle("GET /api/collaboration/sites", ownerMutation(http.HandlerFunc(h.listCollaborationSites)))
	mux.Handle("GET /api/collaboration/sites/{owner}/{sitename}", ownerMutation(http.HandlerFunc(h.getCollaborationSite)))
	mux.Handle("PUT /api/collaboration/sites/{owner}/{sitename}", browserWrite(ownerUpload(http.HandlerFunc(h.updateCollaborationSite))))
	mux.Handle("POST /api/collaboration/sites/{owner}/{sitename}/rollback", browserWrite(ownerMutation(http.HandlerFunc(h.rollbackCollaborationSite))))
	mux.Handle("GET /api/collaboration/sites/{owner}/{sitename}/versions", ownerMutation(http.HandlerFunc(h.listCollaborationVersions)))
	mux.Handle("GET /api/collaboration/sites/{owner}/{sitename}/versions/{version}/archive", collaborationArchive(http.HandlerFunc(h.downloadCollaborationVersion)))
	h.registerViewerRoutes(mux, ownerMutation, browserWrite)
	h.registerAssetAdminRoutes(mux, ownerMutation, browserWrite)
	h.registerStateHistoryRoutes(mux, ownerMutation, browserWrite)
	// Namespace-scoped writes: create, delete and access level in a namespace
	// the caller owns or belongs to — see requireOwnerOrMember.
	mux.Handle("POST /api/collaboration/sites/{owner}/{sitename}", browserWrite(ownerUpload(http.HandlerFunc(h.createCollaborationSite))))
	mux.Handle("DELETE /api/collaboration/sites/{owner}/{sitename}", browserWrite(ownerMutation(http.HandlerFunc(h.deleteCollaborationSite))))
	mux.Handle("POST /api/collaboration/sites/{owner}/{sitename}/access", browserWrite(ownerMutation(http.HandlerFunc(h.setCollaborationSiteAccess))))
}

func (h *SiteHandler) limitUploadConcurrency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release, acquired := h.limits.acquireUpload()
		if !acquired {
			writeConcurrencyRateLimit(w)
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

func (h *SiteHandler) limitManagementClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decision := h.limits.allow(managementClientPolicy, clientLimitKey(r)); !decision.Allowed {
			writeRateLimit(w, decision)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func validatedSiteName(w http.ResponseWriter, r *http.Request) (string, bool) {
	siteName := r.PathValue("sitename")
	if siteName == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "site name is required"})
		return "", false
	}
	if err := safepath.ValidateSegment(siteName); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid site name"})
		return "", false
	}
	return siteName, true
}

// validatedNewSiteName is validatedSiteName plus the rule that applies only
// when a site is being created: reservedSiteNames can never have a short URL
// on an owner host, so they are refused here rather than silently unreachable.
// Every other route keeps using validatedSiteName so a site that already has
// one of those names can still be updated, rolled back, and deleted.
func validatedNewSiteName(w http.ResponseWriter, r *http.Request) (string, bool) {
	siteName, ok := validatedSiteName(w, r)
	if !ok {
		return "", false
	}
	if reservedSiteNames[siteName] {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "site name is reserved"})
		return "", false
	}
	return siteName, true
}

func validateStoredUsername(w http.ResponseWriter, username string) bool {
	if err := safepath.ValidateSegment(username); err != nil {
		log.Printf("reject unsafe stored username %q: %v", username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return false
	}
	return true
}

// mutationTarget names the two identities a site mutation has. Before teams
// they were always the same person, so the handler bodies used the caller for
// both and nothing distinguished them. A team site is owned by a namespace
// nobody signs in as, so every place that used "the caller" had to be asked
// which one it meant.
//
// ActorID is who is doing it: rate limits and versions.uploaded_by.
// Owner* is whose namespace it lands in: the mutation lock, the advisory lock,
// the sites row, and the reported URL.
//
// Getting this wrong is quiet rather than loud. The advisory lock hashes the
// owner id with the site name (db.LockSiteCollaboration), so two people
// deploying one team site under their own ids would take different locks and
// race each other's version numbers; and reconcileSiteCommit looks the site
// up by the id it is handed, so the actor's id would report a committed site
// as missing.
type mutationTarget struct {
	ActorID       string
	ActorUsername string
	OwnerID       string
	OwnerUsername string
}

// selfTarget is the owner-inferred case: the caller acts on their own
// namespace, which is what every route did implicitly before teams existed.
func selfTarget(user *db.User) mutationTarget {
	return mutationTarget{
		ActorID:       user.ID,
		ActorUsername: user.Username,
		OwnerID:       user.ID,
		OwnerUsername: user.Username,
	}
}

// writeRawJSON writes a state document byte-for-byte: deployed pages' own
// JavaScript does `fetch(...).then(r => r.json())` against the state
// routes, so the body must be exactly what was stored, never re-wrapped.
// Shared with SiteAPIHandler (site_api.go), which now owns every state
// route.
func writeRawJSON(w http.ResponseWriter, status int, body json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (h *SiteHandler) createSite(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	siteName, ok := validatedNewSiteName(w, r)
	if !ok {
		return
	}
	h.createSiteForTarget(w, r, selfTarget(user), siteName)
}

// createSiteForTarget is the whole of creating a site, with the namespace it
// lands in passed in rather than inferred from the caller. The owner-inferred
// route above calls it with actor = owner, which is what it always did; the
// owner-qualified route calls it with a namespace the caller is a member of.
//
// This is the shape updateCollaborationSite already had, generalised: the
// actor is used for the rate limit and versions.uploaded_by, the owner for the
// locks, the disk paths, the sites row and the reported URL. See mutationTarget.
func (h *SiteHandler) createSiteForTarget(w http.ResponseWriter, r *http.Request, target mutationTarget, siteName string) {
	if !validateStoredUsername(w, target.OwnerUsername) {
		return
	}
	if decision := h.limits.allow(managementUserPolicy, target.ActorID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}

	files, err := h.readAndValidateFiles(w, r, siteName)
	if err != nil {
		return
	}
	if !h.scanDeploy(w, r, target, siteName, "", files) {
		return
	}
	unlock := h.mutations.lock(target.OwnerID, siteName)
	defer unlock()
	if _, err := db.GetSite(r.Context(), h.database, target.OwnerID, siteName); err == nil {
		writeJSON(w, http.StatusConflict, errorResponse{Error: "site already exists"})
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

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

	site, err := db.CreateSite(r.Context(), tx, target.OwnerID, siteName)
	if err != nil {
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, errorResponse{Error: "site already exists"})
			return
		}

		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	const versionNumber = 1
	versionPrefix := siteVersionPrefix(target.OwnerUsername, siteName, versionNumber)

	version, err := db.CreateVersion(r.Context(), tx, site.ID, versionNumber, versionPrefix, &target.ActorID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	// The object goes up before the commit that makes it live, so a
	// committed version always has its files; until then nothing refers to it.
	put, err := h.store.PutVersion(r.Context(), site.ID, versionNumber, files)
	if err != nil {
		log.Printf("upload files for %s/%s v%d: %v", target.OwnerUsername, siteName, versionNumber, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	keepObject := false
	defer func() {
		if !keepObject {
			h.discardVersion(site.ID, versionNumber)
		}
	}()

	if err := db.ActivateVersion(r.Context(), tx, version.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	if err := db.UpdateSiteActiveVersion(r.Context(), tx, site.ID, versionNumber); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	if !h.enforceQuota(w, r, tx, target.OwnerID, version.ID, put.StoredBytes, 0, true) {
		return
	}

	if err := db.EnqueueSiteSearch(r.Context(), tx, site.ID, db.SiteSearchReconcile); err != nil {
		log.Printf("enqueue site search reconcile for %s/%s: %v", target.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	actorKind, keyID := auditActorKind(r.Context())
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: target.ActorID, ActorKind: actorKind, KeyID: keyID,
		Action: "site_create", OwnerID: target.OwnerID, SiteID: site.ID,
		RequestID: auditRequestID(r.Context()),
		Extra:     map[string]any{"version": versionNumber},
	}); err != nil {
		log.Printf("record audit for site_create %s/%s: %v", target.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	if commitErr := audit.Commit(tx); commitErr != nil {
		result := reconcileSiteCommit(h.database, target.OwnerID, siteName, siteCommitExpectation{
			applied:    existingSiteCommitSnapshot(site.ID, versionNumber),
			rolledBack: siteCommitSnapshot{},
		})
		logSiteCommitOutcome("create", target.OwnerID, siteName, 0, versionNumber, commitErr, result)
		keepObject = result.outcome != siteCommitRolledBack
		if result.outcome != siteCommitApplied {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
	}
	keepObject = true

	site.ActiveVersion = versionNumber
	url := h.siteURL(r.Context(), target.OwnerUsername, siteName, site.ID)
	setSiteETag(w, site)
	writeJSON(w, http.StatusCreated, toSiteResponse(site, url, fmt.Sprintf("Site is available at %s. Only you (for a team site, the team's members) can open it until its access level is changed.", url), db.SiteAnalyticsSummary{}, nil))
}

func (h *SiteHandler) updateSite(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	target := selfTarget(user)

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

	files, err := h.readAndValidateFiles(w, r, siteName)
	if err != nil {
		return
	}
	if !h.scanDeploy(w, r, target, siteName, "", files) {
		return
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
	previousVersion := site.ActiveVersion
	// Only its owner deploys a site in their own namespace, so If-Match is
	// optional here; the owner-qualified routes (team sites) require it.
	if !requireSitePrecondition(w, r, site, false) {
		return
	}

	maxVersion, err := db.GetMaxVersionNumber(r.Context(), tx, site.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	versionNumber := maxVersion + 1
	versionPrefix := siteVersionPrefix(target.OwnerUsername, siteName, versionNumber)

	version, err := db.CreateVersion(r.Context(), tx, site.ID, versionNumber, versionPrefix, &target.ActorID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	put, err := h.store.PutVersion(r.Context(), site.ID, versionNumber, files)
	if err != nil {
		log.Printf("upload files for %s/%s v%d: %v", target.OwnerUsername, siteName, versionNumber, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	keepObject := false
	defer func() {
		if !keepObject {
			h.discardVersion(site.ID, versionNumber)
		}
	}()

	if err := db.ActivateVersion(r.Context(), tx, version.ID); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	if err := db.UpdateSiteActiveVersion(r.Context(), tx, site.ID, versionNumber); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	freed, err := pruneVersions(r.Context(), tx, h.quota, site.ID, versionNumber)
	if err != nil {
		log.Printf("prune versions for %s/%s: %v", target.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if !h.enforceQuota(w, r, tx, target.OwnerID, version.ID, put.StoredBytes, freed, false) {
		return
	}

	if err := db.EnqueueSiteSearch(r.Context(), tx, site.ID, db.SiteSearchReconcile); err != nil {
		log.Printf("enqueue site search reconcile for %s/%s: %v", target.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	actorKind, keyID := auditActorKind(r.Context())
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: target.ActorID, ActorKind: actorKind, KeyID: keyID,
		Action: "site_update", OwnerID: target.OwnerID, SiteID: site.ID,
		RequestID: auditRequestID(r.Context()),
		Extra:     map[string]any{"version": versionNumber, "previous_version": previousVersion},
	}); err != nil {
		log.Printf("record audit for site_update %s/%s: %v", target.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	if commitErr := audit.Commit(tx); commitErr != nil {
		result := reconcileSiteCommit(h.database, target.OwnerID, siteName, siteCommitExpectation{
			applied:    existingSiteCommitSnapshot(site.ID, versionNumber),
			rolledBack: existingSiteCommitSnapshot(site.ID, previousVersion),
		})
		logSiteCommitOutcome("update", target.OwnerID, siteName, previousVersion, versionNumber, commitErr, result)
		keepObject = result.outcome != siteCommitRolledBack
		if result.outcome != siteCommitApplied {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
	}
	keepObject = true

	site.ActiveVersion = versionNumber

	url := h.siteURL(r.Context(), target.OwnerUsername, siteName, site.ID)
	setSiteETag(w, site)
	writeJSON(w, http.StatusOK, toSiteResponse(site, url, fmt.Sprintf("Site is available at %s", url), db.SiteAnalyticsSummary{}, nil))
}

func siteVersionPrefix(username, siteName string, versionNumber int) string {
	return strings.Join([]string{username, siteName, fmt.Sprintf("v%d", versionNumber)}, "/") + "/"
}

func (h *SiteHandler) listSites(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	if decision := h.limits.allow(managementUserPolicy, user.ID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}

	var sites []db.Site
	var err error

	if user.IsAdmin {
		sites, err = db.ListAllSites(r.Context(), h.database)
	} else {
		sites, err = db.ListSitesByUser(r.Context(), h.database, user.ID)
	}

	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	analyticsBySiteID, err := db.ListSiteAnalyticsSummaries(r.Context(), h.database, 7)
	if err != nil {
		log.Printf("list site analytics: %v", err)
		analyticsBySiteID = make(map[string]db.SiteAnalyticsSummary)
	}

	siteIDs := make([]string, 0, len(sites))
	for _, site := range sites {
		siteIDs = append(siteIDs, site.ID)
	}
	downloadsBySiteID, err := db.ListSiteFileDownloadSummaries(r.Context(), h.database, 7, siteIDs)
	if err != nil {
		log.Printf("list site file downloads: %v", err)
		downloadsBySiteID = make(map[string]map[string]db.FileDownloadStat)
	}

	usernameByID := map[string]string{user.ID: user.Username}
	if user.IsAdmin {
		allUsers, err := db.ListAllUsers(r.Context(), h.database)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
		for _, u := range allUsers {
			usernameByID[u.ID] = u.Username
		}
	}

	response := make([]siteResponse, 0, len(sites))
	for _, site := range sites {
		var url string
		if uname := usernameByID[site.UserID]; safepath.IsSegment(uname) && safepath.IsSegment(site.Name) {
			url = h.siteURL(r.Context(), uname, site.Name, site.ID)
		}
		response = append(response, toSiteResponse(site, url, "", analyticsBySiteID[site.ID], downloadsBySiteID[site.ID]))
	}

	writeJSON(w, http.StatusOK, response)
}

func (h *SiteHandler) deleteSite(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	h.deleteSiteForTarget(w, r, selfTarget(user))
}

// deleteSiteForTarget deletes from the namespace named by target rather than
// from the caller's own. See createSiteForTarget and mutationTarget.
func (h *SiteHandler) deleteSiteForTarget(w http.ResponseWriter, r *http.Request, target mutationTarget) {

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
	if err := db.EnqueueSiteSearch(r.Context(), tx, site.ID, db.SiteSearchDelete); err != nil {
		log.Printf("enqueue site search delete for %s/%s: %v", target.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	// Serving stops the moment the row is gone; the objects follow after the
	// grace period, so a replica mid-request on the old version finishes it.
	sitePrefix, err := storage.SitePrefix(site.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := db.RetireObjects(r.Context(), tx, sitePrefix, storage.RetireGrace); err != nil {
		log.Printf("retire objects for %s/%s: %v", target.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := db.DeleteSite(r.Context(), tx, target.OwnerID, siteName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	actorKind, keyID := auditActorKind(r.Context())
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: target.ActorID, ActorKind: actorKind, KeyID: keyID,
		Action: "site_delete", OwnerID: target.OwnerID, SiteID: site.ID,
		RequestID: auditRequestID(r.Context()),
		Extra:     map[string]any{"active_version": site.ActiveVersion},
	}); err != nil {
		log.Printf("record audit for site_delete %s/%s: %v", target.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if commitErr := audit.Commit(tx); commitErr != nil {
		result := reconcileSiteCommit(h.database, target.OwnerID, siteName, siteCommitExpectation{
			applied:    siteCommitSnapshot{},
			rolledBack: existingSiteCommitSnapshot(site.ID, site.ActiveVersion),
		})
		logSiteCommitOutcome("delete", target.OwnerID, siteName, site.ActiveVersion, 0, commitErr, result)
		if result.outcome != siteCommitApplied {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *SiteHandler) rollbackSite(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	target := selfTarget(user)

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

	var req rollbackSiteRequest
	if !decodeSmallJSON(w, r, &req) {
		return
	}

	if req.Version < 1 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "version must be at least 1"})
		return
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
	previousVersion := site.ActiveVersion
	// Only its owner deploys a site in their own namespace, so If-Match is
	// optional here; the owner-qualified routes (team sites) require it.
	if !requireSitePrecondition(w, r, site, false) {
		return
	}

	versions, err := db.ListVersions(r.Context(), tx, site.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	if !versionExists(versions, req.Version) {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "version not found"})
		return
	}

	if err := db.UpdateSiteActiveVersion(r.Context(), tx, site.ID, req.Version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := db.EnqueueSiteSearch(r.Context(), tx, site.ID, db.SiteSearchReconcile); err != nil {
		log.Printf("enqueue site search reconcile for %s/%s: %v", target.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	actorKind, keyID := auditActorKind(r.Context())
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: target.ActorID, ActorKind: actorKind, KeyID: keyID,
		Action: "site_rollback", OwnerID: target.OwnerID, SiteID: site.ID,
		RequestID: auditRequestID(r.Context()),
		Extra:     map[string]any{"version": req.Version, "previous_version": previousVersion},
	}); err != nil {
		log.Printf("record audit for site_rollback %s/%s: %v", target.OwnerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if commitErr := audit.Commit(tx); commitErr != nil {
		result := reconcileSiteCommit(h.database, target.OwnerID, siteName, siteCommitExpectation{
			applied:    existingSiteCommitSnapshot(site.ID, req.Version),
			rolledBack: existingSiteCommitSnapshot(site.ID, previousVersion),
		})
		logSiteCommitOutcome("rollback", target.OwnerID, siteName, previousVersion, req.Version, commitErr, result)
		if result.outcome != siteCommitApplied {
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
	}

	site.ActiveVersion = req.Version
	site.UpdatedAt = time.Now().UTC()

	url := h.siteURL(r.Context(), target.OwnerUsername, siteName, site.ID)
	setSiteETag(w, site)
	writeJSON(w, http.StatusOK, toSiteResponse(site, url, fmt.Sprintf("Rolled back to v%d at %s", req.Version, url), db.SiteAnalyticsSummary{}, nil))
}

func (h *SiteHandler) listVersions(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}

	siteName, ok := validatedSiteName(w, r)
	if !ok {
		return
	}

	site, err := db.GetSite(r.Context(), h.database, user.ID, siteName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
			return
		}

		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	versions, err := db.ListVersions(r.Context(), h.database, site.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	resp := make([]versionResponse, len(versions))
	for i, v := range versions {
		resp[i] = versionResponse{
			ID:            v.ID,
			VersionNumber: v.VersionNumber,
			Status:        v.Status,
			CreatedAt:     v.CreatedAt,
		}
	}
	setSiteETag(w, site)
	writeJSON(w, http.StatusOK, resp)
}

func (h *SiteHandler) readAndValidateFiles(w http.ResponseWriter, r *http.Request, siteName string) (map[string][]byte, error) {
	body, err := readLimitedBody(w, r)
	if err != nil {
		return nil, err
	}

	filename := archiveFilename(siteName, body)
	files, err := tarball.ExtractBytes(body, filename)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid site archive"})
		return nil, err
	}

	if err := tarball.ValidateExtensions(files); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return nil, err
	}

	return files, nil
}

func readLimitedBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSiteArchiveSize)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "request body too large"})
			return nil, err
		}

		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return nil, err
	}

	if len(body) == 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "request body is required"})
		return nil, errors.New("empty request body")
	}

	return body, nil
}

func archiveFilename(siteName string, body []byte) string {
	if len(body) >= 4 && bytes.Equal(body[:4], []byte("PK\x03\x04")) {
		return siteName + ".zip"
	}

	return siteName + ".tar.gz"
}

func toSiteResponse(site db.Site, url, note string, summary db.SiteAnalyticsSummary, downloads map[string]db.FileDownloadStat) siteResponse {
	var fileDownloads map[string]fileDownloadStat
	if len(downloads) > 0 {
		fileDownloads = make(map[string]fileDownloadStat, len(downloads))
		for path, stat := range downloads {
			fileDownloads[path] = fileDownloadStat{
				Total: stat.Total,
				Last7: stat.Last7,
			}
		}
	}

	return siteResponse{
		ID:            site.ID,
		UserID:        site.UserID,
		Name:          site.Name,
		ActiveVersion: site.ActiveVersion,
		CreatedAt:     site.CreatedAt,
		UpdatedAt:     site.UpdatedAt,
		URL:           url,
		Note:          note,
		Analytics: analytics{
			TodayPageviews: summary.TodayPageviews,
			TodayVisits:    summary.TodayVisits,
			Last7Pageviews: summary.Last7Pageviews,
			Last7Visits:    summary.Last7Visits,
			FileDownloads:  fileDownloads,
		},
	}
}

func versionExists(versions []db.Version, versionNumber int) bool {
	for _, version := range versions {
		if version.VersionNumber == versionNumber {
			return true
		}
	}

	return false
}
