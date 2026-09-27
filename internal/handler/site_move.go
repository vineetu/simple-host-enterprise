package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"sort"
	"strings"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// Handing a site over and renaming it are one operation: the site's row
// changes owner or name, and the address it had keeps working as a redirect
// to the one it has now (db.MoveSite, host_gate.go's redirectMovedSite).
// Everything a site holds (files, versions, saved data and its history,
// assets, the access level and named viewers) is keyed by the site's id, so
// none of it is copied or touched.

// ownerRef is a namespace by id and stored name.
type ownerRef struct {
	ID       string
	Username string
}

// siteMover is what a move needs from its handler: the site handler for
// owners and team members, the admin handler for leavers' sites.
type siteMover struct {
	database *sql.DB
	hosts    HostModel
	audit    audit.Recorder
	quota    UploadQuota
}

// moveRefusal is a move the server will not make, and why, in words the
// person can act on.
type moveRefusal struct {
	status int
	body   errorResponse
}

func (r *moveRefusal) write(w http.ResponseWriter) { writeJSON(w, r.status, r.body) }

func refuseMove(status int, code, format string, args ...any) *moveRefusal {
	return &moveRefusal{status: status, body: errorResponse{Error: fmt.Sprintf(format, args...), Code: code}}
}

// siteMoveResponse answers a transfer or rename.
type siteMoveResponse struct {
	Owner             string `json:"owner"`
	Name              string `json:"name"`
	URL               string `json:"url"`
	PreviousOwner     string `json:"previous_owner"`
	PreviousName      string `json:"previous_name"`
	PreviousURL       string `json:"previous_url"`
	PreviousURLStatus string `json:"previous_url_status"`
}

// plannedMove is one site going from one namespace and name to another.
type plannedMove struct {
	SiteID  string
	OldName string
	NewName string
	From    ownerRef
	To      ownerRef
}

func (m plannedMove) fromAddress() db.SiteAddress {
	return db.SiteAddress{OwnerLabel: ownerLabel(m.From.Username), SitePart: siteHostPart(m.OldName)}
}

func (m plannedMove) toAddress() db.SiteAddress {
	return db.SiteAddress{OwnerLabel: ownerLabel(m.To.Username), SitePart: siteHostPart(m.NewName)}
}

// lockMoves takes every name the moves touch, old and new, in one sorted
// order, so two moves in opposite directions cannot deadlock each other,
// and a deploy or delete of either name waits for the move (or finds the
// site gone from it).
func lockMoves(ctx context.Context, tx *sql.Tx, moves []plannedMove) error {
	type key struct{ owner, name string }
	seen := map[key]bool{}
	var keys []key
	for _, m := range moves {
		for _, k := range []key{{m.From.ID, m.OldName}, {m.To.ID, m.NewName}} {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].owner != keys[j].owner {
			return keys[i].owner < keys[j].owner
		}
		return keys[i].name < keys[j].name
	})
	for _, k := range keys {
		if err := db.LockSiteCollaboration(ctx, tx, k.owner, k.name); err != nil {
			return err
		}
	}
	return nil
}

