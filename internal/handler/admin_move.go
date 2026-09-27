package handler

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// An admin acts on sites only when nobody else can: those of a person who
// has been disabled, and those of a team none of whose members can sign in.
// Anyone else's sites are theirs (or their team's) to hand over or delete.

// WithQuota sets the per-owner quota an admin's move or restore is held to,
// the same one deploys and owners' own moves are.
func (h *AdminHandler) WithQuota(quota UploadQuota) *AdminHandler {
	h.quota = quota
	return h
}

func (h *AdminHandler) registerMoveRoutes(mux *http.ServeMux, adminAPI, dashboardCheck func(http.Handler) http.Handler) {
	mux.Handle("POST /api/admin/sites/{owner}/{sitename}/transfer", dashboardCheck(adminAPI(http.HandlerFunc(h.transferLeaverSite))))
	mux.Handle("POST /api/admin/users/{username}/transfer-sites", dashboardCheck(adminAPI(http.HandlerFunc(h.transferLeaverSites))))
	mux.Handle("POST /api/admin/users/{username}/delete-sites", dashboardCheck(adminAPI(http.HandlerFunc(h.deleteLeaverSites))))
}

func (h *AdminHandler) mover() siteMover {
	return siteMover{database: h.database, store: h.store, hosts: h.hosts, audit: h.audit, quota: h.quota, admin: true}
}

