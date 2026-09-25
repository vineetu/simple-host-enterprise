package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/storage"
)

// SiteAPIHandler serves the site-facing API: state and assets,
// reached only through the host gate on an owner or restricted-site host.
// The gate has already resolved which site is being addressed (from the
// host label plus the request path — never a Referer, which this package no
// longer reads at all) and authenticated the caller (a session or an
// X-API-Key) before calling any of these methods; every method here still
// re-checks viewerAllowed/writerAllowed itself; the gate's job stops at
// "who is calling and which site."
type SiteAPIHandler struct {
	database    *sql.DB
	store       *storage.Store
	assetLimits storage.AssetLimits
	audit       audit.Recorder
	hosts       HostModel
	limits      *AbuseLimits
	// stateUsage is SiteHandler's own uses_state/uses_versioned_state
	// dashboard-badge marker (state_usage_marker.go), duplicated here as a
	// separate instance rather than shared: the two handlers already have
	// independent lifetimes and dependencies, and the marker itself is
	// process-local, best-effort, and keyed by site id, so two independent
	// instances double-write at most once per site rather than disagreeing.
	stateUsage *stateUsageMarker
}

// NewSiteAPIHandler constructs the handler. recorder may be audit.NoOp{}
// (main.go's current default; a real DBRecorder is wired in separately) —
// every write here calls Record regardless, so the audit trail activates
// the moment main.go swaps the recorder, with no change here.
func NewSiteAPIHandler(database *sql.DB, store *storage.Store, assetLimits storage.AssetLimits, recorder audit.Recorder, hosts HostModel, limits ...*AbuseLimits) *SiteAPIHandler {
	if recorder == nil {
		recorder = audit.NoOp{}
	}
	return &SiteAPIHandler{
		database:    database,
		store:       store,
		assetLimits: assetLimits,
		audit:       recorder,
		hosts:       hosts,
		limits:      chooseAbuseLimits(limits),
		stateUsage:  newStateUsageMarker(4),
	}
}

// siteAPICall is what the host gate has already established before calling
// into any handler method: which site, which actor, and the via_site claim
// every state and asset write is asked to
// carry. ActorKind and KeyID mirror audit.Event's own fields exactly.
type siteAPICall struct {
	Owner      string
	SiteName   string
	SiteID     string
	Restricted bool

	ActorUserID string
	ActorKind   string // "person" (session) or "key" (X-API-Key)
	KeyID       string

	// ViaSiteLabel is the host label the request arrived on (an owner
	// label, or a full "owner--site" restricted-site label).
	ViaSiteLabel string
	// ViaSiteName is the site name the host+path claimed — the audited
	// record, not necessarily re-verified.
	ViaSiteName string
	// ViaSiteObserved is true only when Sec-Fetch-Dest is "empty" (a
	// fetch, not a navigation) and the Referer's first path segment, when
	// present, agrees with ViaSiteName. A false does not mean the claim is
	// wrong — most clients send neither header — only that it was not
	// independently corroborated.
	ViaSiteObserved bool
}

// auditEvent builds the Event to record for this call. OwnerID must be the
// owner's users.id (audit_events.owner_id is a uuid FK), never c.Owner
// itself — c.Owner is the owner's *username*, which every disk path and
// URL in this file correctly keys on but which fails outright as a uuid
// literal. Resolving it here (a live bug found and fixed in this pass: see
// docs/security-review.md) is what lets a non-admin owner's
// own GET /api/audit scope (matched against owner_id) ever see
// their own site's state/asset writes at all — a NULL owner_id would be
// invisible to that scope regardless of any site_id filter also given.
// q is the same Querier the caller is about to write through — h.database
// for a caller with no transaction of its own, or the open *sql.Tx for one
// of this file's four write methods (PutState, PutStateVersioned,
// CreateAsset, DeleteAsset), all of which now build this event and record
// it inside the same transaction as their own write. Passing
// h.database here instead, while a transaction on that same *sql.DB is
// open and unfinished, is not just wasteful: with a small connection pool
// (this package's own tests run MaxOpenConns(1)) it deadlocks outright,
// since the pool has no free connection left to answer this query on until
// the transaction holding the only one commits or rolls back — which
// itself cannot happen until this call returns. Found the hard way
// wiring RecordTx in for state_write/asset_create/asset_delete: see
// docs/security-review.md's Reviewer findings.
func (h *SiteAPIHandler) auditEvent(ctx context.Context, q db.Querier, c siteAPICall, action string, extra map[string]any) audit.Event {
	return audit.Event{
		ActorID:         c.ActorUserID,
		ActorKind:       c.ActorKind,
		KeyID:           c.KeyID,
		Action:          action,
		OwnerID:         h.resolveOwnerID(ctx, q, c.Owner),
		SiteID:          c.SiteID,
		ViaSiteLabel:    c.ViaSiteLabel,
		ViaSiteName:     c.ViaSiteName,
		ViaSiteObserved: c.ViaSiteObserved,
		RequestID:       auditRequestID(ctx),
		Extra:           extra,
	}
}

