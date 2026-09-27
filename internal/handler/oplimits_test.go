package handler

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/oplimits"
	"github.com/vsriram/simple-host/internal/ratelimit"
)

// The documented defaults (docs/configuration.md) are what the limiters use.
func TestRateLimitDefaults(t *testing.T) {
	want := map[string]RateLimit{
		"auth-client": {20, 5 * time.Second}, "auth-email": {5, 50 * time.Second},
		"management-client": {60, time.Second}, "management-user": {30, 10 * time.Second},
		"api-key-mint": {30, 10 * time.Second}, "state-client": {60, time.Second},
		"state-site": {60, time.Second}, "state-read-client": {120, 500 * time.Millisecond},
		"state-read-site": {300, 200 * time.Millisecond}, "admin-client": {10, 10 * time.Second},
		"admin-identity": {10, 10 * time.Second}, "search-query-peer": {200, 50 * time.Millisecond},
		"search-query-session": {60, time.Second}, "oauth-register": {30, 10 * time.Second},
		"oauth-token": {120, 500 * time.Millisecond},
	}
	got := RateLimitDefaults()
	if len(got) != len(want) {
		t.Fatalf("RateLimitDefaults has %d limits, want %d: %v", len(got), len(want), got)
	}
	for name, limit := range want {
		if got[name] != limit {
			t.Errorf("%s = %+v, want %+v", name, got[name], limit)
		}
	}
}

func TestConfigureRateLimits(t *testing.T) {
	saved := make(map[*ratelimit.Policy]ratelimit.Policy)
	for _, p := range configurablePolicies {
		saved[p] = *p
	}
	t.Cleanup(func() {
		for p, v := range saved {
			*p = v
		}
	})

	if err := ConfigureRateLimits(map[string]RateLimit{"sign-in": {1, time.Second}}); err == nil || !strings.Contains(err.Error(), "RATE_LIMIT_SIGN_IN") {
		t.Fatalf("unknown name: err = %v", err)
	}
	// auth-client is counted across replicas: 20 x 2m is a 40-minute window.
	err := ConfigureRateLimits(map[string]RateLimit{"auth-email": {9, time.Second}, "auth-client": {20, 2 * time.Minute}})
	if err == nil || !strings.Contains(err.Error(), "RATE_LIMIT_AUTH_CLIENT") {
		t.Fatalf("shared window over 30m: err = %v", err)
	}
	if authEmailPolicy.Burst != 5 {
		t.Fatal("a refused configuration was partly applied")
	}
	// A per-pod limit may have a long window.
	if err := ConfigureRateLimits(map[string]RateLimit{"auth-email": {3, time.Hour}, "oauth-token": {240, 250 * time.Millisecond}}); err != nil {
		t.Fatal(err)
	}
	if authEmailPolicy.Burst != 3 || authEmailPolicy.RefillPerSecond != 1.0/3600 || oauthTokenPolicy.Burst != 240 || oauthTokenPolicy.RefillPerSecond != 4 {
		t.Fatalf("applied = %+v, %+v", authEmailPolicy, oauthTokenPolicy)
	}
	if got := RateLimitDefaults()["auth-email"]; got != (RateLimit{3, time.Hour}) {
		t.Fatalf("auth-email now %+v", got)
	}
	if !strings.Contains(expandServedText("{{RATE_LIMIT_OAUTH_TOKEN}}"), "240 at once, then one more every 250ms") {
		t.Fatalf("placeholder = %q", expandServedText("{{RATE_LIMIT_OAUTH_TOKEN}}"))
	}
}

// The served documents (openapi.yaml, the home page, every skill file and
// the plugin bundle) state the values in force, and no placeholder of ours
// reaches a reader.
func TestServedDocumentsFollowTheSettings(t *testing.T) {
	before := oplimits.Get()
	t.Cleanup(func() { oplimits.Set(before) })
	v := oplimits.Defaults()
	v.DeletedRetentionDays = 9
	v.PreviewLinkTTL = 20 * time.Minute
	v.ExportLinkTTL = 3 * time.Minute
	v.MaxArchiveBytes = 200 << 20
	v.MaxFilesPerSite = 70_000
	v.MaxTeamMembers = 75
	v.MaxTeamsPerPerson = 4
	v.APIKeyExpiryWarningDays = 5
	v.IdleGraceDays = 11
	oplimits.Set(v)

	mux := http.NewServeMux()
	RegisterUIRoutes(mux, testSkillBaseURL)
	RegisterPluginRoute(mux, testSkillBaseURL)
	leftover := func(where, text string) {
		t.Helper()
		for _, marker := range []string{"{{DELETED_", "{{PREVIEW_", "{{EXPORT_", "{{MAX_", "{{API_KEY_", "{{IDLE_", "{{SEARCH_", "{{RATE_LIMIT_"} {
			if strings.Contains(text, marker) {
				t.Errorf("%s still carries %s", where, marker)
			}
		}
	}

	openapi := serveUIRequest(t, mux, http.MethodGet, "/openapi.yaml")
	body := openapi.Body.String()
	for _, want := range []string{"For 9 days it can be restored", "limited to 200 MiB", "at most 70,000 entries", "It lasts 20 minutes", "within 3 minutes", "at most 75.", "belongs to 4 teams", "5 days or less left", "Recently deleted 11 days later", "(200 at once, then one more every 50ms)"} {
		if !strings.Contains(body, want) {
			t.Errorf("openapi.yaml does not say %q", want)
		}
	}
	if openapi.Code != http.StatusOK || !strings.HasPrefix(openapi.Header().Get("Content-Type"), "application/yaml") {
		t.Errorf("GET /openapi.yaml = %d %q", openapi.Code, openapi.Header().Get("Content-Type"))
	}
	leftover("openapi.yaml", body)

	home := serveUIRequest(t, mux, http.MethodGet, "/")
	if !strings.Contains(home.Body.String(), "up to 200 MiB per upload") || !strings.HasPrefix(home.Header().Get("Content-Type"), "text/html") {
		t.Errorf("home page = %d %q, want the 200 MiB limit", home.Code, home.Header().Get("Content-Type"))
	}
	leftover("index.html", home.Body.String())

	for _, route := range []string{"/skills.zip", "/plugin.zip"} {
		rec := serveUIRequest(t, mux, http.MethodGet, route)
		zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
		if err != nil {
			t.Fatalf("%s: %v", route, err)
		}
		var all strings.Builder
		for _, f := range zr.File {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			text, _ := io.ReadAll(rc)
			_ = rc.Close()
			leftover(route+" "+f.Name, string(text))
			all.Write(text)
		}
		for _, want := range []string{"can be restored for 9 days", "works for 20 minutes", "at most 75 members", "Archive limits: 200 MiB"} {
			if !strings.Contains(all.String(), want) {
				t.Errorf("%s does not say %q", route, want)
			}
		}
	}
}
