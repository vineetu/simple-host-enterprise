package handler

// OAuth for /mcp, so the AI apps people already use at work (ChatGPT,
// Claude, Copilot, Cursor, Codex) can connect on their behalf.
//
// A person adds <base>/mcp in their app. The app gets 401 with a pointer to
// the protected-resource metadata (RFC 9728), reads the authorization server
// metadata (RFC 8414), registers itself (RFC 7591) and opens
// /oauth/authorize in a browser. That page signs the person in through the
// company's OIDC provider (/auth/login) and then asks once whether to allow
// the app. "Allow" sends the app back with a single-use code, which it trades
// with its PKCE verifier for a short-lived access token and a rotating refresh
// token. The access token is accepted by auth.Middleware as
// "Authorization: Bearer" on /mcp (and the REST calls its tools make) and
// nowhere else, carrying exactly the person's own power; API keys keep
// working for CI.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/ratelimit"
)

const (
	oauthCodeTTL = time.Minute
	// defaultOAuthAccessTTL and defaultOAuthRefreshTTL apply until
	// WithTokenTTLs sets OAUTH_ACCESS_TTL and OAUTH_REFRESH_TTL.
	defaultOAuthAccessTTL  = time.Hour
	defaultOAuthRefreshTTL = 30 * 24 * time.Hour
	oauthScope             = "sites"
	oauthMaxRedirectURIs   = 10
	// maxAuthorizeReturnBytes keeps the /auth/login state cookie, which
	// carries the return path, under browsers' 4 KB cookie limit.
	maxAuthorizeReturnBytes = 2000
)

var (
	// A company's people connecting from a hosted app (ChatGPT, Claude) all
	// arrive from that app's few egress addresses, so these are per address
	// but generous; every value they guard is a 256-bit secret anyway.
	oauthRegisterPolicy = ratelimit.Policy{Name: "oauth-register", Burst: 30, RefillPerSecond: 0.1}
	oauthTokenPolicy    = ratelimit.Policy{Name: "oauth-token", Burst: 120, RefillPerSecond: 2}
)

// ConnectorHandler is the OAuth authorization server for /mcp.
type ConnectorHandler struct {
	database      *sql.DB
	issuer        string // the public base URL, no trailing slash
	mcpResource   string // issuer + "/mcp"
	redirectHosts []string
	session       *DashboardHandler // reads the browser session on /oauth/authorize
	audit         audit.Recorder
	limits        *AbuseLimits
	hosts         HostModel
	now           func() time.Time
	accessTTL     time.Duration
	refreshTTL    time.Duration
}

func NewConnectorHandler(database *sql.DB, publicBaseURL string, redirectHosts []string, signingKeys []auth.SigningKey, sessionIdle time.Duration, recorder audit.Recorder, hosts HostModel, limits ...*AbuseLimits) *ConnectorHandler {
	if recorder == nil {
		recorder = audit.NoOp{}
	}
	issuer := strings.TrimRight(publicBaseURL, "/")
	return &ConnectorHandler{
		database:      database,
		issuer:        issuer,
		mcpResource:   issuer + "/mcp",
		redirectHosts: redirectHosts,
		session:       NewDashboardHandler(database, signingKeys, sessionIdle),
		audit:         recorder,
		limits:        chooseAbuseLimits(limits),
		hosts:         hosts,
		now:           time.Now,
		accessTTL:     defaultOAuthAccessTTL,
		refreshTTL:    defaultOAuthRefreshTTL,
	}
}

// WithTokenTTLs sets the access token lifetime (OAUTH_ACCESS_TTL) and the
// connection lifetime (OAUTH_REFRESH_TTL); zero keeps the default.
func (h *ConnectorHandler) WithTokenTTLs(access, refresh time.Duration) *ConnectorHandler {
	if access > 0 {
		h.accessTTL = access
	}
	if refresh > 0 {
		h.refreshTTL = refresh
	}
	return h
}

