package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echoUpstream stands in for the application router so these tests exercise the
// protocol rather than the REST surface.
func echoUpstream() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"site-1-v2"`)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"path":"` + r.URL.Path + `"}`))
	})
}

func testServer() *Server { return NewServer(echoUpstream(), "simple-host", "0.9.0") }

// post sends one message with the headers a conforming client would send.
func post(t *testing.T, s *Server, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// modern builds the header set this revision requires.
func modern(method, name string) map[string]string {
	headers := map[string]string{
		"MCP-Protocol-Version": protocolVersion,
		"Mcp-Method":           method,
	}
	if name != "" {
		headers["Mcp-Name"] = name
	}
	return headers
}

const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"` + protocolVersion +
	`","io.modelcontextprotocol/clientCapabilities":{}}`

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response was not JSON: %v\nbody: %s", err, rec.Body.String())
	}
	return out
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) float64 {
	t.Helper()
	out := decode(t, rec)
	wrapper, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected a JSON-RPC error, got: %s", rec.Body.String())
	}
	code, _ := wrapper["code"].(float64)
	return code
}

// A notification expects no reply at all, whatever method it names. Answering
// one is a protocol error, and the id must not be echoed into a body.
func TestNotificationsGet202AndNoBody(t *testing.T) {
	s := testServer()
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{` + meta + `}}`,
		`{"jsonrpc":"2.0","method":"tools/list","params":{` + meta + `}}`,
	} {
		rec := post(t, s, body, modern(methodOf(t, body), ""))
		if rec.Code != http.StatusAccepted {
			t.Errorf("notification %s: status %d, want 202", body[:40], rec.Code)
		}
		if strings.TrimSpace(rec.Body.String()) != "" {
			t.Errorf("notification %s returned a body: %s", body[:40], rec.Body.String())
		}
	}
}

func methodOf(t *testing.T, body string) string {
	t.Helper()
	var req struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal([]byte(body), &req)
	return req.Method
}

// A dual-era client uses this exact code to tell a modern server from a legacy
// one. The wrong code sends it down the legacy path.
func TestUnsupportedVersionUsesTheRecognisedCode(t *testing.T) {
	s := testServer()
	rec := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1999-01-01","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		map[string]string{"MCP-Protocol-Version": "1999-01-01", "Mcp-Method": "tools/list"})

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if code := errorCode(t, rec); code != codeUnsupportedVersion {
		t.Errorf("error code = %v, want %d", code, codeUnsupportedVersion)
	}
	data, _ := decode(t, rec)["error"].(map[string]any)["data"].(map[string]any)
	if data["requested"] != "1999-01-01" {
		t.Errorf("error data does not name the requested version: %v", data)
	}
	if _, ok := data["supported"]; !ok {
		t.Error("error data does not list supported versions")
	}
}

// server/discover is a bare MUST: a client asks it before anything else.
func TestServerDiscover(t *testing.T) {
	rec := post(t, testServer(), `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+meta+`}}`,
		modern("server/discover", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res, ok := decode(t, rec)["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %s", rec.Body.String())
	}
	for _, field := range []string{"supportedVersions", "capabilities", "serverInfo", "resultType"} {
		if _, present := res[field]; !present {
			t.Errorf("discover result is missing %q", field)
		}
	}
}

