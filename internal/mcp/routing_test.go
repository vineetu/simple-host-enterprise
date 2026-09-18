package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vsriram/simple-host/internal/handler"
)

// appRoutes registers the real management routes onto a bare mux.
//
// Registration only records patterns, so the handlers' dependencies are never
// dereferenced and no database is needed. That is what makes this test able to
// assert against the actual routing table rather than a copy of it.
func appRoutes(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	passthrough := func(next http.Handler) http.Handler { return next }
	handler.NewUserHandler(nil, nil).Register(mux, passthrough, passthrough)
	handler.NewSiteHandler(nil, nil, nil, "", handler.HostModel{}, nil).Register(mux, passthrough, passthrough)
	handler.NewTeamHandler(nil, nil, nil).Register(mux, passthrough, passthrough, handler.HostModel{}, "")
	return mux
}

// fixture is one tool and the argument sets to resolve it with. A tool with
// two routes — owner-scoped and owner-qualified — needs both, because a call
// only ever takes one of them and the other would break unnoticed.
type fixture struct {
	tool string
	args []map[string]any
}

// fixtures covers the whole surface: the test below fails on a tool with no
// fixture and on a fixture naming no tool, so a tool added later cannot ship
// without one.
var fixtures = []fixture{
	{"get_account", []map[string]any{{}}},
	{"list_sites", []map[string]any{{}}},
	{"get_site", []map[string]any{{"owner": "alice", "site": "demo"}}},
	{"deploy_site", []map[string]any{
		// Four routes, because intent and owner each pick one and a tool that
		// resolved three of them would look healthy.
		{"site": "demo", "files": []any{map[string]any{"path": "index.html", "content": "<h1>hi</h1>"}}},
		{"site": "demo", "intent": "create", "files": []any{map[string]any{"path": "index.html", "content": "<h1>hi</h1>"}}},
		{"site": "demo", "intent": "update", "files": []any{map[string]any{"path": "index.html", "content": "<h1>hi</h1>"}}},
		{"site": "demo", "owner": "alice", "intent": "create", "files": []any{map[string]any{"path": "index.html", "content": "<h1>hi</h1>"}}},
		{"site": "demo", "owner": "alice", "intent": "update", "files": []any{map[string]any{"path": "index.html", "content": "<h1>hi</h1>"}}},
	}},
	{"list_site_versions", []map[string]any{
		{"site": "demo"},
		{"site": "demo", "owner": "alice"},
	}},
	{"rollback_site", []map[string]any{
		{"site": "demo", "version": float64(2)},
		{"site": "demo", "version": float64(2), "owner": "alice"},
	}},
	{"set_site_listing", []map[string]any{
		{"site": "demo", "listed": true},
		{"site": "demo", "listed": true, "owner": "alice"},
	}},
	{"list_site_editors", []map[string]any{{"owner": "alice", "site": "demo"}}},
	{"find_users", []map[string]any{{"owner": "alice", "site": "demo", "query": "bob"}}},
	{"grant_site_editor", []map[string]any{{"owner": "alice", "site": "demo", "usernames": []any{"bob"}}}},
	{"revoke_site_editor", []map[string]any{{"owner": "alice", "site": "demo", "username": "bob"}}},
	{"delete_site", []map[string]any{
		{"site": "demo"},
		{"site": "demo", "owner": "alice"},
	}},
	{"create_team", []map[string]any{{"name": "acme-team"}}},
	{"list_teams", []map[string]any{{}}},
	{"list_team_members", []map[string]any{{"team": "acme-team"}}},
	{"find_team_members", []map[string]any{{"team": "acme-team", "query": "bob"}}},
	{"add_team_member", []map[string]any{{"team": "acme-team", "usernames": []any{"bob"}}}},
	{"remove_team_member", []map[string]any{{"team": "acme-team", "username": "bob"}}},
	{"delete_team", []map[string]any{{"team": "acme-team"}}},
}

// Every tool must resolve to a route that exists. This is the test that fails
// when the REST surface moves and the MCP tools are not moved with it — the
// adapter builds URLs as strings, so nothing else would notice.
func TestEveryToolResolvesToARealRoute(t *testing.T) {
	mux := appRoutes(t)

	for _, tc := range fixtures {
		tool, known := byName(tc.tool)
		if !known {
			t.Errorf("fixture names %q, which is not a registered tool", tc.tool)
			continue
		}
		for _, args := range tc.args {
			up, err := tool.call(args)
			if err != nil {
				t.Errorf("%s: valid arguments %v were rejected: %v", tc.tool, args, err)
				continue
			}
			for _, route := range routesOf(up) {
				req := httptest.NewRequest(route.Method, route.Path, nil)
				if _, pattern := mux.Handler(req); pattern == "" {
					t.Errorf("%s resolves to %s %s, which matches no registered route — "+
						"the REST surface moved and this tool was not updated",
						tc.tool, route.Method, route.Path)
				}
			}
		}
	}
}

