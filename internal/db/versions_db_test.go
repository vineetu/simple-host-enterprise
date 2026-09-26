package db

import (
	"context"
	"testing"
)

func TestVersionExistsSeesOnlyCommittedRows(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	_, siteID := mustCreateUserAndSite(t, database, "alice", "demo")

	if _, err := CreateVersion(ctx, database, siteID, 1, "p1", nil); err != nil {
		t.Fatal(err)
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := CreateVersion(ctx, tx, siteID, 2, "p2", nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		version int
		want    bool
	}{{1, true}, {2, false}, {3, false}} {
		got, err := VersionExists(ctx, database, siteID, tc.version)
		if err != nil || got != tc.want {
			t.Errorf("VersionExists(v%d) = %v, %v; want %v", tc.version, got, err, tc.want)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, err := VersionExists(ctx, database, siteID, 2); err != nil || !got {
		t.Errorf("VersionExists(v2) after commit = %v, %v", got, err)
	}
}