// applyMoves makes every move in tx, all or none: a name already taken at
// the destination, a name that cannot be an address there, or a destination
// over its quota refuses the whole set. It returns the audit events to
// record last (the audit chain's head lock must not be held while row locks
// are still being taken).
func (s siteMover) applyMoves(ctx context.Context, tx *sql.Tx, actorID, action string, moves []plannedMove, extra map[string]any) ([]audit.Event, *moveRefusal, error) {
	if err := lockMoves(ctx, tx, moves); err != nil {
		return nil, nil, err
	}
	destinations := map[string]bool{}
	for _, m := range moves {
		site, err := db.GetSite(ctx, tx, m.From.ID, m.OldName)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && site.ID != m.SiteID) {
			return nil, refuseMove(http.StatusNotFound, "", "site not found"), nil
		}
		if err != nil {
			return nil, nil, err
		}
		if s.hosts.SiteHost(m.To.Username, m.NewName) == "" {
			return nil, refuseMove(http.StatusConflict, "invalid_site_name",
				"%s cannot have a site named %q: its web address would be too long; rename the site first", m.To.Username, m.NewName), nil
		}
		// Held names include the destination's recently deleted sites: a
		// restore brings each back under its name and address, so a move may
		// not take either.
		held, err := db.ListHeldSiteNamesByOwnerUsername(ctx, tx, m.To.Username)
		if err != nil {
			return nil, nil, err
		}
		live, err := db.ListSiteNamesByOwnerUsername(ctx, tx, m.To.Username)
		if err != nil {
			return nil, nil, err
		}
		isLive := make(map[string]bool, len(live))
		for _, name := range live {
			isLive[name] = true
		}
		part := siteHostPart(m.NewName)
		for _, name := range held {
			if m.From.ID == m.To.ID && name == m.OldName {
				continue // the site itself, being renamed
			}
			if name != m.NewName && siteHostPart(name) != part {
				continue
			}
			if !isLive[name] {
				return nil, refuseMove(http.StatusConflict, "name_held",
					"a recently deleted site of %s still holds the name %q; restore it or pick another name (the name frees up when its recovery window ends)", m.To.Username, name), nil
			}
			return nil, refuseMove(http.StatusConflict, "name_conflict",
				"%s already has a site called %q; rename one of the two sites first, then try again", m.To.Username, name), nil
		}
		if err := db.MoveSite(ctx, tx, m.SiteID, m.To.ID, m.NewName, m.fromAddress(), m.toAddress()); err != nil {
			if errors.Is(err, db.ErrSiteNameTaken) {
				return nil, refuseMove(http.StatusConflict, "name_conflict",
					"%s already has a site called %q; rename one of the two sites first, then try again", m.To.Username, m.NewName), nil
			}
			if errors.Is(err, db.ErrUserNotFound) {
				return nil, refuseMove(http.StatusNotFound, "destination_not_found", "%s no longer exists", m.To.Username), nil
			}
			return nil, nil, err
		}
		if err := db.EnqueueSiteSearch(ctx, tx, m.SiteID, db.SiteSearchReconcile); err != nil {
			return nil, nil, err
		}
		if m.From.ID != m.To.ID {
			destinations[m.To.ID] = true
		}
	}
	for ownerID := range destinations {
		if refusal, err := s.checkMoveQuota(ctx, tx, ownerID, moves); refusal != nil || err != nil {
			return nil, refusal, err
		}
	}
	actorKind, keyID := auditActorKind(ctx)
	events := make([]audit.Event, 0, len(moves))
	for _, m := range moves {
		detail := map[string]any{"from": m.From.Username, "to": m.To.Username, "from_name": m.OldName, "name": m.NewName}
		for k, v := range extra {
			detail[k] = v
		}
		events = append(events, audit.Event{
			ActorID: actorID, ActorKind: actorKind, KeyID: keyID,
			Action: action, OwnerID: m.To.ID, SiteID: m.SiteID,
			RequestID: auditRequestID(ctx), Extra: detail,
		})
	}
	return events, nil, nil
}

// checkMoveQuota refuses a move that takes ownerID over its site count or
// stored bytes, the same limits a deploy is held to.
func (s siteMover) checkMoveQuota(ctx context.Context, tx *sql.Tx, ownerID string, moves []plannedMove) (*moveRefusal, error) {
	if !s.quota.enforced() {
		return nil, nil
	}
	if err := db.LockOwnerQuota(ctx, tx, ownerID); err != nil {
		return nil, err
	}
	usage, err := db.OwnerUsageOf(ctx, tx, ownerID)
	if err != nil {
		return nil, err
	}
	name := ""
	for _, m := range moves {
		if m.To.ID == ownerID {
			name = m.To.Username
		}
	}
	if s.quota.MaxSites > 0 && usage.Sites > s.quota.MaxSites {
		return refuseMove(http.StatusConflict, "site_limit",
			"%s would have %s sites, more than the %s allowed; delete a site there first", name, formatCount(usage.Sites), formatCount(s.quota.MaxSites)), nil
	}
	if s.quota.MaxBytes > 0 && usage.Bytes > s.quota.MaxBytes {
		return &moveRefusal{status: http.StatusRequestEntityTooLarge, body: errorResponse{
			Error: fmt.Sprintf("%s would hold %s, more than the %s allowed; free space there first",
				name, formatBytes(uint64(usage.Bytes)), formatBytes(uint64(s.quota.MaxBytes))),
			Code: "storage_quota",
		}}, nil
	}
	return nil, nil
}