// Results that clients may cache must say for how long, and must not be shared
// between callers.
func TestToolsListCarriesCachingHints(t *testing.T) {
	rec := post(t, testServer(), `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+meta+`}}`,
		modern("tools/list", ""))
	res := decode(t, rec)["result"].(map[string]any)

	if res["resultType"] != "complete" {
		t.Errorf("resultType = %v, want complete", res["resultType"])
	}
	if res["cacheScope"] != "private" {
		t.Errorf("cacheScope = %v, want private — the tool list follows the caller's authorization", res["cacheScope"])
	}
	if ttl, _ := res["ttlMs"].(float64); ttl < 0 {
		t.Errorf("ttlMs = %v, must be >= 0", ttl)
	}
}

// Headers are mirrored from the body so intermediaries can route without
// parsing it; a disagreement means two components would act on different values.
func TestHeaderBodyDisagreementIsRejected(t *testing.T) {
	s := testServer()
	for name, tc := range map[string]struct {
		body    string
		headers map[string]string
	}{
		"method mismatch": {
			`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + meta + `}}`,
			map[string]string{"MCP-Protocol-Version": protocolVersion, "Mcp-Method": "tools/call"},
		},
		"tool name mismatch": {
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sites","arguments":{},` + meta + `}}`,
			modern("tools/call", "delete_site"),
		},
		"version mismatch": {
			`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-06-18","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			map[string]string{"MCP-Protocol-Version": protocolVersion, "Mcp-Method": "tools/list"},
		},
		"missing Mcp-Method": {
			`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{` + meta + `}}`,
			map[string]string{"MCP-Protocol-Version": protocolVersion},
		},
	} {
		rec := post(t, s, tc.body, tc.headers)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
			continue
		}
		if code := errorCode(t, rec); code != codeHeaderMismatch {
			t.Errorf("%s: error code = %v, want %d", name, code, codeHeaderMismatch)
		}
	}
}

// A base64-sentinel header value must be decoded before comparison.
func TestBase64SentinelNameMatches(t *testing.T) {
	headers := modern("tools/call", "=?base64?bGlzdF9zaXRlcw==?=") // list_sites
	rec := post(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sites","arguments":{},`+meta+`}}`, headers)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// An unimplemented method is 404 with a JSON-RPC body, which is what tells a
// client this endpoint exists but the method does not.
func TestUnknownMethodIs404WithRPCBody(t *testing.T) {
	for _, method := range []string{"resources/list", "prompts/list", "made/up"} {
		rec := post(t, testServer(),
			`{"jsonrpc":"2.0","id":1,"method":"`+method+`","params":{`+meta+`}}`, modern(method, ""))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", method, rec.Code)
		}
		if code := errorCode(t, rec); code != codeMethodNotFound {
			t.Errorf("%s: error code = %v, want %d", method, code, codeMethodNotFound)
		}
	}
}

func TestNullIDIsRejected(t *testing.T) {
	rec := post(t, testServer(), `{"jsonrpc":"2.0","id":null,"method":"tools/list","params":{`+meta+`}}`,
		modern("tools/list", ""))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 — a null id is malformed, not a notification", rec.Code)
	}
}

func TestNonPostIsRejected(t *testing.T) {
	s := testServer()
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPut} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(method, "/mcp", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "POST" {
			t.Errorf("%s: Allow = %q, want POST", method, allow)
		}
	}
}

// A page the user is merely visiting must not be able to drive this endpoint.
func TestCrossOriginIsRefused(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+meta+`}}`))
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	testServer().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// Malformed input must produce a JSON-RPC error, never a panic or a 500.
func TestMalformedInputIsHandled(t *testing.T) {
	s := testServer()
	for name, body := range map[string]string{
		"empty":            ``,
		"not json":         `{{{`,
		"array":            `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`,
		"not an object":    `"hello"`,
		"missing jsonrpc":  `{"id":1,"method":"tools/list"}`,
		"missing method":   `{"jsonrpc":"2.0","id":1}`,
		"params as string": `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"nope"}`,
	} {
		rec := post(t, s, body, map[string]string{"MCP-Protocol-Version": protocolVersion, "Mcp-Method": "tools/list"})
		if rec.Code >= 500 {
			t.Errorf("%s: status = %d, a malformed request must not be a server error", name, rec.Code)
		}
		if !json.Valid(rec.Body.Bytes()) && rec.Body.Len() > 0 {
			t.Errorf("%s: response was not valid JSON: %s", name, rec.Body.String())
		}
	}
}

