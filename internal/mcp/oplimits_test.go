package mcp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/oplimits"
)

// The tool list and the failure hints state the installation's values.
func TestToolTextFollowsTheSettings(t *testing.T) {
	before := oplimits.Get()
	t.Cleanup(func() { oplimits.Set(before) })

	defaults, _ := json.Marshal(Tools())
	for _, want := range []string{"the last 30 days from your account", "works for 1 hour", "within 10 minutes", "Recently deleted 30 days later"} {
		if !strings.Contains(string(defaults), want) {
			t.Errorf("default tool list does not say %q", want)
		}
	}

	v := oplimits.Defaults()
	v.DeletedRetentionDays = 12
	v.PreviewLinkTTL = 2 * time.Hour
	v.ExportLinkTTL = 5 * time.Minute
	v.IdleGraceDays = 4
	oplimits.Set(v)
	body, _ := json.Marshal(Tools())
	text := string(body)
	for _, want := range []string{"the last 12 days from your account", "For 12 days it stays in Recently deleted", "works for 2 hours", "within 5 minutes", "Recently deleted 4 days later", "2 hours from now"} {
		if !strings.Contains(text, want) {
			t.Errorf("tool list does not say %q", want)
		}
	}
	if strings.Contains(text, "{{") || strings.Contains(text, "30 days later") {
		t.Error("tool list carries a placeholder or a default value")
	}
	hint := explainFailure(409, `{"error":"x","code":"name_held"}`, Tool{Name: "deploy_site"})
	if !strings.Contains(hint, "deleted in the last 12 days") {
		t.Errorf("name_held hint = %q", hint)
	}
}
