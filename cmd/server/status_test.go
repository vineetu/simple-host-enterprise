package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/config"
	"github.com/vsriram/simple-host/internal/oplimits"
)

// The /admin "This instance" card lists the operational values as set.
func TestInstanceLimitsStateTheOperationalSettings(t *testing.T) {
	cfg := config.Config{APIKeyMaxDays: 365, Limits: oplimits.Defaults()}
	cfg.Limits.DeletedRetentionDays = 14
	cfg.Limits.IdleGraceDays = 5
	cfg.Limits.PreviewLinkTTL = 45 * time.Minute
	cfg.Limits.MaxArchiveBytes = 250 << 20
	cfg.Limits.MaxTeamMembers = 80
	cfg.IdleCleanup.Days = 90
	var all strings.Builder
	for _, l := range instanceLimits(cfg) {
		all.WriteString(l.Name + ": " + l.Value + "\n")
	}
	for _, want := range []string{
		"Recently deleted kept: 14 days",
		"move to Recently deleted 5 days later",
		"previews work for 45 minutes, downloads for 10 minutes",
		"archives up to 250 MiB and 50,000 files",
		"10 per person, 80 members each",
		"API keys: 90 days by default, at most 365 days; expiry warned 14 days ahead",
	} {
		if !strings.Contains(all.String(), want) {
			t.Errorf("instance card does not say %q:\n%s", want, all.String())
		}
	}
}