// run applies moves in one transaction and records their audit rows.
func (s siteMover) run(ctx context.Context, actorID, action string, moves []plannedMove, extra map[string]any) (*moveRefusal, error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer audit.Rollback(tx)
	events, refusal, err := s.applyMoves(ctx, tx, actorID, action, moves, extra)
	if refusal != nil || err != nil {
		return refusal, err
	}
	for _, event := range events {
		if err := s.audit.RecordTx(ctx, tx, event); err != nil {
			return nil, err
		}
	}
	return nil, audit.Commit(tx)
}

func (s siteMover) response(m plannedMove) siteMoveResponse {
	return siteMoveResponse{
		Owner: m.To.Username, Name: m.NewName, URL: s.hosts.SiteURL(m.To.Username, m.NewName),
		PreviousOwner: m.From.Username, PreviousName: m.OldName,
		PreviousURL:       s.hosts.SiteURL(m.From.Username, m.OldName),
		PreviousURLStatus: "redirects",
	}
}

// resolveDestination finds the person or team a site is being handed to:
// the exact stored name first, then, for a bare name, the team "team-<name>"
// (so "sales" reaches team-sales unless a person is called sales).
func resolveDestination(ctx context.Context, q db.Querier, typed string) (db.MoveDestination, error) {
	name := strings.ToLower(strings.TrimSpace(typed))
	if name == "" {
		return db.MoveDestination{}, sql.ErrNoRows
	}
	dest, err := db.GetMoveDestination(ctx, q, name)
	if errors.Is(err, sql.ErrNoRows) && !strings.HasPrefix(name, db.TeamPrefix) {
		dest, err = db.GetMoveDestination(ctx, q, teamName(name))
	}
	return dest, err
}

// destinationRefusal applies the rule every destination must pass: a person
// who can still sign in, or a team somebody can still act on. It does not
// decide whether the caller may give to it (see transferSite).
func destinationRefusal(dest db.MoveDestination) *moveRefusal {
	if dest.IsTeam() && dest.ActiveMembers == 0 {
		return refuseMove(http.StatusConflict, "destination_inactive", "nobody in %s can sign in any more, so it cannot receive sites", dest.Username)
	}
	if !dest.IsTeam() && dest.Disabled {
		return refuseMove(http.StatusConflict, "destination_inactive", "%s can no longer sign in, so they cannot receive sites", dest.Username)
	}
	return nil
}

func destinationNotFound(typed string) *moveRefusal {
	return refuseMove(http.StatusNotFound, "destination_not_found",
		"no person or team you can hand sites to is called %q; a team must be one you are in, and a person must have signed in at least once", strings.TrimSpace(typed))
}

func (h *SiteHandler) mover() siteMover {
	return siteMover{database: h.database, hosts: h.hosts, audit: h.audit, quota: h.quota}
}

func (h *SiteHandler) registerMoveRoutes(mux *http.ServeMux, ownerMutation, browserWrite func(http.Handler) http.Handler) {
	mux.Handle("POST /api/sites/{sitename}/transfer", browserWrite(ownerMutation(http.HandlerFunc(h.transferSite))))
	mux.Handle("POST /api/collaboration/sites/{owner}/{sitename}/transfer", browserWrite(ownerMutation(http.HandlerFunc(h.transferSite))))
	mux.Handle("POST /api/sites/{sitename}/rename", browserWrite(ownerMutation(http.HandlerFunc(h.renameSite))))
	mux.Handle("POST /api/collaboration/sites/{owner}/{sitename}/rename", browserWrite(ownerMutation(http.HandlerFunc(h.renameSite))))
}

// movableSite resolves the site a transfer or rename names and checks the
// caller owns it or is in the team that does. The owner-scoped routes name
// the caller's own namespace.
func (h *SiteHandler) movableSite(w http.ResponseWriter, r *http.Request) (*db.User, db.SiteAccess, bool) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return nil, db.SiteAccess{}, false
	}
	siteName, ok := validatedSiteName(w, r)
	if !ok {
		return nil, db.SiteAccess{}, false
	}
	owner := r.PathValue("owner")
	if owner == "" {
		owner = user.Username
	}
	if decision := h.limits.allow(managementUserPolicy, user.ID); !decision.Allowed {
		writeRateLimit(w, decision)
		return nil, db.SiteAccess{}, false
	}
	access, ok := h.resolveCollaborationAccess(w, r, owner, siteName)
	if !ok {
		return nil, db.SiteAccess{}, false
	}
	if !requireOwnerOrMember(w, access.Role) || !validateStoredUsername(w, access.OwnerUsername) {
		return nil, db.SiteAccess{}, false
	}
	return user, access, true
}

