package handler

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/vsriram/simple-host/internal/oplimits"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// KeysHandler serves /api/keys: mint, list, revoke. Every
// route requires a browser session, not an API key — the whole point is that
// an agent holding one key cannot mint itself another or revoke somebody
// else's, so the credential that manages credentials is deliberately harder
// to automate than the credential itself.
type KeysHandler struct {
	database   *sql.DB
	audit      audit.Recorder
	hosts      HostModel
	publicBase string
	limits     *AbuseLimits
	maxDays    int
}

// WithMaxKeyDays sets the longest lifetime a key may be minted with
// (API_KEY_MAX_DAYS); 365 when never called.
func (h *KeysHandler) WithMaxKeyDays(days int) *KeysHandler {
	h.maxDays = days
	return h
}

func NewKeysHandler(database *sql.DB, recorder audit.Recorder, hosts HostModel, publicBaseURL string, limits ...*AbuseLimits) *KeysHandler {
	if recorder == nil {
		recorder = audit.NoOp{}
	}
	return &KeysHandler{database: database, audit: recorder, hosts: hosts, publicBase: publicBaseURL, limits: chooseAbuseLimits(limits), maxDays: 365}
}

func (h *KeysHandler) Register(mux *http.ServeMux, authMiddleware func(http.Handler) http.Handler) {
	originCheck := originCheckMiddleware(h.hosts, h.publicBase)
	protected := func(next http.Handler) http.Handler {
		return authMiddleware(requireSessionAuth(originCheck(next)))
	}
	// GET carries no side effect and needs no origin check — the browser's
	// same-origin policy already keeps a foreign page from reading the
	// response, which is the property Origin-checking a mutation buys.
	mux.Handle("GET /api/keys", authMiddleware(requireSessionAuth(http.HandlerFunc(h.list))))
	mux.Handle("POST /api/keys", protected(http.HandlerFunc(h.mint)))
	mux.Handle("DELETE /api/keys/{id}", protected(http.HandlerFunc(h.revoke)))
	// Connected apps (the chat apps signed in through /mcp): the other
	// credential a person holds, managed the same way.
	mux.Handle("GET /api/me/connections", authMiddleware(requireSessionAuth(http.HandlerFunc(h.listConnections))))
	mux.Handle("DELETE /api/me/connections/{id}", protected(http.HandlerFunc(h.disconnect)))
}

type connectionResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ConnectedAt string `json:"connected_at"`
	LastUsedAt  string `json:"last_used_at"`
	// Device is the browser that allowed the connection ("Chrome on
	// macOS"), "" when not known. Never an IP address.
	Device string `json:"device"`
}