// A bad argument is the model's to fix, so it must come back as a readable tool
// result — not a protocol error, which ends the turn.
func TestBadArgumentsAreToolErrorsNotProtocolErrors(t *testing.T) {
	rec := post(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy_site","arguments":{"site":"demo"},`+meta+`}}`,
		modern("tools/call", "deploy_site"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with an error result", rec.Code)
	}
	res, ok := decode(t, rec)["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result, got: %s", rec.Body.String())
	}
	if res["isError"] != true {
		t.Errorf("isError = %v, want true", res["isError"])
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "files") {
		t.Errorf("error does not say which argument is missing: %q", text)
	}
}

// An unknown tool is a protocol error, because no retry by the model fixes it.
func TestUnknownToolIsAProtocolError(t *testing.T) {
	rec := post(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"no_such_tool","arguments":{},`+meta+`}}`,
		modern("tools/call", "no_such_tool"))
	if code := errorCode(t, rec); code != codeInvalidParams {
		t.Errorf("error code = %v, want %d", code, codeInvalidParams)
	}
}

// A successful call returns the parsed document as well as the text, so the
// model does not have to parse JSON out of prose.
func TestSuccessCarriesStructuredContent(t *testing.T) {
	rec := post(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_sites","arguments":{},`+meta+`}}`,
		modern("tools/call", "list_sites"))
	res := decode(t, rec)["result"].(map[string]any)
	structured, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("no structuredContent: %s", rec.Body.String())
	}
	if structured["ok"] != true {
		t.Errorf("structuredContent did not carry the upstream document: %v", structured)
	}
	if res["isError"] != false {
		t.Errorf("isError = %v, want false", res["isError"])
	}
}

// An initialize-era client is still answered rather than refused.
func TestLegacyInitializeIsAnswered(t *testing.T) {
	rec := post(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{}}}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res, ok := decode(t, rec)["result"].(map[string]any)
	if !ok || res["protocolVersion"] == nil {
		t.Fatalf("legacy handshake was not answered: %s", rec.Body.String())
	}
}

// A legacy client naming a version this server does not list must still be
// answered with one it does, not refused before the handshake can negotiate.
func TestLegacyInitializeWithOldVersionIsDowngradedNotRefused(t *testing.T) {
	rec := post(t, testServer(),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an old client should be downgraded: %s", rec.Code, rec.Body.String())
	}
	res := decode(t, rec)["result"].(map[string]any)
	if res["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v, want %s", res["protocolVersion"], protocolVersion)
	}
}

// ping was dropped from this revision but is still sent as a keepalive; a
// failure reads to the client as a dead connection.
func TestPingIsAnswered(t *testing.T) {
	rec := post(t, testServer(), `{"jsonrpc":"2.0","id":1,"method":"ping","params":{`+meta+`}}`, modern("ping", ""))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// Every one of these once produced a wrong result silently. An argument of the
// wrong type must be refused, never reinterpreted.
func TestWrongTypedArgumentsAreRefusedNotReinterpreted(t *testing.T) {
	s := testServer()
	for name, args := range map[string]string{
		// encoding 7 fell through to "text" and wrote base64 into the file.
		"numeric encoding": `{"site":"d","files":[{"path":"index.html","content":"PGgxPg==","encoding":7}]}`,
		// etag 7 disabled the concurrency guard while looking protected.
		"numeric etag":  `{"site":"d","files":[{"path":"index.html","content":"x"}],"etag":7}`,
		"array etag":    `{"site":"d","files":[{"path":"index.html","content":"x"}],"etag":[]}`,
		"numeric owner": `{"site":"d","owner":1,"files":[{"path":"index.html","content":"x"}]}`,
	} {
		rec := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy_site","arguments":`+args+`,`+meta+`}}`,
			modern("tools/call", "deploy_site"))
		res, ok := decode(t, rec)["result"].(map[string]any)
		if !ok {
			t.Errorf("%s: expected a tool result, got %s", name, rec.Body.String())
			continue
		}
		if res["isError"] != true {
			t.Errorf("%s: accepted silently — this is the class of bug that corrupts files", name)
		}
	}
	// A null argument means "not set", which the MCP spec treats the same as an
	// omitted one — so these must still succeed.
	for name, args := range map[string]string{
		"plain text":    `{"site":"d","files":[{"path":"index.html","content":"x"}]}`,
		"null encoding": `{"site":"d","files":[{"path":"index.html","content":"x","encoding":null}]}`,
		"null etag":     `{"site":"d","files":[{"path":"index.html","content":"x"}],"etag":null}`,
	} {
		rec := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy_site","arguments":`+args+`,`+meta+`}}`,
			modern("tools/call", "deploy_site"))
		if res := decode(t, rec)["result"].(map[string]any); res["isError"] == true {
			t.Errorf("%s was rejected: %v", name, res["content"])
		}
	}
}