func (h *ConnectorHandler) Register(mux *http.ServeMux, authMiddleware func(http.Handler) http.Handler) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", h.protectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", h.protectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", h.authorizationServerMetadata)

	mux.HandleFunc("POST /oauth/register", h.register)
	mux.HandleFunc("GET /oauth/authorize", h.authorize)
	originCheck := originCheckMiddleware(h.hosts, h.issuer)
	mux.Handle("POST /oauth/authorize", authMiddleware(requireSessionAuth(originCheck(http.HandlerFunc(h.decide)))))
	mux.HandleFunc("POST /oauth/token", h.token)
	mux.HandleFunc("POST /oauth/revoke", h.revoke)
}

// StartSweep deletes expired codes and tokens and abandoned registrations
// every hour until ctx ends.
func (h *ConnectorHandler) StartSweep(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				if err := db.SweepOAuth(sweepCtx, h.database); err != nil {
					log.Printf("oauth sweep: %v", err)
				}
				cancel()
			}
		}
	}()
}

// ---- /mcp gate -------------------------------------------------------------

// ProtectMCP requires an API key or an access token on /mcp, and answers a
// missing or refused one with the 401 challenge that starts an OAuth
// client's sign-in. A browser session is not a credential here. It is the
// only place an OAuth access token is accepted (auth.WithMCPCaller).
func (h *ConnectorHandler) ProtectMCP(authMiddleware func(http.Handler) http.Handler, next http.Handler) http.Handler {
	authed := authMiddleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, bearer := auth.BearerToken(r); !bearer && r.Header.Get("X-API-Key") == "" {
			w.Header().Set("WWW-Authenticate", h.challenge(""))
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "sign in to use this server"})
			return
		}
		// The mark is what lets an OAuth access token through auth.Middleware,
		// here and in every REST call the MCP server makes on this request's
		// context; a token sent straight to a REST route has no mark.
		authed.ServeHTTP(&challengeOn401{ResponseWriter: w, challenge: h.challenge("invalid_token")}, r.WithContext(auth.WithMCPCaller(r.Context())))
	})
}

func (h *ConnectorHandler) challenge(errCode string) string {
	c := `Bearer resource_metadata="` + h.issuer + `/.well-known/oauth-protected-resource", scope="` + oauthScope + `"`
	if errCode != "" {
		c = `Bearer error="` + errCode + `", ` + strings.TrimPrefix(c, "Bearer ")
	}
	return c
}

type challengeOn401 struct {
	http.ResponseWriter
	challenge string
}

func (c *challengeOn401) WriteHeader(status int) {
	if status == http.StatusUnauthorized {
		c.Header().Set("WWW-Authenticate", c.challenge)
	}
	c.ResponseWriter.WriteHeader(status)
}

// ---- metadata ---------------------------------------------------------------

// protectedResourceMetadata answers at both the root and the /mcp-suffixed
// address: /mcp is the resource either way, and RFC 9728 3.3 requires the
// document reached from the 401 challenge to name the URL that was called.
func (h *ConnectorHandler) protectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 h.mcpResource,
		"authorization_servers":    []string{h.issuer},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{oauthScope},
		"resource_name":            "Simple Host",
	})
}

func (h *ConnectorHandler) authorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=3600")
	authMethods := []string{"none", "client_secret_post", "client_secret_basic"}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         h.issuer,
		"authorization_endpoint":                         h.issuer + "/oauth/authorize",
		"token_endpoint":                                 h.issuer + "/oauth/token",
		"registration_endpoint":                          h.issuer + "/oauth/register",
		"revocation_endpoint":                            h.issuer + "/oauth/revoke",
		"scopes_supported":                               []string{oauthScope},
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          authMethods,
		"revocation_endpoint_auth_methods_supported":     authMethods,
		"authorization_response_iss_parameter_supported": true,
	})
}

// ---- helpers ------------------------------------------------------------------

// Prefixes make a leaked value recognisable in a log or to a secret scanner.
const (
	prefixAccess  = "shat_"
	prefixRefresh = "shrt_"
	prefixCode    = "shac_"
	prefixClient  = "shc_"
	prefixSecret  = "shcs_"
)

