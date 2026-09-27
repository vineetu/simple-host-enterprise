package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/oplimits"
)

var opLimitKeys = []string{
	"DELETED_RETENTION_DAYS", "IDLE_CLEANUP_GRACE_DAYS", "IDLE_CLEANUP_MAX_EMAILS",
	"PREVIEW_LINK_TTL", "EXPORT_LINK_TTL", "API_KEY_DEFAULT_DAYS", "API_KEY_EXPIRY_WARNING_DAYS",
	"MAX_TEAMS_PER_PERSON", "MAX_TEAM_MEMBERS", "MAX_SITE_VIEWERS", "MAX_ARCHIVE_BYTES",
	"MAX_FILES_PER_SITE", "UPLOAD_CONCURRENCY", "SEARCH_TELEMETRY_RETENTION_DAYS", "SEARCH_SESSION_MAX_AGE",
}

func opLimitsEnv(t *testing.T) {
	t.Helper()
	completeEnv(t)
	for _, key := range opLimitKeys {
		t.Setenv(key, "")
	}
}

func TestLoadOpLimitsDefaultToTodaysValues(t *testing.T) {
	opLimitsEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := oplimits.Values{
		DeletedRetentionDays: 30, IdleGraceDays: 30, IdleMaxEmails: 0,
		PreviewLinkTTL: time.Hour, ExportLinkTTL: 10 * time.Minute,
		APIKeyDefaultDays: 90, APIKeyExpiryWarningDays: 14,
		MaxTeamsPerPerson: 10, MaxTeamMembers: 50, MaxSiteViewers: 50,
		MaxArchiveBytes: 100 << 20, MaxFilesPerSite: 50_000, UploadConcurrency: 2,
		SearchTelemetryRetentionDays: 180, SearchSessionMaxAge: 180 * 24 * time.Hour,
	}
	if cfg.Limits != want {
		t.Fatalf("Limits = %+v, want %+v", cfg.Limits, want)
	}
	if cfg.Limits != oplimits.Defaults() {
		t.Fatalf("Limits differ from oplimits.Defaults(): %+v", oplimits.Defaults())
	}
	if len(cfg.RateLimits) != 0 {
		t.Fatalf("RateLimits = %v, want none", cfg.RateLimits)
	}
}

func TestLoadOpLimitsOverrides(t *testing.T) {
	opLimitsEnv(t)
	for key, value := range map[string]string{
		"DELETED_RETENTION_DAYS": "7", "IDLE_CLEANUP_GRACE_DAYS": "14", "IDLE_CLEANUP_MAX_EMAILS": "25",
		"PREVIEW_LINK_TTL": "30m", "EXPORT_LINK_TTL": "2h", "API_KEY_DEFAULT_DAYS": "30",
		"API_KEY_EXPIRY_WARNING_DAYS": "7", "MAX_TEAMS_PER_PERSON": "3", "MAX_TEAM_MEMBERS": "200",
		"MAX_SITE_VIEWERS": "5", "MAX_ARCHIVE_BYTES": "262144000", "MAX_FILES_PER_SITE": "80000",
		"UPLOAD_CONCURRENCY": "4", "SEARCH_TELEMETRY_RETENTION_DAYS": "30", "SEARCH_SESSION_MAX_AGE": "720h",
		"RATE_LIMIT_AUTH_EMAIL":  "3/1m",
		"RATE_LIMIT_OAUTH_TOKEN": " 240/250ms ",
	} {
		t.Setenv(key, value)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := oplimits.Values{
		DeletedRetentionDays: 7, IdleGraceDays: 14, IdleMaxEmails: 25,
		PreviewLinkTTL: 30 * time.Minute, ExportLinkTTL: 2 * time.Hour,
		APIKeyDefaultDays: 30, APIKeyExpiryWarningDays: 7,
		MaxTeamsPerPerson: 3, MaxTeamMembers: 200, MaxSiteViewers: 5,
		MaxArchiveBytes: 250 << 20, MaxFilesPerSite: 80_000, UploadConcurrency: 4,
		SearchTelemetryRetentionDays: 30, SearchSessionMaxAge: 30 * 24 * time.Hour,
	}
	if cfg.Limits != want {
		t.Fatalf("Limits = %+v, want %+v", cfg.Limits, want)
	}
	wantRates := map[string]RateLimit{
		"auth-email":  {Burst: 3, Every: time.Minute},
		"oauth-token": {Burst: 240, Every: 250 * time.Millisecond},
	}
	if !reflect.DeepEqual(cfg.RateLimits, wantRates) {
		t.Fatalf("RateLimits = %v, want %v", cfg.RateLimits, wantRates)
	}
}

// The default key lifetime never exceeds the maximum: left unset it follows
// a lower API_KEY_MAX_DAYS down, set above it it is refused.
func TestLoadAPIKeyDefaultDaysFollowsTheMaximum(t *testing.T) {
	opLimitsEnv(t)
	t.Setenv("API_KEY_MAX_DAYS", "30")
	cfg, err := Load()
	if err != nil || cfg.Limits.APIKeyDefaultDays != 30 {
		t.Fatalf("APIKeyDefaultDays = %d, %v; want 30", cfg.Limits.APIKeyDefaultDays, err)
	}
	t.Setenv("API_KEY_DEFAULT_DAYS", "31")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "API_KEY_DEFAULT_DAYS must be 1 to 30") {
		t.Fatalf("Load() error = %v, want API_KEY_DEFAULT_DAYS refused above API_KEY_MAX_DAYS", err)
	}
}