// version 1.9 silently rolled a site back to v1 and reported success.
func TestFractionalVersionIsRefused(t *testing.T) {
	s := testServer()
	for _, version := range []string{"1.9", "0", "-3", "1e308", "true"} {
		rec := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rollback_site","arguments":{"site":"d","version":`+version+`},`+meta+`}}`,
			modern("tools/call", "rollback_site"))
		res := decode(t, rec)["result"].(map[string]any)
		if res["isError"] != true {
			t.Errorf("version %s was accepted; a wrong version silently changes what visitors see", version)
		}
	}
}

// structuredContent must be an object; several routes answer with an array.
func TestStructuredContentIsAlwaysAnObject(t *testing.T) {
	arrayUpstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"version_number":2},{"version_number":1}]`))
	})
	s := NewServer(arrayUpstream, "simple-host", "0.9.0")
	rec := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_site_versions","arguments":{"site":"d"},`+meta+`}}`,
		modern("tools/call", "list_site_versions"))

	res := decode(t, rec)["result"].(map[string]any)
	structured, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("structuredContent is not an object: %s", rec.Body.String())
	}
	items, ok := structured["items"].([]any)
	if !ok || len(items) != 2 {
		t.Errorf("array payload was not preserved: %v", structured)
	}
}

// A JSON-RPC error object must carry an id, null when it cannot be determined.
func TestErrorsAlwaysCarryAnID(t *testing.T) {
	s := testServer()
	for name, body := range map[string]string{
		"parse error":   `{{{`,
		"not an object": `42`,
		"batch":         `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`,
	} {
		rec := post(t, s, body, map[string]string{"MCP-Protocol-Version": protocolVersion, "Mcp-Method": "tools/list"})
		out := decode(t, rec)
		if _, present := out["id"]; !present {
			t.Errorf("%s: response has no id field at all: %s", name, rec.Body.String())
		}
	}
}

// A batch must be named as such, not reported as invalid JSON — the client
// needs to know to retry unbatched.
func TestBatchIsReportedAsInvalidRequest(t *testing.T) {
	rec := post(t, testServer(), `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`,
		map[string]string{"MCP-Protocol-Version": protocolVersion, "Mcp-Method": "tools/list"})
	if code := errorCode(t, rec); code != codeInvalidRequest {
		t.Errorf("error code = %v, want %d (invalid request, not parse error)", code, codeInvalidRequest)
	}
}

// Final legacy revisions current editor and CLI clients send are answered in
// their own version; older ones are not listed and get this server's.
func TestLegacyInitializeKeepsSupportedRevisions(t *testing.T) {
	for requested, want := range map[string]string{
		"2025-11-25": "2025-11-25",
		"2025-06-18": "2025-06-18",
		"2025-03-26": protocolVersion,
	} {
		rec := post(t, testServer(),
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+requested+`","capabilities":{}}}`, nil)
		res := decode(t, rec)["result"].(map[string]any)
		if res["protocolVersion"] != want {
			t.Errorf("initialize %s: protocolVersion = %v, want %s", requested, res["protocolVersion"], want)
		}
	}
}
