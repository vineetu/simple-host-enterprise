package handler

import (
	"strings"
	"testing"
)

// renderShareDialog's own markup is exercised here even though the
// dashboard's per-site viewer and asset panels (dashboard.go,
// dashboardSitesScript, Phase 3) ended up as fresh inline markup rather
// than this literal `<dialog>` reused verbatim: its copy is
// editor-specific ("Editors can download, deploy, and roll back..."),
// which would misdescribe a viewer list. The panels reuse this dialog's
// *pattern* instead — a fetch-driven list against the same collaboration
// API shape — with wording that actually describes viewing and asset
// access. The editor-management inventory this dialog used to be embedded
// in (the base-host per-user listing page) is gone with the rest of
// design.md 7.1's base-host site serving, but the dialog markup itself is
// unchanged and still available if a future editor-management UI wants it.
func TestRenderShareDialogHasAccessibleStructureAndClearRevocationCopy(t *testing.T) {
	var builder strings.Builder
	renderShareDialog(&builder)
	markup := builder.String()

	for _, want := range []string{
		`<dialog id="shareDialog"`,
		`aria-labelledby="shareDialogTitle"`,
		`aria-describedby="shareDialogNote"`,
		`aria-label="Close share dialog"`,
		`aria-live="polite"`,
		`id="editorSearch"`,
		`id="addEditorsButton"`,
		`Revoking access blocks future changes and downloads.`,
		`It does not undo content an editor already deployed or erase files they downloaded.`,
	} {
		if !strings.Contains(markup, want) {
			t.Errorf("share dialog missing %q", want)
		}
	}
}
