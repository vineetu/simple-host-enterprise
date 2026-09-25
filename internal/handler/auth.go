package handler

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oidc"
	"github.com/vsriram/simple-host/internal/reqlog"
)

// oauthStateCookie carries the one-time state, nonce and PKCE verifier
// between /auth/login and /auth/callback. It is not the session cookie:
// __Host- prefix rules (Secure, Path=/, no Domain) still apply, but its
// lifetime is minutes, not hours, and it is cleared the moment the callback
// reads it, used or not.
const oauthStateCookie = "__Host-sh_oauth"
const oauthStateMaxAge = 10 * 60 // 10 minutes

// OIDCClaimConfig is the claim-mapping half of design.md 6.1's OIDC_*
// configuration — everything AuthHandler needs beyond the oidc.Provider
// itself. Mirrors config.OIDCConfig; the handler package does not import
// internal/config, the same convention internal/storage's EnvelopeKey
// documents for internal/config.EnvelopeKey.
type OIDCClaimConfig struct {
	// Issuer is OIDC_ISSUER, for the provider-specific rules in
	// refuseIdentity.
	Issuer              string
	EmailClaim          string
	UsernameClaim       string
	AdminClaim          string
	AdminValue          string
	AdminEmails         []string
	AllowedEmailDomains []string
	HintDomain          string
}

func (c OIDCClaimConfig) isAdminEmail(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	for _, admin := range c.AdminEmails {
		if admin == email {
			return true
		}
	}
	return false
}

func (c OIDCClaimConfig) isAllowedDomain(email string) bool {
	if len(c.AllowedEmailDomains) == 0 {
		return true
	}
	_, domain, ok := strings.Cut(strings.ToLower(email), "@")
	if !ok {
		return false
	}
	for _, allowed := range c.AllowedEmailDomains {
		if allowed == domain {
			return true
		}
	}
	return false
}

// emailVerified reports whether the provider vouches for the address. The
// email address both decides admin status (ADMIN_EMAILS) and can bind an
// existing account (resolveUser), so an unverified one is never accepted:
// email_verified must be present and true. Entra ID never sends
// email_verified; its documented equivalent is the optional xms_edov claim
// (the address's domain is verified by the tenant), accepted only from an
// Entra issuer.
func (c OIDCClaimConfig) emailVerified(claims oidc.Claims) bool {
	if claims.EmailVerifiedSet {
		return claims.EmailVerified
	}
	if isEntraIssuer(c.Issuer) {
		switch v := claims.Raw["xms_edov"].(type) {
		case bool:
			return v
		case string:
			return v == "true" || v == "1"
		}
	}
	return false
}

// refuseIdentity applies the sign-in rules on the ID token's identity
// claims. It returns a short reason for the audit trail and the message to
// show, or two empty strings when the sign-in may proceed.
func (c OIDCClaimConfig) refuseIdentity(claims oidc.Claims, email string) (reason, message string) {
	if !c.emailVerified(claims) {
		return "email_not_verified", "your email address is not verified with the identity provider"
	}
	if !c.isAllowedDomain(email) {
		return "domain_not_allowed", "this account's email domain is not allowed to sign in here"
	}
	// Google: a consumer account can carry any verified address, including
	// one at the company's domain it does not control; only a Workspace
	// account carries hd. With ALLOWED_EMAIL_DOMAINS set, hd is required and
	// must be one of them.
	if isGoogleIssuer(c.Issuer) && len(c.AllowedEmailDomains) > 0 && claims.HostedDomain == "" {
		return "google_hd_missing", "this Google account is not part of an allowed Google Workspace domain"
	}
	if claims.HostedDomain != "" && !c.isAllowedDomain("x@"+claims.HostedDomain) {
		return "google_hd_not_allowed", "this account's Google Workspace domain is not allowed to sign in here"
	}
	return "", ""
}

func isGoogleIssuer(issuer string) bool {
	return strings.TrimRight(strings.ToLower(issuer), "/") == "https://accounts.google.com"
}

func isEntraIssuer(issuer string) bool {
	u, err := url.Parse(strings.ToLower(issuer))
	return err == nil && u.Host == "login.microsoftonline.com"
}

