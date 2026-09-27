package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	"github.com/vsriram/simple-host/internal/db"
)

// Recently deleted: deleting a site only marks its row (db.SoftDeleteSite),
// so for db.DeletedSiteRetention it can be restored exactly as it was — its
// files, saved data and history, access level, viewers and uploaded files.
// Owners (and members of the owning team) list and restore their own; an
// admin lists and restores any. After the window the sweeper purges it
// (storage.PurgeDeletedSites).

type deletedSiteResponse struct {
	Owner         string    `json:"owner"`
	Site          string    `json:"site"`
	ActiveVersion int       `json:"active_version"`
	Access        string    `json:"access"`
	DeletedAt     time.Time `json:"deleted_at"`
	DeletedBy     string    `json:"deleted_by,omitempty"`
	// RestorableUntil is when the site is purged for good.
	RestorableUntil time.Time `json:"restorable_until"`
}

func toDeletedSiteResponses(sites []db.DeletedSite) []deletedSiteResponse {
	out := make([]deletedSiteResponse, 0, len(sites))
	for _, s := range sites {
		out = append(out, deletedSiteResponse{
			Owner: s.Owner, Site: s.Name, ActiveVersion: s.ActiveVersion, Access: s.Access,
			DeletedAt: s.DeletedAt, DeletedBy: s.DeletedBy, RestorableUntil: s.PurgeAt(),
		})
	}
	return out
}

type restoredSiteResponse struct {
	Owner         string `json:"owner"`
	Site          string `json:"site"`
	ActiveVersion int    `json:"active_version"`
	Access        string `json:"access"`
	URL           string `json:"url"`
}

var errDeletedSiteNotFound = errors.New("no recently deleted site of that name")

// restoreDeletedSite brings ownerID's deleted site named siteName back, in one
// transaction under the site's advisory lock: the row is undeleted, the
// owner's quota is checked with the site counted again, search is told to
// index it, and a site_restore event is recorded. A quota refusal restores
// nothing. event carries the actor; Action, OwnerID and SiteID are filled in.
func restoreDeletedSite(ctx context.Context, database *sql.DB, quota UploadQuota, recorder audit.Recorder, ownerID, siteName string, event audit.Event) (db.DeletedSite, *quotaRefusal, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return db.DeletedSite{}, nil, err
	}
	defer audit.Rollback(tx)
	if err := db.LockSiteCollaboration(ctx, tx, ownerID, siteName); err != nil {
		return db.DeletedSite{}, nil, err
	}
	site, err := db.GetDeletedSite(ctx, tx, ownerID, siteName)
	if errors.Is(err, sql.ErrNoRows) {
		return db.DeletedSite{}, nil, errDeletedSiteNotFound
	}
	if err != nil {
		return db.DeletedSite{}, nil, err
	}
	if err := db.UndeleteSite(ctx, tx, site.ID); err != nil {
		return db.DeletedSite{}, nil, err
	}
	bytes, err := db.SiteStoredBytes(ctx, tx, site.ID)
	if err != nil {
		return db.DeletedSite{}, nil, err
	}
	refusal, err := checkOwnerQuota(ctx, tx, quota, ownerID, true, bytes, 0)
	if err != nil || refusal != nil {
		return db.DeletedSite{}, refusal, err
	}
	if err := db.EnqueueSiteSearch(ctx, tx, site.ID, db.SiteSearchReconcile); err != nil {
		return db.DeletedSite{}, nil, err
	}
	event.Action = "site_restore"
	event.OwnerID = ownerID
	event.SiteID = site.ID
	if event.Extra == nil {
		event.Extra = map[string]any{}
	}
	event.Extra["from"] = "recently_deleted"
	event.Extra["deleted_at"] = site.DeletedAt.UTC().Format(time.RFC3339)
	event.Extra["active_version"] = site.ActiveVersion
	if err := recorder.RecordTx(ctx, tx, event); err != nil {
		return db.DeletedSite{}, nil, err
	}
	if err := audit.Commit(tx); err != nil {
		return db.DeletedSite{}, nil, err
	}
	return site, nil, nil
}

// writeRestoreError answers a failed restoreDeletedSite.
func writeRestoreError(w http.ResponseWriter, refusal *quotaRefusal, err error, what string) {
	switch {
	case refusal != nil:
		refusal.write(w)
	case errors.Is(err, errDeletedSiteNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "no recently deleted site of that name; it was never deleted, is already restored, or its recovery window has ended", Code: "not_found"})
	default:
		log.Printf("restore %s: %v", what, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
	}
}

// listDeletedSites is GET /api/deleted-sites: the caller's recently deleted
// sites, and those of every team they are in.
func (h *SiteHandler) listDeletedSites(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	sites, err := db.ListDeletedSitesForActor(r.Context(), h.database, user.ID)
	if err != nil {
		log.Printf("list deleted sites for %s: %v", user.ID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, toDeletedSiteResponses(sites))
}

// restoreSite is POST /api/sites/{sitename}/restore, in the caller's own
// namespace.
func (h *SiteHandler) restoreSite(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	siteName, ok := validatedSiteName(w, r)
	if !ok {
		return
	}
	h.restoreSiteForTarget(w, r, selfTarget(user), siteName)
}

// restoreCollaborationSite is POST
// /api/collaboration/sites/{owner}/{sitename}/restore: the owner or any
// member of the owning team, the same people who may delete it.
func (h *SiteHandler) restoreCollaborationSite(w http.ResponseWriter, r *http.Request) {
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
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "namespace not found", Code: "not_found"})
		return
	}
	if !requireOwnerOrMember(w, access.Role) {
		return
	}
	h.restoreSiteForTarget(w, r, mutationTarget{
		ActorID: user.ID, ActorUsername: user.Username,
		OwnerID: access.OwnerID, OwnerUsername: access.OwnerUsername,
	}, siteName)
}

