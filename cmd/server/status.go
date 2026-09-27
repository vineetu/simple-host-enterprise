package main

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/vsriram/simple-host/internal/oplimits"
	"time"

	"github.com/vsriram/simple-host/internal/config"
	"github.com/vsriram/simple-host/internal/handler"
	"github.com/vsriram/simple-host/internal/metrics"
	"github.com/vsriram/simple-host/internal/migrate"
)

// instanceStatus is the /admin "This instance" card's input: the build, a
// pending-migrations check, this replica's bucket check, and the limits in
// force, as the configuration resolved them.
func instanceStatus(cfg config.Config, database *sql.DB, registry *metrics.Registry, schema string) handler.InstanceStatus {
	return handler.InstanceStatus{
		Version: version, Commit: commit, Schema: schema,
		PendingMigrations: func(ctx context.Context) ([]string, error) {
			pending, err := migrate.Pending(ctx, database)
			names := make([]string, 0, len(pending))
			for _, m := range pending {
				names = append(names, m.Name)
			}
			return names, err
		},
		Bucket:           registry.BucketStatus,
		OwnerCertsManual: cfg.OwnerCerts == "manual",
		Limits:           instanceLimits(cfg),
	}
}

func instanceLimits(cfg config.Config) []handler.InstanceLimit {
	scanner := "off"
	if cfg.Clamd.Addr != "" {
		scanner = "on (clamd at " + cfg.Clamd.Addr + ")"
	}
	envelope := "off (server-side encryption " + orNone(cfg.Backup.SSE) + " only)"
	if n := len(cfg.Backup.EnvelopeKeys); n > 0 {
		envelope = fmt.Sprintf("on (%d key(s)), plus server-side encryption %s", n, orNone(cfg.Backup.SSE))
	}
	return []handler.InstanceLimit{
		{Name: "Sites per owner", Value: limitOrNone(cfg.Quota.MaxSites, fmt.Sprint(cfg.Quota.MaxSites))},
		{Name: "Storage per owner", Value: limitOrNone(cfg.Quota.MaxBytes, bytesText(cfg.Quota.MaxBytes))},
		{Name: "Versions kept per site", Value: fmt.Sprint(cfg.Quota.MaxVersions)},
		{Name: "Uploaded files", Value: fmt.Sprintf("%s each, %s and %d files per site", bytesText(cfg.Assets.MaxFileBytes), bytesText(cfg.Assets.MaxSiteBytes), cfg.Assets.MaxSiteCount)},
		{Name: "Sessions", Value: fmt.Sprintf("end after %s, or %s idle", durationText(cfg.Session.TTL), durationText(cfg.Session.Idle))},
		{Name: "Connected apps", Value: fmt.Sprintf("access token %s, sign in again after %s", durationText(cfg.OAuthAccessTTL), durationText(cfg.OAuthRefreshTTL))},
		{Name: "API keys", Value: fmt.Sprintf("%s by default, at most %d days; expiry warned %s ahead", oplimits.Days(cfg.Limits.APIKeyDefaultDays), cfg.APIKeyMaxDays, oplimits.Days(cfg.Limits.APIKeyExpiryWarningDays))},
		{Name: "Uploads", Value: fmt.Sprintf("archives up to %s and %s files; %d at once per replica", oplimits.Bytes(cfg.Limits.MaxArchiveBytes), oplimits.Count(cfg.Limits.MaxFilesPerSite), cfg.Limits.UploadConcurrency)},
		{Name: "Teams", Value: fmt.Sprintf("%s per person, %s members each", oplimits.Count(cfg.Limits.MaxTeamsPerPerson), oplimits.Count(cfg.Limits.MaxTeamMembers))},
		{Name: "Viewers per site", Value: oplimits.Count(cfg.Limits.MaxSiteViewers)},
		{Name: "Links", Value: fmt.Sprintf("previews work for %s, downloads for %s", oplimits.Duration(cfg.Limits.PreviewLinkTTL), oplimits.Duration(cfg.Limits.ExportLinkTTL))},
		{Name: "Upload scanning", Value: scanner},
		{Name: "Envelope encryption", Value: envelope},
		{Name: "Network access approvals", Value: fmt.Sprint(cfg.NetworkAccessApprovals)},
		{Name: "Audit log kept", Value: fmt.Sprintf("%d days", cfg.Audit.RetentionDays)},
		{Name: "Access log kept", Value: fmt.Sprintf("%d days", cfg.Audit.AccessLogRetentionDays)},
		{Name: "Recently deleted kept", Value: oplimits.Days(cfg.Limits.DeletedRetentionDays)},
		{Name: "Search history kept", Value: oplimits.Days(cfg.Limits.SearchTelemetryRetentionDays)},
		{Name: "Idle-site cleanup", Value: idleText(cfg.IdleCleanup, cfg.Limits.IdleGraceDays)},
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func limitOrNone(n int64, text string) string {
	if n <= 0 {
		return "no limit"
	}
	return text
}

func bytesText(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.0f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func durationText(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%d days", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%d hours", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%d minutes", d/time.Minute)
	default:
		return d.String()
	}
}

func idleText(c config.IdleCleanupConfig, grace int) string {
	if c.Days == 0 {
		return "off"
	}
	notice := "dashboard notice"
	if c.SMTPURL != "" {
		notice = "dashboard notice and email"
	}
	return fmt.Sprintf("sites unused for %d days are marked and move to Recently deleted %s later (%s)", c.Days, oplimits.Days(grace), notice)
}

// loadConfig is config.Load plus putting its operational limits in force
// (oplimits, and the handler's rate limits), which every command that loads
// the full configuration does before it builds anything that reads them.
func loadConfig() (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, err
	}
	rates := make(map[string]handler.RateLimit, len(cfg.RateLimits))
	for name, r := range cfg.RateLimits {
		rates[name] = handler.RateLimit{Burst: r.Burst, Every: r.Every}
	}
	if err := handler.ConfigureRateLimits(rates); err != nil {
		return config.Config{}, err
	}
	oplimits.Set(cfg.Limits)
	return cfg, nil
}