func randomToken(prefix string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("oauth: no randomness: " + err.Error())
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Cache-Control", "no-store")
	body := map[string]string{"error": code}
	if description != "" {
		body["error_description"] = description
	}
	writeJSON(w, status, body)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// redirectAllowed is the registration rule: a well-formed absolute URI with
// no fragment or credentials, to a destination OAUTH_REDIRECT_HOSTS allows.
func (h *ConnectorHandler) redirectAllowed(raw string) bool {
	if raw == "" || len(raw) > 2000 || strings.ContainsAny(raw, " \t\r\n#") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" || u.User != nil {
		return false
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Hostname())
	for _, entry := range h.redirectHosts {
		switch {
		case scheme == "https" && (entry == "*" || entry == host):
			return true
		case scheme == "http" && entry == "localhost" && isLoopbackHost(host):
			return true
		case scheme != "http" && scheme != "https" && entry == scheme+"://"+strings.ToLower(u.Host):
			return true
		}
	}
	return false
}

// redirectMatches compares a presented redirect URI with a registered one:
// exactly, except that a loopback redirect may use any port (RFC 8252 7.3),
// because an app on the person's machine picks a free port each time.
func redirectMatches(registered, presented string) bool {
	if registered == presented {
		return true
	}
	reg, err1 := url.Parse(registered)
	pre, err2 := url.Parse(presented)
	if err1 != nil || err2 != nil || reg.Scheme != "http" || pre.Scheme != "http" {
		return false
	}
	return isLoopbackHost(reg.Hostname()) && reg.Hostname() == pre.Hostname() &&
		reg.Path == pre.Path && reg.RawQuery == pre.RawQuery && pre.User == nil && pre.Fragment == ""
}

// cleanClientName makes a self-declared app name safe and short to show.
// It is only ever inserted as escaped text.
func cleanClientName(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if r := []rune(out); len(r) > 60 {
		out = string(r[:60])
	}
	if out == "" {
		out = "An app"
	}
	return out
}

func validPKCEValue(s string) bool {
	if len(s) < 43 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~') {
			return false
		}
	}
	return true
}

