package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/mcp"
	"github.com/vsriram/simple-host/internal/migrate"
)

const connectorTestBase = "https://sh.example"

var connectorTestRedirects = []string{"chatgpt.com", "claude.ai", "vscode.dev", "localhost", "cursor://anysphere.cursor-mcp"}

// connectorTestMux wires the connector, a guarded /mcp, and one upstream
// route list_sites calls, the way main.go does.
func connectorTestMux(t *testing.T, database *sql.DB) (*http.ServeMux, *ConnectorHandler, []auth.SigningKey) {
	t.Helper()
	keys := []auth.SigningKey{{ID: "k1", Key: []byte(strings.Repeat("k", 32))}}
	authMW := auth.Middleware(database, keys, time.Hour)
	mux := http.NewServeMux()
	var recorder audit.Recorder
	if database != nil {
		recorder = audit.NewDBRecorder(database)
	}
	h := NewConnectorHandler(database, connectorTestBase, connectorTestRedirects, keys, time.Hour, recorder, newTestHostModel(t, connectorTestBase), testAbuseLimits(time.Now))
	h.Register(mux, authMW)
	mux.Handle("GET /api/collaboration/sites", authMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"username": auth.GetUser(r.Context()).Username})
	})))
	guarded := h.ProtectMCP(authMW, mcp.NewServer(mux, "simple-host", "0.0.0"))
	mux.Handle("POST /mcp", guarded)
	mux.Handle("GET /mcp", guarded)
	return mux, h, keys
}

func serve(mux http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func mcpRequest(body, bearer string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, connectorTestBase+"/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

const toolsList = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

func TestConnectorMetadata(t *testing.T) {
	mux, _, _ := connectorTestMux(t, nil)
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		rec := serve(mux, httptest.NewRequest(http.MethodGet, connectorTestBase+p, nil))
		var doc map[string]any
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &doc) != nil {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
		if doc["resource"] != connectorTestBase+"/mcp" {
			t.Errorf("%s: resource = %v", p, doc["resource"])
		}
		if as, _ := doc["authorization_servers"].([]any); len(as) != 1 || as[0] != connectorTestBase {
			t.Errorf("%s: authorization_servers = %v", p, doc["authorization_servers"])
		}
	}
	rec := serve(mux, httptest.NewRequest(http.MethodGet, connectorTestBase+"/.well-known/oauth-authorization-server", nil))
	var as map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &as); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"issuer":                 connectorTestBase,
		"authorization_endpoint": connectorTestBase + "/oauth/authorize",
		"token_endpoint":         connectorTestBase + "/oauth/token",
		"registration_endpoint":  connectorTestBase + "/oauth/register",
		"revocation_endpoint":    connectorTestBase + "/oauth/revoke",
	}
	for k, v := range want {
		if as[k] != v {
			t.Errorf("%s = %v, want %s", k, as[k], v)
		}
	}
	if m, _ := as["code_challenge_methods_supported"].([]any); len(m) != 1 || m[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v", as["code_challenge_methods_supported"])
	}
}

// Without a credential, /mcp answers every method, tools/list included, with
// the challenge that starts an OAuth client's sign-in.
func TestMCPWithoutCredentialIsChallenged(t *testing.T) {
	mux, _, _ := connectorTestMux(t, nil)
	for _, r := range []*http.Request{
		mcpRequest(toolsList, ""),
		httptest.NewRequest(http.MethodGet, connectorTestBase+"/mcp", nil),
	} {
		rec := serve(mux, r)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s /mcp = %d, want 401: %s", r.Method, rec.Code, rec.Body.String())
		}
		got := rec.Header().Get("WWW-Authenticate")
		if !strings.HasPrefix(got, "Bearer ") || !strings.Contains(got, `resource_metadata="`+connectorTestBase+`/.well-known/oauth-protected-resource"`) {
			t.Errorf("WWW-Authenticate = %q", got)
		}
		if strings.Contains(rec.Body.String(), "list_sites") {
			t.Error("unauthenticated tools/list leaked the tool list")
		}
	}
	// A browser session is not a credential for /mcp.
	r := mcpRequest(toolsList, "")
	r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "anything"})
	if rec := serve(mux, r); rec.Code != http.StatusUnauthorized {
		t.Errorf("cookie-only /mcp = %d, want 401", rec.Code)
	}
}