// AuthHandler serves sign-in, callback, sign-out and the sessions page on
// the base host (design.md 6.1). There is no equivalent on an owner host in
// this phase: the hand-off that gives an owner host its own session cookie
// is design.md 6.1's own "hand-off to an owner host" section, explicitly
// deferred to Phase 2 (handoff.md's Phase 1 row).
type AuthHandler struct {
	database     *sql.DB
	provider     *oidc.Provider
	claims       OIDCClaimConfig
	signingKeys  []auth.SigningKey
	sessionTTL   time.Duration
	sessionIdle  time.Duration
	cookieSecure bool
	audit        audit.Recorder
	limits       *AbuseLimits
	hosts        HostModel
	publicBase   string
}

func NewAuthHandler(
	database *sql.DB,
	provider *oidc.Provider,
	claims OIDCClaimConfig,
	signingKeys []auth.SigningKey,
	sessionTTL, sessionIdle time.Duration,
	recorder audit.Recorder,
	hosts HostModel,
	publicBaseURL string,
	limits ...*AbuseLimits,
) *AuthHandler {
	if recorder == nil {
		recorder = audit.NoOp{}
	}
	return &AuthHandler{
		database:    database,
		provider:    provider,
		claims:      claims,
		signingKeys: signingKeys,
		sessionTTL:  sessionTTL,
		sessionIdle: sessionIdle,
		audit:       recorder,
		limits:      chooseAbuseLimits(limits),
		hosts:       hosts,
		publicBase:  publicBaseURL,
	}
}

func (h *AuthHandler) Register(mux *http.ServeMux, authMiddleware func(http.Handler) http.Handler) {
	originCheck := originCheckMiddleware(h.hosts, h.publicBase)

	mux.HandleFunc("GET /auth/login", h.login)
	mux.HandleFunc("GET /auth/callback", h.callback)
	mux.Handle("POST /auth/logout", originCheck(authMiddleware(requireSessionAuth(http.HandlerFunc(h.logout)))))
	mux.Handle("GET /auth/sessions", authMiddleware(http.HandlerFunc(h.listSessions)))
	mux.Handle("POST /auth/sessions/{id}/revoke", originCheck(authMiddleware(requireSessionAuth(http.HandlerFunc(h.revokeSession)))))
}

