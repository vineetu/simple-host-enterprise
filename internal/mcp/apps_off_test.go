package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// Snapshot captured from pre-port commit 4671623; list_site_files updated
// for the metadata route and optional retry hashes (2026-10-24).
// It covers discovery, tools/list, and every existing routing fixture result.
func TestAppsOffByteIdentical(t *testing.T) {
	s := NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(`{"ok":true}`))
	}), "simple-host", "test").WithApps("https://sites.example.test", map[string]string{"MCP_APP_PHOTO_PICKER": "off", "MCP_APP_MY_SITES": "off", "MCP_APP_DASHBOARD": "off", "MCP_APP_SITE_PREVIEW": "off"})
	snapshots := map[string]json.RawMessage{}
	r := httptest.NewRequest("POST", "https://sites.example.test/mcp", nil)
	for _, method := range []string{"tools/list", "initialize", "server/discover"} {
		resp, _ := s.dispatch(r, request{ID: json.RawMessage("1"), Method: method})
		b, _ := json.Marshal(resp)
		snapshots[method] = b
	}
	for _, f := range fixtures {
		if f.tool == "add_photos" {
			continue
		}
		for i, args := range f.args {
			params, _ := json.Marshal(map[string]any{"name": f.tool, "arguments": args})
			resp := s.callTool(r, request{ID: json.RawMessage("1"), Params: params})
			b, _ := json.Marshal(resp)
			key, _ := json.Marshal([]any{f.tool, i})
			snapshots[string(key)] = b
		}
	}
	var expected map[string]json.RawMessage
	b, err := os.ReadFile("testdata/apps-off.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &expected); err != nil {
		t.Fatal(err)
	}
	if len(expected) != len(snapshots) {
		t.Fatalf("snapshot count: %d != %d", len(expected), len(snapshots))
	}
	for key, got := range snapshots {
		var compact bytes.Buffer
		json.Compact(&compact, expected[key])
		if !bytes.Equal(got, compact.Bytes()) {
			t.Errorf("%s changed with Apps off", key)
		}
	}
}
