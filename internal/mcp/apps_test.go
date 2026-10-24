package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vsriram/simple-host/internal/auth"
	"github.com/vsriram/simple-host/internal/db"
)

func appRequest(handle string) *http.Request {
	r := httptest.NewRequest("POST", "https://sites.example.test/mcp", nil)
	return r.WithContext(auth.ContextWithTestAuth(context.Background(), &db.User{Username: handle}, "", ""))
}
func TestAppFlagsAndResources(t *testing.T) {
	for _, tc := range []struct {
		value, handle string
		want          bool
	}{{"on", "alice", true}, {"off", "alice", false}, {"", "alice", false}, {"accounts:alice, bob", "bob", true}, {"accounts:alice", "bob", false}, {"accounts:Alice", "alice", false}, {"accounts:alice@example.test", "alice", false}} {
		if got := appFlag(tc.value, tc.handle); got != tc.want {
			t.Errorf("%q %q: %v", tc.value, tc.handle, got)
		}
	}
	flags := map[string]string{}
	for _, screen := range appScreens {
		flags[screen.Flag] = "accounts:alice"
	}
	s := NewServer(nil, "simple-host", "test").WithApps("https://sites.example.test", flags)
	for _, handle := range []string{"alice", "bob"} {
		r := appRequest(handle)
		want := 0
		if handle == "alice" {
			want = 4
		}
		if got := len(s.appResources(r)); got != want {
			t.Fatalf("%s resources: %d", handle, got)
		}
		for _, screen := range appScreens {
			params, _ := json.Marshal(map[string]any{"uri": screen.URI})
			resp := s.readApp(request{Params: params}, r)
			if handle == "bob" {
				if resp.Error == nil {
					t.Fatal("cross-account resource")
				}
				continue
			}
			if resp.Error != nil {
				t.Fatal(resp.Error)
			}
			content := resp.Result.(map[string]any)["contents"].([]any)[0].(map[string]any)
			if content["mimeType"] != appMIME {
				t.Fatal("wrong MIME")
			}
			page := content["text"].(string)
			for _, bad := range []string{"__STYLE__", "__BRIDGE__", "__CONFIG__", "localStorage", "X-API-Key", "document.cookie", "fetch("} {
				if strings.Contains(page, bad) {
					t.Fatalf("%s contains %s", screen.ID, bad)
				}
			}
			csp := content["_meta"].(map[string]any)["ui"].(map[string]any)["csp"].(map[string][]string)
			if len(csp["connectDomains"]) != 0 || len(csp["frameDomains"]) != 0 {
				t.Fatal("unexpected network or frames")
			}
			if screen.ID == "photo-picker" && (len(csp["resourceDomains"]) != 1 || csp["resourceDomains"][0] != "https://sites.example.test") {
				t.Fatal("photo assets must be self hosted")
			}
		}
	}
}
func TestAppsMetadataDoesNotMutateInventory(t *testing.T) {
	s := NewServer(nil, "simple-host", "test").WithApps("https://sites.example.test", map[string]string{"MCP_APP_MY_SITES": "accounts:alice"})
	before, _ := json.Marshal(s.tools)
	for _, handle := range []string{"alice", "bob", "alice", "bob"} {
		for _, tool := range s.appTools(appRequest(handle)) {
			if tool.Name == "list_sites" && (tool.Meta != nil) != (handle == "alice") {
				t.Fatal("metadata leaked across accounts")
			}
		}
	}
	after, _ := json.Marshal(s.tools)
	if string(before) != string(after) {
		t.Fatal("cached tools changed")
	}
}
func TestPhotoToolFallbackAndAccountScope(t *testing.T) {
	s := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"name":"demo","owner_username":"team-sales","access_role":"member"},{"name":"view","owner_username":"bob","access_role":"viewer"}]`))
	}), "simple-host", "test").WithApps("https://sites.example.test", map[string]string{"MCP_APP_PHOTO_PICKER": "on"})
	for _, name := range []string{"team-sales/demo", "view"} {
		params, _ := json.Marshal(map[string]any{"name": "add_photos", "arguments": map[string]any{"site": name}})
		out := s.callTool(appRequest("alice"), request{Params: params}).Result.(map[string]any)
		if name == "view" {
			if out["isError"] != true {
				t.Fatal("viewer may not edit")
			}
			continue
		}
		text := out["content"].([]any)[0].(map[string]any)["text"].(string)
		if text != "https://sites.example.test/photos?account=alice&site=team-sales%2Fdemo" {
			t.Fatal(text)
		}
		if out["_meta"] == nil {
			t.Fatal("no picker metadata")
		}
	}
}
func TestDashboardSevenDayQuery(t *testing.T) {
	up, err := siteActivity(map[string]any{"owner": "alice", "site": "demo", "days": float64(7)})
	if err != nil || !strings.Contains(up.Also[1].Path, "&from=") {
		t.Fatalf("weekly query: %v", err)
	}
	up, err = siteActivity(map[string]any{"owner": "alice", "site": "demo"})
	if err != nil || strings.Contains(up.Also[1].Path, "&from=") {
		t.Fatal("default activity changed")
	}
}

func TestModernAppResourceEnvelope(t *testing.T) {
	s := NewServer(nil, "simple-host", "test").WithApps("https://sites.example.test", map[string]string{"MCP_APP_MY_SITES": "on"})
	for _, method := range []string{"resources/list", "resources/read", "resources/templates/list"} {
		params := map[string]any{"uri": "ui://simple-host/my-sites", "_meta": map[string]any{"io.modelcontextprotocol/clientCapabilities": map[string]any{}}}
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		r := httptest.NewRequest("POST", "https://sites.example.test/mcp", strings.NewReader(string(body)))
		r.Header.Set("MCP-Protocol-Version", protocolVersion)
		r.Header.Set("Mcp-Method", method)
		r.Header.Set("Mcp-Name", "ui://simple-host/my-sites")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, r)
		var reply struct{ Result map[string]any }
		json.Unmarshal(rec.Body.Bytes(), &reply)
		if rec.Code != 200 || reply.Result["resultType"] != "complete" || reply.Result["cacheScope"] != "private" || reply.Result["ttlMs"] == nil {
			t.Fatalf("%s: %d %s", method, rec.Code, rec.Body)
		}
		if method == "resources/read" {
			r = httptest.NewRequest("POST", "https://sites.example.test/mcp", strings.NewReader(string(body)))
			r.Header.Set("MCP-Protocol-Version", protocolVersion)
			r.Header.Set("Mcp-Method", method)
			r.Header.Set("Mcp-Name", "ui://other")
			rec = httptest.NewRecorder()
			s.ServeHTTP(rec, r)
			if rec.Code != 400 {
				t.Fatal("resource header mismatch accepted")
			}
		}
	}
}