// requireSessionAuth rejects a request authenticated with an API key rather
// than a browser session. Design.md 6.3 scopes /api/keys to "session
// required"; the same reasoning applies to sign-out and session management —
// an agent holding one key must not be able to sign out or revoke sessions
// on the human's behalf. Must run after authMiddleware.
func requireSessionAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.SessionID(r.Context()) == "" {
			writeJSON(w, http.StatusForbidden, errorResponse{Error: "this operation requires a browser session, not an API key"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

type oauthState struct {
	State    string `json:"state"`
	Nonce    string `json:"nonce"`
	Verifier string `json:"verifier"`
	To       string `json:"to,omitempty"`
}

func (h *AuthHandler) login(w http.ResponseWriter, r *http.Request) {
	if decision := h.limits.allow(authClientPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	req, err := oidc.NewAuthRequest()
	if err != nil {
		log.Printf("auth: build auth request: %v", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	state := oauthState{
		State:    req.State,
		Nonce:    req.Nonce,
		Verifier: req.CodeVerifier,
		To:       sanitizeRedirectPath(r.URL.Query().Get("to")),
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateCookie,
		Value:    base64.RawURLEncoding.EncodeToString(encoded),
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   oauthStateMaxAge,
	})
	http.Redirect(w, r, h.provider.AuthCodeURL(req, h.claims.HintDomain), http.StatusFound)
}

func (h *AuthHandler) callback(w http.ResponseWriter, r *http.Request) {
	if decision := h.limits.allow(authClientPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	// Single use regardless of outcome: clear it now so a replayed callback
	// (a bookmarked URL, a resent link) has no state cookie to validate
	// against and fails cleanly.
	stateCookie, cookieErr := r.Cookie(oauthStateCookie)
	http.SetCookie(w, &http.Cookie{
		Name: oauthStateCookie, Value: "", Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	if cookieErr != nil || stateCookie.Value == "" {
		writeAuthError(w, http.StatusBadRequest, "sign-in expired or was started in a different browser session; please try again")
		return
	}
	rawState, err := base64.RawURLEncoding.DecodeString(stateCookie.Value)
	if err != nil {
		writeAuthError(w, http.StatusBadRequest, "sign-in state could not be read; please try again")
		return
	}
	var state oauthState
	if err := json.Unmarshal(rawState, &state); err != nil {
		writeAuthError(w, http.StatusBadRequest, "sign-in state could not be read; please try again")
		return
	}

	if errParam := r.URL.Query().Get("error"); errParam != "" {
		writeAuthError(w, http.StatusForbidden, "sign-in was refused by the identity provider: "+html.EscapeString(errParam))
		return
	}
	returnedState := r.URL.Query().Get("state")
	if returnedState == "" || subtle.ConstantTimeCompare([]byte(returnedState), []byte(state.State)) != 1 {
		writeAuthError(w, http.StatusBadRequest, "sign-in state did not match; please try again")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeAuthError(w, http.StatusBadRequest, "the identity provider did not return an authorization code")
		return
	}

	tok, err := h.provider.Exchange(r.Context(), code, state.Verifier)
	if err != nil {
		log.Printf("auth: token exchange: %v", err)
		writeAuthError(w, http.StatusBadGateway, "could not complete sign-in with the identity provider")
		return
	}
	claims, err := h.provider.VerifyIDToken(r.Context(), tok.IDToken, state.Nonce)
	if err != nil {
		log.Printf("auth: verify id_token: %v", err)
		h.signInFailed(r, "id_token_invalid", "")
		writeAuthError(w, http.StatusForbidden, "sign-in could not be verified")
		return
	}

	email := claims.Email
	if h.claims.EmailClaim != "email" {
		if v, ok := claims.StringClaim(h.claims.EmailClaim); ok {
			email = v
		}
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		h.signInFailed(r, "email_missing", "")
		writeAuthError(w, http.StatusForbidden, "the identity provider did not send an email address")
		return
	}
	if reason, refusal := h.claims.refuseIdentity(claims, email); refusal != "" {
		h.signInFailed(r, reason, email)
		writeAuthError(w, http.StatusForbidden, refusal)
		return
	}

	isAdmin := h.claims.isAdminEmail(email)
	if !isAdmin && h.claims.AdminClaim != "" && claims.HasClaimValue(h.claims.AdminClaim, h.claims.AdminValue) {
		isAdmin = true
	}

	usernameHint := ""
	if h.claims.UsernameClaim != "" {
		usernameHint, _ = claims.StringClaim(h.claims.UsernameClaim)
	}
	user, notice, err := h.resolveUser(r.Context(), claims.Subject, email, usernameHint, isAdmin)
	if err != nil {
		if errors.Is(err, db.ErrAccountDisabled) {
			h.signInFailed(r, "account_disabled", email)
			writeAuthError(w, http.StatusForbidden, "this account has been disabled")
			return
		}
		log.Printf("auth: resolve user for sub %s: %v", claims.Subject, err)
		writeAuthError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	ip := reqlog.ClientIP(r)
	expiresAt := time.Now().Add(h.sessionTTL)
	session, err := db.CreateSession(r.Context(), h.database, user.ID, expiresAt, ip, r.UserAgent())
	if err != nil {
		log.Printf("auth: create session for %s: %v", user.Username, err)
		writeAuthError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	cookieValue, err := auth.SignSession(h.signingKeys, session.ID, user.ID, expiresAt)
	if err != nil {
		log.Printf("auth: sign session cookie: %v", err)
		writeAuthError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    cookieValue,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
	})
	h.audit.Record(r.Context(), audit.Event{ActorID: user.ID, Action: "sign_in", Detail: "session " + session.ID, RequestID: auditRequestID(r.Context())})

	target := state.To
	if target == "" {
		target = "/dashboard"
	}
	if notice != "" {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + "notice=" + url.QueryEscape(notice)
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// signInFailed records a refused sign-in that got as far as the identity
// provider's answer. email is the address the token claimed, when known.
func (h *AuthHandler) signInFailed(r *http.Request, reason, email string) {
	h.audit.Record(r.Context(), audit.Event{Action: "sign_in_failed", Detail: email, Extra: map[string]any{"reason": reason}})
}

// resolveUser finds or creates the account this sign-in belongs to, per
// design.md 6.1: by oidc_sub first, then by email but only inside an allowed
// domain and only once, then by creating a new person. It also refreshes
// is_admin on every sign-in (design.md 6.2) and refuses a disabled account
// (design.md 6.4) regardless of which path found it.
func (h *AuthHandler) resolveUser(ctx context.Context, sub, email, usernameHint string, isAdmin bool) (db.User, string, error) {
	user, err := db.GetUserByOIDCSub(ctx, h.database, sub)
	if err == nil {
		return h.finishResolve(ctx, user, isAdmin)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return db.User{}, "", err
	}

	// Not bound yet. Try binding an existing account by email, but only
	// inside an allowed domain — design.md 6.1: "without the list, an
	// existing row is never claimed by email." An empty AllowedEmailDomains
	// list therefore skips this branch entirely rather than claiming an
	// arbitrary account by address alone.
	if len(h.claims.AllowedEmailDomains) > 0 {
		existing, emailErr := db.GetUserByEmail(ctx, h.database, email)
		if emailErr == nil && !existing.IsTeam() {
			if bindErr := db.BindOIDCSub(ctx, h.database, existing.ID, sub); bindErr == nil {
				return h.finishResolve(ctx, existing, isAdmin)
			} else if !errors.Is(bindErr, sql.ErrNoRows) {
				return db.User{}, "", bindErr
			}
			// ErrNoRows: already bound (by this sign-in racing itself, or
			// bound to a different sub already). Re-read by sub, which the
			// winner of that race just satisfied.
			if bySub, subErr := db.GetUserByOIDCSub(ctx, h.database, sub); subErr == nil {
				return h.finishResolve(ctx, bySub, isAdmin)
			}
		} else if emailErr != nil && !errors.Is(emailErr, sql.ErrNoRows) {
			return db.User{}, "", emailErr
		}
	}

	return h.createUser(ctx, sub, email, usernameHint, isAdmin)
}

func (h *AuthHandler) finishResolve(ctx context.Context, user db.User, isAdmin bool) (db.User, string, error) {
	disabled, err := db.IsUserDisabled(ctx, h.database, user.ID)
	if err != nil {
		return db.User{}, "", err
	}
	if disabled {
		return db.User{}, "", db.ErrAccountDisabled
	}
	if err := db.RefreshAdminStatus(ctx, h.database, user.ID, isAdmin); err != nil {
		return db.User{}, "", err
	}
	user.IsAdmin = isAdmin
	return user, "", nil
}

const maxUsernameSuffixAttempts = 20

func (h *AuthHandler) createUser(ctx context.Context, sub, email, usernameHint string, isAdmin bool) (db.User, string, error) {
	base := strings.ToLower(strings.TrimSpace(usernameHint))
	if base == "" || validateOwnerName(base) != nil {
		// No usable OIDC_USERNAME_CLAIM value (unset, absent from this
		// token, or not a valid label): fall back to the same derivation
		// registration always used.
		base = deriveUsernameFromEmail(email)
	}
	if base == "" {
		return db.User{}, "", errors.New("could not derive a valid username from the sign-in email")
	}

	// A derived name may be unusable in two different ways: reserved (design
	// 6.1's "on collision, append a short suffix and tell the person" — an
	// admin's own address is very often "admin@...", which is exactly
	// reservedLabels["admin"]) or actually malformed (not a valid DNS label
	// at all, which a numeric suffix cannot fix). Only the first is worth
	// retrying; the loop below tries the bare name once and then only
	// suffixed forms, and validates each before ever reaching the database.
	username := base
	notice := ""
	attempted := false
	for attempt := 0; attempt < maxUsernameSuffixAttempts; attempt++ {
		if attempt > 0 {
			username = fmt.Sprintf("%s-%d", base, attempt+1) // -2, -3, ...
			notice = "username_suffixed"
		}
		if err := validateOwnerName(username); err != nil {
			if errors.Is(err, errOwnerNameReserved) {
				continue // try the next suffix
			}
			if !attempted {
				return db.User{}, "", fmt.Errorf("derived username %q is not usable: %w", username, err)
			}
			continue
		}
		attempted = true
		user, err := db.CreateOIDCUser(ctx, h.database, username, sub, email, isAdmin)
		if err == nil {
			return user, notice, nil
		}
		if !isUniqueViolation(err) {
			return db.User{}, "", err
		}
	}
	return db.User{}, "", fmt.Errorf("could not find an available username derived from %q", base)
}

func (h *AuthHandler) logout(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	sessionID := auth.SessionID(r.Context())
	if user != nil && sessionID != "" {
		if err := db.RevokeSession(r.Context(), h.database, user.ID, sessionID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("auth: revoke session on sign-out: %v", err)
		}
		h.audit.Record(r.Context(), audit.Event{ActorID: user.ID, Action: "sign_out", Detail: "session " + sessionID, RequestID: auditRequestID(r.Context())})
	}
	http.SetCookie(w, &http.Cookie{
		Name: auth.SessionCookieName, Value: "", Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (h *AuthHandler) revokeSession(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	id := r.PathValue("id")
	if err := db.RevokeSession(r.Context(), h.database, user.ID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		log.Printf("auth: revoke session %s: %v", id, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), audit.Event{ActorID: user.ID, Action: "session_revoke", Detail: "session " + id, RequestID: auditRequestID(r.Context())})
	http.Redirect(w, r, "/auth/sessions", http.StatusSeeOther)
}

func (h *AuthHandler) listSessions(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		http.Redirect(w, r, "/auth/login?to="+url.QueryEscape("/auth/sessions"), http.StatusFound)
		return
	}
	sessions, err := db.ListSessionsForUser(r.Context(), h.database, user.ID)
	if err != nil {
		log.Printf("auth: list sessions for %s: %v", user.Username, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	currentSessionID := auth.SessionID(r.Context())

	var b strings.Builder
	b.WriteString(dashboardHeadHTML)
	fmt.Fprintf(&b, `<header class="bar">
  <div class="mast">Simple Host<span class="dot">.</span> <span class="kicker">sessions</span></div>
  <nav class="dash-nav"><a href="/dashboard">Keys</a> <span class="sep" aria-hidden="true"></span> <a href="/auth/sessions">Sessions</a></nav>
</header>
<main>
<h2 class="section-title">Signed in as %s</h2>
<div class="rank-list">`, html.EscapeString(user.Username))
	for _, s := range sessions {
		status := "active"
		if s.RevokedAt != nil {
			status = "revoked"
		} else if time.Now().After(s.ExpiresAt) {
			status = "expired"
		}
		current := ""
		if s.ID == currentSessionID {
			current = ` <span class="chip">this session</span>`
		}
		fmt.Fprintf(&b, `<div class="rank-row">
  <span class="rank-name">%s%s <span class="rank-sub">%s · created %s</span></span>
  <span class="rank-metric">%s</span>`,
			html.EscapeString(status), current,
			html.EscapeString(s.UserAgent),
			localTimeHTML(s.CreatedAt, "datetime"),
			"",
		)
		if status == "active" {
			fmt.Fprintf(&b, `<form method="POST" action="/auth/sessions/%s/revoke"><button type="submit" class="btn-reject">Revoke</button></form>`, html.EscapeString(s.ID))
		}
		b.WriteString(`</div>`)
	}
	if len(sessions) == 0 {
		b.WriteString(`<div class="rank-empty">No sessions yet.</div>`)
	}
	b.WriteString(`</div>
<form method="POST" action="/auth/logout" style="margin-top:24px"><button type="submit" class="btn-logout">Sign out of this session</button></form>
</main>`)
	b.WriteString(`</body></html>`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func writeAuthError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Sign-in problem</title></head>
<body><main style="max-width:640px;margin:80px auto;font-family:sans-serif">
<h1>Sign-in didn't complete</h1>
<p>%s</p>
<p><a href="/auth/login">Try again</a></p>
</main></body></html>`, html.EscapeString(message))
}

// sanitizeRedirectPath is the "to" validation design.md 6.1 specifies,
// shared by the post-sign-in redirect here and by the hand-off's own final
// redirect (handoff.go's redeemHandoffSession, called after redeeming the
// one-time code): must start with a single "/", must not start with "//" or
// "/\\", must contain no backslash, no scheme, no host. Anything else
// becomes "".
//
// A raw control character (tab, newline, carriage return) is rejected too,
// but not by any check in this function: url.Parse itself refuses to parse
// a string containing one ("net/url: invalid control character in URL") and
// this function treats that parse error the same as any other rejection.
// That is deliberate reliance, not an oversight — url.Parse's own refusal
// is exactly what stops a value like "/\t//evil.com" reaching the
// early-return checks above with its `//` disguised behind a tab a browser
// or a downstream proxy might still strip. A percent-encoded value such as
// "/%2F%2Fevil.com" passes through unrejected and unchanged: url.Parse
// leaves an escaped "%2F" alone (RawPath keeps it verbatim; only the
// decoded Path unescapes it), so parsed.Host is never populated from it,
// and a browser resolving a relative reference does the same — the network-
// path-reference form ("//host/...") requires two literal, unencoded
// slashes at the very start, which "/%" can never produce.
func sanitizeRedirectPath(to string) string {
	if to == "" {
		return ""
	}
	if !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") || strings.HasPrefix(to, "/\\") {
		return ""
	}
	if strings.ContainsAny(to, "\\") {
		return ""
	}
	parsed, err := url.Parse(to)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.User != nil {
		return ""
	}
	return to
}
