package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oplimits"
)

// setLimits puts v in force for one test.
func setLimits(t *testing.T, change func(*oplimits.Values)) {
	t.Helper()
	before := oplimits.Get()
	t.Cleanup(func() { oplimits.Set(before) })
	v := oplimits.Defaults()
	change(&v)
	oplimits.Set(v)
}

// bodyMailer keeps every message's body.
type bodyMailer struct {
	mu     sync.Mutex
	bodies []string
}

func (m *bodyMailer) Send(_ context.Context, _ []string, _, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bodies = append(m.bodies, body)
	return nil
}

func within(t *testing.T, what string, got time.Time, want time.Duration) {
	t.Helper()
	if d := time.Until(got); d < want-time.Minute || d > want+time.Minute {
		t.Errorf("%s is %s away, want about %s", what, d.Round(time.Second), want)
	}
}

// Every configurable limit is enforced at the configured value, and every
// place that states it states that value.
func TestConfiguredLimitsAreEnforcedAndStated(t *testing.T) {
	setLimits(t, func(v *oplimits.Values) {
		v.DeletedRetentionDays = 7
		v.IdleGraceDays = 3
		v.IdleMaxEmails = 1
		v.PreviewLinkTTL = 15 * time.Minute
		v.ExportLinkTTL = 2 * time.Minute
		v.APIKeyDefaultDays = 20
		v.MaxTeamsPerPerson = 1
		v.MaxTeamMembers = 2
		v.MaxSiteViewers = 1
		v.MaxArchiveBytes = 1 << 20
	})
	w := newAccessWorld(t)
	ctx := context.Background()

	// Teams: one per person, two members each, and the refusals say so.
	w.newTeam("crew", "alice", "vera")
	if rec := w.api("alice", http.MethodPost, "/api/teams", map[string]string{"name": "other"}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "(1)") {
		t.Fatalf("second team = %d %s, want 409 naming 1", rec.Code, rec.Body)
	}
	if rec := w.api("alice", http.MethodPost, "/api/teams/team-crew/members", map[string]any{"usernames": []string{"olly"}}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "at most 2 members") {
		t.Fatalf("third member = %d %s, want 409 naming 2", rec.Code, rec.Body)
	}

	// Viewers: one per site.
	w.deploy("alice", "/api/sites/demo")
	if rec := w.api("alice", http.MethodPost, "/api/collaboration/sites/alice/demo/viewers", map[string]any{"usernames": []string{"vera"}}); rec.Code != http.StatusOK {
		t.Fatalf("first viewer = %d %s", rec.Code, rec.Body)
	}
	if rec := w.api("alice", http.MethodPost, "/api/collaboration/sites/alice/demo/viewers", map[string]any{"usernames": []string{"olly"}}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "at most 1 listed viewers") {
		t.Fatalf("second viewer = %d %s, want 409 naming 1", rec.Code, rec.Body)
	}

	// Archive size: over 1 MiB is refused, and the refusal names the limit.
	big := zipOf(t, map[string][]byte{"index.html": incompressible(1, 1<<20+1)})
	if rec := w.api("alice", http.MethodPost, "/api/sites/big", big); rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "at most 1 MiB") {
		t.Fatalf("oversized upload = %d %s, want 413 naming 1 MiB", rec.Code, rec.Body)
	}

	// Preview and download links last as configured.
	if rec := w.api("alice", http.MethodPut, "/api/sites/demo?publish=false", zipOf(t, map[string][]byte{"index.html": []byte("draft")})); rec.Code != http.StatusOK {
		t.Fatalf("held deploy = %d %s", rec.Code, rec.Body)
	}
	var link struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	_ = json.Unmarshal(w.api("alice", http.MethodGet, "/api/collaboration/sites/alice/demo/versions/2/preview", nil).Body.Bytes(), &link)
	within(t, "preview link expiry", link.ExpiresAt, 15*time.Minute)
	var export siteExportLinkResponse
	_ = json.Unmarshal(w.api("alice", http.MethodPost, "/api/collaboration/sites/alice/demo/export-link", nil).Body.Bytes(), &export)
	within(t, "download link expiry", export.ExpiresAt, 2*time.Minute)

	// A dashboard-minted key lives API_KEY_DEFAULT_DAYS.
	hosts, _ := NewHostModel("https://" + accessBase)
	keysMux := http.NewServeMux()
	NewKeysHandler(w.database, audit.NewDBRecorder(w.database), hosts, "https://"+accessBase).Register(keysMux, auth.Middleware(w.database, w.keys, time.Hour))
	mint := httptest.NewRequest(http.MethodPost, "https://"+accessBase+"/api/keys", strings.NewReader(`{"name":"ci"}`))
	mint.AddCookie(w.cookie("alice", ""))
	mint.Header.Set("Origin", "https://"+accessBase)
	mint.Header.Set("Content-Type", "application/json")
	minted := httptest.NewRecorder()
	keysMux.ServeHTTP(minted, mint)
	var key struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	_ = json.Unmarshal(minted.Body.Bytes(), &key)
	within(t, "new key expiry", key.ExpiresAt, 20*24*time.Hour)

	// A deleted site is restorable for DELETED_RETENTION_DAYS.
	w.deploy("alice", "/api/sites/gone")
	if rec := w.api("alice", http.MethodDelete, "/api/sites/gone", nil); rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	var deleted []struct {
		RestorableUntil time.Time `json:"restorable_until"`
	}
	_ = json.Unmarshal(w.api("alice", http.MethodGet, "/api/deleted-sites", nil).Body.Bytes(), &deleted)
	if len(deleted) != 1 {
		t.Fatalf("Recently deleted = %v", deleted)
	}
	within(t, "restorable until", deleted[0].RestorableUntil, 7*24*time.Hour)

	// The dashboard states the values in force and no placeholder survives.
	dashboard := http.NewServeMux()
	NewDashboardHandler(w.database, w.keys, time.Hour).Register(dashboard, nil)
	page := httptest.NewRequest(http.MethodGet, "https://"+accessBase+"/dashboard", nil)
	page.AddCookie(w.cookie("alice", ""))
	out := httptest.NewRecorder()
	dashboard.ServeHTTP(out, page)
	html := out.Body.String()
	for _, want := range []string{"Keys expire after 20 days", "stays here for 7 days", "for 15 minutes", "Recently deleted for 7 days"} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard does not say %q", want)
		}
	}
	if strings.Contains(html, "{{") || strings.Contains(html, "for 30 days") {
		t.Error("dashboard still carries a placeholder or the default restore window")
	}

	// Idle cleanup: one email per run, grace and restore window as set.
	w.deploy("olly", "/api/sites/a")
	w.deploy("olly", "/api/sites/b")
	w.makeUnused("a", "b")
	mailer := &bodyMailer{}
	cleanup := NewIdleCleanup(w.database, audit.NewDBRecorder(w.database), 60, mailer, "https://"+accessBase)
	if err := cleanup.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM sites WHERE idle_since IS NOT NULL`); n != 1 || len(mailer.bodies) != 1 {
		t.Fatalf("first run marked %d and emailed %d, want 1 and 1", n, len(mailer.bodies))
	}
	if !strings.Contains(mailer.bodies[0], "restored for 7 days") {
		t.Errorf("idle email = %q, want the 7-day restore window", mailer.bodies[0])
	}
	if err := cleanup.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM sites WHERE idle_since IS NOT NULL`); n != 2 || len(mailer.bodies) != 2 {
		t.Fatalf("second run: %d marked, %d emailed, want 2 and 2", n, len(mailer.bodies))
	}
	sites, err := db.ListIdleSites(ctx, w.database, w.users["olly"])
	if err != nil || len(sites) != 2 {
		t.Fatalf("idle sites = %v, %v", sites, err)
	}
	within(t, "moves to Recently deleted", sites[0].DeleteOn(), 3*24*time.Hour)
}
