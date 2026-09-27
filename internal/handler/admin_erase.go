package handler

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	db "github.com/vsriram/simple-host/internal/db"
)

// Data subject requests, run by an admin for a disabled person: export
// everything the service holds about them as one archive, or delete them and
// all their data for good. Employees do not delete their own work accounts;
// the company does, the same way it offboards them (disable first).

// personExportTimeout bounds one export stream: every site's live files and
// assets can take a while, but not forever.
const personExportTimeout = 30 * time.Minute

func (h *AdminHandler) registerEraseRoutes(mux *http.ServeMux, adminAPI, dashboardCheck func(http.Handler) http.Handler) {
	mux.Handle("GET /api/admin/users/{username}/export", adminAPI(http.HandlerFunc(h.exportPerson)))
	mux.Handle("POST /api/admin/users/{username}/erase", dashboardCheck(adminAPI(http.HandlerFunc(h.erasePerson))))
}

// erasablePerson resolves username to a disabled person, or writes the
// refusal and returns false.
func (h *AdminHandler) erasablePerson(w http.ResponseWriter, r *http.Request, q db.Querier, lock bool) (db.ErasablePerson, bool) {
	username := strings.ToLower(strings.TrimSpace(r.PathValue("username")))
	person, err := db.GetErasablePerson(r.Context(), q, username, lock)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		h.respondAdmin(w, r, http.StatusNotFound, "no person is called "+username)
		return person, false
	case errors.Is(err, db.ErrNotAPerson):
		h.respondAdmin(w, r, http.StatusBadRequest, username+" is a team; delete the team instead")
		return person, false
	case err != nil:
		log.Printf("admin: resolve person %q: %v", username, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return person, false
	case !person.Disabled:
		h.respondAdmin(w, r, http.StatusConflict, person.Username+" can still sign in; disable the account first")
		return person, false
	}
	return person, true
}

// exportPerson streams one zip of everything held about a disabled person:
// account.json, teams, viewer grants, key/connected-app/session metadata
// (never a secret), every site they own (recently deleted ones too) with its
// live files, saved data and its history, version list and assets, and
// audit-events.jsonl (every audited action they took).
func (h *AdminHandler) exportPerson(w http.ResponseWriter, r *http.Request) {
	person, ok := h.erasablePerson(w, r, h.database, false)
	if !ok {
		return
	}
	release, acquired := h.limits.acquireArchiveDownload()
	if !acquired {
		writeConcurrencyRateLimit(w)
		return
	}
	defer release()
	h.audit.Record(r.Context(), h.userAuditEvent(r, "admin_user_export", person.ID, "", nil))

	deadline := time.Now().Add(personExportTimeout)
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(deadline)
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()

	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": person.Username + "-data.zip",
	}))
	w.WriteHeader(http.StatusOK)
	zw := zip.NewWriter(w)
	if err := h.writePersonExport(ctx, zw, person); err != nil {
		// The status is already sent: a truncated archive fails to open,
		// which is the signal the admin gets.
		log.Printf("admin: export %s: %v", person.Username, err)
		return
	}
	if err := zw.Close(); err != nil {
		log.Printf("admin: finish export %s: %v", person.Username, err)
	}
}

func (h *AdminHandler) writePersonExport(ctx context.Context, zw *zip.Writer, person db.ErasablePerson) error {
	writeJSONEntry := func(name, query, id string) error {
		body, err := db.QueryJSON(ctx, h.database, query, id)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = f.Write(body)
		return err
	}
	for _, part := range db.PersonExportQueries {
		if err := writeJSONEntry(part.File, part.Query, person.ID); err != nil {
			return err
		}
	}
	sites, err := db.ListAllOwnerSites(ctx, h.database, person.ID)
	if err != nil {
		return err
	}
	for _, site := range sites {
		dir := "sites/" + site.Name + "/"
		for _, part := range db.SiteExportQueries {
			if err := writeJSONEntry(dir+part.File, part.Query, site.ID); err != nil {
				return err
			}
		}
		if err := h.exportSiteFiles(ctx, zw, site, dir); err != nil {
			return fmt.Errorf("site %s: %w", site.Name, err)
		}
	}
	return h.exportActorAudit(ctx, zw, person.ID)
}

