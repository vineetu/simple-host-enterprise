package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// Meta Muse cannot register itself. Its pre-registered client exists only
// where the company's OAUTH_REDIRECT_HOSTS allows agent.meta.ai.
func TestMetaMuseClientFollowsTheRedirectAllowlist(t *testing.T) {
	database := connectorTestDB(t)
	keys := []auth.SigningKey{{ID: "k1", Key: []byte(strings.Repeat("k", 32))}}
	build := func(hosts []string) (*http.ServeMux, *ConnectorHandler) {
		mux := http.NewServeMux()
		h := NewConnectorHandler(database, connectorTestBase, hosts, keys, time.Hour, nil, newTestHostModel(t, connectorTestBase), testAbuseLimits(time.Now))
		h.Register(mux, auth.Middleware(database, keys, time.Hour))
		return mux, h
	}
	ctx := context.Background()
	get := func(mux *http.ServeMux, target string) *httptest.ResponseRecorder {
		return serve(mux, httptest.NewRequest(http.MethodGet, connectorTestBase+target, nil))
	}
	authorize := "/oauth/authorize?response_type=code&client_id=meta-muse&redirect_uri=" +
		"https%3A%2F%2Fagent.meta.ai%2Fapi%2Fhatch%2Foauth%2Fcallback&code_challenge=" + strings.Repeat("a", 43) +
		"&code_challenge_method=S256&state=s&resource=" + connectorTestBase + "%2Fmcp"

	// The default allowlist does not name Muse: nothing is registered or served.
	mux, h := build(connectorTestRedirects)
	if err := h.EnsureBuiltinClients(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetOAuthClient(ctx, database, "meta-muse"); err == nil {
		t.Fatal("meta-muse registered without agent.meta.ai in OAUTH_REDIRECT_HOSTS")
	}
	if rec := get(mux, "/.well-known/muse-client.json"); rec.Code != http.StatusNotFound {
		t.Errorf("metadata without Muse allowed = %d, want 404", rec.Code)
	}

	// A company that allows Muse gets the client under both ids.
	mux, h = build(append(append([]string{}, connectorTestRedirects...), "agent.meta.ai"))
	if err := h.EnsureBuiltinClients(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"meta-muse", connectorTestBase + "/.well-known/muse-client.json"} {
		c, err := db.GetOAuthClient(ctx, database, id)
		if err != nil || c.TokenEndpointAuthMethod != "none" || len(c.RedirectURIs) != 1 {
			t.Errorf("client %s = %+v (%v)", id, c, err)
		}
	}
	if err := h.EnsureBuiltinClients(ctx); err != nil {
		t.Fatalf("second boot: %v", err)
	}
	// The sweep of abandoned registrations keeps it.
	if _, err := database.Exec(`UPDATE oauth_clients SET created_at = now() - interval '2 days' WHERE client_id = 'meta-muse'`); err != nil {
		t.Fatal(err)
	}
	if err := db.SweepOAuth(ctx, database); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetOAuthClient(ctx, database, "meta-muse"); err != nil {
		t.Errorf("sweep removed meta-muse: %v", err)
	}
	if rec := get(mux, "/.well-known/muse-client.json"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"client_id":"`+connectorTestBase+`/.well-known/muse-client.json"`) {
		t.Errorf("metadata = %d %s", rec.Code, rec.Body.String())
	}
	if rec := get(mux, authorize); rec.Code != http.StatusFound || !strings.HasPrefix(rec.Header().Get("Location"), "/auth/login") {
		t.Errorf("authorize with meta-muse = %d %s, want the company sign-in", rec.Code, rec.Header().Get("Location"))
	}
	typed := strings.Replace(authorize, "client_id=meta-muse", "client_id=someone%40example.com", 1)
	if rec := get(mux, typed); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "<code>meta-muse</code>") {
		t.Errorf("unknown client id = %d, want the Muse help: %s", rec.Code, rec.Body.String())
	}

	// Narrowing the allowlist later stops it at once, as for every app.
	mux, _ = build(connectorTestRedirects)
	if rec := get(mux, authorize); rec.Code != http.StatusBadRequest {
		t.Errorf("authorize after Muse was removed from the allowlist = %d, want 400", rec.Code)
	}
}
