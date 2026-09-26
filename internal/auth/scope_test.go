package auth

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/db"
)

// registeredPatterns finds every literal "METHOD /pattern" passed to
// mux.Handle/HandleFunc in the server's Go source — the same rule
// scripts/check-features.sh applies.
func registeredPatterns(t *testing.T) map[string]string {
	t.Helper()
	re := regexp.MustCompile(`Handle(?:Func)?\("([A-Z]+ /[^"]*)"`)
	found := map[string]string{}
	for _, root := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range re.FindAllStringSubmatch(string(src), -1) {
				found[m[1]] = path
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(found) < 50 {
		t.Fatalf("found only %d routes; the pattern needs updating", len(found))
	}
	return found
}

// A route nobody has classified is refused to every API key, which would
// break a full key silently; this makes adding one to routeKeyAccess part of
// adding the route, and so a decision about whether a publish key gets it.
func TestEveryRouteIsClassified(t *testing.T) {
	routes := registeredPatterns(t)
	for pattern, file := range routes {
		if _, ok := routeKeyAccess[pattern]; !ok {
			t.Errorf("%s (%s) is not in routeKeyAccess (internal/auth/scope.go)", pattern, file)
		}
	}
	for pattern := range routeKeyAccess {
		if _, ok := routes[pattern]; !ok && pattern != SiteAPIPattern {
			t.Errorf("routeKeyAccess lists %q, which no longer exists", pattern)
		}
	}
}

func TestKeyScopeAllows(t *testing.T) {
	for _, c := range []struct {
		scope, pattern string
		want           bool
	}{
		{db.APIKeyScopePublish, "POST /api/sites/{sitename}", true},
		{db.APIKeyScopePublish, SiteAPIPattern, true},
		{db.APIKeyScopePublish, "POST /mcp", true},
		{db.APIKeyScopePublish, "DELETE /api/sites/{sitename}", false},
		{db.APIKeyScopePublish, "POST /api/sites/{sitename}/access", false},
		{db.APIKeyScopePublish, "GET /api/admin/export", false},
		{db.APIKeyScopePublish, "POST /api/admin/users/disable", false},
		{db.APIKeyScopeFull, "DELETE /api/sites/{sitename}", true},
		{db.APIKeyScopeFull, "GET /api/me", true},
		{db.APIKeyScopeFull, "GET /api/admin/export", false},
		{db.APIKeyScopeFull, "POST /api/admin/users/disable", false},
		{db.APIKeyScopeOffboard, "POST /api/admin/users/disable", true},
		{db.APIKeyScopeOffboard, "GET /api/me", false},
		{db.APIKeyScopeOffboard, "POST /api/admin/users/{username}/disable", false},
		{db.APIKeyScopeFull, "", false},
		{db.APIKeyScopeFull, "GET /not-a-route", false},
		{"", "GET /api/me", false},
	} {
		if got := KeyScopeAllows(c.scope, c.pattern); got != c.want {
			t.Errorf("KeyScopeAllows(%q, %q) = %v, want %v", c.scope, c.pattern, got, c.want)
		}
	}
}

// An OAuth access token is refused off /mcp before any lookup: the nil
// database would panic if Middleware tried one.
func TestBearerRefusedWithoutMCPMark(t *testing.T) {
	reached := false
	handler := Middleware(nil, nil, time.Hour)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	r := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	r.Header.Set("Authorization", "Bearer shat_anything")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	if reached || rec.Code != http.StatusUnauthorized {
		t.Fatalf("bearer off /mcp: status %d, reached %v; want 401", rec.Code, reached)
	}
}
