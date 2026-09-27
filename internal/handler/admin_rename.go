package handler

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/vsriram/simple-host/internal/audit"
	db "github.com/vsriram/simple-host/internal/db"
)

// renameUser is an admin's rename of a person's address after a name
// change (POST /api/admin/users/{username}/rename, field "name"): the
// username, which is the label in every one of their addresses, becomes
// the new name. Their sites move with it (same ids, nothing copied), each
// old site address redirects to the new one for people allowed to open the
// site, their old owner page redirects for anyone signed in, the old name
// is held so nobody else inherits the links, and the owner-hosts
// reconciler requests the new name's certificate (sites answer at
// "<new>.<base>/<site>/" until it is ready). Refused for a team, for a
// name another person or team has or had (held after a rename or an
// erasure), a team's old address, and a name that is not a valid address.
// Audited as admin_rename_user.
func (h *AdminHandler) renameUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	newName := strings.ToLower(strings.TrimSpace(adminFormField(w, r, "name")))
	if newName == "" {
		h.respondAdmin(w, r, http.StatusBadRequest, "a new name is required")
		return
	}
	if strings.Contains(newName, ".") || validateOwnerName(newName) != nil {
		h.respondAdmin(w, r, http.StatusBadRequest, fmt.Sprintf("%q cannot be an address: use lowercase letters, numbers and single hyphens, starting and ending with a letter or number, at most 63 characters (and not a reserved name)", newName))
		return
	}
	target, err := db.GetUserByUsername(r.Context(), h.database, username)
	if errors.Is(err, sql.ErrNoRows) {
		h.respondAdmin(w, r, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		log.Printf("admin: resolve user %q: %v", username, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		log.Printf("admin: rename %q: begin: %v", username, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	defer audit.Rollback(tx)
	oldName, sites, err := db.RenamePerson(r.Context(), tx, target.ID, newName, siteHostPart)
	if err != nil {
		if msg, refused := db.RenameFailed(err); refused {
			status := http.StatusConflict
			if errors.Is(err, db.ErrRenameTeamPrefix) || errors.Is(err, db.ErrRenameSameName) {
				status = http.StatusBadRequest
			}
			h.respondAdmin(w, r, status, msg)
			return
		}
		log.Printf("admin: rename %q to %q: %v", username, newName, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	for _, s := range sites {
		if s.Deleted {
			continue
		}
		// Search results link to the site's address and credit its owner.
		if err := db.EnqueueSiteSearch(r.Context(), tx, s.ID, db.SiteSearchReconcile); err != nil {
			log.Printf("admin: rename %q: reindex %s: %v", username, s.ID, err)
			h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
			return
		}
	}
	if err := h.audit.RecordTx(r.Context(), tx, h.userAuditEvent(r, "admin_rename_user", target.ID, newName,
		map[string]any{"from": oldName, "to": newName, "sites": len(sites)})); err != nil {
		log.Printf("admin: rename %q: audit: %v", username, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	if err := audit.Commit(tx); err != nil {
		log.Printf("admin: rename %q: commit: %v", username, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	for _, s := range sites {
		refreshSiteManifest(r.Context(), h.database, h.store, s.ID)
	}
	h.respondAdmin(w, r, http.StatusOK, fmt.Sprintf("%s is now %s; their old addresses redirect", oldName, newName))
}
