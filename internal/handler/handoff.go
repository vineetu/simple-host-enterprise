package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// handoffNonceCookie is set on the owner or restricted-site host itself, at
// the moment it first redirects an unauthenticated navigation to the base
// host (design.md 6.1). Its value is never sent anywhere except back to the
// same host's own /auth/session, which is what stops a login CSRF: an
// attacker who starts a hand-off under their own session cannot make a
// colleague's browser redeem the resulting code, because the colleague's
// browser never held the matching nonce.
const handoffNonceCookie = "__Host-sh_handoff"
const handoffNonceMaxAge = 120 // seconds

// HandoffHandler serves the base-host leg of the session hand-off
// (design.md 6.1): GET /auth/handoff mints a one-time code for a validated
// target host, using the caller's existing base session. The target host's
// own leg, redeemHandoffSession, is called directly by the host gate (it is
// not a normal mux route: every owner and restricted-site host must answer
// it, and the gate already decides which hostname a request is for).
type HandoffHandler struct {
	database    *sql.DB
	signingKeys []auth.SigningKey
	hosts       HostModel
	audit       audit.Recorder
	limits      *AbuseLimits
}

func NewHandoffHandler(database *sql.DB, signingKeys []auth.SigningKey, hosts HostModel, recorder audit.Recorder, limits ...*AbuseLimits) *HandoffHandler {
	if recorder == nil {
		recorder = audit.NoOp{}
	}
	return &HandoffHandler{database: database, signingKeys: signingKeys, hosts: hosts, audit: recorder, limits: chooseAbuseLimits(limits)}
}

func (h *HandoffHandler) Register(mux *http.ServeMux, authMiddleware func(http.Handler) http.Handler) {
	mux.Handle("GET /auth/handoff", authMiddleware(requireSessionAuth(http.HandlerFunc(h.handoff))))
}

