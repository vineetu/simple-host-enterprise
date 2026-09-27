package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/oplimits"
)

// Bounds on the operational settings in oplimits. The low ends stop a
// setting from switching a feature off by accident; the high ends stop a
// typo (8760h for 8h, an extra zero) from turning a limit into none.
const (
	maxDeletedRetentionDays     = 365
	maxIdleGraceDays            = 365
	maxIdleMaxEmails            = 100_000
	minLinkTTL                  = time.Minute
	maxLinkTTL                  = 24 * time.Hour
	maxAPIKeyExpiryWarningDays  = 365
	maxTeamsPerPerson           = 1000
	maxTeamMembers              = 1000
	maxSiteViewers              = 1000
	minArchiveBytes             = 1 << 20
	maxArchiveBytes             = 500 << 20 // the fixed uncompressed-size guard in internal/tarball
	maxFilesPerSite             = 100_000   // the per-version file cap in internal/storage
	maxUploadConcurrency        = 64
	maxSearchTelemetryRetention = 3650
	minSearchSessionMaxAge      = time.Hour
	maxSearchSessionMaxAge      = 400 * 24 * time.Hour // browsers cap a cookie's lifetime at 400 days
	maxRateLimitBurst           = 100_000
	minRateLimitEvery           = time.Millisecond
	maxRateLimitEvery           = time.Hour
	rateLimitPrefix             = "RATE_LIMIT_"
)

// RateLimit is one RATE_LIMIT_<NAME>=<burst>/<interval> setting: a bucket
// of Burst requests, refilled by one every Every.
type RateLimit struct {
	Burst int
	Every time.Duration
}

// loadOpLimits reads the settings in oplimits.Values, each defaulting to
// what the server did before it could be set. apiKeyMaxDays is the already
// validated API_KEY_MAX_DAYS, which bounds API_KEY_DEFAULT_DAYS.
func loadOpLimits(apiKeyMaxDays int64) (oplimits.Values, error) {
	v := oplimits.Defaults()
	var err error
	intIn := func(key string, fallback, lo, hi int) int {
		if err != nil {
			return fallback
		}
		var n int64
		n, err = nonNegativeInt64Env(key, int64(fallback))
		if err == nil && (n < int64(lo) || n > int64(hi)) {
			err = fmt.Errorf("%s must be %d to %d, got %d", key, lo, hi, n)
		}
		return int(n)
	}
	durationIn := func(key string, fallback, lo, hi time.Duration) time.Duration {
		if err != nil {
			return fallback
		}
		var d time.Duration
		d, err = durationEnv(key, fallback)
		if err == nil && (d < lo || d > hi) {
			err = fmt.Errorf("%s must be %s to %s, got %s", key, lo, hi, d)
		}
		return d
	}
	v.DeletedRetentionDays = intIn("DELETED_RETENTION_DAYS", v.DeletedRetentionDays, 1, maxDeletedRetentionDays)
	v.IdleGraceDays = intIn("IDLE_CLEANUP_GRACE_DAYS", v.IdleGraceDays, 1, maxIdleGraceDays)
	v.IdleMaxEmails = intIn("IDLE_CLEANUP_MAX_EMAILS", v.IdleMaxEmails, 0, maxIdleMaxEmails)
	v.PreviewLinkTTL = durationIn("PREVIEW_LINK_TTL", v.PreviewLinkTTL, minLinkTTL, maxLinkTTL)
	v.ExportLinkTTL = durationIn("EXPORT_LINK_TTL", v.ExportLinkTTL, minLinkTTL, maxLinkTTL)
	v.APIKeyDefaultDays = intIn("API_KEY_DEFAULT_DAYS", min(v.APIKeyDefaultDays, int(apiKeyMaxDays)), 1, int(apiKeyMaxDays))
	v.APIKeyExpiryWarningDays = intIn("API_KEY_EXPIRY_WARNING_DAYS", v.APIKeyExpiryWarningDays, 1, maxAPIKeyExpiryWarningDays)
	v.MaxTeamsPerPerson = intIn("MAX_TEAMS_PER_PERSON", v.MaxTeamsPerPerson, 1, maxTeamsPerPerson)
	v.MaxTeamMembers = intIn("MAX_TEAM_MEMBERS", v.MaxTeamMembers, 1, maxTeamMembers)
	v.MaxSiteViewers = intIn("MAX_SITE_VIEWERS", v.MaxSiteViewers, 1, maxSiteViewers)
	v.MaxFilesPerSite = intIn("MAX_FILES_PER_SITE", v.MaxFilesPerSite, 1, maxFilesPerSite)
	v.UploadConcurrency = intIn("UPLOAD_CONCURRENCY", v.UploadConcurrency, 1, maxUploadConcurrency)
	v.SearchTelemetryRetentionDays = intIn("SEARCH_TELEMETRY_RETENTION_DAYS", v.SearchTelemetryRetentionDays, 1, maxSearchTelemetryRetention)
	v.SearchSessionMaxAge = durationIn("SEARCH_SESSION_MAX_AGE", v.SearchSessionMaxAge, minSearchSessionMaxAge, maxSearchSessionMaxAge)
	if err != nil {
		return oplimits.Values{}, err
	}
	if v.MaxArchiveBytes, err = nonNegativeInt64Env("MAX_ARCHIVE_BYTES", v.MaxArchiveBytes); err != nil {
		return oplimits.Values{}, err
	}
	if v.MaxArchiveBytes < minArchiveBytes || v.MaxArchiveBytes > maxArchiveBytes {
		return oplimits.Values{}, fmt.Errorf("MAX_ARCHIVE_BYTES must be %d (1 MiB) to %d (500 MiB), got %d", minArchiveBytes, maxArchiveBytes, v.MaxArchiveBytes)
	}
	return v, nil
}