func pkceS256(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// normalizeResource maps a requested RFC 8707 resource to the one stored on
// the grant. Only this server's /mcp, or the server itself, are resources.
func (h *ConnectorHandler) normalizeResource(raw string) (string, bool) {
	if raw == "" {
		return h.mcpResource, true
	}
	trimmed := strings.TrimRight(raw, "/")
	switch {
	case strings.EqualFold(trimmed, h.mcpResource):
		return h.mcpResource, true
	case strings.EqualFold(trimmed, h.issuer):
		return h.issuer, true
	}
	return "", false
}

// ---- dynamic client registration (RFC 7591) ------------------------------------

func (h *ConnectorHandler) register(w http.ResponseWriter, r *http.Request) {
	if decision := h.limits.allow(oauthRegisterPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	var req struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, smallJSONBodyBytes)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "body must be a JSON client metadata document")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > oauthMaxRedirectURIs {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", fmt.Sprintf("between 1 and %d redirect_uris are required", oauthMaxRedirectURIs))
		return
	}
	for _, u := range req.RedirectURIs {
		if !h.redirectAllowed(u) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URI "+u+" is not allowed on this server (OAUTH_REDIRECT_HOSTS)")
			return
		}
	}
	method := req.TokenEndpointAuthMethod
	if method == "" {
		method = "client_secret_basic" // RFC 7591 2
	}
	if method != "none" && method != "client_secret_post" && method != "client_secret_basic" {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "token_endpoint_auth_method must be none, client_secret_post or client_secret_basic")
		return
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "only the authorization_code and refresh_token grants are supported")
			return
		}
	}
	for _, t := range req.ResponseTypes {
		if t != "code" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "only the code response type is supported")
			return
		}
	}

	client := db.OAuthClient{
		ClientID:                randomToken(prefixClient),
		Name:                    cleanClientName(req.ClientName),
		RedirectURIs:            req.RedirectURIs,
		TokenEndpointAuthMethod: method,
	}
	var secret string
	if method != "none" {
		secret = randomToken(prefixSecret)
		client.SecretHash = db.HashAPIKey(secret)
	}
	if err := db.InsertOAuthClient(r.Context(), h.database, client); err != nil {
		log.Printf("oauth: register client: %v", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	resp := map[string]any{
		"client_id":                  client.ClientID,
		"client_id_issued_at":        h.now().Unix(),
		"client_name":                client.Name,
		"redirect_uris":              client.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": method,
		"scope":                      oauthScope,
	}
	if secret != "" {
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, resp)
}

// ---- authorization endpoint ------------------------------------------------------

type authzRequest struct {
	Client        db.OAuthClient
	RedirectURI   string
	State         string
	CodeChallenge string
	Resource      string
}

// authzError is a refused authorization request. With redirect set it goes
// back to the app at its (verified) redirect URI; without, the person sees
// it, because the redirect URI itself could not be trusted.
type authzError struct {
	redirect    bool
	code        string
	description string
}

func (h *ConnectorHandler) parseAuthorize(ctx context.Context, q url.Values) (authzRequest, *authzError) {
	var req authzRequest
	for k, v := range q {
		if len(v) > 1 && (k == "client_id" || k == "redirect_uri") {
			return req, &authzError{code: "invalid_request", description: "This sign-in link is not valid (a parameter is repeated)."}
		}
	}
	clientID := q.Get("client_id")
	if clientID == "" {
		return req, &authzError{code: "invalid_request", description: "This sign-in link does not say which app is asking. Go back to the app and connect again."}
	}
	client, err := db.GetOAuthClient(ctx, h.database, clientID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("oauth: get client: %v", err)
		}
		return req, &authzError{code: "invalid_client", description: "The app that sent you here is not registered. Go back to the app and connect again."}
	}
	redirectURI := q.Get("redirect_uri")
	matched := false
	for _, registered := range client.RedirectURIs {
		if redirectURI != "" && redirectMatches(registered, redirectURI) {
			matched = true
			break
		}
	}
	// Checked again here, not only at registration, so narrowing
	// OAUTH_REDIRECT_HOSTS takes effect for apps already registered.
	if !matched || !h.redirectAllowed(redirectURI) {
		return req, &authzError{code: "invalid_request", description: "The app asked to send you back to an address this server does not allow, so sign-in stopped here."}
	}
	req.Client, req.RedirectURI = client, redirectURI

	// From here on the redirect URI is trusted and errors go back to the app,
	// carrying state.
	req.State = q.Get("state")
	if len(req.State) > 2000 {
		req.State = ""
		return req, &authzError{redirect: true, code: "invalid_request", description: "state is too long"}
	}
	for k, v := range q {
		if len(v) > 1 {
			return req, &authzError{redirect: true, code: "invalid_request", description: "parameter " + k + " is repeated"}
		}
	}
	if q.Get("response_type") != "code" {
		return req, &authzError{redirect: true, code: "unsupported_response_type", description: "response_type must be code"}
	}
	challenge := q.Get("code_challenge")
	if challenge == "" || q.Get("code_challenge_method") != "S256" || !validPKCEValue(challenge) {
		return req, &authzError{redirect: true, code: "invalid_request", description: "PKCE is required: send code_challenge with code_challenge_method=S256"}
	}
	req.CodeChallenge = challenge
	resource, ok := h.normalizeResource(q.Get("resource"))
	if !ok {
		return req, &authzError{redirect: true, code: "invalid_target", description: "resource must be " + h.mcpResource}
	}
	req.Resource = resource
	// There is one scope, and it is what is granted. Other requested scopes
	// are ignored rather than refused, so a client asking for something
	// generic still connects; it can do no more than a "sites" token.
	return req, nil
}

// redirectWith builds the redirect back to the app, keeping any query the
// registered URI already had, and naming this issuer (RFC 9207).
func (h *ConnectorHandler) redirectWith(redirectURI string, params map[string]string) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return redirectURI
	}
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	q.Set("iss", h.issuer)
	u.RawQuery = q.Encode()
	return u.String()
}

func (h *ConnectorHandler) errorRedirect(req authzRequest, e *authzError) string {
	return h.redirectWith(req.RedirectURI, map[string]string{"error": e.code, "error_description": e.description, "state": req.State})
}

