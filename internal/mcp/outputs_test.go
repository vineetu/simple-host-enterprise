package mcp

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestEveryToolDeclaresAWellFormedOutputSchema(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range Tools() {
		names[tool.Name] = true
		if tool.OutputSchema == nil {
			t.Errorf("%s has no outputSchema", tool.Name)
			continue
		}
		if err := CheckOutputSchema(tool.OutputSchema); err != nil {
			t.Errorf("%s outputSchema: %v", tool.Name, err)
		}
	}
	for name := range outputSchemas() {
		if !names[name] {
			t.Errorf("outputSchemas has %q, which is not a tool", name)
		}
	}
}

// The checker must actually refuse bad values and bad schemas, or every test
// built on it passes vacuously.
func TestSchemaCheckerRefusesWhatItShould(t *testing.T) {
	schema := outObject(map[string]any{
		"name":  outString("n"),
		"count": outInteger("c"),
		"kind":  outEnum("k", "a", "b"),
		"list":  outArray("l", outObject(map[string]any{"x": outBool("x")}, "x")),
		"free":  anyJSON("f"),
	}, "name", "count")
	good := map[string]any{"name": "n", "count": 2, "kind": "a", "list": []any{map[string]any{"x": true}}, "free": []any{1, "two"}}
	if err := ValidateOutput(schema, good, nil); err != nil {
		t.Fatalf("valid value refused: %v", err)
	}
	for label, bad := range map[string]map[string]any{
		"missing required": {"name": "n"},
		"extra property":   {"name": "n", "count": 1, "other": 1},
		"wrong type":       {"name": 3, "count": 1},
		"fraction":         {"name": "n", "count": 1.5},
		"not in enum":      {"name": "n", "count": 1, "kind": "c"},
		"bad array item":   {"name": "n", "count": 1, "list": []any{map[string]any{}}},
	} {
		if err := ValidateOutput(schema, bad, nil); err == nil {
			t.Errorf("%s: accepted %v", label, bad)
		}
	}
	for label, bad := range map[string]any{
		"root not object":     map[string]any{"type": "array", "items": map[string]any{}},
		"unknown keyword":     outObject(map[string]any{"a": map[string]any{"type": "string", "format": "uri"}}),
		"required undeclared": outObject(map[string]any{"a": outString("a")}, "b"),
		"array without items": outObject(map[string]any{"a": map[string]any{"type": "array"}}),
	} {
		if err := CheckOutputSchema(bad); err == nil {
			t.Errorf("%s: schema accepted", label)
		}
	}
}

// Every tool states all four hints; an absent destructiveHint means true.
func TestEveryToolStatesAllFourHints(t *testing.T) {
	for _, tool := range Tools() {
		for _, hint := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
			if _, ok := tool.Annotations[hint].(bool); !ok {
				t.Errorf("%s does not state %s", tool.Name, hint)
			}
		}
	}
	want := map[string][2]bool{ // destructive, idempotent
		"rollback_site": {false, true},
		"deploy_site":   {true, false},
		"delete_site":   {true, true},
		"update_state":  {true, false},
	}
	for _, tool := range Tools() {
		if w, ok := want[tool.Name]; ok {
			if tool.Annotations["destructiveHint"] != w[0] || tool.Annotations["idempotentHint"] != w[1] {
				t.Errorf("%s hints = %v, want destructive %v idempotent %v", tool.Name, tool.Annotations, w[0], w[1])
			}
		}
	}
}

// countingUpstream answers every request with one fixed response and counts
// how many it saw, so a test can prove a refused call never reached it.
type countingUpstream struct {
	status int
	body   []byte
	calls  int
	last   *http.Request
}

func (u *countingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.calls++
	u.last = r
	w.WriteHeader(u.status)
	_, _ = w.Write(u.body)
}

func callTool(t *testing.T, s *Server, name string, args map[string]any) map[string]any {
	t.Helper()
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + strings.TrimSuffix(string(params), "}") + `,` + meta + `}}`
	return decode(t, post(t, s, body, modern("tools/call", name)))["result"].(map[string]any)
}

func resultText(res map[string]any) string {
	return res["content"].([]any)[0].(map[string]any)["text"].(string)
}

func TestUnknownArgumentsAreRefused(t *testing.T) {
	up := &countingUpstream{status: 200, body: []byte(`{}`)}
	s := NewServer(up, "simple-host", "0.10.0")
	for _, c := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"get_account", map[string]any{"verbose": true}, "verbose"},
		{"get_site", map[string]any{"site": "d", "owner": "a", "sitename": "d"}, "sitename"},
		{"deploy_site", map[string]any{"site": "d", "files": []any{map[string]any{"path": "index.html", "content": "x", "mode": "755"}}}, "files[0].mode"},
	} {
		res := callTool(t, s, c.tool, c.args)
		if res["isError"] != true || !strings.Contains(resultText(res), c.want) {
			t.Errorf("%s %v = %v, want a refusal naming %q", c.tool, c.args, res, c.want)
		}
	}
	if up.calls != 0 {
		t.Errorf("a refused call reached the router %d times", up.calls)
	}
}

func TestErrorResultsCarryNoStructuredContent(t *testing.T) {
	s := NewServer(&countingUpstream{status: 404, body: []byte(`{"error":"site not found","code":"not_found"}`)}, "simple-host", "0.10.0")
	res := callTool(t, s, "get_site", map[string]any{"site": "d", "owner": "a"})
	if res["isError"] != true {
		t.Fatalf("isError = %v", res["isError"])
	}
	if _, present := res["structuredContent"]; present {
		t.Errorf("an error result carried structuredContent: %v", res)
	}
}

