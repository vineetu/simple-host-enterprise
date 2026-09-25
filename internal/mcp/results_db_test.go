package mcp

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/handler"
	"github.com/vsriram/simple-host/internal/migrate"
	"github.com/vsriram/simple-host/internal/storage"
)

// resultsTestDB is a fresh, fully migrated database on the server
// MIGRATE_TEST_DSN names (the same harness internal/db uses). Plain `go test`
// skips; `make test-db`-style runs set the variable.
func resultsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MIGRATE_TEST_DSN")
	if dsn == "" {
		t.Skip("MIGRATE_TEST_DSN not set")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse MIGRATE_TEST_DSN: %v", err)
	}
	adminDSN := *parsed
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("postgres", adminDSN.String())
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	name := "mcp_results_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(`CREATE DATABASE ` + pq.QuoteIdentifier(name)); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("postgres", adminDSN.String())
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(`DROP DATABASE IF EXISTS ` + pq.QuoteIdentifier(name) + ` WITH (FORCE)`)
	})
	testDSN := *parsed
	testDSN.Path = "/" + name
	database, err := sql.Open("postgres", testDSN.String())
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := migrate.Apply(context.Background(), database, 0, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return database
}

// realServer wires the MCP server over the real management routes and the
// real host gate, the way cmd/server/main.go does.
func realServer(t *testing.T, database *sql.DB) *Server {
	t.Helper()
	const base = "https://hosting.corp.test"
	disk, err := storage.NewDiskStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := handler.NewHostModel(base)
	if err != nil {
		t.Fatal(err)
	}
	version, err := handler.PluginVersion()
	if err != nil {
		t.Fatal(err)
	}
	skillMW, err := handler.SkillVersionMiddleware(version, handler.MinimumSupportedSkillVersion)
	if err != nil {
		t.Fatal(err)
	}
	keys := []auth.SigningKey{{ID: "k1", Key: []byte(strings.Repeat("s", 32))}}
	authMW := auth.Middleware(database, keys, time.Hour)
	limits := handler.NewAbuseLimits()
	recorder := audit.NewDBRecorder(database)

	mux := http.NewServeMux()
	handler.NewUserHandler(database, limits).Register(mux, authMW, skillMW)
	handler.NewSiteHandler(database, disk, nil, base, hosts, limits).WithAudit(recorder).Register(mux, authMW, skillMW)
	handler.NewTeamHandler(database, disk, limits).WithAudit(recorder).Register(mux, authMW, skillMW, hosts, base)
	files := handler.NewSiteFiles(disk, database, handler.CookiePolicy{Secure: true}, keys, time.Hour)
	siteAPI := handler.NewSiteAPIHandler(database, disk, storage.AssetLimits{MaxFileBytes: 1 << 20, MaxSiteBytes: 8 << 20, MaxSiteCount: 100}, recorder, hosts, limits)
	handoff := handler.NewHandoffHandler(database, keys, hosts, recorder, limits)
	negCache := auth.NewNegativeSessionCache(database, time.Hour)
	gate := handler.NewHostGate(hosts, files, database, keys, negCache, handoff, siteAPI, authMW, base)
	return NewServer(mux, "simple-host", version).WithSiteAPI(gate(mux), hosts.SiteHost)
}

func createPerson(t *testing.T, database *sql.DB, username string) string {
	t.Helper()
	ctx := context.Background()
	user, err := db.CreateOIDCUser(ctx, database, username, "sub-"+username, username+"@example.com", false)
	if err != nil {
		t.Fatalf("create %s: %v", username, err)
	}
	key := "key-" + username + "-" + strings.Repeat("0", 40)
	if _, err := db.CreateAPIKey(ctx, database, user.ID, "test", db.HashAPIKey(key), key[:8]); err != nil {
		t.Fatalf("create key for %s: %v", username, err)
	}
	return key
}

