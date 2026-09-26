package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/handler"
)

// An OAuth access token works through /mcp — management routes and the
// site API on the owner's host — and nowhere else; an API key's scope holds
// for every REST call a tool makes.
func TestCredentialsThroughMCP(t *testing.T) {
	database := resultsTestDB(t)
	s, mux, authMW := realApp(t, database)
	fullKey := createPerson(t, database, "alice")
	ctx := context.Background()

	// A connected app's access token, written the way the token endpoint does.
	if _, err := database.Exec(`INSERT INTO oauth_clients (client_id, client_name, redirect_uris) VALUES ('c1', 'app', '[]')`); err != nil {
		t.Fatal(err)
	}
	var aliceID string
	if err := database.QueryRow(`SELECT id FROM users WHERE username = 'alice'`).Scan(&aliceID); err != nil {
		t.Fatal(err)
	}
	const token = "shat_" + "0123456789abcdef0123456789abcdef"
	tx, _ := database.Begin()
	grantID, _, err := db.InsertOAuthGrant(ctx, tx, aliceID, "c1", "https://hosting.corp.test/mcp")
	if err == nil {
		err = db.InsertOAuthToken(ctx, tx, db.HashAPIKey(token), grantID, "access", time.Now().Add(time.Hour))
	}
	if err != nil || tx.Commit() != nil {
		t.Fatalf("insert token: %v", err)
	}
	publishKey := "key-alice-publish-" + strings.Repeat("2", 30)
	if _, err := db.CreateAPIKey(ctx, database, aliceID, "ci", db.HashAPIKey(publishKey), publishKey[:8], time.Now().Add(time.Hour), db.APIKeyScopePublish); err != nil {
		t.Fatal(err)
	}

	hosts, _ := handler.NewHostModel("https://hosting.corp.test")
	connector := handler.NewConnectorHandler(database, "https://hosting.corp.test", nil, nil, time.Hour, nil, hosts)
	protected := connector.ProtectMCP(authMW, s)
	mux.Handle("POST /mcp", protected) // as main.go does: the pattern is what a key's scope is checked against
	call := func(headers map[string]string, name string, args map[string]any) (bool, string) {
		t.Helper()
		params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + strings.TrimSuffix(string(params), "}") + `,` + meta + `}}`
		req := httptest.NewRequest(http.MethodPost, "https://hosting.corp.test/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range modern("tools/call", name) {
			req.Header.Set(k, v)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		res, _ := decode(t, rec)["result"].(map[string]any)
		if res == nil {
			t.Fatalf("%s: no result: %d %s", name, rec.Code, rec.Body)
		}
		text := res["content"].([]any)[0].(map[string]any)["text"].(string)
		return res["isError"] == false, text
	}
	bearer := map[string]string{"Authorization": "Bearer " + token}
	publish := map[string]string{"X-API-Key": publishKey}
	index := map[string]any{"path": "index.html", "content": "<h1>hi</h1>"}

	if ok, text := call(bearer, "get_account", map[string]any{}); !ok {
		t.Fatalf("get_account by bearer through /mcp failed: %s", text)
	}
	if ok, text := call(bearer, "deploy_site", map[string]any{"site": "demo", "files": []any{index}}); !ok {
		t.Fatalf("deploy_site by bearer failed: %s", text)
	}
	if ok, text := call(bearer, "get_state", map[string]any{"site": "demo", "owner": "alice"}); !ok {
		t.Fatalf("get_state by bearer (site API through the host gate) failed: %s", text)
	}
	if ok, text := call(publish, "get_account", map[string]any{}); !ok {
		t.Fatalf("get_account by publish key failed: %s", text)
	}
	offboardKey := "key-alice-offboard-" + strings.Repeat("3", 30)
	if _, err := db.CreateAPIKey(ctx, database, aliceID, "hr", db.HashAPIKey(offboardKey), offboardKey[:8], time.Now().Add(time.Hour), db.APIKeyScopeOffboard); err != nil {
		t.Fatal(err)
	}
	offReq := httptest.NewRequest(http.MethodGet, "https://hosting.corp.test/api/me", nil)
	offReq.Header.Set("X-API-Key", offboardKey)
	offRec := httptest.NewRecorder()
	mux.ServeHTTP(offRec, offReq)
	if offRec.Code != http.StatusForbidden {
		t.Fatalf("offboard key on GET /api/me = %d, want 403", offRec.Code)
	}
	if ok, text := call(publish, "get_state", map[string]any{"site": "demo", "owner": "alice"}); !ok {
		t.Fatalf("get_state by publish key failed: %s", text)
	}
	if ok, text := call(publish, "deploy_site", map[string]any{"site": "ci", "files": []any{index}}); !ok {
		t.Fatalf("deploy_site by publish key failed: %s", text)
	}
	if ok, _ := call(publish, "delete_site", map[string]any{"site": "ci", "confirm_name": "ci"}); ok {
		t.Fatal("delete_site by a publish key succeeded")
	}
	if ok, _ := call(publish, "set_site_access", map[string]any{"site": "ci", "level": "company"}); ok {
		t.Fatal("set_site_access by a publish key succeeded")
	}
	if ok, text := call(map[string]string{"X-API-Key": fullKey}, "delete_site", map[string]any{"site": "ci", "confirm_name": "ci"}); !ok {
		t.Fatalf("delete_site by a full key failed: %s", text)
	}

	// Straight to the REST route, the same token is refused.
	req := httptest.NewRequest(http.MethodGet, "https://hosting.corp.test/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bearer on GET /api/me = %d, want 401", rec.Code)
	}
}
