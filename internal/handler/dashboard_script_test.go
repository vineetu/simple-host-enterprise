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
	if len(blocks) < 3 {
		t.Fatalf("found %d dashboard scripts, want at least 3", len(blocks))
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

// The site panel offers what the page says it does, each over its route.
func TestDashboardSitePanelControls(t *testing.T) {
	for _, want := range []string{
		"make-live", "'/rollback'", "'If-Match': site.etag",
		"'/state-versions'", "restore-state",
		"'/export-link'", "Download site",
		"delete-site-button", "Type the site name to confirm",
		"include=shared", "renderShared",
		"location.origin + '/mcp'", "/plugin.zip", "/install.html",
	} {
		if !strings.Contains(dashboardScript, want) {
			t.Errorf("dashboard script lacks %q", want)
		}
	}
}
