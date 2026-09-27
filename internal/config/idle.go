package config

import (
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"strings"
)

// IDLE_CLEANUP_DAYS is 0 (off) or 30 days to ten years: a shorter window
// would mark sites that were only unused over a holiday.
const (
	minIdleCleanupDays = 30
	maxIdleCleanupDays = 3650
)

// IdleCleanupConfig is the opt-in cleanup of sites nobody visits or
// updates. Days 0 (the default) turns it off.
type IdleCleanupConfig struct {
	// Days is IDLE_CLEANUP_DAYS: a site nobody opened, deployed to, or read
	// or wrote saved data on for this many days is marked, and moves to Recently deleted 30 days later
	// unless someone keeps it or it is used again.
	Days int
	// SMTPURL is SMTP_URL (smtp://user:pass@host:587, STARTTLS required
	// unless it ends in ?insecure=1, or smtps://host:465): when set, the owner (or every
	// member of a team) is emailed when a site is marked. Without it the
	// dashboard notice and the admin list are the only notice.
	SMTPURL string
	// SMTPFrom is SMTP_FROM, the sender address; required with SMTP_URL.
	SMTPFrom string
}

func loadIdleCleanup() (IdleCleanupConfig, error) {
	days, err := nonNegativeInt64Env("IDLE_CLEANUP_DAYS", 0)
	if err != nil {
		return IdleCleanupConfig{}, err
	}
	if days != 0 && (days < minIdleCleanupDays || days > maxIdleCleanupDays) {
		return IdleCleanupConfig{}, fmt.Errorf("IDLE_CLEANUP_DAYS must be 0 (off) or %d to %d, got %d", minIdleCleanupDays, maxIdleCleanupDays, days)
	}
	cfg := IdleCleanupConfig{
		Days:     int(days),
		SMTPURL:  strings.TrimSpace(os.Getenv("SMTP_URL")),
		SMTPFrom: strings.TrimSpace(os.Getenv("SMTP_FROM")),
	}
	if cfg.SMTPURL == "" && cfg.SMTPFrom == "" {
		return cfg, nil
	}
	u, err := url.Parse(cfg.SMTPURL)
	if err != nil || (u.Scheme != "smtp" && u.Scheme != "smtps") || u.Hostname() == "" ||
		(u.RawQuery != "" && u.RawQuery != "insecure=1") || (u.Scheme == "smtps" && u.RawQuery != "") {
		return IdleCleanupConfig{}, fmt.Errorf("SMTP_URL must look like smtp://user:password@host:587 (STARTTLS required; add ?insecure=1 only for a relay without TLS) or smtps://host:465")
	}
	if _, err := mail.ParseAddress(cfg.SMTPFrom); err != nil {
		return IdleCleanupConfig{}, fmt.Errorf("SMTP_FROM must be an email address when SMTP_URL is set")
	}
	return cfg, nil
}
