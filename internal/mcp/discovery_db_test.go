package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/db"
)

func TestRunToolEnforcesRealScopeAuthorizationAndETags(t *testing.T) {
	database := resultsTestDB(t)
	ctx := context.Background()
	keys := map[string]string{}
	for _, name := range []string{"alice", "bob"} {
		user, err := db.CreateOIDCUser(ctx, database, name, "sub-"+name, name+"@example.com", false)
		if err != nil {
			t.Fatal(err)
		}
		for _, scope := range []string{db.APIKeyScopeFull, db.APIKeyScopePublish} {
			key := name + "-" + scope + strings.Repeat("x", 32)
			if _, err := db.CreateAPIKey(ctx, database, user.ID, "test", db.HashAPIKey(key), "test", "", time.Now().Add(time.Hour), scope); err != nil {
				t.Fatal(err)
			}
			keys[name+"-"+scope] = key
		}
	}
	s := realServer(t, database)
	call := func(key, name string, args map[string]any, wrapped bool) map[string]any {
		t.Helper()
		r := httptest.NewRequest("POST", "https://hosting.corp.test/mcp", nil)
		r.Header.Set("X-API-Key", keys[key])
		if wrapped {
			args = map[string]any{"name": name, "arguments": args}
			name = "run_tool"
		}
		out := discoveryCall(t, s, r, name, args)
		if out.Error != nil {
			t.Fatal(out.Error)
		}
		return out.Result.(map[string]any)
	}
	deployed := call("alice-full", "deploy_site", map[string]any{"owner": "alice", "site": "demo", "intent": "create", "files": []any{map[string]any{"path": "index.html", "content": "<title>Private inventory</title>"}}}, true)
	if deployed["isError"] != false {
		t.Fatal(deployed)
	}
	if etag, ok := deployed["structuredContent"].(map[string]any)["etag"].(string); !ok || etag == "" {
		t.Fatal("missing ETag")
	}
	for _, tc := range []struct {
		key, status string
	}{{"alice-publish", "HTTP 403"}, {"bob-full", "HTTP 404"}} {
		for _, wrapped := range []bool{false, true} {
			got := call(tc.key, "delete_site", map[string]any{"owner": "alice", "site": "demo", "confirm_name": "demo"}, wrapped)
			text := got["content"].([]any)[0].(map[string]any)["text"].(string)
			if got["isError"] != true || !strings.Contains(text, tc.status) {
				t.Fatalf("%s wrapped=%v: %s", tc.key, wrapped, text)
			}
		}
	}
	for _, wrapped := range []bool{false, true} {
		got := call("alice-full", "rollback_site", map[string]any{"owner": "alice", "site": "demo", "version": float64(1), "etag": `"stale"`}, wrapped)
		text := got["content"].([]any)[0].(map[string]any)["text"].(string)
		if got["isError"] != true || !strings.Contains(text, "HTTP 412") {
			t.Fatalf("stale ETag wrapped=%v: %s", wrapped, text)
		}
	}
	found := call("alice-publish", "list_sites", map[string]any{"query": "PRIVATE inventory", "limit": float64(1)}, true)
	if found["isError"] != false || found["structuredContent"].(map[string]any)["count"] != 1 {
		t.Fatal(found)
	}
	removed := call("alice-full", "delete_site", map[string]any{"owner": "alice", "site": "demo", "confirm_name": "demo"}, true)
	if removed["isError"] != false {
		t.Fatal(removed)
	}
}