// handoff validates `to`, mints a one-time code, and redirects the browser
// to the target host's own /auth/session. It requires the caller's base
// session (not an API key: a hand-off exists only for browser navigation).
func (h *HandoffHandler) handoff(w http.ResponseWriter, r *http.Request) {
	if decision := h.limits.allow(authClientPolicy, remoteClientKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	user := auth.GetUser(r.Context())
	sessionID := auth.SessionID(r.Context())
	if user == nil || sessionID == "" {
		writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}

	to := r.URL.Query().Get("to")
	nonceHashParam := r.URL.Query().Get("n")
	targetHost, targetPath, ok := h.validateHandoffTarget(to)
	if !ok {
		writeAuthError(w, http.StatusBadRequest, "this sign-in hand-off link is not valid")
		return
	}
	nonceHash, err := base64.RawURLEncoding.DecodeString(nonceHashParam)
	if err != nil || len(nonceHash) != sha256.Size {
		writeAuthError(w, http.StatusBadRequest, "this sign-in hand-off link is not valid")
		return
	}

	code, err := randomURLSafeToken(32)
	if err != nil {
		log.Printf("handoff: generate code: %v", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	if err := db.CreateHandoffCode(r.Context(), h.database, code, sessionID, targetHost, nonceHash); err != nil {
		log.Printf("handoff: create code for %s -> %s: %v", user.Username, targetHost, err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	target := url.URL{
		Scheme:   h.hosts.scheme,
		Host:     targetHost,
		Path:     "/auth/session",
		RawQuery: url.Values{"code": {code}, "to": {targetPath}}.Encode(),
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// validateHandoffTarget applies design.md 6.1's rule for `to`: scheme https,
// host classifies as an owner or restricted-site host of this base, no
// userinfo, no fragment. It returns the target host (as Classify normalized
// it, with the base suffix restored) and the path+query to redirect to
// afterward — not yet re-validated as path-only, since /auth/session
// applies that rule itself to whatever this hands it.
func (h *HandoffHandler) validateHandoffTarget(to string) (targetHost, targetPath string, ok bool) {
	if to == "" {
		return "", "", false
	}
	parsed, err := url.Parse(to)
	if err != nil {
		return "", "", false
	}
	if !strings.EqualFold(parsed.Scheme, "https") || parsed.User != nil || parsed.Fragment != "" {
		return "", "", false
	}
	kind, _ := h.hosts.Classify(parsed.Host)
	if kind != hostOwner && kind != hostRestrictedSite {
		return "", "", false
	}
	path := parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	if parsed.RawQuery != "" {
		path += "?" + parsed.RawQuery
	}
	return normalizeHost(parsed.Host), path, true
}

// redeemHandoffSession is the target host's own leg (design.md 6.1): it
// requires the __Host-sh_handoff cookie this host itself set, redeems the
// code only if the request's own host matches what the code was minted for
// and sha256 of the cookie matches the nonce hash the code carries, mints
// this host's own session cookie from the same session row, and redirects
// to the validated path-only `to`. Called directly by the host gate for
// every owner and restricted-site host; not a registered mux route, because
// which hostname is answering is exactly what the gate already knows and
// this handler must trust rather than re-derive from a spoofable Host header
// read some other way.
func (h *HandoffHandler) redeemHandoffSession(w http.ResponseWriter, r *http.Request, requestHost string) {
	if decision := h.limits.allow(authClientPolicy, remoteClientKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		writeAuthError(w, http.StatusBadRequest, "this sign-in link is not valid")
		return
	}
	// Any attempt spends the code, including one with no nonce cookie: a
	// code that reached a browser which cannot redeem it (a colleague sent
	// here by a page that started the hand-off with its own nonce) must not
	// survive to be read off that browser's address bar and redeemed by
	// whoever holds the matching nonce.
	var nonceHash []byte
	nonceCookie, cookieErr := r.Cookie(handoffNonceCookie)
	if cookieErr == nil && nonceCookie.Value != "" {
		sum := sha256.Sum256([]byte(nonceCookie.Value))
		nonceHash = sum[:]
	}
	// Single use of the nonce cookie regardless of outcome, the same
	// discipline the OAuth state cookie in auth.go follows.
	http.SetCookie(w, &http.Cookie{
		Name: handoffNonceCookie, Value: "", Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	sessionID, err := db.RedeemHandoffCode(r.Context(), h.database, code, requestHost, nonceHash)
	if nonceHash == nil {
		writeAuthError(w, http.StatusBadRequest, "this sign-in link expired; please open the page again")
		return
	}
	if err != nil {
		if !errors.Is(err, db.ErrHandoffCodeInvalid) {
			log.Printf("handoff: redeem code: %v", err)
		}
		writeAuthError(w, http.StatusForbidden, "this sign-in link is not valid or has expired; please open the page again")
		return
	}

	withUser, err := db.GetValidSession(r.Context(), h.database, sessionID, 0)
	if err != nil {
		writeAuthError(w, http.StatusForbidden, "your sign-in session has ended; please sign in again")
		return
	}
	cookieValue, err := auth.SignHostSession(h.signingKeys, withUser.Session.ID, withUser.User.ID, requestHost, withUser.Session.ExpiresAt)
	if err != nil {
		log.Printf("handoff: sign session cookie: %v", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    cookieValue,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		Expires:  withUser.Session.ExpiresAt,
	})
	h.audit.Record(r.Context(), audit.Event{ActorID: withUser.User.ID, Action: "hand_off", Detail: "host " + requestHost})

	target := sanitizeRedirectPath(r.URL.Query().Get("to"))
	if target == "" {
		target = "/"
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusFound)
}

// beginHandoff is the owner/restricted-site host's own first hop
// (design.md 6.1): it sets the nonce cookie on this host and redirects the
// browser to <base>/auth/handoff, naming this exact request's URL (which
// /auth/session will send the browser back to, path-only, once the round
// trip completes) as `to`. Called by the host gate when a navigation
// arrives on an owner or restricted-site host with no valid host session.
func (h *HandoffHandler) beginHandoff(w http.ResponseWriter, r *http.Request, requestHost string) {
	nonce, err := randomURLSafeToken(32)
	if err != nil {
		log.Printf("handoff: generate nonce: %v", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     handoffNonceCookie,
		Value:    nonce,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   handoffNonceMaxAge,
	})
	nonceHash := sha256.Sum256([]byte(nonce))
	to := url.URL{Scheme: h.hosts.scheme, Host: requestHost, Path: r.URL.EscapedPath(), RawQuery: r.URL.RawQuery}
	target := url.URL{
		Scheme: h.hosts.scheme,
		Host:   h.hosts.BaseHost(),
		Path:   "/auth/handoff",
		RawQuery: url.Values{
			"to": {to.String()},
			"n":  {base64.RawURLEncoding.EncodeToString(nonceHash[:])},
		}.Encode(),
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func randomURLSafeToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// wantsNavigation reports whether a request is a browser navigation rather
// than a script's fetch or an agent's plain request: design.md 7.2's rule
// for whether a missing host session gets the hand-off redirect (a
// navigation) or a 401 (anything else, so a page's own fetch calls fail
// fast and predictably instead of following a redirect chain a script was
// never meant to follow).
func wantsNavigation(r *http.Request) bool {
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" {
		return dest == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
