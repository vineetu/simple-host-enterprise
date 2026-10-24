package mcp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateToolsCarryIdempotencyKey(t *testing.T) {
	tests := map[string]map[string]any{
		"deploy_site":     {"owner": "alice", "site": "test", "intent": "create", "files": []any{map[string]any{"path": "index.html", "content": "hello"}}},
		"storage_add_row": {"owner": "alice", "site": "test", "resource": "data", "table": "tasks", "values": map[string]any{"title": "one"}},
		"export_site":     {"owner": "alice", "site": "test"},
	}
	for _, tool := range Tools() {
		args, ok := tests[tool.Name]
		if !ok {
			continue
		}
		args["idempotency_key"] = "retry-one"
		up, err := tool.call(args)
		if err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		again, err := tool.call(args)
		if err != nil || !bytes.Equal(up.Body, again.Body) {
			t.Fatalf("%s retry body changed", tool.Name)
		}
		if up.IdempotencyKey != "retry-one" || up.Method != "POST" {
			t.Fatalf("%s lost key", tool.Name)
		}
		called := false
		upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			if r.Header.Get("Idempotency-Key") != "retry-one" {
				t.Error("header lost")
			}
			w.WriteHeader(201)
		})
		server := NewServer(upstream, "test", "test")
		server.WithSiteAPI(upstream, func(_ context.Context, owner, site string) (string, error) { return "test.alice.example.com", nil })
		server.serveUpstream(httptest.NewRequest("POST", "https://example.com/mcp", nil), up)
		if !called {
			t.Fatal("upstream not called")
		}
	}
}

func TestTransportMethodErrorsAlwaysJSONRPC(t *testing.T) {
	for _, method := range []string{"GET", "HEAD", "DELETE", "PUT", "PATCH"} {
		rec := httptest.NewRecorder()
		testServer().ServeHTTP(rec, httptest.NewRequest(method, "/mcp", nil))
		if rec.Code != 405 || rec.Header().Get("Allow") != "POST" || errorCode(t, rec) != codeInvalidRequest {
			t.Fatalf("%s: %d %s", method, rec.Code, rec.Body.String())
		}
	}
}
