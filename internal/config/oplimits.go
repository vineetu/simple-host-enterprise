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
	minDeletedRetentionDays     = 7 // a long weekend must not purge a deleted site
	maxDeletedRetentionDays     = 365
	minIdleGraceDays            = 7
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
	minSiteStorageBytes         = 1_000
	maxSiteStorageBytes         = 1_000_000_000
	minSiteStorageValueBytes    = 1_024
	maxSiteStorageValueBytes    = 1_048_576
	maxSiteStorageResources     = 200
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
	v.NotifyMaxPerSite = intIn("NOTIFY_MAX_PER_SITE", v.NotifyMaxPerSite, 1, 1000)
	v.NotifyPerMinute = intIn("NOTIFY_PER_MINUTE", v.NotifyPerMinute, 1, 100000)
	v.NotifyPerDay = intIn("NOTIFY_PER_DAY", v.NotifyPerDay, 1, 10000000)
	v.NotifyHistoryDays = intIn("NOTIFY_HISTORY_DAYS", v.NotifyHistoryDays, 1, 365)
	v.NotifyOwnerConcurrency = intIn("NOTIFY_OWNER_CONCURRENCY", v.NotifyOwnerConcurrency, 1, 200)
	v.NotifyPerURLPerMinute = intIn("NOTIFY_PER_URL_PER_MINUTE", v.NotifyPerURLPerMinute, 1, 100000)
	v.NotifyWorkers = intIn("NOTIFY_WORKERS", v.NotifyWorkers, 1, 200)
	v.NotifyAttempts = intIn("NOTIFY_ATTEMPTS", v.NotifyAttempts, 1, 20)
	v.NotifyFieldMaxBytes = intIn("NOTIFY_FIELD_MAX_BYTES", v.NotifyFieldMaxBytes, 1, 65536)
	v.NotifyBodyMaxBytes = intIn("NOTIFY_BODY_MAX_BYTES", v.NotifyBodyMaxBytes, 4096, 262144)
	v.NotifySuspendAttempts = intIn("NOTIFY_SUSPEND_ATTEMPTS", v.NotifySuspendAttempts, 1, 100000)
	v.NotifySuspendFailurePercent = intIn("NOTIFY_SUSPEND_FAILURE_PERCENT", v.NotifySuspendFailurePercent, 1, 100)
	v.NotifyTimeout = durationIn("NOTIFY_TIMEOUT", v.NotifyTimeout, time.Millisecond, 30*time.Second)
	v.NotifyDefaultTTL = durationIn("NOTIFY_DEFAULT_TTL", v.NotifyDefaultTTL, time.Hour, 8760*time.Hour)
	v.NotifySuspendWindow = durationIn("NOTIFY_SUSPEND_WINDOW", v.NotifySuspendWindow, time.Hour, 168*time.Hour)
	v.NotifySuspendAfter = durationIn("NOTIFY_SUSPEND_AFTER", v.NotifySuspendAfter, 24*time.Hour, 8760*time.Hour)
	v.DeletedRetentionDays = intIn("DELETED_RETENTION_DAYS", v.DeletedRetentionDays, minDeletedRetentionDays, maxDeletedRetentionDays)
	v.IdleGraceDays = intIn("IDLE_CLEANUP_GRACE_DAYS", v.IdleGraceDays, minIdleGraceDays, maxIdleGraceDays)
	v.IdleMaxEmails = intIn("IDLE_CLEANUP_MAX_EMAILS", v.IdleMaxEmails, 0, maxIdleMaxEmails)
	v.PreviewLinkTTL = durationIn("PREVIEW_LINK_TTL", v.PreviewLinkTTL, minLinkTTL, maxLinkTTL)
	v.ExportLinkTTL = durationIn("EXPORT_LINK_TTL", v.ExportLinkTTL, minLinkTTL, maxLinkTTL)
	v.APIKeyDefaultDays = intIn("API_KEY_DEFAULT_DAYS", min(v.APIKeyDefaultDays, int(apiKeyMaxDays)), 1, int(apiKeyMaxDays))
	v.APIKeyExpiryWarningDays = intIn("API_KEY_EXPIRY_WARNING_DAYS", v.APIKeyExpiryWarningDays, 1, maxAPIKeyExpiryWarningDays)
	v.MaxTeamsPerPerson = intIn("MAX_TEAMS_PER_PERSON", v.MaxTeamsPerPerson, 1, maxTeamsPerPerson)
	v.MaxTeamMembers = intIn("MAX_TEAM_MEMBERS", v.MaxTeamMembers, 1, maxTeamMembers)
	v.ShowcaseBioMaxLength = intIn("SHOWCASE_BIO_MAX_LENGTH", v.ShowcaseBioMaxLength, 1, 2000)
	v.MaxSiteViewers = intIn("MAX_SITE_VIEWERS", v.MaxSiteViewers, 1, maxSiteViewers)
	v.MaxFilesPerSite = intIn("MAX_FILES_PER_SITE", v.MaxFilesPerSite, 1, maxFilesPerSite)
	v.UploadConcurrency = intIn("UPLOAD_CONCURRENCY", v.UploadConcurrency, 1, maxUploadConcurrency)
	v.SearchTelemetryRetentionDays = intIn("SEARCH_TELEMETRY_RETENTION_DAYS", v.SearchTelemetryRetentionDays, 1, maxSearchTelemetryRetention)
	v.SiteStorageMaxBytes = intIn("SITE_STORAGE_MAX_BYTES", v.SiteStorageMaxBytes, minSiteStorageBytes, maxSiteStorageBytes)
	v.SiteStorageFilesMaxBytes = intIn("SITE_STORAGE_FILES_MAX_BYTES", v.SiteStorageFilesMaxBytes, minSiteStorageBytes, maxSiteStorageBytes)
	v.SiteStorageFileMaxBytes = intIn("SITE_STORAGE_FILE_MAX_BYTES", v.SiteStorageFileMaxBytes, minSiteStorageBytes, maxSiteStorageBytes)
	v.SiteStorageValueMaxBytes = intIn("SITE_STORAGE_VALUE_MAX_BYTES", v.SiteStorageValueMaxBytes, minSiteStorageValueBytes, maxSiteStorageValueBytes)
	v.SiteStorageMaxResources = intIn("SITE_STORAGE_MAX_RESOURCES", v.SiteStorageMaxResources, 1, maxSiteStorageResources)
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

