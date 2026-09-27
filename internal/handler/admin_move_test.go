package handler

import (
	"strings"
	"testing"
)

func TestLeaverSiteActions(t *testing.T) {
	if got := leaverSiteActions("alice", 0, true); got != "" {
		t.Errorf("no sites: %q", got)
	}
	person := leaverSiteActions("alice", 3, true)
	for _, want := range []string{`action="/api/admin/users/alice/transfer-sites"`, "Move to team…", `name="to"`, `action="/api/admin/users/alice/delete-sites"`, "Delete sites", "3 sites"} {
		if !strings.Contains(person, want) {
			t.Errorf("disabled person's actions lack %q: %s", want, person)
		}
	}
	team := leaverSiteActions("team-gone", 1, false)
	if !strings.Contains(team, "transfer-sites") || strings.Contains(team, "delete-sites") {
		t.Errorf("abandoned team's actions: %s", team)
	}
}
