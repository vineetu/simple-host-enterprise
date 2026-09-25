package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/reqlog"
)

type contextKey int

const (
	userContextKey contextKey = iota
	sessionIDContextKey
	apiKeyIDContextKey
	sessionHostContextKey
)

type errorResponse struct {
	Error string `json:"error"`
}

func GenerateAPIKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}

	return hex.EncodeToString(key), nil
}

// Middleware authenticates a request one of two ways (design.md 6.3): an
// X-API-Key header, hashed and looked up in api_keys, or the
// SessionCookieName session cookie, verified against signingKeys and then
// checked against the sessions table. The header takes precedence when both
// are present — an agent presenting a key on a browser-shared origin should
// never be silently authenticated as whoever's browser session happens to be
// riding along.
//
// There is no more synthetic admin principal and no ADMIN_API_KEY: every
// principal this middleware produces is a real *db.User row, and admin
// status is whatever users.is_admin says (design.md 6.2).
func Middleware(database *sql.DB, signingKeys []SigningKey, sessionIdle time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey := r.Header.Get("X-API-Key")
			if apiKey != "" {
				user, keyID, err := db.GetUserByAPIKeyHash(r.Context(), database, db.HashAPIKey(apiKey))
				if err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
						return
					}
					writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
					return
				}
				touchAPIKey(r.Context(), database, keyID)
				reqlog.SetUser(r.Context(), user.ID)
				ctx := context.WithValue(r.Context(), userContextKey, &user)
				ctx = context.WithValue(ctx, apiKeyIDContextKey, keyID)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
				verified, err := VerifySessionCookie(signingKeys, c.Value)
				if err == nil && !strings.EqualFold(verified.Host, ExpectedSessionHost(r.Context())) {
					// A cookie is accepted only on the host it was minted
					// for: a base-host cookie carries no Host, and a hand-off
					// cookie names its owner or restricted-site host.
					err = ErrSessionCookieInvalid
				}
				if err == nil {
					withUser, dbErr := db.GetValidSession(r.Context(), database, verified.SessionID, sessionIdle)
					if dbErr == nil && withUser.Session.UserID == verified.UserID {
						touchSession(r.Context(), database, verified.SessionID)
						reqlog.SetUser(r.Context(), withUser.User.ID)
						ctx := context.WithValue(r.Context(), userContextKey, &withUser.User)
						ctx = context.WithValue(ctx, sessionIDContextKey, verified.SessionID)
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
					if dbErr != nil && !errors.Is(dbErr, db.ErrSessionInvalid) {
						log.Printf("auth: load session: %v", dbErr)
					}
				}
				// Invalid signature or a stale/revoked session: fall through
				// to unauthorized so the browser is sent back to sign in,
				// same as a missing cookie.
			}

			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		})
	}
}

// touchAPIKey and touchSession are detached from the request context — a
// client that disconnects mid-request has still proved the credential
// works — and failures are only logged: refusing an authenticated request
// over a last-used stamp would be the worse outcome.
func touchAPIKey(ctx context.Context, database *sql.DB, keyID string) {
	if err := db.TouchAPIKey(context.WithoutCancel(ctx), database, keyID); err != nil {
		log.Printf("auth: touch api key %s: %v", keyID, err)
	}
}

func touchSession(ctx context.Context, database *sql.DB, sessionID string) {
	if err := db.TouchSession(context.WithoutCancel(ctx), database, sessionID); err != nil {
		log.Printf("auth: touch session %s: %v", sessionID, err)
	}
}

func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := GetUser(r.Context())
		if user == nil || !user.IsAdmin {
			writeJSON(w, http.StatusForbidden, errorResponse{Error: "forbidden"})
			return
		}

		next.ServeHTTP(w, r)
	})
}

// RequireRealUser previously rejected a synthetic header/cookie admin
// principal (a fixed, non-UUID id with no users row) from owner-scoped
// endpoints, because feeding it into a `user_id = $1` UUID filter threw a
// Postgres cast error. That principal no longer exists (design.md 6.2: the
// ADMIN_API_KEY-backed synthetic admin is deleted, and every principal
// Middleware produces is a real users row with a real UUID), so this is now
// a pass-through kept only so call sites in site.go and team.go do not need
// to change: every authenticated user reaching this point is already real.
func RequireRealUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if GetUser(r.Context()) == nil {
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WithExpectedSessionHost tells Middleware which host a session cookie on
// this request must be bound to. The host gate sets it for the site-facing
// API on an owner or restricted-site host; left unset (the base host), only
// a cookie with no Host binding is accepted.
func WithExpectedSessionHost(ctx context.Context, host string) context.Context {
	return context.WithValue(ctx, sessionHostContextKey, strings.ToLower(host))
}

// ExpectedSessionHost is the host WithExpectedSessionHost recorded, or "".
func ExpectedSessionHost(ctx context.Context) string {
	host, _ := ctx.Value(sessionHostContextKey).(string)
	return host
}

// VerifyBaseSessionCookie is VerifySessionCookie for a base-host page: a
// cookie bound to an owner or restricted-site host is refused.
func VerifyBaseSessionCookie(keys []SigningKey, value string) (VerifiedSession, error) {
	verified, err := VerifySessionCookie(keys, value)
	if err != nil {
		return VerifiedSession{}, err
	}
	if verified.Host != "" {
		return VerifiedSession{}, ErrSessionCookieInvalid
	}
	return verified, nil
}

func GetUser(ctx context.Context) *db.User {
	user, _ := ctx.Value(userContextKey).(*db.User)
	return user
}

// SessionID returns the session id this request authenticated with, or ""
// if it authenticated with an API key (or not at all).
func SessionID(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDContextKey).(string)
	return id
}

// APIKeyID returns the api_keys id this request authenticated with, or ""
// if it authenticated with a session cookie (or not at all).
func APIKeyID(ctx context.Context) string {
	id, _ := ctx.Value(apiKeyIDContextKey).(string)
	return id
}

// ContextWithTestAuth attaches exactly what Middleware would have (a user,
// and a session id or an API key id) to ctx, for a test in another package
// that needs GetUser/SessionID/APIKeyID to answer downstream of
// authentication without a live Postgres. Pass "" for whichever of
// sessionID/apiKeyID does not apply — never both at once, the same
// invariant Middleware itself keeps. Not called anywhere outside tests.
func ContextWithTestAuth(ctx context.Context, user *db.User, sessionID, apiKeyID string) context.Context {
	ctx = context.WithValue(ctx, userContextKey, user)
	if sessionID != "" {
		ctx = context.WithValue(ctx, sessionIDContextKey, sessionID)
	}
	if apiKeyID != "" {
		ctx = context.WithValue(ctx, apiKeyIDContextKey, apiKeyID)
	}
	return ctx
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
