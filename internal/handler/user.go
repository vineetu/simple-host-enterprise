package handler

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/lib/pq"

	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/safepath"
)

var validEmail = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)
var invalidUsernameChars = regexp.MustCompile(`[^a-zA-Z0-9.-]+`)

type UserHandler struct {
	database *sql.DB
	limits   *AbuseLimits
}

type errorResponse struct {
	Error string `json:"error"`
	// Code is a stable machine-readable reason, set where a caller has to tell
	// refusals apart to do the right thing — an agent deciding whether to
	// retry, a dialog deciding what to say. Omitted where the status alone is
	// the whole answer, so adding one later is additive rather than a change.
	Code string `json:"code,omitempty"`
}

func NewUserHandler(database *sql.DB, limits ...*AbuseLimits) *UserHandler {
	return &UserHandler{database: database, limits: chooseAbuseLimits(limits)}
}

// Register wires only /api/me. Registration by email (POST /api/auth) and
// the reset-request intake (POST /api/reset-requests) are gone: identity now
// comes from OIDC sign-in (internal/handler/auth.go), and a lost credential
// is a lost API key, revoked and re-minted from the dashboard rather than
// recovered by emailing the platform. See design.md 6.1, 6.3.
func (h *UserHandler) Register(mux *http.ServeMux, authMiddleware, skillVersionMiddleware func(http.Handler) http.Handler) {
	mux.Handle("GET /api/me", authMiddleware(skillVersionMiddleware(http.HandlerFunc(h.me))))

	// Explicit 404s, not left to fall through. The static UI catch-all
	// ("GET /", ui.go) matches every path net/http has no other pattern
	// for, and net/http's own behavior for a path that pattern DOES match
	// but on the wrong method is 405, not 404 — so without these, an old
	// client's POST here would see "Method Not Allowed" instead of the
	// "this route doesn't exist" signal design.md's Phase 1 exit criteria
	// calls for.
	mux.HandleFunc("POST /api/auth", http.NotFound)
	mux.HandleFunc("POST /api/reset-requests", http.NotFound)
}

func (h *UserHandler) me(w http.ResponseWriter, r *http.Request) {
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
	memberships := make([]meTeam, 0, len(teams))
	for _, team := range teams {
		memberships = append(memberships, meTeam{ID: team.ID, Name: team.Username})
	}

	kind := user.Kind
	if kind == "" {
		kind = "person"
	}
	writeJSON(w, http.StatusOK, meResponse{
		ID:       user.ID,
		Username: user.Username,
		IsAdmin:  user.IsAdmin,
		Kind:     kind,
		Email:    user.Email,
		Teams:    memberships,
	})
}

type meResponse struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	IsAdmin  bool   `json:"is_admin"`
	Kind     string `json:"kind"`
	Email    string `json:"email,omitempty"`
	// Teams carries an id per team, not just a name. An agent binds a project
	// to a namespace by its immutable id, so that a name which changed hands —
	// a team deleted and re-registered by somebody else — resolves to a
	// different id and the agent stops instead of publishing into a stranger's
	// namespace. Without the id here that check cannot be made at all.
	Teams []meTeam `json:"teams"`
}

type meTeam struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// deriveUsernameFromEmail is the default username derivation, shared by the
// OIDC sign-in path (when OIDC_USERNAME_CLAIM is unset) and still referenced
// here as it always has been.
func deriveUsernameFromEmail(email string) string {
	localPart, _, _ := strings.Cut(email, "@")
	username := invalidUsernameChars.ReplaceAllString(localPart, "")
	if safepath.ValidateSegment(username) != nil {
		return ""
	}
	return username
}