func TestLoadRefusesBadOpLimits(t *testing.T) {
	for _, c := range []struct{ key, value string }{
		{"DELETED_RETENTION_DAYS", "0"},
		{"DELETED_RETENTION_DAYS", "6"},
		{"IDLE_CLEANUP_GRACE_DAYS", "6"},
		{"DELETED_RETENTION_DAYS", "366"},
		{"DELETED_RETENTION_DAYS", "thirty"},
		{"IDLE_CLEANUP_GRACE_DAYS", "0"},
		{"IDLE_CLEANUP_MAX_EMAILS", "-1"},
		{"IDLE_CLEANUP_MAX_EMAILS", "100001"},
		{"PREVIEW_LINK_TTL", "30s"},
		{"PREVIEW_LINK_TTL", "25h"},
		{"PREVIEW_LINK_TTL", "1 hour"},
		{"EXPORT_LINK_TTL", "0s"},
		{"EXPORT_LINK_TTL", "48h"},
		{"API_KEY_DEFAULT_DAYS", "0"},
		{"API_KEY_EXPIRY_WARNING_DAYS", "0"},
		{"API_KEY_EXPIRY_WARNING_DAYS", "400"},
		{"MAX_TEAMS_PER_PERSON", "0"},
		{"MAX_TEAMS_PER_PERSON", "1001"},
		{"MAX_TEAM_MEMBERS", "0"},
		{"MAX_TEAM_MEMBERS", "5000"},
		{"MAX_SITE_VIEWERS", "0"},
		{"MAX_SITE_VIEWERS", "1001"},
		{"MAX_ARCHIVE_BYTES", "1000"},
		{"MAX_ARCHIVE_BYTES", "524288001"},
		{"MAX_ARCHIVE_BYTES", "100MiB"},
		{"MAX_FILES_PER_SITE", "0"},
		{"MAX_FILES_PER_SITE", "100001"},
		{"UPLOAD_CONCURRENCY", "0"},
		{"UPLOAD_CONCURRENCY", "65"},
		{"SEARCH_TELEMETRY_RETENTION_DAYS", "0"},
		{"SEARCH_TELEMETRY_RETENTION_DAYS", "3651"},
		{"SEARCH_SESSION_MAX_AGE", "30m"},
		{"SEARCH_SESSION_MAX_AGE", "9601h"},
	} {
		t.Run(c.key+"="+c.value, func(t *testing.T) {
			opLimitsEnv(t)
			t.Setenv(c.key, c.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), c.key) {
				t.Fatalf("Load() error = %v, want a refusal naming %s", err, c.key)
			}
		})
	}
}

// An unknown RATE_LIMIT_* name (a typo, or another program's variable such
// as a service mesh's RATE_LIMIT_ENABLED) is a startup warning, not a
// refusal, and is not read.
func TestLoadWarnsOnUnknownRateLimit(t *testing.T) {
	opLimitsEnv(t)
	t.Setenv("RATE_LIMIT_ENABLED", "true")
	t.Setenv("RATE_LIMIT_AUTH_EMAIL", "3/1m")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "RATE_LIMIT_ENABLED") {
		t.Fatalf("Warnings = %v", cfg.Warnings)
	}
	if len(cfg.RateLimits) != 1 || cfg.RateLimits["auth-email"] != (RateLimit{3, time.Minute}) {
		t.Fatalf("RateLimits = %v", cfg.RateLimits)
	}
}

func TestLoadRefusesBadRateLimits(t *testing.T) {
	for _, value := range []string{"20", "20/", "/5s", "twenty/5s", "20/5", "0/5s", "100001/1s", "20/0s", "20/500us", "20/2h", "-1/5s"} {
		t.Run(value, func(t *testing.T) {
			opLimitsEnv(t)
			t.Setenv("RATE_LIMIT_AUTH_CLIENT", value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "RATE_LIMIT_AUTH_CLIENT") {
				t.Fatalf("Load() error = %v, want a refusal naming RATE_LIMIT_AUTH_CLIENT", err)
			}
		})
	}
}
