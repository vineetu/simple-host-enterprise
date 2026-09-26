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
	apiKeyScopeContextKey
)

type errorResponse struct {
	Error string `json:"error"`
}

// APIKeyPrefix starts every newly minted key, so secret scanners (and
// people) can recognise one in a log, a commit or a paste.
const APIKeyPrefix = "shk_"

func GenerateAPIKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}

	return APIKeyPrefix + hex.EncodeToString(key), nil
}

// Middleware authenticates a request one of three ways: an
// X-API-Key header, hashed and looked up in api_keys; an OAuth access token
// in "Authorization: Bearer" (handler/connector.go); or the
// SessionCookieName session cookie, verified against signingKeys and then
// checked against the sessions table. The header takes precedence when both
// are present — an agent presenting a key on a browser-shared origin should
// never be silently authenticated as whoever's browser session happens to be
// riding along.
//
// An API key is then held to its scope (scope.go): the route the mux matched
// must be one the key's scope allows, or the request is refused with 403
// naming the scope. An OAuth access token is accepted only on a request that
// came in through /mcp (WithMCPCaller); anywhere else it is refused before
// it is even looked up.
//
// There is no more synthetic admin principal and no ADMIN_API_KEY: every
// principal this middleware produces is a real *db.User row, and admin
// status is whatever users.is_admin says.
func Middleware(database *sql.DB, signingKeys []SigningKey, sessionIdle time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKey := r.Header.Get("X-API-Key")
			if apiKey != "" {
				user, keyID, scope, err := db.GetUserByAPIKeyHash(r.Context(), database, db.HashAPIKey(apiKey))
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
				if !KeyScopeAllows(scope, requestPattern(r)) {
					writeJSON(w, http.StatusForbidden, map[string]string{
						"error": "this API key's scope (" + scope + ") does not allow this request; a person can mint a key with the scope it needs on the dashboard",
						"scope": scope,
					})
					return
				}
				ctx := context.WithValue(r.Context(), userContextKey, &user)
				ctx = context.WithValue(ctx, apiKeyIDContextKey, keyID)
				ctx = context.WithValue(ctx, apiKeyScopeContextKey, scope)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// An OAuth access token from an AI app connected on the person's
			// behalf. Checked before the cookie, and never falls through to
			// it: a bad token is a bad token even with a browser session.
			if token, ok := BearerToken(r); ok {
				// The token was issued for the /mcp resource (its audience),
				// so on any other request it is not a valid token: 401 with
				// the same body a bad token gets, and no lookup, so a stolen
				// token cannot even be tested against the REST routes. The
				// MCP server's own calls into those routes carry the mark.
				if !viaMCP(r.Context()) {
					writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
					return
				}
				user, _, err := db.GetUserByOAuthAccessToken(r.Context(), database, db.HashAPIKey(token))
				if err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
						return
					}
					writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
					return
				}
				reqlog.SetUser(r.Context(), user.ID)
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey, &user)))
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

// BearerToken returns the token of an "Authorization: Bearer" header.
func BearerToken(r *http.Request) (string, bool) {
	authz := r.Header.Get("Authorization")
	if len(authz) < 7 || !strings.EqualFold(authz[:7], "bearer ") {
		return "", false
	}
	token := strings.TrimSpace(authz[7:])
	return token, token != ""
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
// Postgres cast error. That principal no longer exists: the
// ADMIN_API_KEY-backed synthetic admin is deleted, and every principal
// Middleware produces is a real users row with a real UUID, so this is now
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

// APIKeyScope returns the scope of the API key this request authenticated
// with, or "" for a session or an OAuth token.
func APIKeyScope(ctx context.Context) string {
	scope, _ := ctx.Value(apiKeyScopeContextKey).(string)
	return scope
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