// exportSiteFiles adds the live version's files under dir/files/ and each
// live asset under dir/assets/<id> (assets.json names them).
func (h *AdminHandler) exportSiteFiles(ctx context.Context, zw *zip.Writer, site db.OwnedSite, dir string) error {
	if h.store == nil {
		return nil
	}
	if site.ActiveVersion > 0 {
		lease, err := h.store.OpenVersion(ctx, site.ID, site.ActiveVersion)
		if err != nil {
			return err
		}
		entries, err := collectArchiveEntries(ctx, lease.FS())
		if err == nil {
			err = streamArchiveEntries(ctx, zw, lease.FS(), entries, dir+"files/")
		}
		_ = lease.Close()
		if err != nil {
			return err
		}
	}
	assets, err := db.ListAssets(ctx, h.database, site.ID)
	if err != nil {
		return err
	}
	for _, asset := range assets {
		lease, err := h.store.OpenAsset(ctx, site.ID, asset.ID, asset.Size, asset.SHA256)
		if err != nil {
			return fmt.Errorf("asset %s: %w", asset.ID, err)
		}
		f, err := zw.Create(dir + "assets/" + asset.ID)
		if err == nil {
			_, err = io.Copy(f, &contextReader{ctx: ctx, reader: lease.File})
		}
		_ = lease.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// exportActorAudit adds audit-events.jsonl: every audit row they are the
// actor of, newest first, in the shape GET /api/admin/export writes.
func (h *AdminHandler) exportActorAudit(ctx context.Context, zw *zip.Writer, userID string) error {
	if h.auditReader == nil {
		return nil
	}
	f, err := zw.Create("audit-events.jsonl")
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(f)
	cursor := ""
	for {
		page, err := h.auditReader.ListAuditEvents(ctx, audit.AuditQuery{Admin: true, Actor: userID, Cursor: cursor})
		if err != nil {
			return err
		}
		for _, e := range page.Events {
			if err := encoder.Encode(toAuditEventResponse(e)); err != nil {
				return err
			}
		}
		if page.NextCursor == "" || page.NextCursor == cursor {
			return nil
		}
		cursor = page.NextCursor
	}
}

// errLastTeamMember carries the teams an erasure would leave empty.
type errLastTeamMember struct{ teams []string }

func (e errLastTeamMember) Error() string { return "last member of " + strings.Join(e.teams, ", ") }

// erasePerson deletes a disabled person and everything of theirs, for good:
// every site they own (live and recently deleted, skipping the recovery
// window; the bucket objects are queued for the retire sweep), saved data
// and its history, assets, keys, connected apps, sessions, viewer grants
// they hold, pending grants naming their email, team memberships, the
// access-log rows of their visits, and the account. The address label stays
// held. The admin confirms by typing the username (`confirm`).
//
// Audit rows stay: the chain must keep verifying. They keep the opaque user
// id until retention prunes them, and the user_erased row written here names
// the person by that id only.
func (h *AdminHandler) erasePerson(w http.ResponseWriter, r *http.Request) {
	confirm := strings.ToLower(strings.TrimSpace(adminFormField(w, r, "confirm")))
	if confirm == "" || confirm != strings.ToLower(strings.TrimSpace(r.PathValue("username"))) {
		h.respondAdmin(w, r, http.StatusBadRequest, "type the username to confirm")
		return
	}
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		log.Printf("admin: erase: begin: %v", err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	defer audit.Rollback(tx)
	person, ok := h.erasablePerson(w, r, tx, true)
	if !ok {
		return
	}
	var sitesDeleted int
	err = func() error {
		lastOf, err := db.LockTeamsOfMember(r.Context(), tx, person.ID)
		if err != nil {
			return err
		}
		if len(lastOf) > 0 {
			return errLastTeamMember{teams: lastOf}
		}
		owned, err := db.ListAllOwnerSites(r.Context(), tx, person.ID)
		if err != nil {
			return err
		}
		sites := make([]db.TeamSite, 0, len(owned))
		for _, s := range owned {
			sites = append(sites, s.TeamSite)
		}
		events, err := retireSites(r.Context(), tx, adminActorID(r), person.ID, sites, map[string]any{"by_admin": true, "erasure": true})
		if err != nil {
			return err
		}
		sitesDeleted = len(events)
		counts, err := db.ErasePerson(r.Context(), tx, person, ownerLabel(person.Username))
		if err != nil {
			return err
		}
		extra := map[string]any{"sites_deleted": sitesDeleted}
		raw, _ := json.Marshal(counts)
		_ = json.Unmarshal(raw, &extra)
		// No username and no email: the row outlives the person and names
		// them by id only.
		events = append(events, h.userAuditEvent(r, "user_erased", person.ID, "", extra))
		// Audit rows last: each insert takes the chain head lock.
		for _, event := range events {
			if err := h.audit.RecordTx(r.Context(), tx, event); err != nil {
				return err
			}
		}
		return audit.Commit(tx)
	}()
	var last errLastTeamMember
	switch {
	case errors.As(err, &last):
		h.respondAdmin(w, r, http.StatusConflict, fmt.Sprintf(
			"%s is the last member of %s; move or delete that team's sites, or delete the team, first",
			person.Username, strings.Join(last.teams, ", ")))
		return
	case err != nil:
		log.Printf("admin: erase %s: %v", person.Username, err)
		h.respondAdmin(w, r, http.StatusInternalServerError, "internal server error")
		return
	}
	h.respondAdmin(w, r, http.StatusOK, person.Username+" and all their data deleted ("+
		pluralize(sitesDeleted, "1 site", formatCount(int64(sitesDeleted))+" sites")+")")
}
