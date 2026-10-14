package handler

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var scriptBlock = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

// TestDashboardScriptsParse runs every inline script of the dashboard
// through node --check, so a quoting slip in the Go string does not ship a
// page whose controls silently do nothing.
func TestDashboardScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required to parse the dashboard scripts")
	}
	blocks := scriptBlock.FindAllStringSubmatch(dashboardScript, -1)
	if len(blocks) < 4 {
		t.Fatalf("found %d dashboard scripts, want at least 4", len(blocks))
	}
	for i, block := range blocks {
		path := filepath.Join(t.TempDir(), "s.js")
		if err := os.WriteFile(path, []byte(block[1]), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
			t.Fatalf("dashboard script %d does not parse: %v\n%s", i, err, out)
		}
	}
}

// The site card offers what the page says it does, each over its route.
// Deleting a site is recoverable, so the sites script no longer asks for
// the name. Team deletion still does, and still uses the browser dialogs.
func TestDashboardSitePanelControls(t *testing.T) {
	for _, want := range []string{
		"make-live", "'/rollback'", "'If-Match': site.etag",
		"'/state-versions'", "restore-state",
		"'/export-link'",
		"include=shared", "renderShared",
		"location.origin + '/mcp'", "/plugin.zip", "/install.html",
		"'/api/teams'", "'/members'", "'/leave'", "confirm_team_delete", "?confirm_name=", "Type the team name to confirm",
		"Delete site", "gone for good", "{{DELETED_RETENTION}}",
		"Search your sites", "No sites match.",
		`aria-haspopup="menu"`, "Who can open", "Uploaded files", "Move to a team",
	} {
		if !strings.Contains(dashboardScript, want) {
			t.Errorf("dashboard script lacks %q", want)
		}
	}
	for _, gone := range []string{
		"Type the site name to confirm",
		"confirm(",
		"prompt(",
		"alert(",
	} {
		if strings.Contains(dashboardSitesScript, gone) {
			t.Errorf("sites script still has %q", gone)
		}
	}
}

// TestDashboardSitesScriptBehavior runs the pure menu, search, and delete
// helpers in the sites script. It skips when node is not installed.
func TestDashboardSitesScriptBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the sites menu behavior test")
	}
	script := strings.TrimSuffix(strings.TrimPrefix(dashboardSitesScript, "<script>"), "</script>")
	command := exec.Command(node, "testdata/dashboard_sites_test.js")
	command.Env = append(os.Environ(), "DASHBOARD_SITES_SCRIPT="+script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run sites menu behavior test: %v\n%s", err, output)
	}
}