func TestRedirectRules(t *testing.T) {
	h := &ConnectorHandler{redirectHosts: connectorTestRedirects}
	for raw, want := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback":               true,
		"https://chatgpt.com/connector_platform_oauth_redirect": true,
		"http://127.0.0.1:33418/callback":                       true,
		"http://localhost/callback":                             true,
		"cursor://anysphere.cursor-mcp/oauth/callback":          true,
		"https://evil.example/cb":                               false,
		"http://claude.ai/cb":                                   false,
		"https://claude.ai/cb#frag":                             false,
		"https://user@claude.ai/cb":                             false,
		"javascript://claude.ai/x":                              false,
		"https://claude.ai.evil.example/cb":                     false,
	} {
		if got := h.redirectAllowed(raw); got != want {
			t.Errorf("redirectAllowed(%q) = %v, want %v", raw, got, want)
		}
	}
	if !(&ConnectorHandler{redirectHosts: []string{"*"}}).redirectAllowed("https://any.example/cb") {
		t.Error(`"*" should allow any https host`)
	}
	if !redirectMatches("http://127.0.0.1/callback", "http://127.0.0.1:5555/callback") {
		t.Error("loopback redirect should match on any port")
	}
	for _, p := range []string{"http://127.0.0.1:5555/other", "http://localhost:5555/callback", "https://127.0.0.1/callback"} {
		if redirectMatches("http://127.0.0.1/callback", p) {
			t.Errorf("redirectMatches accepted %q", p)
		}
	}
	if redirectMatches("https://claude.ai/a", "https://claude.ai/a?x=1") {
		t.Error("non-loopback redirect must match exactly")
	}
}

// ---- live Postgres -----------------------------------------------------------

func connectorTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MIGRATE_TEST_DSN")
	if dsn == "" {
		t.Skip("MIGRATE_TEST_DSN not set")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminDSN := *parsed
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("postgres", adminDSN.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	name := "oauth_test_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(`CREATE DATABASE ` + pq.QuoteIdentifier(name)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := sql.Open("postgres", adminDSN.String())
		if err == nil {
			_, _ = c.Exec(`DROP DATABASE IF EXISTS ` + pq.QuoteIdentifier(name) + ` WITH (FORCE)`)
			c.Close()
		}
	})
	testDSN := *parsed
	testDSN.Path = "/" + name
	database, err := sql.Open("postgres", testDSN.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := migrate.Apply(context.Background(), database, 0, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return database
}

type connectorFlow struct {
	t        *testing.T
	mux      *http.ServeMux
	database *sql.DB
	keys     []auth.SigningKey
	user     db.User
	cookie   *http.Cookie
}

func newConnectorFlow(t *testing.T) *connectorFlow {
	database := connectorTestDB(t)
	mux, _, keys := connectorTestMux(t, database)
	ctx := context.Background()
	user, err := db.CreateOIDCUser(ctx, database, "alice", "sub-alice", "alice@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	session, err := db.CreateSession(ctx, database, user.ID, expires, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	value, err := auth.SignSession(keys, session.ID, user.ID, expires)
	if err != nil {
		t.Fatal(err)
	}
	return &connectorFlow{t: t, mux: mux, database: database, keys: keys, user: user,
		cookie: &http.Cookie{Name: auth.SessionCookieName, Value: value}}
}

func (f *connectorFlow) register(redirect string) string {
	f.t.Helper()
	body := `{"client_name":"Claude","redirect_uris":["` + redirect + `"],"token_endpoint_auth_method":"none"}`
	rec := serve(f.mux, httptest.NewRequest(http.MethodPost, connectorTestBase+"/oauth/register", strings.NewReader(body)))
	var out struct {
		ClientID string `json:"client_id"`
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.ClientID == "" {
		f.t.Fatalf("register = %d %s", rec.Code, rec.Body.String())
	}
	return out.ClientID
}

func pkcePair() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorize walks sign-in and consent and returns the code.
func (f *connectorFlow) authorize(clientID, redirect, challenge string) string {
	f.t.Helper()
	q := url.Values{
		"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"st8"},
		"resource": {connectorTestBase + "/mcp"},
	}
	authorizeURL := connectorTestBase + "/oauth/authorize?" + q.Encode()

	rec := serve(f.mux, httptest.NewRequest(http.MethodGet, authorizeURL, nil))
	if rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/auth/login?to=") {
		f.t.Fatalf("signed-out authorize = %d %q, want redirect to sign-in", rec.Code, rec.Header().Get("Location"))
	}
	to, _ := url.Parse(rec.Header().Get("Location"))
	if sanitizeRedirectPath(to.Query().Get("to")) == "" {
		f.t.Fatalf("sign-in return path %q would be refused", to.Query().Get("to"))
	}

	r := httptest.NewRequest(http.MethodGet, authorizeURL, nil)
	r.AddCookie(f.cookie)
	rec = serve(f.mux, r)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "to use Simple Host as alice?") {
		f.t.Fatalf("consent page = %d %s", rec.Code, rec.Body.String())
	}

	form := url.Values{"query": {q.Encode()}, "decision": {"allow"}}
	r = httptest.NewRequest(http.MethodPost, connectorTestBase+"/oauth/authorize", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", connectorTestBase)
	r.AddCookie(f.cookie)
	rec = serve(f.mux, r)
	if rec.Code != http.StatusSeeOther {
		f.t.Fatalf("allow = %d %s", rec.Code, rec.Body.String())
	}
	back, _ := url.Parse(rec.Header().Get("Location"))
	if back.Query().Get("state") != "st8" || back.Query().Get("iss") != connectorTestBase || back.Query().Get("code") == "" {
		f.t.Fatalf("redirect back = %s", back)
	}
	return back.Query().Get("code")
}

func (f *connectorFlow) tokenRequest(form url.Values) (*httptest.ResponseRecorder, map[string]any) {
	r := httptest.NewRequest(http.MethodPost, connectorTestBase+"/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := serve(f.mux, r)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// connect runs the whole flow and returns the client id and tokens.
func (f *connectorFlow) connect(redirect string) (clientID, access, refresh string) {
	f.t.Helper()
	clientID = f.register(redirect)
	verifier, challenge := pkcePair()
	code := f.authorize(clientID, redirect, challenge)
	rec, out := f.tokenRequest(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {clientID}, "code_verifier": {verifier}})
	if rec.Code != http.StatusOK {
		f.t.Fatalf("token = %d %s", rec.Code, rec.Body.String())
	}
	return clientID, out["access_token"].(string), out["refresh_token"].(string)
}

func (f *connectorFlow) mcpStatus(bearer string) int {
	return serve(f.mux, mcpRequest(toolsList, bearer)).Code
}

func TestConnectorFullFlowAndBearerOnMCP(t *testing.T) {
	f := newConnectorFlow(t)
	_, access, _ := f.connect("https://claude.ai/api/mcp/auth_callback")

	if got := f.mcpStatus(access); got != http.StatusOK {
		t.Fatalf("tools/list with bearer = %d", got)
	}
	// The token is forwarded to the REST route a tool calls, as this person.
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_sites","arguments":{}}}`
	rec := serve(f.mux, mcpRequest(call, access))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `\"username\":\"alice\"`) {
		t.Fatalf("tools/call list_sites = %d %s", rec.Code, rec.Body.String())
	}

	rec = serve(f.mux, mcpRequest(toolsList, "shat_not-a-token"))
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("bad bearer = %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	// A bad bearer is refused even with a valid browser session alongside it.
	r := mcpRequest(toolsList, "shat_not-a-token")
	r.AddCookie(f.cookie)
	if rec := serve(f.mux, r); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad bearer with cookie = %d", rec.Code)
	}

	var n int
	if err := f.database.QueryRow(`SELECT count(*) FROM audit_events WHERE action = 'connector_sign_in' AND actor_id = $1`, f.user.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("connector_sign_in audit rows = %d (%v), want 1", n, err)
	}
}

func TestConnectorLoopbackRedirectAnyPort(t *testing.T) {
	f := newConnectorFlow(t)
	clientID := f.register("http://127.0.0.1/callback")
	verifier, challenge := pkcePair()
	redirect := "http://127.0.0.1:49152/callback"
	code := f.authorize(clientID, redirect, challenge)
	rec, _ := f.tokenRequest(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {clientID}, "code_verifier": {verifier}})
	if rec.Code != http.StatusOK {
		t.Fatalf("token = %d %s", rec.Code, rec.Body.String())
	}
}

func TestConnectorDCRRefusesDisallowedRedirect(t *testing.T) {
	f := newConnectorFlow(t)
	body := `{"redirect_uris":["https://evil.example/cb"],"token_endpoint_auth_method":"none"}`
	rec := serve(f.mux, httptest.NewRequest(http.MethodPost, connectorTestBase+"/oauth/register", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_redirect_uri") {
		t.Fatalf("register = %d %s", rec.Code, rec.Body.String())
	}
	// An authorize for an unregistered redirect is shown to the person, not
	// sent anywhere.
	clientID := f.register("https://claude.ai/cb")
	_, challenge := pkcePair()
	q := url.Values{"client_id": {clientID}, "redirect_uri": {"https://evil.example/cb"}, "response_type": {"code"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	rec = serve(f.mux, httptest.NewRequest(http.MethodGet, connectorTestBase+"/oauth/authorize?"+q.Encode(), nil))
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Fatalf("authorize with foreign redirect = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestConnectorPKCEAndCodeReuse(t *testing.T) {
	f := newConnectorFlow(t)
	redirect := "https://claude.ai/api/mcp/auth_callback"
	clientID := f.register(redirect)

	// No PKCE at all goes back to the app as an error.
	q := url.Values{"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"}}
	r := httptest.NewRequest(http.MethodGet, connectorTestBase+"/oauth/authorize?"+q.Encode(), nil)
	r.AddCookie(f.cookie)
	rec := serve(f.mux, r)
	if loc, _ := url.Parse(rec.Header().Get("Location")); rec.Code != http.StatusFound || loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("authorize without PKCE = %d %q", rec.Code, rec.Header().Get("Location"))
	}

	verifier, challenge := pkcePair()
	code := f.authorize(clientID, redirect, challenge)
	wrong, _ := pkcePair()
	rec, out := f.tokenRequest(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {clientID}, "code_verifier": {wrong}})
	if rec.Code != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier = %d %v", rec.Code, out)
	}
	// The failed attempt spent the code.
	rec, _ = f.tokenRequest(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {clientID}, "code_verifier": {verifier}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code after failed attempt = %d", rec.Code)
	}

	// A code redeemed twice revokes what the first redemption issued.
	verifier, challenge = pkcePair()
	code = f.authorize(clientID, redirect, challenge)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {clientID}, "code_verifier": {verifier}}
	rec, out = f.tokenRequest(form)
	if rec.Code != http.StatusOK {
		t.Fatalf("first redemption = %d", rec.Code)
	}
	access := out["access_token"].(string)
	if rec, _ = f.tokenRequest(form); rec.Code != http.StatusBadRequest {
		t.Fatalf("second redemption = %d", rec.Code)
	}
	if got := f.mcpStatus(access); got != http.StatusUnauthorized {
		t.Fatalf("access token after code reuse = %d, want 401", got)
	}
}