// transferSite hands a site the caller owns (or a team site of a team they
// are in) to a team they belong to or to any person who can still sign in.
func (h *SiteHandler) transferSite(w http.ResponseWriter, r *http.Request) {
	user, access, ok := h.movableSite(w, r)
	if !ok {
		return
	}
	var req struct {
		To string `json:"to"`
	}
	if !decodeSmallJSON(w, r, &req) {
		return
	}
	dest, err := resolveDestination(r.Context(), h.database, req.To)
	if errors.Is(err, sql.ErrNoRows) {
		destinationNotFound(req.To).write(w)
		return
	}
	if err != nil {
		log.Printf("transfer: resolve destination %q: %v", req.To, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if dest.IsTeam() {
		member, err := db.IsTeamMember(r.Context(), h.database, dest.ID, user.ID)
		if err != nil {
			log.Printf("transfer: team membership %s: %v", dest.Username, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return
		}
		if !member {
			destinationNotFound(req.To).write(w)
			return
		}
	}
	if refusal := destinationRefusal(dest); refusal != nil {
		refusal.write(w)
		return
	}
	if dest.ID == access.OwnerID {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("%s already owns this site", dest.Username), Code: "same_owner"})
		return
	}
	move := plannedMove{
		SiteID: access.Site.ID, OldName: access.Site.Name, NewName: access.Site.Name,
		From: ownerRef{ID: access.OwnerID, Username: access.OwnerUsername},
		To:   ownerRef{ID: dest.ID, Username: dest.Username},
	}
	h.finishMove(w, r, user.ID, "site_transfer", move)
}

// renameSite gives a site a new name under the same owner; the old address
// redirects to the new one until a site takes the old name again.
func (h *SiteHandler) renameSite(w http.ResponseWriter, r *http.Request) {
	user, access, ok := h.movableSite(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decodeSmallJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if reservedSiteNames[name] {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "site name is reserved"})
		return
	}
	if !h.hosts.ValidNewSiteName(access.OwnerUsername, name) {
		writeJSON(w, http.StatusBadRequest, errorResponse{
			Error: fmt.Sprintf("site names use lowercase letters, numbers and hyphens, start and end with a letter or number, and are at most %d characters; the name becomes the site's own web address", MaxSiteNameLen(access.OwnerUsername)),
			Code:  "invalid_site_name",
		})
		return
	}
	if name == access.Site.Name {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "the site already has that name", Code: "same_name"})
		return
	}
	owner := ownerRef{ID: access.OwnerID, Username: access.OwnerUsername}
	move := plannedMove{SiteID: access.Site.ID, OldName: access.Site.Name, NewName: name, From: owner, To: owner}
	h.finishMove(w, r, user.ID, "site_rename", move)
}

func (h *SiteHandler) finishMove(w http.ResponseWriter, r *http.Request, actorID, action string, move plannedMove) {
	mover := h.mover()
	refusal, err := mover.run(r.Context(), actorID, action, []plannedMove{move}, nil)
	if refusal != nil {
		refusal.write(w)
		return
	}
	if err != nil {
		log.Printf("%s %s/%s -> %s/%s: %v", action, move.From.Username, move.OldName, move.To.Username, move.NewName, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, mover.response(move))
}

// adminMoveTarget reads the destination of an admin move from the dashboard
// form (`to`) or a JSON body.
func adminMoveTarget(w http.ResponseWriter, r *http.Request) string {
	return adminFormField(w, r, "to")
}

// adminFormField reads one string field of an admin action, sent either as
// the dashboard's form or as a JSON body.
func adminFormField(w http.ResponseWriter, r *http.Request, name string) string {
	r.Body = http.MaxBytesReader(w, r.Body, smallFormBodyBytes)
	if mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mediaType == "application/json" {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		value, _ := body[name].(string)
		return value
	}
	return r.FormValue(name)
}
