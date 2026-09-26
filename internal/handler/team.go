package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/safepath"
	"github.com/vsriram/simple-host/internal/storage"
)

// TeamHandler owns the team namespace lifecycle: creating one, who is in it,
// and deleting it. Sites belonging to a team are not handled here — a team is
// a namespace like a person, so its sites go through the owner-qualified
// collaboration routes, which authorise membership per request.
//
// There is one role. Every route below that requires membership requires only
// membership: a person is in the team or they are not, and being in it grants
// all of it.
type TeamHandler struct {
	database *sql.DB
	limits   *AbuseLimits
	// audit defaults to audit.NoOp{} (see WithAudit), the same chaining
	// shape SiteHandler.WithAudit uses.
	audit audit.Recorder
}

func NewTeamHandler(database *sql.DB, limits ...*AbuseLimits) *TeamHandler {
	return &TeamHandler{
		database: database,
		limits:   chooseAbuseLimits(limits),
		audit:    audit.NoOp{},
	}
}

// WithAudit attaches an audit recorder and returns h for chaining. Called
// once from main.go alongside the other handlers' own audit wiring.
func (h *TeamHandler) WithAudit(recorder audit.Recorder) *TeamHandler {
	if recorder != nil {
		h.audit = recorder
	}
	return h
}

// maxTeamsPerPerson caps how many teams one person may create. It bounds name
// squatting without needing moderation: the platform-admin route can still add
// somebody to a team that has run out of reachable members.
const maxTeamsPerPerson = 10

func (h *TeamHandler) Register(mux *http.ServeMux, authMiddleware, skillVersionMiddleware func(http.Handler) http.Handler, hosts HostModel, publicBaseURL string) {
	member := func(next http.Handler) http.Handler {
		return h.limitManagementClient(authMiddleware(skillVersionMiddleware(auth.RequireRealUser(next))))
	}
	// Cookie-authenticated writes must prove they came from this site, the
	// same guard the site routes use. An X-API-Key request passes untouched.
	browserWrite := cookieOriginCheck(hosts, publicBaseURL)

	mux.Handle("POST /api/teams", browserWrite(member(http.HandlerFunc(h.createTeam))))
	mux.Handle("GET /api/teams", member(http.HandlerFunc(h.listTeams)))
	mux.Handle("GET /api/teams/{team}/members", member(http.HandlerFunc(h.listMembers)))
	mux.Handle("GET /api/teams/{team}/member-candidates", member(http.HandlerFunc(h.searchMemberCandidates)))
	mux.Handle("POST /api/teams/{team}/members", browserWrite(member(http.HandlerFunc(h.addMembers))))
	mux.Handle("DELETE /api/teams/{team}/members/{username}", browserWrite(member(http.HandlerFunc(h.removeMember))))
	mux.Handle("POST /api/teams/{team}/leave", browserWrite(member(http.HandlerFunc(h.leaveTeam))))
	mux.Handle("DELETE /api/teams/{team}", browserWrite(member(http.HandlerFunc(h.deleteTeam))))
}

type teamResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type teamMemberResponse struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	JoinedAt string `json:"joined_at"`
}

// requireMember resolves the team named in the path and checks the caller
// belongs to it. A team that does not exist and a team the caller is not in
// both answer 404: whether a given name is a team is not something a stranger
// needs told.
func (h *TeamHandler) requireMember(w http.ResponseWriter, r *http.Request, user *db.User) (db.Team, bool) {
	name := strings.ToLower(strings.TrimSpace(r.PathValue("team")))
	if safepath.ValidateSegment(name) != nil {
		http.NotFound(w, r)
		return db.Team{}, false
	}
	team, err := db.GetTeamByName(r.Context(), h.database, name)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("resolve team %q: %v", name, err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
			return db.Team{}, false
		}
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "team not found", Code: "not_found"})
		return db.Team{}, false
	}
	isMember, err := db.IsTeamMember(r.Context(), h.database, team.ID, user.ID)
	if err != nil {
		log.Printf("check membership of %q: %v", name, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return db.Team{}, false
	}
	if !isMember {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "team not found", Code: "not_found"})
		return db.Team{}, false
	}
	return team, true
}