func TestBodylessSuccessIsDone(t *testing.T) {
	s := NewServer(&countingUpstream{status: 204}, "simple-host", "0.10.0")
	res := callTool(t, s, "revoke_site_viewer", map[string]any{"site": "d", "owner": "a", "username": "bob"})
	structured, _ := res["structuredContent"].(map[string]any)
	if structured["done"] != true {
		t.Errorf("structuredContent = %v, want {done: true}", res["structuredContent"])
	}
}

func TestDeleteSiteNeedsTheNameTwice(t *testing.T) {
	up := &countingUpstream{status: 204}
	s := NewServer(up, "simple-host", "0.10.0")
	for _, args := range []map[string]any{
		{"site": "demo", "owner": "a"},
		{"site": "demo", "owner": "a", "confirm_name": "Demo"},
		{"site": "demo", "owner": "a", "confirm_name": ""},
	} {
		if res := callTool(t, s, "delete_site", args); res["isError"] != true {
			t.Errorf("delete_site %v was not refused", args)
		}
	}
	if up.calls != 0 {
		t.Fatalf("an unconfirmed delete reached the router")
	}
	if res := callTool(t, s, "delete_site", map[string]any{"site": "demo", "owner": "a", "confirm_name": "demo"}); res["isError"] != false || up.calls != 1 {
		t.Errorf("confirmed delete = %v after %d calls", res, up.calls)
	}
}

func zipOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	_, _ = zw.Create("css/")
	for name, body := range files {
		w, _ := zw.Create(name)
		_, _ = w.Write(body)
	}
	_ = zw.Close()
	return buf.Bytes()
}

func TestSiteFilesAreListedAndRead(t *testing.T) {
	up := &countingUpstream{status: 200, body: zipOf(t, map[string][]byte{
		"index.html":   []byte("<p>hi</p>"),
		"css/site.css": []byte("p{}"),
		"big.txt":      bytes.Repeat([]byte("a"), maxFileReadBytes+1),
	})}
	s := NewServer(up, "simple-host", "0.10.0")
	listed := callTool(t, s, "list_site_files", map[string]any{"site": "d", "owner": "a", "version": 3})
	if up.last.URL.Path != "/api/collaboration/sites/a/d/versions/3/archive" {
		t.Errorf("archive route = %s", up.last.URL.Path)
	}
	if structured, _ := listed["structuredContent"].(map[string]any); structured["count"] != float64(3) {
		t.Errorf("list_site_files = %v, want three files and no directory", listed)
	}
	read := callTool(t, s, "read_site_file", map[string]any{"site": "d", "owner": "a", "version": 3, "path": "./css/site.css"})
	if structured, _ := read["structuredContent"].(map[string]any); structured["content"] != "p{}" {
		t.Errorf("read_site_file = %v", read)
	}
	for path, want := range map[string]string{"big.txt": "over the 1 MiB", "missing.html": "list_site_files", "../x": "escapes"} {
		res := callTool(t, s, "read_site_file", map[string]any{"site": "d", "owner": "a", "version": 3, "path": path})
		if res["isError"] != true || !strings.Contains(resultText(res), want) {
			t.Errorf("read_site_file %s = %v, want a refusal mentioning %q", path, resultText(res), want)
		}
	}
}

func TestOversizedArchiveIsRefusedNotTruncated(t *testing.T) {
	c := &capture{header: http.Header{}, limit: 10}
	if _, err := c.Write(make([]byte, 11)); err == nil || !c.overflow {
		t.Fatal("a write past the limit was accepted")
	}
}

// The state tools are served on the owner's own host, through the handler
// WithSiteAPI supplies; without one they say so rather than reach the router.
func TestStateToolsUseTheOwnersHost(t *testing.T) {
	router := &countingUpstream{status: 200, body: []byte(`{}`)}
	s := NewServer(router, "simple-host", "0.10.0")
	if res := callTool(t, s, "get_state", map[string]any{"site": "d", "owner": "alice"}); res["isError"] != true {
		t.Errorf("get_state with no site API = %v", res)
	}
	siteAPI := &countingUpstream{status: 200, body: []byte(`{"version":4,"state":{"a":1}}`)}
	s.WithSiteAPI(siteAPI, func(_ context.Context, owner, _ string) (string, error) { return owner + ".hosting.test", nil })
	res := callTool(t, s, "update_state", map[string]any{"site": "d", "owner": "alice", "version": 3, "state": map[string]any{"a": 1}})
	if res["isError"] != false || siteAPI.last.Host != "alice.hosting.test" || siteAPI.last.URL.Path != "/api/sites/d/state/versioned" || siteAPI.last.Method != "PUT" {
		t.Errorf("update_state went to %s %s%s: %v", siteAPI.last.Method, siteAPI.last.Host, siteAPI.last.URL.Path, res)
	}
	if router.calls != 0 {
		t.Error("a state call reached the bare router")
	}
	for _, bad := range []any{-1, 1.5, "3"} {
		if res := callTool(t, s, "update_state", map[string]any{"site": "d", "owner": "alice", "version": bad, "state": 1}); res["isError"] != true {
			t.Errorf("update_state accepted version %v", bad)
		}
	}
}