func (h *SiteHandler) restoreSiteForTarget(w http.ResponseWriter, r *http.Request, target mutationTarget, siteName string) {
	if decision := h.limits.allow(managementUserPolicy, target.ActorID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	unlock := h.mutations.lock(target.OwnerID, siteName)
	defer unlock()
	actorKind, keyID := auditActorKind(r.Context())
	site, refusal, err := restoreDeletedSite(r.Context(), h.database, h.quota, h.audit, target.OwnerID, siteName, audit.Event{
		ActorID: target.ActorID, ActorKind: actorKind, KeyID: keyID, RequestID: auditRequestID(r.Context()),
	})
	if refusal != nil || err != nil {
		writeRestoreError(w, refusal, err, target.OwnerUsername+"/"+siteName)
		return
	}
	writeJSON(w, http.StatusOK, restoredSiteResponse{
		Owner: target.OwnerUsername, Site: site.Name, ActiveVersion: site.ActiveVersion, Access: site.Access,
		URL: h.siteURL(r.Context(), target.OwnerUsername, site.Name, site.ID),
	})
}

// WithQuota sets the per-owner quota a restore from /admin is checked
// against, and returns h for chaining.
func (h *AdminHandler) WithQuota(quota UploadQuota) *AdminHandler {
	h.quota = quota
	return h
}

// listAllDeletedSites is GET /api/admin/deleted-sites: every site in its
// recovery window, across every owner.
func (h *AdminHandler) listAllDeletedSites(w http.ResponseWriter, r *http.Request) {
	sites, err := db.ListAllDeletedSites(r.Context(), h.database)
	if err != nil {
		log.Printf("admin: list deleted sites: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, toDeletedSiteResponses(sites))
}

// restoreDeletedSiteAsAdmin is POST
// /api/admin/deleted-sites/{owner}/{sitename}/restore, submitted by the
// Recently deleted card on /admin.
func (h *AdminHandler) restoreDeletedSiteAsAdmin(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	owner, err := db.GetUserByUsername(r.Context(), h.database, ownerUsername)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			h.respondAdmin(w, r, http.StatusNotFound, "no recently deleted site of that name")
			return
		}
		log.Printf("admin: resolve owner %q: %v", ownerUsername, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	actorID := ""
	if actor := auth.GetUser(r.Context()); actor != nil {
		actorID = actor.ID
	}
	_, refusal, err := restoreDeletedSite(r.Context(), h.database, h.quota, h.audit, owner.ID, siteName, audit.Event{
		ActorID: actorID, RequestID: auditRequestID(r.Context()),
		Extra: map[string]any{"by_admin": true},
	})
	switch {
	case refusal != nil:
		h.respondAdmin(w, r, refusal.status, refusal.body.Error)
	case errors.Is(err, errDeletedSiteNotFound):
		h.respondAdmin(w, r, http.StatusNotFound, "no recently deleted site of that name")
	case err != nil:
		log.Printf("admin: restore %s/%s: %v", ownerUsername, siteName, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
	default:
		h.respondAdmin(w, r, http.StatusOK, "restored: "+ownerUsername+"/"+siteName)
	}
}

// writeNameHeld refuses a new site whose name a recently deleted site still
// holds: creating it would make that site unrestorable.
func writeNameHeld(w http.ResponseWriter) {
	writeJSON(w, http.StatusConflict, errorResponse{
		Error: "a recently deleted site still holds this name: restore it, or pick another name (the name frees up when its recovery window ends)",
		Code:  "name_held",
	})
}

// renderDeletedSites is the admin page's "Recently deleted" card: every site
// in its recovery window, across every owner, each with Restore. Nothing is
// rendered when there is none.
func (h *AdminHandler) renderDeletedSites(r *http.Request, b *strings.Builder) {
	sites, err := db.ListAllDeletedSites(r.Context(), h.database)
	if err != nil {
		log.Printf("admin: list deleted sites: %v", err)
		return
	}
	if len(sites) == 0 {
		return
	}
	b.WriteString(`<section id="deleted-sites" class="overview"><div class="overview-card"><h2 class="section-title">Recently deleted</h2>
<p class="login-copy">Sites deleted in the last 30 days. Restore brings one back as it was, for its owner: files, saved data, who can open it, viewers and uploaded files. After 30 days a site is removed for good.</p>
<div class="rank-list" role="region" aria-label="Recently deleted sites">`)
	for _, s := range sites {
		action := "/api/admin/deleted-sites/" + url.PathEscape(s.Owner) + "/" + url.PathEscape(s.Name) + "/restore"
		by := "deleted"
		if s.DeletedBy != "" {
			by = "deleted by " + html.EscapeString(s.DeletedBy)
		}
		fmt.Fprintf(b, `<div class="rank-row"><span class="rank-name">%s/%s <span class="rank-sub">%s %s · restorable until %s</span></span><span class="rank-metric"><form method="POST" action="%s"><button type="submit" class="btn-reset">Restore</button></form></span></div>`,
			html.EscapeString(s.Owner), html.EscapeString(s.Name), by,
			localTimeHTML(s.DeletedAt, "datetime"), localTimeHTML(s.PurgeAt(), "datetime"),
			html.EscapeString(action))
	}
	b.WriteString(`</div></div></section>`)
}