func (h *TeamHandler) createTeam(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	if decision := h.limits.allow(managementUserPolicy, user.ID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}

	var request struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, smallFormBodyBytes)).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}
	name := strings.ToLower(strings.TrimSpace(request.Name))

	// A team name is the first name on this platform somebody types rather
	// than has derived from their email, so it goes through the stricter rule:
	// dots are refused outright instead of folded to hyphens, because
	// "first.last" and "first-last" would otherwise be one hostname under two
	// spellings and the person would silently get a name they did not type.
	if err := validateTypedName(name); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: teamNameRefusal(err)})
		return
	}

	teams, err := db.ListTeamsForUser(r.Context(), h.database, user.ID)
	if err != nil {
		log.Printf("list teams for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if len(teams) >= maxTeamsPerPerson {
		writeJSON(w, http.StatusConflict, errorResponse{
			Error: "you already belong to the maximum number of teams",
			Code:  "team_limit",
		})
		return
	}

	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer audit.Rollback(tx)

	team, err := db.CreateTeam(r.Context(), tx, name, user.ID)
	if err != nil {
		// Two spellings of one hostname collide on the label index rather than
		// on the username column, and they need different words: "taken" is
		// true but baffling when the name you typed is nowhere on screen.
		if isOwnerLabelViolation(err) {
			writeJSON(w, http.StatusConflict, errorResponse{
				Error: "that name is too close to an existing account name; they would share one web address",
				Code:  "name_conflict",
			})
			return
		}
		if isUniqueViolation(err) {
			writeJSON(w, http.StatusConflict, errorResponse{Error: "that name is already taken", Code: "name_taken"})
			return
		}
		log.Printf("create team %q: %v", name, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	actorKind, keyID := auditActorKind(r.Context())
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: user.ID, ActorKind: actorKind, KeyID: keyID,
		Action: "team_create", TeamID: team.ID,
		RequestID: auditRequestID(r.Context()),
		Detail:    name,
	}); err != nil {
		log.Printf("record audit for team_create %q: %v", name, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := audit.Commit(tx); err != nil {
		log.Printf("commit create team %q: %v", name, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusCreated, teamResponse{ID: team.ID, Name: team.Username})
}

// teamNameRefusal turns a validation sentinel into words for somebody who just
// typed a name. The dotted case is the one worth spelling out, because every
// existing account name has a dot in it and the rule looks arbitrary without
// the reason.
func teamNameRefusal(err error) string {
	switch {
	case errors.Is(err, errTypedNameDotted):
		return "team names use letters, numbers and hyphens only — no dots"
	case errors.Is(err, errOwnerNameReserved):
		return "that name is reserved"
	default:
		return "team names use lowercase letters, numbers and hyphens, and must start and end with a letter or number"
	}
}

func (h *TeamHandler) listTeams(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	teams, err := db.ListTeamsForUser(r.Context(), h.database, user.ID)
	if err != nil {
		log.Printf("list teams for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	response := make([]teamResponse, 0, len(teams))
	for _, team := range teams {
		response = append(response, teamResponse{ID: team.ID, Name: team.Username})
	}
	writeJSON(w, http.StatusOK, map[string]any{"teams": response})
}

func (h *TeamHandler) listMembers(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	team, ok := h.requireMember(w, r, user)
	if !ok {
		return
	}
	members, err := db.ListTeamMembers(r.Context(), h.database, team.ID)
	if err != nil {
		log.Printf("list members of %q: %v", team.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	response := make([]teamMemberResponse, 0, len(members))
	for _, member := range members {
		response = append(response, teamMemberResponse{
			UserID:   member.UserID,
			Username: member.Username,
			JoinedAt: member.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"team": team.Username, "members": response})
}

func (h *TeamHandler) searchMemberCandidates(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	team, ok := h.requireMember(w, r, user)
	if !ok {
		return
	}
	search := strings.TrimSpace(r.URL.Query().Get("q"))
	if search == "" {
		writeJSON(w, http.StatusOK, map[string]any{"candidates": []db.UserCandidate{}})
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	candidates, err := db.SearchTeamMemberCandidates(r.Context(), h.database, team.ID, search, limit)
	if err != nil {
		log.Printf("search member candidates for %q: %v", team.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	response := make([]map[string]any, 0, len(candidates))
	for _, candidate := range candidates {
		response = append(response, map[string]any{
			"user_id":        candidate.UserID,
			"username":       candidate.Username,
			"already_member": candidate.AlreadyMember,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": response})
}

func (h *TeamHandler) addMembers(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	if decision := h.limits.allow(managementUserPolicy, user.ID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	team, ok := h.requireMember(w, r, user)
	if !ok {
		return
	}

	var request struct {
		Usernames []string `json:"usernames"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, smallFormBodyBytes)).Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		return
	}
	if len(request.Usernames) == 0 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "no usernames given"})
		return
	}
	if len(request.Usernames) > db.MaxTeamMembers {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "too many usernames"})
		return
	}

	err := h.inTeamTransaction(r, team.ID, func(tx *sql.Tx) error {
		if err := db.AddTeamMembers(r.Context(), tx, team.ID, request.Usernames, user.ID); err != nil {
			return err
		}
		if err := db.TeamAudit(r.Context(), tx, team.ID, user.ID, "add_members", "", strings.Join(request.Usernames, ",")); err != nil {
			return err
		}
		actorKind, keyID := auditActorKind(r.Context())
		return h.audit.RecordTx(r.Context(), tx, audit.Event{
			ActorID: user.ID, ActorKind: actorKind, KeyID: keyID,
			Action: "member_add", TeamID: team.ID,
			RequestID: auditRequestID(r.Context()),
			Extra:     map[string]any{"usernames": request.Usernames},
		})
	})
	switch {
	case errors.Is(err, db.ErrTeamMemberNotFound):
		writeJSON(w, http.StatusConflict, errorResponse{
			Error: "one or more of those names is not a person on Simple Host",
			Code:  "member_not_found",
		})
		return
	case errors.Is(err, db.ErrTeamMemberLimit):
		writeJSON(w, http.StatusConflict, errorResponse{Error: "team member limit reached", Code: "member_limit"})
		return
	case err != nil:
		log.Printf("add members to %q: %v", team.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	h.writeMembers(w, r, team)
}

func (h *TeamHandler) removeMember(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	if decision := h.limits.allow(managementUserPolicy, user.ID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	team, ok := h.requireMember(w, r, user)
	if !ok {
		return
	}
	username := strings.ToLower(strings.TrimSpace(r.PathValue("username")))
	if safepath.ValidateSegment(username) != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid username"})
		return
	}
	subject, err := db.GetUserByUsername(r.Context(), h.database, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "not a member", Code: "not_found"})
			return
		}
		log.Printf("resolve member %q: %v", username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if subject.ID == user.ID {
		h.leave(w, r, user, team)
		return
	}

	err = h.inTeamTransaction(r, team.ID, func(tx *sql.Tx) error {
		if err := db.RemoveTeamMember(r.Context(), tx, team.ID, subject.ID); err != nil {
			return err
		}
		if err := db.TeamAudit(r.Context(), tx, team.ID, user.ID, "remove_member", subject.ID, ""); err != nil {
			return err
		}
		actorKind, keyID := auditActorKind(r.Context())
		return h.audit.RecordTx(r.Context(), tx, audit.Event{
			ActorID: user.ID, ActorKind: actorKind, KeyID: keyID,
			Action: "member_remove", TeamID: team.ID, SubjectID: subject.ID,
			RequestID: auditRequestID(r.Context()),
		})
	})
	switch {
	case errors.Is(err, db.ErrLastTeamMember):
		// Unreachable from here — removing yourself goes through leave, and
		// the caller stays — but RemoveTeamMember still refuses to empty a
		// team, so the answer stays mapped.
		writeJSON(w, http.StatusConflict, errorResponse{
			Error: "a team keeps at least one member — leave or delete the team instead",
			Code:  "last_member",
		})
		return
	case errors.Is(err, db.ErrTeamMemberNotFound):
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "not a member", Code: "not_found"})
		return
	case err != nil:
		log.Printf("remove member %q from %q: %v", username, team.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	h.writeMembers(w, r, team, "team_deleted", false)
}

func (h *TeamHandler) deleteTeam(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	if decision := h.limits.allow(managementUserPolicy, user.ID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	team, ok := h.requireMember(w, r, user)
	if !ok {
		return
	}

	var sites int
	err := h.inTeamTransaction(r, team.ID, func(tx *sql.Tx) error {
		var err error
		if sites, err = db.CountTeamSites(r.Context(), tx, team.ID); err != nil {
			return err
		}
		// A team with sites takes its name typed back: this deletes them.
		if sites > 0 && !confirmedTeamName(r, team) {
			return errTeamDeleteUnconfirmed
		}
		return deleteTeamAndSites(r.Context(), tx, h.audit, user.ID, team, "deleted")
	})
	switch {
	case errors.Is(err, errTeamDeleteUnconfirmed):
		writeTeamDeleteUnconfirmed(w, team, sites, "Deleting")
		return
	case err != nil:
		log.Printf("delete team %q: %v", team.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (h *TeamHandler) leaveTeam(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	if decision := h.limits.allow(managementUserPolicy, user.ID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	team, ok := h.requireMember(w, r, user)
	if !ok {
		return
	}
	h.leave(w, r, user, team)
}

// leave takes the caller out of the team. When nobody who can still sign in
// would be left — disabled accounts do not count — the team and every site
// it owns are deleted instead, which needs the team's name typed back as
// confirm_name. Answers with the remaining members, or with team_deleted
// and how many sites went with it.
func (h *TeamHandler) leave(w http.ResponseWriter, r *http.Request, user *db.User, team db.Team) {
	var sites int
	deleted := false
	err := h.inTeamTransaction(r, team.ID, func(tx *sql.Tx) error {
		others, err := db.CountOtherActiveTeamMembers(r.Context(), tx, team.ID, user.ID)
		if err != nil {
			return err
		}
		if others == 0 {
			if sites, err = db.CountTeamSites(r.Context(), tx, team.ID); err != nil {
				return err
			}
			if !confirmedTeamName(r, team) {
				return errTeamDeleteUnconfirmed
			}
			deleted = true
			return deleteTeamAndSites(r.Context(), tx, h.audit, user.ID, team, "last_member_left")
		}
		if err := db.RemoveTeamMember(r.Context(), tx, team.ID, user.ID); err != nil {
			return err
		}
		if err := db.TeamAudit(r.Context(), tx, team.ID, user.ID, "leave", user.ID, ""); err != nil {
			return err
		}
		actorKind, keyID := auditActorKind(r.Context())
		return h.audit.RecordTx(r.Context(), tx, audit.Event{
			ActorID: user.ID, ActorKind: actorKind, KeyID: keyID,
			Action: "member_remove", TeamID: team.ID, SubjectID: user.ID,
			RequestID: auditRequestID(r.Context()),
			Detail:    "left",
		})
	})
	switch {
	case errors.Is(err, errTeamDeleteUnconfirmed):
		writeTeamDeleteUnconfirmed(w, team, sites, "You are the last active member, so leaving deletes")
		return
	case err != nil:
		log.Printf("leave team %q: %v", team.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if deleted {
		writeJSON(w, http.StatusOK, map[string]any{
			"team": team.Username, "members": []teamMemberResponse{},
			"team_deleted": true, "sites_deleted": sites,
		})
		return
	}
	h.writeMembers(w, r, team, "team_deleted", false)
}

var errTeamDeleteUnconfirmed = errors.New("team delete not confirmed")

// confirmedTeamName reports whether the request typed the team's name back
// as ?confirm_name=, the guard on every path that deletes a team's sites.
func confirmedTeamName(r *http.Request, team db.Team) bool {
	return strings.ToLower(strings.TrimSpace(r.URL.Query().Get("confirm_name"))) == team.Username
}

// writeTeamDeleteUnconfirmed says how much would be destroyed, so the agent
// can put that number to the person before asking again.
func writeTeamDeleteUnconfirmed(w http.ResponseWriter, team db.Team, sites int, lead string) {
	writeJSON(w, http.StatusConflict, struct {
		errorResponse
		SiteCount int `json:"site_count"`
	}{
		errorResponse: errorResponse{
			Error: fmt.Sprintf("%s team %s and its %s permanently. Confirm with the person, then repeat with confirm_name=%s.",
				lead, team.Username, pluralize(sites, "1 site", fmt.Sprintf("%d sites", sites)), team.Username),
			Code: "confirm_team_delete",
		},
		SiteCount: sites,
	})
}

// deleteTeamAndSites deletes a team and every site it owns in tx, which must
// already hold LockTeam: while the lock is held no site can be created under
// the team (the insert's foreign-key check waits on it). Each site goes the
// way deleteSiteForTarget sends one — search entry dropped, files queued for
// retirement after the grace period, a site_delete audit row — and then the
// team itself, with a team_delete row carrying why.
func deleteTeamAndSites(ctx context.Context, tx *sql.Tx, recorder audit.Recorder, actorID string, team db.Team, reason string) error {
	sites, err := db.ListTeamSites(ctx, tx, team.ID)
	if err != nil {
		return err
	}
	actorKind, keyID := auditActorKind(ctx)
	var events []audit.Event
	// Each site's own lock first, in name order, so a deploy or rollback in
	// flight finishes (its new files land under the prefix retired below)
	// or, arriving after, finds the site gone.
	for _, site := range sites {
		if err := db.LockSiteCollaboration(ctx, tx, team.ID, site.Name); err != nil {
			return err
		}
	}
	for _, site := range sites {
		if err := db.EnqueueSiteSearch(ctx, tx, site.ID, db.SiteSearchDelete); err != nil {
			return err
		}
		prefix, err := storage.SitePrefix(site.ID)
		if err != nil {
			return err
		}
		if err := db.RetireObjects(ctx, tx, prefix, storage.RetireGrace); err != nil {
			return err
		}
		if err := db.DeleteSite(ctx, tx, team.ID, site.Name); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue // deleted by somebody else before our lock; its own delete queued it
			}
			return err
		}
		events = append(events, audit.Event{
			ActorID: actorID, ActorKind: actorKind, KeyID: keyID,
			Action: "site_delete", OwnerID: team.ID, SiteID: site.ID,
			RequestID: auditRequestID(ctx),
			Extra:     map[string]any{"active_version": site.ActiveVersion, "team_delete": reason},
		})
	}
	if err := db.TeamAudit(ctx, tx, team.ID, actorID, "delete", "", team.Username); err != nil {
		return err
	}
	if err := db.DeleteTeam(ctx, tx, team.ID); err != nil {
		return err
	}
	// The audit rows go last: inserting one takes the audit chain's head
	// lock (migration 0036), which must not be held while this transaction
	// still waits for site or team row locks.
	events = append(events, audit.Event{
		ActorID: actorID, ActorKind: actorKind, KeyID: keyID,
		Action: "team_delete", TeamID: team.ID,
		RequestID: auditRequestID(ctx),
		Detail:    team.Username,
		Extra:     map[string]any{"reason": reason, "sites_deleted": len(sites)},
	})
	for _, event := range events {
		if err := recorder.RecordTx(ctx, tx, event); err != nil {
			return err
		}
	}
	return nil
}

// inTeamTransaction runs fn with the team's row locked for the rest of the
// transaction. Every membership change goes through here so two of them
// serialize instead of both reading a pre-change member count — which is what
// stops two people removing each other and emptying the team.
func (h *TeamHandler) inTeamTransaction(r *http.Request, teamID string, fn func(tx *sql.Tx) error) error {
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer audit.Rollback(tx)
	if err := db.LockTeam(r.Context(), tx, teamID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return audit.Commit(tx)
}

// writeMembers answers with the team's members. extra is key/value pairs added
// to the body: removing somebody says the team was not deleted, which adding
// has no reason to.
func (h *TeamHandler) writeMembers(w http.ResponseWriter, r *http.Request, team db.Team, extra ...any) {
	members, err := db.ListTeamMembers(r.Context(), h.database, team.ID)
	if err != nil {
		log.Printf("list members of %q: %v", team.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	response := make([]teamMemberResponse, 0, len(members))
	for _, member := range members {
		response = append(response, teamMemberResponse{
			UserID:   member.UserID,
			Username: member.Username,
			JoinedAt: member.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	body := map[string]any{"team": team.Username, "members": response}
	for i := 0; i+1 < len(extra); i += 2 {
		body[extra[i].(string)] = extra[i+1]
	}
	writeJSON(w, http.StatusOK, body)
}

// limitManagementClient bounds the team routes per client the way the site
// routes are bounded, so a loop in an agent cannot walk the member endpoints.
func (h *TeamHandler) limitManagementClient(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if decision := h.limits.allow(managementClientPolicy, clientLimitKey(r)); !decision.Allowed {
			writeRateLimit(w, decision)
			return
		}
		next.ServeHTTP(w, r)
	})
}
