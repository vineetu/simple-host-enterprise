// Package oplimits holds the operational times and limits an installation may
// change through the environment (internal/config reads and validates them;
// cmd/server calls Set once at startup). Every package that enforces one of
// these, or tells a person or an agent about it, reads it here, so the rule
// and the sentence describing it can never disagree.
package oplimits

import (
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Values is every setting this package carries. Defaults returns what the
// server used before each one was configurable.
type Values struct {
	// DeletedRetentionDays is DELETED_RETENTION_DAYS: how long a deleted site
	// stays in Recently deleted, restorable and counted toward its owner's quota.
	DeletedRetentionDays int
	// IdleGraceDays is IDLE_CLEANUP_GRACE_DAYS: how long a site marked idle
	// waits before it moves to Recently deleted.
	IdleGraceDays int
	// IdleMaxEmails is IDLE_CLEANUP_MAX_EMAILS: the most idle-cleanup emails
	// one run sends; 0 means no limit.
	IdleMaxEmails int
	// PreviewLinkTTL is PREVIEW_LINK_TTL: how long a preview link works.
	PreviewLinkTTL time.Duration
	// ExportLinkTTL is EXPORT_LINK_TTL: how long a whole-site download link works.
	ExportLinkTTL time.Duration
	// APIKeyDefaultDays is API_KEY_DEFAULT_DAYS: a new key's lifetime when the
	// mint request names none.
	APIKeyDefaultDays int
	// APIKeyExpiryWarningDays is API_KEY_EXPIRY_WARNING_DAYS: a key this close
	// to its expiry is marked "expires soon" and answers with X-Key-Expires.
	APIKeyExpiryWarningDays int
	// MaxTeamsPerPerson is MAX_TEAMS_PER_PERSON: teams one person may belong
	// to before they can create another.
	MaxTeamsPerPerson int
	// MaxTeamMembers is MAX_TEAM_MEMBERS: members (and pending invitations)
	// one team may have.
	MaxTeamMembers int
	// MaxSiteViewers is MAX_SITE_VIEWERS: entries on one site's viewer list.
	MaxSiteViewers int
	// MaxArchiveBytes is MAX_ARCHIVE_BYTES: the largest compressed archive an
	// upload may send.
	MaxArchiveBytes int64
	// MaxFilesPerSite is MAX_FILES_PER_SITE: entries one uploaded archive may hold.
	MaxFilesPerSite int
	// UploadConcurrency is UPLOAD_CONCURRENCY: uploads (and archive
	// downloads) one replica processes at once.
	UploadConcurrency int
	// SearchTelemetryRetentionDays is SEARCH_TELEMETRY_RETENTION_DAYS: how
	// long search queries, impressions and clicks are kept.
	SearchTelemetryRetentionDays int
	// SearchSessionMaxAge is SEARCH_SESSION_MAX_AGE: the lifetime of the
	// anonymous search session cookie.
	SearchSessionMaxAge time.Duration
}

// Defaults are the values every installation used before they could be set.
func Defaults() Values {
	return Values{
		DeletedRetentionDays:         30,
		IdleGraceDays:                30,
		IdleMaxEmails:                0,
		PreviewLinkTTL:               time.Hour,
		ExportLinkTTL:                10 * time.Minute,
		APIKeyDefaultDays:            90,
		APIKeyExpiryWarningDays:      14,
		MaxTeamsPerPerson:            10,
		MaxTeamMembers:               50,
		MaxSiteViewers:               50,
		MaxArchiveBytes:              100 << 20,
		MaxFilesPerSite:              50_000,
		UploadConcurrency:            2,
		SearchTelemetryRetentionDays: 180,
		SearchSessionMaxAge:          180 * 24 * time.Hour,
	}
}

var current atomic.Pointer[Values]

func init() {
	v := Defaults()
	current.Store(&v)
}

// Get returns the values in force.
func Get() Values { return *current.Load() }

// Set replaces the values in force. cmd/server calls it once, before any
// handler or MCP tool list is built; tests restore the previous values.
func Set(v Values) { current.Store(&v) }

// DeletedRetention is DeletedRetentionDays as a duration.
func (v Values) DeletedRetention() time.Duration { return days(v.DeletedRetentionDays) }

// IdleGrace is IdleGraceDays as a duration.
func (v Values) IdleGrace() time.Duration { return days(v.IdleGraceDays) }

// APIKeyExpiryWarning is APIKeyExpiryWarningDays as a duration.
func (v Values) APIKeyExpiryWarning() time.Duration { return days(v.APIKeyExpiryWarningDays) }

// SearchTelemetryRetention is SearchTelemetryRetentionDays as a duration.
func (v Values) SearchTelemetryRetention() time.Duration {
	return days(v.SearchTelemetryRetentionDays)
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// Days writes a number of days for people: "1 day", "30 days".
func Days(n int) string {
	if n == 1 {
		return "1 day"
	}
	return Count(n) + " days"
}

// Duration writes a lifetime for people in its largest whole unit: "1 hour",
// "10 minutes", "7 days"; anything else as Go writes it ("1h30m0s").
func Duration(d time.Duration) string {
	unit := func(n int64, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.FormatInt(n, 10) + " " + one + "s"
	}
	switch {
	case d <= 0:
		return d.String()
	case d%(24*time.Hour) == 0:
		return unit(int64(d/(24*time.Hour)), "day")
	case d%time.Hour == 0:
		return unit(int64(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return unit(int64(d/time.Minute), "minute")
	case d%time.Second == 0:
		return unit(int64(d/time.Second), "second")
	default:
		return d.String()
	}
}

// Count writes a whole number with thousands separators: "50,000".
func Count(n int) string {
	if n < 0 {
		return "-" + Count(-n)
	}
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// Bytes writes a size in binary units, to one decimal place when it is not
// whole: "100 MiB", "1.5 GiB", "512 B".
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	text := strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64)
	return strings.TrimSuffix(text, ".0") + " " + string("KMGTPE"[exp]) + "iB"
}

// Placeholders are the "{{NAME}}" markers a served document (the skill
// files, the plugin bundle, openapi.yaml) may use for these values, each
// with the text that replaces it.
func (v Values) Placeholders() []string {
	return []string{
		"{{DELETED_RETENTION}}", Days(v.DeletedRetentionDays),
		"{{IDLE_CLEANUP_GRACE}}", Days(v.IdleGraceDays),
		"{{PREVIEW_LINK_TTL}}", Duration(v.PreviewLinkTTL),
		"{{EXPORT_LINK_TTL}}", Duration(v.ExportLinkTTL),
		"{{API_KEY_DEFAULT_DAYS}}", strconv.Itoa(v.APIKeyDefaultDays),
		"{{API_KEY_EXPIRY_WARNING}}", Days(v.APIKeyExpiryWarningDays),
		"{{MAX_TEAMS_PER_PERSON}}", Count(v.MaxTeamsPerPerson),
		"{{MAX_TEAM_MEMBERS}}", Count(v.MaxTeamMembers),
		"{{MAX_SITE_VIEWERS}}", Count(v.MaxSiteViewers),
		"{{MAX_ARCHIVE}}", Bytes(v.MaxArchiveBytes),
		"{{MAX_FILES_PER_SITE}}", Count(v.MaxFilesPerSite),
		"{{SEARCH_TELEMETRY_RETENTION}}", Days(v.SearchTelemetryRetentionDays),
		"{{SEARCH_SESSION_MAX_AGE}}", Duration(v.SearchSessionMaxAge),
	}
}

// Expand fills every placeholder in text with the values in force.
func Expand(text string) string {
	return strings.NewReplacer(Get().Placeholders()...).Replace(text)
}