// A tool with no fixture is a tool nothing proves resolves, which is the state
// every new tool starts in. Requiring the pairing in both directions is what
// makes the test above cover the surface rather than whatever was remembered.
func TestEveryToolHasExactlyOneFixture(t *testing.T) {
	covered := map[string]int{}
	for _, tc := range fixtures {
		covered[tc.tool]++
		if len(tc.args) == 0 {
			t.Errorf("fixture for %q has no argument sets", tc.tool)
		}
	}
	for _, tool := range Tools() {
		switch covered[tool.Name] {
		case 1:
		case 0:
			t.Errorf("tool %q has no routing fixture — add one to fixtures, "+
				"or nothing proves it resolves to a route that exists", tool.Name)
		default:
			t.Errorf("tool %q has %d fixtures; put every argument set in one entry",
				tool.Name, covered[tool.Name])
		}
		delete(covered, tool.Name)
	}
	for name := range covered {
		t.Errorf("fixture for %q names no tool — it was renamed or removed and the fixture was left behind", name)
	}
}

// Names are part of the contract a model learns. The spec constrains the
// character set, and mixed naming styles make a tool list harder to read.
func TestToolNamesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range Tools() {
		if seen[tool.Name] {
			t.Errorf("duplicate tool name %q", tool.Name)
		}
		seen[tool.Name] = true

		if len(tool.Name) == 0 || len(tool.Name) > 128 {
			t.Errorf("tool name %q must be 1-128 characters", tool.Name)
		}
		for _, r := range tool.Name {
			valid := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.'
			if !valid {
				t.Errorf("tool name %q contains %q, outside the allowed set", tool.Name, r)
			}
		}
		if strings.ToLower(tool.Name) != tool.Name {
			t.Errorf("tool name %q should be lower case for consistency", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("tool %q has no description; a model has nothing to go on", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no inputSchema; the spec requires a schema object", tool.Name)
		}
		// Unset means explainFailure would steer a failure by status alone,
		// which is how a team error comes back saying "call list_sites".
		switch tool.family {
		case familyAccount, familySite, familyTeam:
		default:
			t.Errorf("tool %q has no family; its failures cannot be explained in the right terms", tool.Name)
		}
	}
}

// The list a model sees must be stable, or clients cannot cache it and prompt
// caches miss on every call.
func TestToolOrderIsDeterministic(t *testing.T) {
	first, second := Tools(), Tools()
	if len(first) != len(second) {
		t.Fatalf("tool count differs between calls: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Name != second[i].Name {
			t.Errorf("tool %d differs between calls: %q vs %q", i, first[i].Name, second[i].Name)
		}
	}
}

// routesOf includes a tool's fallback: create-or-update depends on both
// routes existing, and a broken fallback would only show up on a first deploy.
func routesOf(up upstream) []upstream {
	routes := []upstream{up}
	if up.Fallback != nil {
		routes = append(routes, *up.Fallback)
	}
	return routes
}

func byName(name string) (Tool, bool) {
	for _, tool := range Tools() {
		if tool.Name == name {
			return tool, true
		}
	}
	return Tool{}, false
}

// deploy_site is the tool where picking the wrong request is not a 404 but a
// second site in a namespace nobody meant, so the mapping from intent to
// request is asserted directly rather than inferred from the routing table.
func TestDeploySiteIntentPicksTheRequest(t *testing.T) {
	tool, known := byName("deploy_site")
	if !known {
		t.Fatal("deploy_site is not registered")
	}
	files := []any{map[string]any{"path": "index.html", "content": "<h1>hi</h1>"}}

	for _, tc := range []struct {
		name     string
		args     map[string]any
		method   string
		path     string
		ifMatch  string
		fallback bool
	}{
		{
			name:   "create in a team goes to the namespace route",
			args:   map[string]any{"site": "demo", "owner": "acme-team", "intent": "create", "files": files},
			method: http.MethodPost,
			path:   "/api/collaboration/sites/acme-team/demo",
		},
		{
			name:    "update in a team carries the etag",
			args:    map[string]any{"site": "demo", "owner": "acme-team", "intent": "update", "etag": `"v1"`, "files": files},
			method:  http.MethodPut,
			path:    "/api/collaboration/sites/acme-team/demo",
			ifMatch: `"v1"`,
		},
		{
			name:   "create with no owner stays in the caller's account",
			args:   map[string]any{"site": "demo", "intent": "create", "files": files},
			method: http.MethodPost,
			path:   "/api/sites/demo",
		},
		{
			name:     "neither argument is the pre-0.9 client, which still falls back",
			args:     map[string]any{"site": "demo", "files": files},
			method:   http.MethodPost,
			path:     "/api/sites/demo",
			fallback: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, err := tool.call(tc.args)
			if err != nil {
				t.Fatalf("valid arguments were rejected: %v", err)
			}
			if up.Method != tc.method || up.Path != tc.path {
				t.Errorf("resolved to %s %s, want %s %s", up.Method, up.Path, tc.method, tc.path)
			}
			if up.IfMatch != tc.ifMatch {
				t.Errorf("If-Match is %q, want %q", up.IfMatch, tc.ifMatch)
			}
			// A named intent must not be convertible into the other one.
			if (up.Fallback != nil) != tc.fallback {
				t.Errorf("fallback present = %v, want %v", up.Fallback != nil, tc.fallback)
			}
		})
	}

	// An owner with no intent is the call that used to guess. It is refused
	// where the model can still fix it, rather than resolved by a status.
	if _, err := tool.call(map[string]any{"site": "demo", "owner": "acme-team", "files": files}); err == nil {
		t.Error("owner without intent was accepted; it must be refused before it reaches the API")
	}
	if _, err := tool.call(map[string]any{"site": "demo", "intent": "publish", "files": files}); err == nil {
		t.Error("an intent outside create/update was accepted")
	}
}