func (h *KeysHandler) listConnections(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	connections, err := db.ListOAuthConnectionsForUser(r.Context(), h.database, user.ID)
	if err != nil {
		log.Printf("keys: list connections for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	out := make([]connectionResponse, 0, len(connections))
	for _, c := range connections {
		out = append(out, connectionResponse{ID: c.ID, Name: c.ClientName, ConnectedAt: c.CreatedAt.Format(time.RFC3339), LastUsedAt: c.LastUsedAt.Format(time.RFC3339), Device: c.DeviceHint})
	}
	writeJSON(w, http.StatusOK, out)
}

// disconnect deletes one of the person's connected apps: its grant and, by
// cascade, every token in it, so the app's next call is refused and it
// must be connected again. Audited as connector_revoke in the same
// transaction.
func (h *KeysHandler) disconnect(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	id := r.PathValue("id")
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		log.Printf("keys: begin disconnect %s for %s: %v", id, user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer audit.Rollback(tx)
	name, err := db.DeleteOAuthGrantForUser(r.Context(), tx, user.ID, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "connected app not found"})
			return
		}
		log.Printf("keys: disconnect %s for %s: %v", id, user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	err = h.audit.RecordTx(r.Context(), tx, audit.Event{ActorID: user.ID, Action: "connector_revoke", Detail: id, Extra: map[string]any{"app": name}, RequestID: auditRequestID(r.Context())})
	if err == nil {
		err = audit.Commit(tx)
	}
	if err != nil {
		log.Printf("keys: record/commit disconnect %s for %s: %v", id, user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disconnected"})
}

type mintKeyRequest struct {
	Name string `json:"name"`
	// ExpiresInDays is the key's lifetime; 0 means the default (90 days, or
	// the maximum if that is lower).
	ExpiresInDays int `json:"expires_in_days"`
	// Scope is what the key may call: "publish" (the default), "full", or
	// "offboard" (admins only). See internal/auth/scope.go.
	Scope string `json:"scope"`
}

type apiKeyResponse struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Prefix string `json:"prefix"`
	// Last4 is the key's own last four characters, "" for a key minted
	// before they were kept (shown as "earlier key").
	Last4      string  `json:"last4,omitempty"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at,omitempty"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
	ExpiresAt  string  `json:"expires_at"`
	Scope      string  `json:"scope"`
	// APIKey carries the plaintext, present only in the mint response. It is
	// never stored and never returned again by any other route.
	APIKey string `json:"api_key,omitempty"`
}

func (h *KeysHandler) mint(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	if decision := h.limits.allow(apiKeyMintPolicy, user.ID); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	var req mintKeyRequest
	if !decodeSmallJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		req.Name = "unnamed key"
	}
	if len(req.Name) > 200 {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "name must be 200 characters or fewer"})
		return
	}
	days := req.ExpiresInDays
	if days == 0 {
		days = min(oplimits.Get().APIKeyDefaultDays, h.maxDays)
	}
	if days < 1 || days > h.maxDays {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("expires_in_days must be between 1 and %d", h.maxDays)})
		return
	}

	switch req.Scope {
	case "":
		req.Scope = db.APIKeyScopePublish
	case db.APIKeyScopePublish, db.APIKeyScopeFull:
	case db.APIKeyScopeOffboard:
		// An offboard key disables people; only an admin could do that with
		// their own session, so only an admin may hand it to automation.
		if !user.IsAdmin {
			writeJSON(w, http.StatusForbidden, errorResponse{Error: "only an admin can create an offboard key"})
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: `scope must be "publish", "full" or "offboard"`})
		return
	}

	plaintext, err := auth.GenerateAPIKey()
	if err != nil {
		log.Printf("keys: generate key for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	hash := db.HashAPIKey(plaintext)
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		log.Printf("keys: begin mint for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer audit.Rollback(tx)
	key, err := db.CreateAPIKey(r.Context(), tx, user.ID, req.Name, hash, db.KeyPrefix(hash), plaintext[len(plaintext)-4:], time.Now().AddDate(0, 0, days), req.Scope)
	if err != nil {
		if isUniqueViolation(err) {
			// A hash collision on 256 random bits is not a real-world event;
			// treat it as a request to retry rather than a hard failure.
			writeJSON(w, http.StatusConflict, errorResponse{Error: "could not mint a unique key; try again"})
			return
		}
		log.Printf("keys: create key for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{ActorID: user.ID, Action: "key_mint", Detail: key.Prefix, Extra: map[string]any{"scope": key.Scope}, RequestID: auditRequestID(r.Context())}); err != nil {
		log.Printf("keys: record key_mint for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	if err := audit.Commit(tx); err != nil {
		log.Printf("keys: commit mint for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusCreated, apiKeyResponse{
		ID:        key.ID,
		Name:      key.Name,
		Prefix:    key.Prefix,
		Last4:     key.Last4,
		CreatedAt: key.CreatedAt.Format(time.RFC3339),
		ExpiresAt: key.ExpiresAt.Format(time.RFC3339),
		Scope:     key.Scope,
		APIKey:    plaintext,
	})
}

func (h *KeysHandler) list(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	keys, err := db.ListAPIKeysForUser(r.Context(), h.database, user.ID)
	if err != nil {
		log.Printf("keys: list for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	out := make([]apiKeyResponse, 0, len(keys))
	for _, k := range keys {
		item := apiKeyResponse{ID: k.ID, Name: k.Name, Prefix: k.Prefix, Last4: k.Last4, CreatedAt: k.CreatedAt.Format(time.RFC3339), ExpiresAt: k.ExpiresAt.Format(time.RFC3339), Scope: k.Scope}
		if k.LastUsedAt != nil {
			s := k.LastUsedAt.Format(time.RFC3339)
			item.LastUsedAt = &s
		}
		if k.RevokedAt != nil {
			s := k.RevokedAt.Format(time.RFC3339)
			item.RevokedAt = &s
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *KeysHandler) revoke(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	id := r.PathValue("id")
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		log.Printf("keys: begin revoke %s for %s: %v", id, user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	defer audit.Rollback(tx)
	if err := db.RevokeAPIKey(r.Context(), tx, user.ID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "key not found"})
			return
		}
		log.Printf("keys: revoke %s for %s: %v", id, user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	err = h.audit.RecordTx(r.Context(), tx, audit.Event{ActorID: user.ID, Action: "key_revoke", Detail: id, RequestID: auditRequestID(r.Context())})
	if err == nil {
		err = audit.Commit(tx)
	}
	if err != nil {
		log.Printf("keys: record/commit revoke %s for %s: %v", id, user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// keyLabel is how a key is told apart in a list: the end of the key itself,
// which is what a person holding it (or finding it in a log) can see.
func keyLabel(last4 string) string {
	if last4 == "" {
		return "earlier key"
	}
	return "ends …" + last4
}

// keyStatus is a key row's state: revoked, expired, "expires soon" inside
// API_KEY_EXPIRY_WARNING_DAYS, or active.
func keyStatus(k db.APIKey, now time.Time) string {
	switch {
	case k.RevokedAt != nil:
		return "revoked"
	case !k.ExpiresAt.After(now):
		return "expired"
	case k.ExpiresAt.Sub(now) <= oplimits.Get().APIKeyExpiryWarning():
		return "expires soon"
	default:
		return "active"
	}
}

// keyLastUsed is a key row's last-used text.
func keyLastUsed(k db.APIKey) string {
	if k.LastUsedAt == nil {
		return "never used"
	}
	return "last used " + localTimeHTML(*k.LastUsedAt, "datetime")
}
