package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// setStateWriteMode is a test-only helper: no production route sets
// sites.state_write_mode in this phase (design.md 7.3 defines the rule
// writerAllowed reads, not an endpoint to change it — see
// docs/security-review.md's Remains), so the column is
// exercised directly here rather than through a Go setter that does not
// exist yet.
func setStateWriteMode(t *testing.T, database *sql.DB, siteID, mode string) {
	t.Helper()
	if _, err := database.Exec(`UPDATE sites SET state_write_mode = $1 WHERE id = $2::uuid`, mode, siteID); err != nil {
		t.Fatalf("set state_write_mode: %v", err)
	}
}

// TestWriterAllowedAnyoneModeDefersToViewerAllowed covers design.md 7.3's
// default ("today's behaviour, minus anonymous"): an unrestricted site
// admits any signed-in writer, and a restricted one narrows to its viewer
// list exactly the way ViewerAllowed already does for reads.
func TestWriterAllowedAnyoneModeDefersToViewerAllowed(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	ownerID, siteID := mustCreateUserAndSite(t, database, "alice", "demo")
	strangerID, _ := mustCreateUserAndSite(t, database, "stranger", "unrelated")

	// state_write_mode defaults to 'anyone' (migration 0024); confirm the
	// default rather than assuming it.
	allowed, err := WriterAllowed(ctx, database, siteID, strangerID)
	if err != nil {
		t.Fatalf("WriterAllowed (unrestricted, default mode): %v", err)
	}
	if !allowed {
		t.Fatal("unrestricted site with the default write mode refused a signed-in stranger")
	}

	// Restrict the site to its owner only; the stranger can no longer view,
	// so writerAllowed's 'anyone' branch (== viewerAllowed) refuses them too.
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := GrantSiteViewers(ctx, tx, ownerID, "demo", siteID, &ownerID, []string{"alice"}); err != nil {
		t.Fatalf("GrantSiteViewers: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	allowed, err = WriterAllowed(ctx, database, siteID, strangerID)
	if err != nil {
		t.Fatalf("WriterAllowed (restricted, default mode): %v", err)
	}
	if allowed {
		t.Fatal("restricted site admitted a non-viewer writer under the default write mode")
	}
	allowed, err = WriterAllowed(ctx, database, siteID, ownerID)
	if err != nil {
		t.Fatalf("WriterAllowed (restricted, owner): %v", err)
	}
	if !allowed {
		t.Fatal("restricted site refused its own owner as a writer")
	}
}

// TestWriterAllowedEditorsModeNarrowsToOwnerTeamOrEditor covers the
// 'editors' branch: an unrestricted site (so anyone can view it) still
// refuses a plain signed-in stranger the write, admitting only the owner, a
// member of the owner's team, or an editor.
func TestWriterAllowedEditorsModeNarrowsToOwnerTeamOrEditor(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	ownerID, siteID := mustCreateUserAndSite(t, database, "alice", "demo")
	strangerID, _ := mustCreateUserAndSite(t, database, "stranger", "unrelated")
	editorUser, err := CreateOIDCUser(ctx, database, "editor", "sub-editor", "editor@example.com", false)
	if err != nil {
		t.Fatalf("CreateOIDCUser(editor): %v", err)
	}

	setStateWriteMode(t, database, siteID, "editors")

	// The site is unrestricted, so the stranger can still view it — but not
	// write to it under 'editors' mode.
	viewable, err := ViewerAllowed(ctx, database, siteID, strangerID)
	if err != nil {
		t.Fatalf("ViewerAllowed (unrestricted): %v", err)
	}
	if !viewable {
		t.Fatal("expected the unrestricted site to remain viewable by a stranger")
	}
	writable, err := WriterAllowed(ctx, database, siteID, strangerID)
	if err != nil {
		t.Fatalf("WriterAllowed (stranger, editors mode): %v", err)
	}
	if writable {
		t.Fatal("'editors' mode let a non-owner, non-editor stranger write")
	}

	writable, err = WriterAllowed(ctx, database, siteID, ownerID)
	if err != nil {
		t.Fatalf("WriterAllowed (owner, editors mode): %v", err)
	}
	if !writable {
		t.Fatal("'editors' mode refused the site's own owner")
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if _, err := GrantSiteEditors(ctx, tx, ownerID, "demo", siteID, &ownerID, []string{"editor"}); err != nil {
		t.Fatalf("GrantSiteEditors: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	writable, err = WriterAllowed(ctx, database, siteID, editorUser.ID)
	if err != nil {
		t.Fatalf("WriterAllowed (editor, editors mode): %v", err)
	}
	if !writable {
		t.Fatal("'editors' mode refused a granted editor")
	}
}

func TestWriterAllowedUnknownSiteReturnsErrSiteNotFound(t *testing.T) {
	database := assetsTestDB(t)
	_, err := WriterAllowed(context.Background(), database, "00000000-0000-4000-8000-000000000000", "00000000-0000-4000-8000-000000000001")
	if !errors.Is(err, ErrSiteNotFound) {
		t.Fatalf("WriterAllowed(unknown site) error = %v, want ErrSiteNotFound", err)
	}
}
