package handler

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// Asset administration from the dashboard (an assets list per site). This
// is deliberately a base-host, owner-role-gated pair of routes — the same
// shape collaboration.go and viewers.go already use — rather than the
// site-facing API in site_api.go: the dashboard is served on the base host,
// the asset routes live on the owner or restricted-site host, and no route
// on any host ever emits an Access-Control-* header, so a dashboard page
// cannot call the site-facing API cross-origin at all. This pair answers
// the same underlying storage and site_assets rows through the owner's
// existing base-host session instead.
type assetAdminResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
}

// registerAssetAdminRoutes is called from (*SiteHandler).Register alongside
// the viewer routes it mirrors.
func (h *SiteHandler) registerAssetAdminRoutes(mux *http.ServeMux, ownerMutation func(http.Handler) http.Handler, browserWrite func(http.Handler) http.Handler) {
	mux.Handle("GET /api/collaboration/sites/{owner}/{sitename}/assets", ownerMutation(http.HandlerFunc(h.listCollaborationAssets)))
	mux.Handle("DELETE /api/collaboration/sites/{owner}/{sitename}/assets/{id}", browserWrite(ownerMutation(http.HandlerFunc(h.deleteCollaborationAsset))))
}

func (h *SiteHandler) assetResponses(ctx context.Context, access db.SiteAccess, rows []db.Asset) []assetAdminResponse {
	restricted, err := db.IsSiteRestricted(ctx, h.database, access.Site.ID)
	if err != nil {
		restricted = false
	}
	base := h.hosts.SiteURL(access.OwnerUsername, access.Site.Name, restricted)
	out := make([]assetAdminResponse, 0, len(rows))
	for _, a := range rows {
		out = append(out, assetAdminResponse{
			ID: a.ID, Name: a.Name, ContentType: a.ContentType, Size: a.Size,
			URL: base + "_assets/" + a.ID,
		})
	}
	return out
}

func (h *SiteHandler) listCollaborationAssets(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok || !requireOwnerRole(w, access) {
		return
	}
	assets, err := db.ListAssets(r.Context(), h.database, access.Site.ID)
	if err != nil {
		log.Printf("list assets for %s/%s: %v", ownerUsername, siteName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, h.assetResponses(r.Context(), access, assets))
}

func (h *SiteHandler) deleteCollaborationAsset(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok || !requireOwnerRole(w, access) {
		return
	}
	id := r.PathValue("id")
	if _, err := db.GetAsset(r.Context(), h.database, access.Site.ID, id); err != nil {
		if errors.Is(err, db.ErrAssetNotFound) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "asset not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	actorID := ""
	if user := auth.GetUser(r.Context()); user != nil {
		actorID = user.ID
	}
	actorKind, keyID := auditActorKind(r.Context())
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer tx.Rollback()
	if err := db.SoftDeleteAsset(r.Context(), tx, access.Site.ID, id); err != nil && !errors.Is(err, db.ErrAssetNotFound) {
		log.Printf("soft-delete asset row for %s/%s id=%s: %v", ownerUsername, siteName, id, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	// The object goes with the row: queued for the sweep in this same
	// transaction, as site_api.go's DeleteAsset does.
	if err := retireAssetObject(r.Context(), tx, access.Site.ID, id); err != nil {
		log.Printf("retire asset object for %s/%s id=%s: %v", ownerUsername, siteName, id, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: actorID, ActorKind: actorKind, KeyID: keyID, Action: "asset_delete",
		OwnerID: access.OwnerID, SiteID: access.Site.ID,
		RequestID: auditRequestID(r.Context()),
		Extra:     map[string]any{"asset_id": id, "via": "dashboard"},
	}); err != nil {
		log.Printf("record audit for asset_delete %s/%s id=%s: %v", ownerUsername, siteName, id, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("commit asset_delete %s/%s id=%s: %v", ownerUsername, siteName, id, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
