package main

import (
	"bytes"
	"context"
	"database/sql"
	"io/fs"
	"strings"
	"testing"

	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/storage"
)

// drillPage is what the drill site serves; the test reads it back through
// the store, the same path a visitor's request takes.
const drillPage = "<h1>restore drill</h1>"

func drillServes(t *testing.T, store *storage.Store, owner, site string) string {
	t.Helper()
	lease, err := store.OpenCurrent(context.Background(), owner, site)
	if err != nil {
		return "error: " + err.Error()
	}
	defer lease.Close()
	body, err := fs.ReadFile(lease.FS(), "index.html")
	if err != nil {
		return "error: " + err.Error()
	}
	return string(body)
}

// The restore drill, end to end against a real database and an in-memory
// bucket: publish a site, delete it, bring it back with `restore`, and it
// serves again; then the same after the sweeper has purged its row (a new
// site from the old one's objects); and verify-storage names a live object
// the bucket lost.
func TestRestoreDrill(t *testing.T) {
	database := operatorTestDB(t)
	ctx := context.Background()
	objects := storage.NewMemoryObjects()
	store, err := storage.New(storage.Options{Objects: objects, Index: storage.NewDBIndex(database), CacheDir: t.TempDir(), CacheMaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := db.CreateOIDCUser(ctx, database, "drill", "sub-drill", "drill@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	site, err := db.CreateSite(ctx, database, user.ID, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutVersion(ctx, site.ID, 1, map[string][]byte{"index.html": []byte(drillPage)}); err != nil {
		t.Fatal(err)
	}
	key, _ := storage.VersionKey(site.ID, 1)
	row, err := db.CreateVersion(ctx, database, site.ID, 1, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ActivateVersion(ctx, database, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSiteActiveVersion(ctx, database, site.ID, 1); err != nil {
		t.Fatal(err)
	}
	if got := drillServes(t, store, "drill", "demo"); got != drillPage {
		t.Fatalf("published site serves %q", got)
	}
	var report bytes.Buffer
	if missing, checked, err := verifyStorage(ctx, database, objects, &report); err != nil || missing != 0 || checked != 1 {
		t.Fatalf("verify-storage on a healthy install = %d missing of %d, %v: %s", missing, checked, err, report.String())
	}

	// Deleted, then restored: the same site comes back and serves.
	if err := db.SoftDeleteSite(ctx, database, site.ID, user.ID, "demo", user.ID); err != nil {
		t.Fatal(err)
	}
	if got := drillServes(t, store, "drill", "demo"); got == drillPage {
		t.Fatal("a deleted site still serves")
	}
	if err := restoreVersion(ctx, database, objects, "https://hosting.corp.test", restoreRequest{fromSiteID: site.ID, version: 1, owner: "drill", site: "demo", setCurrent: true}); err != nil {
		t.Fatalf("restore a deleted site: %v", err)
	}
	if got := drillServes(t, store, "drill", "demo"); got != drillPage {
		t.Fatalf("restored site serves %q", got)
	}

	// Purged (the row is gone, the objects are still in the bucket): restore
	// makes a new site from the old one's version, and it serves.
	if _, err := database.Exec(`DELETE FROM sites WHERE id = $1`, site.ID); err != nil {
		t.Fatal(err)
	}
	if err := restoreVersion(ctx, database, objects, "https://hosting.corp.test", restoreRequest{fromSiteID: site.ID, version: 1, owner: "drill", site: "demo", setCurrent: true}); err != nil {
		t.Fatalf("restore a purged site: %v", err)
	}
	if got := drillServes(t, store, "drill", "demo"); got != drillPage {
		t.Fatalf("site restored after purge serves %q", got)
	}
	var restored int
	if err := database.QueryRow(`SELECT count(*) FROM audit_events WHERE action = 'site_restore'`).Scan(&restored); err != nil || restored != 2 {
		t.Fatalf("site_restore events = %d, %v; want 2", restored, err)
	}

	// The bucket loses the live version: verify-storage names its key.
	var newID string
	if err := database.QueryRow(`SELECT id::text FROM sites WHERE name = 'demo'`).Scan(&newID); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	lost, _ := storage.VersionKey(newID, 1)
	if err := objects.Delete(ctx, lost); err != nil {
		t.Fatal(err)
	}
	report.Reset()
	missing, _, err := verifyStorage(ctx, database, objects, &report)
	if err != nil || missing != 1 || !strings.Contains(report.String(), "missing "+lost) || !strings.Contains(report.String(), "drill/demo") {
		t.Fatalf("verify-storage after losing %s = %d missing, %v: %q", lost, missing, err, report.String())
	}
}
