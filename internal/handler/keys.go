package handler

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// KeysHandler serves /api/keys (design.md 6.3): mint, list, revoke. Every
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

// defaultAPIKeyDays is a new key's lifetime when the request names none.
const defaultAPIKeyDays = 90

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
}

type mintKeyRequest struct {
	Name string `json:"name"`
	// ExpiresInDays is the key's lifetime; 0 means the default (90 days, or
	// the maximum if that is lower).
	ExpiresInDays int `json:"expires_in_days"`
}

type apiKeyResponse struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at,omitempty"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
	ExpiresAt  string  `json:"expires_at"`
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
	if decision := h.limits.allow(managementUserPolicy, user.ID); !decision.Allowed {
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
		days = min(defaultAPIKeyDays, h.maxDays)
	}
	if days < 1 || days > h.maxDays {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("expires_in_days must be between 1 and %d", h.maxDays)})
		return
	}

	plaintext, err := auth.GenerateAPIKey()
	if err != nil {
		log.Printf("keys: generate key for %s: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	hash := db.HashAPIKey(plaintext)
	key, err := db.CreateAPIKey(r.Context(), h.database, user.ID, req.Name, hash, db.KeyPrefix(hash), time.Now().AddDate(0, 0, days))
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
	h.audit.Record(r.Context(), audit.Event{ActorID: user.ID, Action: "key_mint", Detail: key.Prefix, RequestID: auditRequestID(r.Context())})
	writeJSON(w, http.StatusCreated, apiKeyResponse{
		ID:        key.ID,
		Name:      key.Name,
		Prefix:    key.Prefix,
		CreatedAt: key.CreatedAt.Format(time.RFC3339),
		ExpiresAt: key.ExpiresAt.Format(time.RFC3339),
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
		item := apiKeyResponse{ID: k.ID, Name: k.Name, Prefix: k.Prefix, CreatedAt: k.CreatedAt.Format(time.RFC3339), ExpiresAt: k.ExpiresAt.Format(time.RFC3339)}
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
	if err := db.RevokeAPIKey(r.Context(), h.database, user.ID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, errorResponse{Error: "key not found"})
			return
		}
		log.Printf("keys: revoke %s for %s: %v", id, user.Username, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return
	}
	h.audit.Record(r.Context(), audit.Event{ActorID: user.ID, Action: "key_revoke", Detail: id, RequestID: auditRequestID(r.Context())})
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}