// loadRateLimits reads every RATE_LIMIT_<NAME> variable in the environment.
// The names are the limiter names in internal/handler (RATE_LIMIT_AUTH_CLIENT
// is "auth-client"); the handler refuses a name it does not have. The value is
// "<burst>/<interval>": RATE_LIMIT_AUTH_EMAIL=5/50s allows 5 at once and one
// more every 50 seconds.
func loadRateLimits() (map[string]RateLimit, error) {
	out := map[string]RateLimit{}
	var keys []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, rateLimitPrefix) && len(key) > len(rateLimitPrefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		limit, err := parseRateLimit(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		name := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(key, rateLimitPrefix), "_", "-"))
		out[name] = limit
	}
	return out, nil
}

func parseRateLimit(raw string) (RateLimit, error) {
	const shape = `must be "<burst>/<interval>", for example "20/5s" (20 at once, then one more every 5 seconds)`
	burstText, everyText, ok := strings.Cut(raw, "/")
	if !ok {
		return RateLimit{}, fmt.Errorf("%s, got %q", shape, raw)
	}
	burst, err := strconv.Atoi(strings.TrimSpace(burstText))
	if err != nil {
		return RateLimit{}, fmt.Errorf("%s, got %q", shape, raw)
	}
	every, err := time.ParseDuration(strings.TrimSpace(everyText))
	if err != nil {
		return RateLimit{}, fmt.Errorf("%s, got %q", shape, raw)
	}
	if burst < 1 || burst > maxRateLimitBurst {
		return RateLimit{}, fmt.Errorf("burst must be 1 to %d, got %d", maxRateLimitBurst, burst)
	}
	if every < minRateLimitEvery || every > maxRateLimitEvery {
		return RateLimit{}, fmt.Errorf("interval must be %s to %s, got %s", minRateLimitEvery, maxRateLimitEvery, every)
	}
	return RateLimit{Burst: burst, Every: every}, nil
}