// RateLimitNames are the limits RATE_LIMIT_<NAME> may set: the configurable
// limiter names in internal/handler (a handler test keeps the two lists
// equal).
var RateLimitNames = []string{
	"auth-client", "auth-email", "management-client", "management-user", "api-key-mint",
	"state-client", "state-site", "state-read-client", "state-read-site",
	"admin-client", "admin-identity", "search-query-peer", "search-query-session",
	"oauth-register", "oauth-token",
}

// loadRateLimits reads every RATE_LIMIT_<NAME> variable in the environment.
// The names are the limiter names in internal/handler (RATE_LIMIT_AUTH_CLIENT
// is "auth-client"). A name that is not one of RateLimitNames (a typo, or a
// variable another program in the pod reads) is not read: it comes back as a
// warning for the startup log instead of stopping the server. The value is
// "<burst>/<interval>": RATE_LIMIT_AUTH_EMAIL=5/50s allows 5 at once and one
// more every 50 seconds.
func loadRateLimits() (map[string]RateLimit, []string, error) {
	known := make(map[string]bool, len(RateLimitNames))
	for _, n := range RateLimitNames {
		known[n] = true
	}
	var warnings []string
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
		name := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(key, rateLimitPrefix), "_", "-"))
		if !known[name] {
			warnings = append(warnings, key+" is not a limit this server has, so it changes nothing (docs/configuration.md lists them)")
			continue
		}
		limit, err := parseRateLimit(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", key, err)
		}
		out[name] = limit
	}
	return out, warnings, nil
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