// leaverOwner resolves username and checks it is a namespace nobody can act
// on any more: a disabled person, or a team with no active member. It writes
// the refusal and returns false otherwise.
func (h *AdminHandler) leaverOwner(w http.ResponseWriter, r *http.Request, username string) (db.MoveDestination, bool) {
	owner, err := db.GetMoveDestination(r.Context(), h.database, strings.ToLower(strings.TrimSpace(username)))
	if errors.Is(err, sql.ErrNoRows) {
		h.respondAdmin(w, r, http.StatusNotFound, "no person or team is called "+username)
		return owner, false
	}
	if err != nil {
		log.Printf("admin: resolve owner %q: %v", username, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return owner, false
	}
	if owner.IsTeam() && owner.ActiveMembers > 0 {
		h.respondAdmin(w, r, http.StatusConflict, owner.Username+" still has a member who can sign in; its members can hand its sites over themselves")
		return owner, false
	}
	if !owner.IsTeam() && !owner.Disabled {
		h.respondAdmin(w, r, http.StatusConflict, owner.Username+" can still sign in and hand their sites over themselves; disable the account first if they have left")
		return owner, false
	}
	return owner, true
}

// adminDestination resolves where an admin is moving sites to: any team
// somebody can still act on, or any person who can still sign in.
func (h *AdminHandler) adminDestination(w http.ResponseWriter, r *http.Request, from db.MoveDestination) (db.MoveDestination, bool) {
	typed := adminMoveTarget(w, r)
	dest, err := resolveDestination(r.Context(), h.database, typed)
	if errors.Is(err, sql.ErrNoRows) {
		h.respondAdmin(w, r, http.StatusNotFound, "no person or team is called "+strings.TrimSpace(typed))
		return dest, false
	}
	if err != nil {
		log.Printf("admin: resolve destination %q: %v", typed, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return dest, false
	}
	if refusal := destinationRefusal(dest); refusal != nil {
		h.respondAdmin(w, r, refusal.status, refusal.body.Error)
		return dest, false
	}
	if dest.ID == from.ID {
		h.respondAdmin(w, r, http.StatusBadRequest, dest.Username+" already owns these sites")
		return dest, false
	}
	return dest, true
}

func adminActorID(r *http.Request) string {
	if actor := auth.GetUser(r.Context()); actor != nil {
		return actor.ID
	}
	return ""
}

// transferLeaverSite moves one site of a leaver or an abandoned team.
func (h *AdminHandler) transferLeaverSite(w http.ResponseWriter, r *http.Request) {
	from, ok := h.leaverOwner(w, r, r.PathValue("owner"))
	if !ok {
		return
	}
	site, err := db.GetSite(r.Context(), h.database, from.ID, r.PathValue("sitename"))
	if errors.Is(err, sql.ErrNoRows) {
		h.respondAdmin(w, r, http.StatusNotFound, "site not found")
		return
	}
	if err != nil {
		log.Printf("admin: resolve site %s/%s: %v", from.Username, r.PathValue("sitename"), err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	dest, ok := h.adminDestination(w, r, from)
	if !ok {
		return
	}
	h.runAdminMoves(w, r, []db.TeamSite{{ID: site.ID, Name: site.Name}}, from, dest)
}

// transferLeaverSites moves every site of a leaver or an abandoned team, all
// or none.
func (h *AdminHandler) transferLeaverSites(w http.ResponseWriter, r *http.Request) {
	from, ok := h.leaverOwner(w, r, r.PathValue("username"))
	if !ok {
		return
	}
	dest, ok := h.adminDestination(w, r, from)
	if !ok {
		return
	}
	sites, err := db.ListOwnerSites(r.Context(), h.database, from.ID)
	if err != nil {
		log.Printf("admin: list sites of %s: %v", from.Username, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	if len(sites) == 0 {
		h.respondAdmin(w, r, http.StatusOK, "no sites to move")
		return
	}
	h.runAdminMoves(w, r, sites, from, dest)
}

func (h *AdminHandler) runAdminMoves(w http.ResponseWriter, r *http.Request, sites []db.TeamSite, from, dest db.MoveDestination) {
	moves := make([]plannedMove, 0, len(sites))
	for _, site := range sites {
		moves = append(moves, plannedMove{
			SiteID: site.ID, OldName: site.Name, NewName: site.Name,
			From: ownerRef{ID: from.ID, Username: from.Username},
			To:   ownerRef{ID: dest.ID, Username: dest.Username},
		})
	}
	refusal, err := h.mover().run(r.Context(), adminActorID(r), "site_transfer", moves, map[string]any{"by_admin": true})
	if refusal != nil {
		h.respondAdmin(w, r, refusal.status, refusal.body.Error)
		return
	}
	if err != nil {
		log.Printf("admin: move sites of %s to %s: %v", from.Username, dest.Username, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	h.respondAdmin(w, r, http.StatusOK, pluralize(len(moves), "1 site", formatCount(int64(len(moves)))+" sites")+" moved to "+dest.Username)
}

// deleteLeaverSites deletes every site of a leaver or an abandoned team, the
// way an owner's delete does: each goes to Recently deleted and can be
// restored for db.DeletedSiteRetention.
func (h *AdminHandler) deleteLeaverSites(w http.ResponseWriter, r *http.Request) {
	from, ok := h.leaverOwner(w, r, r.PathValue("username"))
	if !ok {
		return
	}
	var deleted []audit.Event
	err := func() error {
		tx, err := h.database.BeginTx(r.Context(), nil)
		if err != nil {
			return err
		}
		defer audit.Rollback(tx)
		sites, err := db.ListOwnerSites(r.Context(), tx, from.ID)
		if err != nil {
			return err
		}
		events, err := softDeleteSites(r.Context(), tx, adminActorID(r), from.ID, sites)
		if err != nil {
			return err
		}
		for _, event := range events {
			if err := h.audit.RecordTx(r.Context(), tx, event); err != nil {
				return err
			}
		}
		deleted = events
		return audit.Commit(tx)
	}()
	if err != nil {
		log.Printf("admin: delete sites of %s: %v", from.Username, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	for _, event := range deleted {
		refreshSiteManifest(r.Context(), h.database, h.store, event.SiteID)
	}
	h.respondAdmin(w, r, http.StatusOK, pluralize(len(deleted), "1 site", formatCount(int64(len(deleted)))+" sites")+" deleted")
}

// softDeleteSites marks every one of ownerID's sites listed deleted, in tx,
// as the owner's own delete does (site.go), and returns the site_delete
// events to record. A site deleted, moved or renamed by somebody else
// before its lock is skipped.
func softDeleteSites(ctx context.Context, tx *sql.Tx, actorID, ownerID string, sites []db.TeamSite) ([]audit.Event, error) {
	actorKind, keyID := auditActorKind(ctx)
	for _, site := range sites {
		if err := db.LockSiteCollaboration(ctx, tx, ownerID, site.Name); err != nil {
			return nil, err
		}
	}
	restorableUntil := time.Now().Add(db.DeletedSiteRetention()).UTC().Format(time.RFC3339)
	var events []audit.Event
	for _, site := range sites {
		// Constrained to this owner and name: a site handed to someone else
		// after the list was read is theirs now and is skipped.
		if err := db.SoftDeleteSite(ctx, tx, site.ID, ownerID, site.Name, actorID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, err
		}
		if err := db.EnqueueSiteSearch(ctx, tx, site.ID, db.SiteSearchDelete); err != nil {
			return nil, err
		}
		events = append(events, audit.Event{
			ActorID: actorID, ActorKind: actorKind, KeyID: keyID,
			Action: "site_delete", OwnerID: ownerID, SiteID: site.ID,
			RequestID: auditRequestID(ctx),
			Extra:     map[string]any{"active_version": site.ActiveVersion, "by_admin": true, "restorable_until": restorableUntil},
		})
	}
	return events, nil
}
