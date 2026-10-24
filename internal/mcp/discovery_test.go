package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCoreInventoryAndDiscovery(t *testing.T) {
	s := NewServer(nil, "company-sites", "test")
	r := appRequest("alice")
	listed := s.listedTools(r)
	if len(listed) != 20 {
		t.Fatalf("listed %d tools", len(listed))
	}
	for i, name := range coreToolNames {
		if listed[i].Name != name {
			t.Fatalf("missing core %s", name)
		}
	}
	for _, tool := range s.tools {
		if tool.Name == "find_tools" || tool.Name == "run_tool" {
			continue
		}
		matches := newToolSearch(s.tools).rank(tool.Name, 1)
		if len(matches) != 1 || matches[0].tool.Name != tool.Name {
			t.Errorf("not reachable: %s", tool.Name)
		}
	}
	for _, tool := range discoveryTools() {
		if tool.Name == "find_tools" {
			if err := CheckOutputSchema(tool.OutputSchema); err != nil {
				t.Fatal(err)
			}
		}
		for _, key := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			if _, ok := tool.Annotations[key].(bool); !ok {
				t.Errorf("%s missing %s", tool.Name, key)
			}
		}
	}
}

func TestDiscoveryRanking(t *testing.T) {
	search := newToolSearch(Tools())
	for _, tc := range []struct{ query, want string }{
		{"undo last change", "rollback_site"},
		{"recover deleted website", "restore_site"},
		{"remove viewer", "revoke_site_viewer"},
		{"remove team member", "remove_team_member"},
		{"create team", "create_team"},
		{"rename website", "rename_site"},
		{"download entire website", "export_site"},
		{"delete website", "delete_site"},
		{"remove saved setting", "storage_delete_kv"},
		{"delete row", "storage_delete_row"},
		{"show uploaded photos", "storage_list_file_objects"},
		{"how many people visited", "site_activity"},
		{"company showcase", "search_sites"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			got := search.rank(tc.query, 3)
			if len(got) == 0 || got[0].tool.Name != tc.want {
				var names []string
				for _, v := range got {
					names = append(names, v.tool.Name)
				}
				t.Fatalf("got %v want %s first", names, tc.want)
			}
		})
	}
}

func discoveryCall(t *testing.T, s *Server, r *http.Request, name string, args map[string]any) response {
	t.Helper()
	params, err := json.Marshal(callParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return s.callTool(r, request{ID: json.RawMessage("1"), Params: params})
}

// Every REST adapter fixture must behave identically through the wrapper,
// including errors, arguments, credentials, ETags, and target annotations.
func TestRunToolMatchesDirectCalls(t *testing.T) {
	type seenRequest struct{ method, path, key, bearer, etag, version string }
	var seen []seenRequest
	s := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, seenRequest{r.Method, r.URL.String(), r.Header.Get("X-API-Key"), r.Header.Get("Authorization"), r.Header.Get("If-Match"), r.Header.Get("X-Skill-Version")})
		w.Header().Set("ETag", `"new-version"`)
		w.Write([]byte(`{"ok":true}`))
	}), "sites", "test")
	r := appRequest("alice")
	r.Header.Set("X-API-Key", "test-key")
	r.Header.Set("Authorization", "Bearer test-token")
	for _, fixture := range fixtures {
		for _, args := range fixture.args {
			seen = nil
			direct := discoveryCall(t, s, r, fixture.tool, args)
			requests := append([]seenRequest(nil), seen...)
			seen = nil
			wrapped := discoveryCall(t, s, r, "run_tool", map[string]any{"name": fixture.tool, "arguments": args})
			if !reflect.DeepEqual(direct, wrapped) || !reflect.DeepEqual(requests, seen) {
				t.Errorf("different result/request for %s", fixture.tool)
			}
		}
	}
	for _, status := range []int{401, 403, 404, 409, 412, 428} {
		s.upstream = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"error":"refused"}`))
		})
		args := map[string]any{"site": "demo", "owner": "alice", "etag": "stale", "version": float64(1)}
		direct := discoveryCall(t, s, r, "rollback_site", args)
		wrapped := discoveryCall(t, s, r, "run_tool", map[string]any{"name": "rollback_site", "arguments": args})
		if !reflect.DeepEqual(direct, wrapped) {
			t.Errorf("different refusal at %d", status)
		}
	}
	for _, args := range []map[string]any{{"name": "run_tool"}, {"name": "find_tools"}, {"name": "delete_site", "arguments": 3}, {"name": "delete_site", "arguments": map[string]any{"extra": true}}} {
		got := discoveryCall(t, s, r, "run_tool", args)
		if got.Error == nil && got.Result.(map[string]any)["isError"] != true {
			t.Errorf("accepted %v", args)
		}
	}
}

func TestFindToolsCatalogAndAnnotations(t *testing.T) {
	s := NewServer(nil, "sites", "test")
	for _, query := range []string{"", "nonsensezzzz", "delete_site"} {
		got := discoveryCall(t, s, appRequest("alice"), "find_tools", map[string]any{"query": query})
		data := got.Result.(map[string]any)["structuredContent"].(map[string]any)
		if err := ValidateOutput(s.byName["find_tools"].OutputSchema, data, nil); err != nil {
			t.Fatal(err)
		}
		if query == "delete_site" {
			tool := data["tools"].([]any)[0].(map[string]any)
			if !reflect.DeepEqual(tool["annotations"], s.byName["delete_site"].Annotations) {
				t.Fatal("lost annotations")
			}
		}
	}
}

func TestInstructionsBudgetForInstances(t *testing.T) {
	// Check the actual protocol outputs after instance and Apps configuration.
	for _, origin := range []string{"https://sites.example.test", "https://" + strings.Repeat("a", 63) + ".example.test"} {
		s := NewServer(nil, "Enterprise sites", "test").WithApps(origin, map[string]string{"MCP_APP_MY_SITES": "on"})
		for _, method := range []string{"initialize", "server/discover"} {
			resp, _ := s.dispatch(httptest.NewRequest("POST", origin+"/mcp", nil), request{ID: json.RawMessage("1"), Method: method})
			text := resp.Result.(map[string]any)["instructions"].(string)
			if utf8.RuneCountInString(text) >= 2000 {
				t.Fatalf("instructions: %d characters", utf8.RuneCountInString(text))
			}
			if !strings.Contains(text, "find_tools and run_tool") {
				t.Fatal("missing discovery guidance")
			}
		}
	}
}

func TestListSitesSearchArguments(t *testing.T) {
	tool, _ := byName("list_sites")
	up, err := tool.call(map[string]any{
		"query": "REAL coastal", "created_after": "2026-10-01", "created_before": "2026-10-31",
		"updated_after": "2026-10-01T00:00:00Z", "updated_before": "2026-10-31T23:59:59Z",
		"limit": float64(1000000), "offset": float64(1000000),
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(up.Path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"include": "shared", "query": "REAL coastal", "created_after": "2026-10-01", "created_before": "2026-10-31", "updated_after": "2026-10-01T00:00:00Z", "updated_before": "2026-10-31T23:59:59Z", "limit": "1000000", "offset": "1000000"}
	for key, value := range want {
		if u.Query().Get(key) != value {
			t.Errorf("%s: %q want %q", key, u.Query().Get(key), value)
		}
	}
}