func TestConnectorRefreshRotationAndReuse(t *testing.T) {
	f := newConnectorFlow(t)
	clientID, access, refresh := f.connect("https://chatgpt.com/connector_platform_oauth_redirect")

	rec, out := f.tokenRequest(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body.String())
	}
	access2, refresh2 := out["access_token"].(string), out["refresh_token"].(string)
	if refresh2 == refresh || access2 == access {
		t.Fatal("refresh did not rotate")
	}
	if got := f.mcpStatus(access2); got != http.StatusOK {
		t.Fatalf("rotated access token = %d", got)
	}
	// Another client cannot use it.
	other := f.register("https://claude.ai/cb")
	if rec, _ := f.tokenRequest(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh2}, "client_id": {other}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("refresh by another client = %d", rec.Code)
	}
	// The rotated-out token coming back revokes the whole connection.
	rec, out = f.tokenRequest(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	if rec.Code != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("reuse = %d %v", rec.Code, out)
	}
	if got := f.mcpStatus(access2); got != http.StatusUnauthorized {
		t.Fatalf("access after reuse = %d, want 401", got)
	}
	if rec, _ := f.tokenRequest(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh2}, "client_id": {clientID}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("newest refresh after reuse = %d, want 400", rec.Code)
	}
}

func TestConnectorRevoke(t *testing.T) {
	f := newConnectorFlow(t)
	clientID, access, refresh := f.connect("https://claude.ai/api/mcp/auth_callback")
	r := httptest.NewRequest(http.MethodPost, connectorTestBase+"/oauth/revoke",
		strings.NewReader(url.Values{"token": {refresh}, "client_id": {clientID}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := serve(f.mux, r); rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body.String())
	}
	if got := f.mcpStatus(access); got != http.StatusUnauthorized {
		t.Fatalf("access after revoking the refresh token = %d, want 401", got)
	}
	if rec, _ := f.tokenRequest(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("refresh after revoke = %d", rec.Code)
	}
	if err := db.SweepOAuth(context.Background(), f.database); err != nil {
		t.Fatalf("SweepOAuth: %v", err)
	}
}

func TestConnectorDisabledUser(t *testing.T) {
	f := newConnectorFlow(t)
	clientID, access, refresh := f.connect("https://claude.ai/api/mcp/auth_callback")
	if err := db.SetUserDisabled(context.Background(), f.database, f.user.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := f.mcpStatus(access); got != http.StatusUnauthorized {
		t.Fatalf("access for a disabled person = %d, want 401", got)
	}
	if rec, _ := f.tokenRequest(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("refresh for a disabled person = %d", rec.Code)
	}
	var n int
	if err := f.database.QueryRow(`SELECT count(*) FROM oauth_grants WHERE user_id = $1`, f.user.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("grants left after disabling = %d (%v)", n, err)
	}
}

func TestConnectorConfidentialClient(t *testing.T) {
	f := newConnectorFlow(t)
	redirect := "https://chatgpt.com/cb"
	rec := serve(f.mux, httptest.NewRequest(http.MethodPost, connectorTestBase+"/oauth/register",
		strings.NewReader(`{"redirect_uris":["`+redirect+`"]}`)))
	var reg struct{ ClientID, ClientSecret string }
	var raw map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	reg.ClientID, reg.ClientSecret = raw["client_id"], raw["client_secret"]
	if rec.Code != http.StatusCreated || reg.ClientSecret == "" {
		t.Fatalf("register = %d %s", rec.Code, rec.Body.String())
	}
	verifier, challenge := pkcePair()
	code := f.authorize(reg.ClientID, redirect, challenge)
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {verifier}}
	r := httptest.NewRequest(http.MethodPost, connectorTestBase+"/oauth/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.SetBasicAuth(reg.ClientID, "wrong")
	if rec := serve(f.mux, r); rec.Code != http.StatusUnauthorized {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("wrong secret = %d %s", rec.Code, body)
	}
}

func TestPluginZipIsTemplatedWithBaseURL(t *testing.T) {
	mux := http.NewServeMux()
	RegisterPluginRoute(mux, connectorTestBase+"/")
	rec := serve(mux, httptest.NewRequest(http.MethodGet, connectorTestBase+"/plugin.zip", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /plugin.zip = %d", rec.Code)
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = string(body)
		if strings.Contains(string(body), "{{BASE_URL}}") || strings.Contains(string(body), "{{VERSION}}") {
			t.Errorf("%s still has a placeholder", f.Name)
		}
	}
	for _, name := range []string{"simple-host/.claude-plugin/plugin.json", "simple-host/.mcp.json", "simple-host/plugin.json", "simple-host/mcp.json", "simple-host/skills/simple-host/SKILL.md"} {
		if _, ok := files[name]; !ok {
			t.Errorf("plugin.zip is missing %s", name)
		}
	}
	for _, name := range []string{"simple-host/.mcp.json", "simple-host/mcp.json"} {
		var doc struct {
			MCPServers map[string]struct{ URL string } `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(files[name]), &doc); err != nil || doc.MCPServers["simple-host"].URL != connectorTestBase+"/mcp" {
			t.Errorf("%s: url = %q (%v)", name, doc.MCPServers["simple-host"].URL, err)
		}
	}
	var manifest map[string]any
	if err := json.Unmarshal([]byte(files["simple-host/plugin.json"]), &manifest); err != nil || manifest["version"] == "" {
		t.Errorf("plugin.json: %v %v", manifest, err)
	}
}

func TestConnectorOverlongAuthorizeIsRefusedBeforeSignIn(t *testing.T) {
	f := newConnectorFlow(t)
	redirect := "https://claude.ai/api/mcp/auth_callback"
	clientID := f.register(redirect)
	_, challenge := pkcePair()
	q := url.Values{"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {strings.Repeat("s", 1990)}}
	rec := serve(f.mux, httptest.NewRequest(http.MethodGet, connectorTestBase+"/oauth/authorize?"+q.Encode(), nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("overlong authorize = %d %q, want 400", rec.Code, rec.Header().Get("Location"))
	}
}