// TestOutputSchemasMatchRealResults drives every tool, through each route that
// shapes its output, against the real application and a real database, and
// validates every structuredContent against the tool's outputSchema. It also
// requires every declared property to come back from at least one call, so a
// schema cannot describe a field the tool never sends.
func TestOutputSchemasMatchRealResults(t *testing.T) {
	database := resultsTestDB(t)
	s := realServer(t, database)
	aliceKey := createPerson(t, database, "alice")
	createPerson(t, database, "bob")

	byName := map[string]Tool{}
	for _, tool := range Tools() {
		byName[tool.Name] = tool
	}
	seen := map[string]map[string]bool{}
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
			strings.TrimSuffix(string(params), "}") + `,` + meta + `}}`
		headers := modern("tools/call", name)
		headers["X-API-Key"] = aliceKey
		res, ok := decode(t, post(t, s, body, headers))["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no result", name)
		}
		text := res["content"].([]any)[0].(map[string]any)["text"].(string)
		structured, _ := res["structuredContent"].(map[string]any)
		if res["isError"] != false || structured == nil {
			t.Fatalf("%s %v failed: %s", name, args, text)
		}
		if seen[name] == nil {
			seen[name] = map[string]bool{}
		}
		if err := ValidateOutput(byName[name].OutputSchema, structured, seen[name]); err != nil {
			t.Errorf("%s %v: %v\nstructuredContent: %s", name, args, err, text)
		}
		return structured
	}

	index := map[string]any{"path": "index.html", "content": "<h1>hi</h1>"}
	logo := map[string]any{"path": "logo.png", "content": base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', 0, 1, 0xff}), "encoding": "base64"}

	call("get_account", map[string]any{})
	call("create_team", map[string]any{"name": "acme-team"})
	call("list_teams", map[string]any{})
	call("get_account", map[string]any{}) // now with a team
	call("find_team_members", map[string]any{"team": "acme-team", "query": "bo"})
	call("add_team_member", map[string]any{"team": "acme-team", "usernames": []any{"bob"}})
	call("list_team_members", map[string]any{"team": "acme-team"})

	call("deploy_site", map[string]any{"site": "demo", "files": []any{index, logo}})
	call("deploy_site", map[string]any{"site": "other", "owner": "alice", "intent": "create", "files": []any{index}})
	etag := call("get_site", map[string]any{"site": "demo", "owner": "alice"})["etag"].(string)
	call("deploy_site", map[string]any{"site": "demo", "owner": "alice", "intent": "update", "etag": etag, "files": []any{index, logo}})
	call("list_site_versions", map[string]any{"site": "demo"})
	call("list_site_versions", map[string]any{"site": "demo", "owner": "alice"})
	call("rollback_site", map[string]any{"site": "demo", "version": 1})
	etag = call("get_site", map[string]any{"site": "demo", "owner": "alice"})["etag"].(string)
	call("rollback_site", map[string]any{"site": "demo", "owner": "alice", "version": 2, "etag": etag})
	call("set_site_listing", map[string]any{"site": "demo", "listed": true})
	call("set_site_listing", map[string]any{"site": "demo", "owner": "alice", "listed": false})

	call("find_users", map[string]any{"site": "demo", "owner": "alice", "query": "bo"})
	call("find_users", map[string]any{"site": "demo", "owner": "alice", "query": "a", "for": "viewer"})
	call("grant_site_editor", map[string]any{"site": "demo", "owner": "alice", "usernames": []any{"bob"}})
	call("list_site_editors", map[string]any{"site": "demo", "owner": "alice"})
	call("revoke_site_editor", map[string]any{"site": "demo", "owner": "alice", "username": "bob"})
	call("grant_site_viewer", map[string]any{"site": "demo", "owner": "alice", "usernames": []any{"acme-team"}})
	call("list_site_viewers", map[string]any{"site": "demo", "owner": "alice"})

	// State and files, on a restricted site: the owner host still answers.
	active := int(call("get_site", map[string]any{"site": "demo", "owner": "alice"})["active_version"].(float64))
	listed := call("list_site_files", map[string]any{"site": "demo", "owner": "alice", "version": active})
	if listed["count"] != float64(2) {
		t.Errorf("list_site_files = %v, want index.html and logo.png", listed)
	}
	if got := call("read_site_file", map[string]any{"site": "demo", "owner": "alice", "version": active, "path": "index.html"}); got["content"] != "<h1>hi</h1>" || got["encoding"] != "text" {
		t.Errorf("read_site_file index.html = %v", got)
	}
	if got := call("read_site_file", map[string]any{"site": "demo", "owner": "alice", "version": active, "path": "logo.png"}); got["encoding"] != "base64" || got["content"] != logo["content"] {
		t.Errorf("read_site_file logo.png = %v", got)
	}
	if got := call("get_state", map[string]any{"site": "demo", "owner": "alice"}); got["version"] != float64(0) {
		t.Errorf("fresh state = %v, want version 0", got)
	}
	if got := call("update_state", map[string]any{"site": "demo", "owner": "alice", "version": 0, "state": map[string]any{"rsvps": []any{"Ann"}}}); got["version"] != float64(1) {
		t.Errorf("update_state = %v, want version 1", got)
	}
	if got := call("get_state", map[string]any{"site": "demo", "owner": "alice"}); got["version"] != float64(1) {
		t.Errorf("state after update = %v", got)
	}
	// A stale save is refused with the current state and no structuredContent.
	params, _ := json.Marshal(map[string]any{"name": "update_state", "arguments": map[string]any{"site": "demo", "owner": "alice", "version": 0, "state": map[string]any{}}})
	headers := modern("tools/call", "update_state")
	headers["X-API-Key"] = aliceKey
	stale := decode(t, post(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+strings.TrimSuffix(string(params), "}")+`,`+meta+`}}`, headers))["result"].(map[string]any)
	if staleText := stale["content"].([]any)[0].(map[string]any)["text"].(string); stale["isError"] != true || stale["structuredContent"] != nil || !strings.Contains(staleText, "Ann") || !strings.Contains(staleText, "reapply") {
		t.Errorf("stale update_state = %v", stale)
	}
	call("revoke_site_viewer", map[string]any{"site": "demo", "owner": "alice", "username": "acme-team"})

	call("list_sites", map[string]any{})
	call("delete_site", map[string]any{"site": "other", "owner": "alice", "confirm_name": "other"})
	call("delete_site", map[string]any{"site": "demo", "confirm_name": "demo"})
	call("remove_team_member", map[string]any{"team": "acme-team", "username": "bob"})
	call("delete_team", map[string]any{"team": "acme-team"})

	// Properties that a single-account fixture cannot produce: download
	// counts need a real browser download, and admin/team kinds are never the
	// signed-in account's.
	exempt := map[string]bool{
		"$.analytics.file_downloads":         true,
		"$.items[].analytics.file_downloads": true,
	}
	for name, tool := range byName {
		if seen[name] == nil {
			t.Errorf("%s was never called", name)
			continue
		}
		var unseen []string
		for _, path := range SchemaPropertyPaths(tool.OutputSchema) {
			if !seen[name][path] && !exempt[path] {
				unseen = append(unseen, path)
			}
		}
		sort.Strings(unseen)
		if len(unseen) > 0 {
			t.Errorf("%s declares properties no call returned: %v", name, unseen)
		}
	}
}