// resolveOwnerID looks up username's users.id for auditEvent above, via q
// (see auditEvent's comment on why that must be the same Querier the
// caller is about to write through). Best-effort: a lookup failure (which
// should not happen for a username the gate has already resolved a live
// site under) logs and leaves the event's OwnerID empty — NULL in
// audit_events — rather than blocking the write the event describes.
func (h *SiteAPIHandler) resolveOwnerID(ctx context.Context, q db.Querier, username string) string {
	owner, err := db.GetUserByUsername(ctx, q, username)
	if err != nil {
		log.Printf("site API: resolve owner id for %q: %v", username, err)
		return ""
	}
	return owner.ID
}

// siteURL returns the address this call's asset routes are reachable at:
// the short owner-host address, or the restricted site's own root, per
// the URL in the {id, url} create response.
func (h *SiteAPIHandler) siteURL(call siteAPICall) string {
	return h.hosts.SiteURL(call.Owner, call.SiteName, call.Restricted)
}

// GetState answers GET .../state. viewerAllowed is the gate's job (it calls
// this only after confirming it); this method's own job is the query and
// the response shape.
func (h *SiteAPIHandler) GetState(w http.ResponseWriter, r *http.Request, call siteAPICall) {
	if decision := h.limits.allow(stateReadClientPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	if decision := h.limits.allow(stateReadSitePolicy, siteLimitKey(call.Owner, call.SiteName)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	state, siteID, err := db.GetSiteState(r.Context(), h.database, call.Owner, call.SiteName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	h.markStateUsage(siteID, call.Owner, call.SiteName, false)
	writeRawJSON(w, http.StatusOK, state)
}

// PutState answers PUT .../state.
func (h *SiteAPIHandler) PutState(w http.ResponseWriter, r *http.Request, call siteAPICall) {
	if decision := h.limits.allow(stateClientPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	if decision := h.limits.allow(stateSitePolicy, siteLimitKey(call.Owner, call.SiteName)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSiteStateSize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}
	if !json.Valid(body) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}
	state := json.RawMessage(body)

	// A mutation without its audit row must not commit. The
	// write and its audit_events row share one transaction so a failure
	// recording the audit row rolls the state write back with it, rather
	// than leaving a write with no trail (or vice versa).
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer tx.Rollback()
	if err := db.UpdateSiteState(r.Context(), tx, call.Owner, call.SiteName, state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := db.RecordStateHistory(r.Context(), tx, call.SiteID, call.ActorUserID); err != nil {
		log.Printf("record state history %s/%s: %v", call.Owner, call.SiteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := h.audit.RecordTx(r.Context(), tx, h.auditEvent(r.Context(), tx, call, "state_write", map[string]any{"versioned": false})); err != nil {
		log.Printf("record audit for state_write %s/%s: %v", call.Owner, call.SiteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("commit state_write %s/%s: %v", call.Owner, call.SiteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeRawJSON(w, http.StatusOK, state)
}

// GetStateVersioned answers GET .../state/versioned.
func (h *SiteAPIHandler) GetStateVersioned(w http.ResponseWriter, r *http.Request, call siteAPICall) {
	if decision := h.limits.allow(stateReadClientPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	if decision := h.limits.allow(stateReadSitePolicy, siteLimitKey(call.Owner, call.SiteName)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	state, version, siteID, err := db.GetSiteStateVersioned(r.Context(), h.database, call.Owner, call.SiteName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	h.markStateUsage(siteID, call.Owner, call.SiteName, true)
	writeJSON(w, http.StatusOK, versionedStateResponse{Version: version, State: state})
}

// PutStateVersioned answers PUT .../state/versioned.
func (h *SiteAPIHandler) PutStateVersioned(w http.ResponseWriter, r *http.Request, call siteAPICall) {
	if decision := h.limits.allow(stateClientPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	if decision := h.limits.allow(stateSitePolicy, siteLimitKey(call.Owner, call.SiteName)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSiteStateSize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}
	var req versionedStatePutRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}
	if req.Version == nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "version is required"})
		return
	}
	if *req.Version < 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "version must be at least 0"})
		return
	}
	if req.State == nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "state is required"})
		return
	}
	// Same one-transaction-for-write-and-audit-row shape as PutState; see
	// its comment. A version conflict or a not-found aborts before the
	// audit row is even attempted, so the deferred Rollback is the only
	// cleanup those two paths need — writeStateConflict re-reads through
	// h.database (not tx) since the caller has already decided not to
	// commit anything from this request.
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer tx.Rollback()
	newVersion, err := db.UpdateSiteStateCAS(r.Context(), tx, call.Owner, call.SiteName, req.State, *req.Version)
	if err != nil {
		if errors.Is(err, db.ErrVersionConflict) {
			h.writeStateConflict(w, r, call)
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := db.RecordStateHistory(r.Context(), tx, call.SiteID, call.ActorUserID); err != nil {
		log.Printf("record state history %s/%s: %v", call.Owner, call.SiteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := h.audit.RecordTx(r.Context(), tx, h.auditEvent(r.Context(), tx, call, "state_write", map[string]any{"versioned": true})); err != nil {
		log.Printf("record audit for state_write %s/%s: %v", call.Owner, call.SiteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("commit state_write %s/%s: %v", call.Owner, call.SiteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, versionedStateSavedResponse{Version: newVersion})
}

func (h *SiteAPIHandler) writeStateConflict(w http.ResponseWriter, r *http.Request, call siteAPICall) {
	state, version, siteID, err := db.GetSiteStateVersioned(r.Context(), h.database, call.Owner, call.SiteName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "site not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	h.markStateUsage(siteID, call.Owner, call.SiteName, true)
	writeJSON(w, http.StatusConflict, versionedStateConflictResponse{Error: "version conflict", Version: version, State: state})
}

// markStateUsage mirrors SiteHandler.markUsesState exactly (same marker
// shape, same fire-and-forget contract): it records that a site read its
// state via the given backend variant, deduped and rate-limited by the
// bounded stateUsageMarker so an unauthenticated-in-spirit, high-frequency
// read path never opens an unbounded number of goroutines or database
// writes. A write error is logged, never surfaced to the caller.
func (h *SiteAPIHandler) markStateUsage(siteID, username, siteName string, versioned bool) {
	key := stateUsageMarkerKey(siteID, versioned)
	h.stateUsage.Schedule(key, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), analyticsWriteLimit)
		defer cancel()
		err := db.MarkSiteUsesState(ctx, h.database, siteID, versioned)
		if err != nil {
			log.Printf("mark uses_state %s/%s (versioned=%t): %v", username, siteName, versioned, err)
		}
		return err
	})
}

// createAssetResponse is the POST response shape.
type createAssetResponse struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// assetResponse is one row of the GET list response.
type assetResponse struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	URL         string    `json:"url"`
	CreatedAt   time.Time `json:"created_at"`
}

type listAssetsResponse struct {
	Assets []assetResponse `json:"assets"`
}

// CreateAsset answers POST .../assets: a multipart upload with exactly one
// file part, any field name (a plain <input type=file> form or a minimal
// fetch(FormData) call each shape one this way). Two or more file parts —
// whether under the same field name or different ones — are rejected
// outright rather than silently picking one: an upload naming which single
// file it means is unambiguous, and ranging over r.MultipartForm.File (a
// map keyed by field name) to pick "the first one found" would make that
// choice depend on Go's randomized map iteration order, a different answer
// on every request. storage.CreateAsset is called first (it validates,
// sniffs, and uploads the object) and only once that succeeds is the row
// inserted with the same id, by db.CreateAssetWithinQuota, which enforces
// the per-site quota under a per-site lock in the same transaction.
func (h *SiteAPIHandler) CreateAsset(w http.ResponseWriter, r *http.Request, call siteAPICall) {
	if decision := h.limits.allow(stateClientPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	if decision := h.limits.allow(stateSitePolicy, siteLimitKey(call.Owner, call.SiteName)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	release, acquired := h.limits.acquireUpload()
	if !acquired {
		writeConcurrencyRateLimit(w)
		return
	}
	defer release()

	// One extra byte over the per-file cap so an oversized part fails with
	// ErrAssetTooLarge from the streaming check rather than a silent
	// truncation the client would never see reflected in the stored size.
	r.Body = http.MaxBytesReader(w, r.Body, h.assetLimits.MaxFileBytes+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid multipart upload"})
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	var fileHeader *multipart.FileHeader
	fileParts := 0
	for _, headers := range r.MultipartForm.File {
		fileParts += len(headers)
		if len(headers) > 0 {
			fileHeader = headers[0]
		}
	}
	if fileParts == 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "no file in upload"})
		return
	}
	if fileParts > 1 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "exactly one file is required per upload"})
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid multipart upload"})
		return
	}
	defer file.Close()

	if fileHeader.Size > h.assetLimits.MaxFileBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "asset exceeds the per-file size limit"})
		return
	}
	stored, err := h.store.CreateAsset(r.Context(), call.SiteID, fileHeader.Header.Get("Content-Type"), file, h.assetLimits)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrAssetTooLarge):
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "asset exceeds the per-file size limit"})
		case errors.Is(err, storage.ErrAssetTypeNotAllowed):
			writeJSON(w, http.StatusUnsupportedMediaType, errorResponse{Error: "asset content type is not allowed"})
		default:
			log.Printf("create asset for %s/%s: %v", call.Owner, call.SiteName, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		}
		return
	}

	var createdBy *string
	if call.ActorUserID != "" {
		createdBy = &call.ActorUserID
	}
	name := fileHeader.Filename
	if name == "" {
		name = stored.ID
	}

	// The object is uploaded before this transaction, under a fresh id
	// nothing refers to yet. The row and its audit_events row commit
	// together. If the transaction is known not to have
	// committed, the object is deleted again; after an ambiguous commit it
	// is left, since the row may exist.
	keepObject := false
	defer func() {
		if !keepObject {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := h.store.DeleteAsset(ctx, call.SiteID, stored.ID); err != nil {
				log.Printf("discard uncommitted asset %s/%s id=%s: %v", call.Owner, call.SiteName, stored.ID, err)
			}
		}
	}()
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer tx.Rollback()
	if _, err := db.CreateAssetWithinQuota(r.Context(), tx, stored.ID, call.SiteID, name, stored.ContentType, stored.Size, stored.SHA256[:], createdBy, h.assetLimits.MaxSiteCount, h.assetLimits.MaxSiteBytes); err != nil {
		if errors.Is(err, db.ErrAssetQuotaExceeded) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "site asset quota exceeded"})
			return
		}
		log.Printf("record asset row for %s/%s id=%s: %v", call.Owner, call.SiteName, stored.ID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := h.audit.RecordTx(r.Context(), tx, h.auditEvent(r.Context(), tx, call, "asset_create", map[string]any{
		"asset_id": stored.ID, "name": name, "content_type": stored.ContentType, "size": stored.Size,
	})); err != nil {
		log.Printf("record audit for asset_create %s/%s id=%s: %v", call.Owner, call.SiteName, stored.ID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := tx.Commit(); err != nil {
		keepObject = true
		log.Printf("commit asset_create %s/%s id=%s: %v", call.Owner, call.SiteName, stored.ID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	keepObject = true
	writeJSON(w, http.StatusCreated, createAssetResponse{
		ID:  stored.ID,
		URL: h.siteURL(call) + "_assets/" + url.PathEscape(stored.ID) + "/" + url.PathEscape(name),
	})
}

// ListAssets answers GET .../assets.
func (h *SiteAPIHandler) ListAssets(w http.ResponseWriter, r *http.Request, call siteAPICall) {
	if decision := h.limits.allow(stateReadClientPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	if decision := h.limits.allow(stateReadSitePolicy, siteLimitKey(call.Owner, call.SiteName)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	assets, err := db.ListAssets(r.Context(), h.database, call.SiteID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	out := make([]assetResponse, 0, len(assets))
	base := h.siteURL(call)
	for _, a := range assets {
		out = append(out, assetResponse{
			ID:          a.ID,
			Name:        a.Name,
			ContentType: a.ContentType,
			Size:        a.Size,
			URL:         base + "_assets/" + url.PathEscape(a.ID) + "/" + url.PathEscape(a.Name),
			CreatedAt:   a.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, listAssetsResponse{Assets: out})
}

// DeleteAsset answers DELETE .../assets/{id}. The row is soft-deleted and the
// object queued for the sweep in one transaction with the audit row, so the
// object goes if and only if the delete commits.
func (h *SiteAPIHandler) DeleteAsset(w http.ResponseWriter, r *http.Request, call siteAPICall, id string) {
	if decision := h.limits.allow(stateClientPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	if decision := h.limits.allow(stateSitePolicy, siteLimitKey(call.Owner, call.SiteName)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	if _, err := db.GetAsset(r.Context(), h.database, call.SiteID, id); err != nil {
		if errors.Is(err, db.ErrAssetNotFound) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "asset not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer tx.Rollback()
	if err := db.SoftDeleteAsset(r.Context(), tx, call.SiteID, id); err != nil && !errors.Is(err, db.ErrAssetNotFound) {
		log.Printf("soft-delete asset row %s/%s id=%s: %v", call.Owner, call.SiteName, id, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := retireAssetObject(r.Context(), tx, call.SiteID, id); err != nil {
		log.Printf("retire asset object %s/%s id=%s: %v", call.Owner, call.SiteName, id, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := h.audit.RecordTx(r.Context(), tx, h.auditEvent(r.Context(), tx, call, "asset_delete", map[string]any{"asset_id": id})); err != nil {
		log.Printf("record audit for asset_delete %s/%s id=%s: %v", call.Owner, call.SiteName, id, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("commit asset_delete %s/%s id=%s: %v", call.Owner, call.SiteName, id, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// retireAssetObject queues an asset's object for deletion in tx, the same
// way retired versions are (storage.RetireGrace).
func retireAssetObject(ctx context.Context, tx *sql.Tx, siteID, id string) error {
	key, err := storage.AssetKey(siteID, id)
	if err != nil {
		return err
	}
	return db.RetireObjects(ctx, tx, key, storage.RetireGrace)
}

// ServeAsset answers GET /{site}/_assets/{id}[/{name}] (or its restricted-
// host root-served equivalent) — viewerAllowed only, never writerAllowed:
// This is a read for anyone who may view the site.
func (h *SiteAPIHandler) ServeAsset(w http.ResponseWriter, r *http.Request, call siteAPICall, id string) {
	if decision := h.limits.allow(stateReadClientPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	row, err := db.GetAsset(r.Context(), h.database, call.SiteID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	openCtx, cancel := context.WithTimeout(r.Context(), siteOpenTimeout)
	defer cancel()
	asset, err := h.store.OpenAsset(openCtx, call.SiteID, row.ID, max(row.Size, h.assetLimits.MaxFileBytes))
	if err != nil {
		if errors.Is(err, storage.ErrAssetNotFound) {
			http.NotFound(w, r)
			return
		}
		log.Printf("open asset %s/%s id=%s: %v", call.Owner, call.SiteName, row.ID, err)
		http.Error(w, "asset temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	defer asset.Close()

	w.Header().Set("Content-Type", row.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if !isInlineAssetType(row.ContentType) {
		w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeAssetFilename(row.Name)+`"`)
	}
	http.ServeContent(w, r, row.Name, row.CreatedAt, asset.File)
}

func isInlineAssetType(contentType string) bool {
	base, _, _ := strings.Cut(contentType, ";")
	base = strings.TrimSpace(strings.ToLower(base))
	return strings.HasPrefix(base, "image/") || strings.HasPrefix(base, "video/") || strings.HasPrefix(base, "audio/") || base == "application/pdf"
}

// sanitizeAssetFilename strips characters that would let a stored name break
// out of the quoted Content-Disposition parameter (a literal quote or a
// control character); it is display text, not a path, so nothing here is
// ever used to open a file.
func sanitizeAssetFilename(name string) string {
	replacer := strings.NewReplacer(`"`, "'", "\r", " ", "\n", " ")
	return replacer.Replace(name)
}
