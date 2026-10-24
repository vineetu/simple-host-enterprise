package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestAgentDiscoveryRepresentationsAndHostGate(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "https://foo.example")
	RegisterDiscovery(mux, "https://foo.example", "test-version", 30, "test-protocol", []DiscoveryTool{{Name: "real_tool", Description: "Real tool."}})
	gate := testHostGate(t, newTestStore(t), testHostModel(t))
	app := gate.wrap(APIErrors(mux, mux))
	paths := []string{"/index.md", "/auth.md", "/llms.txt", "/.well-known/mcp/server-card.json", "/.well-known/agent-card.json", "/.well-known/ard.json", "/.well-known/api-catalog", "/openapi.yaml"}
	for _, path := range paths {
		rec := gateRequest(app, "GET", "foo.example", path)
		if rec.Code != 200 || rec.Body.Len() == 0 || strings.Contains(rec.Body.String(), "{{") {
			t.Fatalf("%s: %d unresolved or empty body", path, rec.Code)
		}
		if strings.HasPrefix(path, "/.well-known/") && !json.Valid(rec.Body.Bytes()) {
			t.Fatalf("%s is not JSON", path)
		}
		if path == "/auth.md" && !strings.Contains(rec.Body.String(), "30 days") {
			t.Fatal("key expiry not expanded")
		}
		if path == "/.well-known/agent-card.json" && (!strings.Contains(rec.Body.String(), "real_tool") || !strings.Contains(rec.Body.String(), "test-version")) {
			t.Fatal("inventory or version missing")
		}
		if path == "/llms.txt" && len(strings.Fields(rec.Body.String())) > 160 {
			t.Fatal("quickstart grew beyond 160 words")
		}
		for _, host := range []string{"alice.foo.example", "my-site.alice.foo.example", "unknown.invalid"} {
			other := gateRequest(app, "GET", host, path)
			if other.Code == 200 && other.Body.String() == rec.Body.String() {
				t.Errorf("%s exposes metadata on %s", path, host)
			}
		}
		head := gateRequest(app, "HEAD", "foo.example", path)
		if head.Code != 200 || head.Body.Len() != 0 {
			t.Errorf("HEAD %s: %d, %d bytes", path, head.Code, head.Body.Len())
		}
	}
	for _, tc := range []struct {
		accept   string
		markdown bool
	}{
		{"text/markdown", true}, {"text/html", false}, {"text/markdown;q=0", false}, {"text/html;q=1, text/markdown;q=0.2", false}, {"*/*", false},
	} {
		r := httptest.NewRequest("GET", "https://foo.example/", nil)
		r.Header.Set("Accept", tc.accept)
		rec := serve(app, r)
		if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/markdown") != tc.markdown || !strings.Contains(rec.Header().Get("Vary"), "Accept") || !strings.Contains(rec.Header().Get("Link"), "/openapi.yaml") {
			t.Errorf("Accept %s: %v", tc.accept, rec.Header())
		}
	}
}

func TestAPIErrorsPreserveFormatsAndAllow(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/create", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 201, map[string]bool{"created": true}) })
	mux.HandleFunc("POST /mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSON(w, 401, errorResponse{Error: "sign in"})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(404)
		_, _ = w.Write([]byte("<p>not found</p>"))
	})
	app := APIErrors(mux, mux)
	for _, tc := range []struct {
		method, path string
		status       int
		code         string
	}{
		{"GET", "/api/missing", 404, "not_found"}, {"POST", "/api", 404, "not_found"}, {"GET", "/api/create", 405, "method_not_allowed"}, {"PATCH", "/api/create", 405, "method_not_allowed"}, {"POST", "/mcp/missing", 404, "jsonrpc"}, {"POST", "/mcp", 401, "jsonrpc"},
	} {
		rec := serve(app, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.status || !json.Valid(rec.Body.Bytes()) || !strings.Contains(rec.Body.String(), tc.code) {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if tc.status == 405 && rec.Header().Get("Allow") != "POST" {
			t.Errorf("Allow: %s", rec.Header().Get("Allow"))
		}
		if tc.status == 401 && rec.Header().Get("WWW-Authenticate") == "" {
			t.Fatal("lost OAuth challenge")
		}
	}
	oauth := serve(app, httptest.NewRequest("POST", "/oauth/token", nil))
	if !strings.Contains(oauth.Body.String(), `"error":"invalid_grant"`) || strings.Contains(oauth.Body.String(), "jsonrpc") {
		t.Fatal("changed OAuth error")
	}
	web := serve(app, httptest.NewRequest("GET", "/missing", nil))
	if web.Header().Get("Content-Type") != "text/html" {
		t.Fatal("changed website error")
	}
}

func TestRateHeadersUseSharedDecision(t *testing.T) {
	database := connectorTestDB(t)
	podA := NewAbuseLimits().WithSharedStore(database)
	podB := NewAbuseLimits().WithSharedStore(database)
	for i, pod := range []*AbuseLimits{podA, podB} {
		rec := httptest.NewRecorder()
		d := pod.allowHTTP(rec, authClientPolicy, "header-test")
		if !d.Allowed || d.Remaining != authClientPolicy.Burst-i-1 {
			t.Fatalf("shared balance %+v", d)
		}
		if rec.Header().Get("RateLimit-Remaining") != strconv.Itoa(d.Remaining) || rec.Header().Get("RateLimit-Reset") != strconv.Itoa(int(d.Reset.Seconds())) || !strings.Contains(rec.Header().Get("RateLimit"), "r="+strconv.Itoa(d.Remaining)) || rec.Header().Get("RateLimit-Policy") == "" {
			t.Fatalf("headers %v for %+v", rec.Header(), d)
		}
	}
}

func TestAgentCardSummariesStayShort(t *testing.T) {
	if got := agentCardSummary("First sentence. Further details."); got != "First sentence." {
		t.Fatal(got)
	}
	if got := agentCardSummary(strings.Repeat("界", 200)); len([]rune(got)) != 120 {
		t.Fatalf("summary length %d", len([]rune(got)))
	}
}