func (h *ConnectorHandler) authorize(w http.ResponseWriter, r *http.Request) {
	if decision := h.limits.allow(authClientPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	req, aerr := h.parseAuthorize(r.Context(), r.URL.Query())
	if aerr != nil {
		if aerr.redirect {
			http.Redirect(w, r, h.errorRedirect(req, aerr), http.StatusFound)
			return
		}
		writeConnectError(w, aerr.description)
		return
	}
	user := h.session.optionalUser(r)
	if user == nil {
		// Sign in through the company's identity provider, then come back
		// here with the same request.
		back := "/oauth/authorize?" + r.URL.RawQuery
		// The return path travels in the sign-in state cookie, which a
		// browser drops past about 4 KB; say so rather than fail later.
		if len(back) > maxAuthorizeReturnBytes {
			writeConnectError(w, "The app's sign-in link is too long to carry through your company sign-in. Try connecting again, or ask your admin.")
			return
		}
		http.Redirect(w, r, "/auth/login?to="+url.QueryEscape(back), http.StatusFound)
		return
	}

	redirectHost := req.RedirectURI
	if u, err := url.Parse(req.RedirectURI); err == nil {
		redirectHost = u.Host
		if u.Scheme == "http" && isLoopbackHost(u.Hostname()) {
			redirectHost = "an app on this computer (" + u.Hostname() + ")"
		}
	}
	var b strings.Builder
	b.WriteString(dashboardHeadHTML)
	fmt.Fprintf(&b, `<header class="bar"><div class="mast">Simple Host<span class="dot">.</span> <span class="kicker">connect an app</span></div></header>
<main>
<section class="login-block">
  <h2 class="section-title">Allow %s to use Simple Host as %s?</h2>
  <p class="login-copy">It will be able to do anything you can do here: publish, change and delete your sites and your teams' sites, and read and write their saved data. After you allow it you will be sent back to %s.</p>
  <p class="login-copy">Only allow an app you started connecting yourself.</p>
  <form method="POST" action="/oauth/authorize">
    <input type="hidden" name="query" value="%s">
    <button type="submit" name="decision" value="allow" class="btn-login">Allow</button>
    <button type="submit" name="decision" value="deny" class="btn-reject">Cancel</button>
  </form>
</section>
</main></body></html>`,
		html.EscapeString(req.Client.Name), html.EscapeString(user.Username), html.EscapeString(redirectHost),
		html.EscapeString(r.URL.RawQuery))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// decide takes Allow or Cancel from the consent page. It runs behind the
// session, same-origin check and session-only rule the key routes use, and
// is the only place an authorization code is minted.
func (h *ConnectorHandler) decide(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	user := auth.GetUser(r.Context())
	if user == nil {
		writeConnectError(w, "Sign in again, then connect the app again.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, smallFormBodyBytes)
	if err := r.ParseForm(); err != nil {
		writeConnectError(w, "This request could not be read. Go back to the app and connect again.")
		return
	}
	q, err := url.ParseQuery(r.PostForm.Get("query"))
	if err != nil {
		writeConnectError(w, "This request could not be read. Go back to the app and connect again.")
		return
	}
	q.Del("notice") // added by the sign-in redirect, not part of the request
	req, aerr := h.parseAuthorize(r.Context(), q)
	if aerr != nil {
		if aerr.redirect {
			http.Redirect(w, r, h.errorRedirect(req, aerr), http.StatusSeeOther)
			return
		}
		writeConnectError(w, aerr.description)
		return
	}
	switch r.PostForm.Get("decision") {
	case "deny":
		http.Redirect(w, r, h.errorRedirect(req, &authzError{code: "access_denied", description: "The person cancelled."}), http.StatusSeeOther)
		return
	case "allow":
	default:
		writeConnectError(w, "Choose Allow or Cancel.")
		return
	}

	code := randomToken(prefixCode)
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		log.Printf("oauth: begin code: %v", err)
		writeConnectError(w, "Something went wrong. Try connecting again.")
		return
	}
	defer tx.Rollback()
	if err := db.InsertOAuthCode(r.Context(), tx, db.HashAPIKey(code), db.OAuthCode{
		ClientID:      req.Client.ClientID,
		UserID:        user.ID,
		RedirectURI:   req.RedirectURI,
		CodeChallenge: req.CodeChallenge,
		Resource:      req.Resource,
		ExpiresAt:     h.now().Add(oauthCodeTTL),
	}); err != nil {
		log.Printf("oauth: insert code: %v", err)
		writeConnectError(w, "Something went wrong. Try connecting again.")
		return
	}
	err = h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: user.ID, Action: "connector_sign_in", RequestID: auditRequestID(r.Context()),
		Detail: "app " + req.Client.Name,
		Extra:  map[string]any{"client_id": req.Client.ClientID, "redirect_uri": req.RedirectURI},
	})
	if err == nil {
		err = audit.Commit(tx)
	}
	if err != nil {
		log.Printf("oauth: record/commit code: %v", err)
		writeConnectError(w, "Something went wrong. Try connecting again.")
		return
	}
	http.Redirect(w, r, h.redirectWith(req.RedirectURI, map[string]string{"code": code, "state": req.State}), http.StatusSeeOther)
}

func writeConnectError(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Connecting an app</title></head>
<body><main style="max-width:640px;margin:80px auto;font-family:sans-serif">
<h1>The app could not be connected</h1>
<p>%s</p>
</main></body></html>`, html.EscapeString(message))
}

// ---- token endpoint ----------------------------------------------------------------

// authenticateClient applies RFC 6749 2.3: a confidential client proves its
// secret (Basic or in the body); a public client names itself and is held
// to PKCE.
func (h *ConnectorHandler) authenticateClient(r *http.Request) (db.OAuthClient, bool, string) {
	formID, formSecret := r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	if basicID, basicSecret, hasBasic := r.BasicAuth(); hasBasic {
		// Basic credentials are form-encoded before base64 (RFC 6749 2.3.1).
		if v, err := url.QueryUnescape(basicID); err == nil {
			basicID = v
		}
		if v, err := url.QueryUnescape(basicSecret); err == nil {
			basicSecret = v
		}
		if formSecret != "" || (formID != "" && formID != basicID) {
			return db.OAuthClient{}, false, "use one client authentication method"
		}
		formID, formSecret = basicID, basicSecret
	}
	if formID == "" {
		return db.OAuthClient{}, false, "client_id is required"
	}
	client, err := db.GetOAuthClient(r.Context(), h.database, formID)
	if err != nil {
		return db.OAuthClient{}, false, "unknown client"
	}
	if client.SecretHash != nil {
		if formSecret == "" || subtle.ConstantTimeCompare(db.HashAPIKey(formSecret), client.SecretHash) != 1 {
			return db.OAuthClient{}, false, "client authentication failed"
		}
	}
	return client, true, ""
}

func (h *ConnectorHandler) parseTokenForm(w http.ResponseWriter, r *http.Request) (db.OAuthClient, bool) {
	if decision := h.limits.allow(oauthTokenPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return db.OAuthClient{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "body must be application/x-www-form-urlencoded")
		return db.OAuthClient{}, false
	}
	for k, v := range r.PostForm {
		if len(v) > 1 {
			oauthError(w, http.StatusBadRequest, "invalid_request", "parameter "+k+" is repeated")
			return db.OAuthClient{}, false
		}
	}
	client, ok, why := h.authenticateClient(r)
	if !ok {
		if _, _, basic := r.BasicAuth(); basic {
			w.Header().Set("WWW-Authenticate", `Basic realm="simple-host"`)
		}
		oauthError(w, http.StatusUnauthorized, "invalid_client", why)
		return db.OAuthClient{}, false
	}
	return client, true
}

func (h *ConnectorHandler) token(w http.ResponseWriter, r *http.Request) {
	client, ok := h.parseTokenForm(w, r)
	if !ok {
		return
	}
	_ = db.TouchOAuthClient(r.Context(), h.database, client.ClientID)
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		h.redeemCode(w, r, client)
	case "refresh_token":
		h.refresh(w, r, client)
	case "":
		oauthError(w, http.StatusBadRequest, "invalid_request", "grant_type is required")
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "")
	}
}

// personActive reports whether userID is still a person allowed to act.
func (h *ConnectorHandler) personActive(ctx context.Context, userID string) bool {
	disabled, err := db.IsUserDisabled(ctx, h.database, userID)
	return err == nil && !disabled
}

func (h *ConnectorHandler) redeemCode(w http.ResponseWriter, r *http.Request, client db.OAuthClient) {
	code := r.PostForm.Get("code")
	if !strings.HasPrefix(code, prefixCode) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown code")
		return
	}
	codeHash := db.HashAPIKey(code)
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	defer tx.Rollback()

	stored, err := db.ConsumeOAuthCode(r.Context(), tx, codeHash)
	if errors.Is(err, db.ErrOAuthCodeUsed) {
		// A code presented twice was intercepted or replayed: what the first
		// redemption issued is revoked (RFC 6749 4.1.2).
		_ = tx.Rollback()
		if stored.GrantID.Valid {
			if derr := db.DeleteOAuthGrant(r.Context(), h.database, stored.GrantID.String); derr != nil {
				log.Printf("oauth: revoke on code reuse: %v", derr)
			}
		}
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code already used")
		return
	}
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("oauth: consume code: %v", err)
			oauthError(w, http.StatusInternalServerError, "server_error", "")
			return
		}
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown code")
		return
	}
	// The code is spent from here on: every refusal below commits the spend,
	// so a failed attempt cannot be retried.
	fail := func(code, desc string) {
		_ = audit.Commit(tx)
		oauthError(w, http.StatusBadRequest, code, desc)
	}
	switch {
	case stored.ClientID != client.ClientID:
		fail("invalid_grant", "code was issued to another client")
		return
	case h.now().After(stored.ExpiresAt):
		fail("invalid_grant", "code expired")
		return
	case r.PostForm.Get("redirect_uri") != stored.RedirectURI:
		fail("invalid_grant", "redirect_uri does not match the authorization request")
		return
	}
	if verifier := r.PostForm.Get("code_verifier"); !validPKCEValue(verifier) || !pkceS256(verifier, stored.CodeChallenge) {
		fail("invalid_grant", "PKCE verification failed")
		return
	}
	if res := r.PostForm.Get("resource"); res != "" {
		if norm, ok := h.normalizeResource(res); !ok || norm != stored.Resource {
			fail("invalid_target", "resource does not match the authorization request")
			return
		}
	}
	if !h.personActive(r.Context(), stored.UserID) {
		fail("invalid_grant", "account unavailable")
		return
	}
	grantID, grantStart, err := db.InsertOAuthGrant(r.Context(), tx, stored.UserID, client.ClientID, stored.Resource)
	if err != nil {
		log.Printf("oauth: insert grant: %v", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	if err := db.SetOAuthCodeGrant(r.Context(), tx, codeHash, grantID); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	h.issueTokens(w, r, tx, grantID, grantStart)
}

func (h *ConnectorHandler) refresh(w http.ResponseWriter, r *http.Request, client db.OAuthClient) {
	presented := r.PostForm.Get("refresh_token")
	if !strings.HasPrefix(presented, prefixRefresh) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown refresh token")
		return
	}
	tokenHash := db.HashAPIKey(presented)
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	defer tx.Rollback()
	tok, err := db.GetOAuthToken(r.Context(), tx, tokenHash, true)
	if err != nil || tok.Kind != "refresh" || tok.ClientID != client.ClientID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "unknown refresh token")
		return
	}
	if h.now().After(tok.ExpiresAt) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token expired")
		return
	}
	if tok.UsedAt != nil {
		// A rotated-out refresh token came back: the app or a thief holds a
		// copy. The whole grant goes, so the thief's copy dies too (OAuth 2.1
		// 4.3.1). The person connects the app once more.
		_ = tx.Rollback()
		if derr := db.DeleteOAuthGrant(r.Context(), h.database, tok.GrantID); derr != nil {
			log.Printf("oauth: revoke on refresh reuse: %v", derr)
		}
		h.audit.Record(r.Context(), audit.Event{
			ActorID: tok.UserID, ActorKind: "system", Action: "connector_revoke", RequestID: auditRequestID(r.Context()),
			Detail: "refresh token reused; connection revoked", Extra: map[string]any{"client_id": tok.ClientID},
		})
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token already used; the connection was revoked")
		return
	}
	if scope := r.PostForm.Get("scope"); scope != "" {
		for _, s := range strings.Fields(scope) {
			if s != oauthScope {
				oauthError(w, http.StatusBadRequest, "invalid_scope", "scope exceeds the original grant")
				return
			}
		}
	}
	if res := r.PostForm.Get("resource"); res != "" {
		if norm, ok := h.normalizeResource(res); !ok || norm != tok.Resource {
			oauthError(w, http.StatusBadRequest, "invalid_target", "resource does not match the original grant")
			return
		}
	}
	if !h.personActive(r.Context(), tok.UserID) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "account unavailable")
		return
	}
	if err := db.MarkOAuthTokenUsed(r.Context(), tx, tokenHash); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	_ = db.TouchOAuthGrant(r.Context(), tx, tok.GrantID)
	h.issueTokens(w, r, tx, tok.GrantID, tok.GrantCreatedAt)
}

// issueTokens mints an access and a refresh token in grantID, commits, and
// writes the token response.
//
// The refresh token expires refreshTTL after the grant began (the person's
// sign-in and "Allow"), however often it rotates: a rotation that granted a
// fresh full lifetime would keep a connection alive forever without the
// person ever signing in at the IdP again. The access token never outlives
// the refresh token either.
func (h *ConnectorHandler) issueTokens(w http.ResponseWriter, r *http.Request, tx *sql.Tx, grantID string, grantStart time.Time) {
	access := randomToken(prefixAccess)
	refresh := randomToken(prefixRefresh)
	now := h.now()
	refreshExpires := grantStart.Add(h.refreshTTL)
	if !refreshExpires.After(now) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "the connection has expired; connect the app again")
		return
	}
	accessExpires := now.Add(h.accessTTL)
	if accessExpires.After(refreshExpires) {
		accessExpires = refreshExpires
	}
	if err := db.InsertOAuthToken(r.Context(), tx, db.HashAPIKey(access), grantID, "access", accessExpires); err != nil {
		log.Printf("oauth: insert access token: %v", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	if err := db.InsertOAuthToken(r.Context(), tx, db.HashAPIKey(refresh), grantID, "refresh", refreshExpires); err != nil {
		log.Printf("oauth: insert refresh token: %v", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	if err := audit.Commit(tx); err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessExpires.Sub(now) / time.Second),
		"refresh_token": refresh,
		"scope":         oauthScope,
	})
}

// revoke is RFC 7009. It answers 200 whether or not the token was known, so
// it cannot be used to test tokens. Revoking a refresh token disconnects the
// app; revoking an access token ends only that token.
func (h *ConnectorHandler) revoke(w http.ResponseWriter, r *http.Request) {
	client, ok := h.parseTokenForm(w, r)
	if !ok {
		return
	}
	token := r.PostForm.Get("token")
	if token == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "token is required")
		return
	}
	tokenHash := db.HashAPIKey(token)
	if tok, err := db.GetOAuthToken(r.Context(), h.database, tokenHash, false); err == nil && tok.ClientID == client.ClientID {
		tx, err := h.database.BeginTx(r.Context(), nil)
		if err == nil {
			defer tx.Rollback()
			if tok.Kind == "refresh" {
				err = db.DeleteOAuthGrant(r.Context(), tx, tok.GrantID)
			} else {
				err = db.DeleteOAuthToken(r.Context(), tx, tokenHash)
			}
		}
		if err == nil {
			err = h.audit.RecordTx(r.Context(), tx, audit.Event{
				ActorID: tok.UserID, Action: "connector_revoke", RequestID: auditRequestID(r.Context()),
				Detail: "app revoked its " + tok.Kind + " token", Extra: map[string]any{"client_id": tok.ClientID},
			})
		}
		if err == nil {
			err = audit.Commit(tx)
		}
		if err != nil {
			log.Printf("oauth: revoke: %v", err)
			oauthError(w, http.StatusServiceUnavailable, "temporarily_unavailable", "")
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}
