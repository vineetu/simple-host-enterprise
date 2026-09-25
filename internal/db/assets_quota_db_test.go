package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestCreateAssetWithinQuotaLimits(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	userID, siteID := mustCreateUserAndSite(t, database, "alice", "demo")
	_, otherSiteID := mustCreateUserAndSite(t, database, "bob", "other")
	sum := make([]byte, 32)

	create := func(id, site string, size, maxCount, maxBytes int64) error {
		t.Helper()
		tx, err := database.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := CreateAssetWithinQuota(ctx, tx, id, site, "f", "text/plain", size, sum, &userID, maxCount, maxBytes); err != nil {
			return err
		}
		return tx.Commit()
	}

	// Count limit: two fit, the third does not.
	if err := create("10000000-0000-4000-8000-000000000001", siteID, 10, 2, 1000); err != nil {
		t.Fatal(err)
	}
	if err := create("10000000-0000-4000-8000-000000000002", siteID, 10, 2, 1000); err != nil {
		t.Fatal(err)
	}
	if err := create("10000000-0000-4000-8000-000000000003", siteID, 10, 2, 1000); !errors.Is(err, ErrAssetQuotaExceeded) {
		t.Fatalf("third asset under a count of 2: %v, want ErrAssetQuotaExceeded", err)
	}
	// Byte limit: 20 used; 80 more fits exactly, 81 does not.
	if err := create("10000000-0000-4000-8000-000000000004", siteID, 81, 10, 100); !errors.Is(err, ErrAssetQuotaExceeded) {
		t.Fatalf("81 bytes over 20 of 100: %v, want ErrAssetQuotaExceeded", err)
	}
	if err := create("10000000-0000-4000-8000-000000000005", siteID, 80, 10, 100); err != nil {
		t.Fatalf("exactly at the byte limit: %v", err)
	}
	// Soft-deleted assets stop counting.
	if err := SoftDeleteAsset(ctx, database, siteID, "10000000-0000-4000-8000-000000000005"); err != nil {
		t.Fatal(err)
	}
	if err := create("10000000-0000-4000-8000-000000000006", siteID, 80, 10, 100); err != nil {
		t.Fatalf("after soft delete: %v", err)
	}
	// Quota is per site.
	if err := create("10000000-0000-4000-8000-000000000007", otherSiteID, 100, 1, 100); err != nil {
		t.Fatalf("other site: %v", err)
	}
	usage, err := SumAssetUsage(ctx, database, siteID)
	if err != nil || usage.Count != 3 || usage.Bytes != 100 {
		t.Fatalf("usage = %+v, %v; want 3 assets, 100 bytes", usage, err)
	}
}

// Two concurrent transactions each fitting on their own must not both commit
// when together they exceed the limit: the second waits on the per-site lock
// and then sees the first's row.
func TestCreateAssetWithinQuotaSerializesConcurrentUploads(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	_, siteID := mustCreateUserAndSite(t, database, "alice", "demo")
	sum := make([]byte, 32)

	first, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback()
	if _, err := CreateAssetWithinQuota(ctx, first, "20000000-0000-4000-8000-000000000001", siteID, "a", "text/plain", 60, sum, nil, 10, 100); err != nil {
		t.Fatal(err)
	}

	second, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback()
	result := make(chan error, 1)
	go func() {
		_, err := CreateAssetWithinQuota(ctx, second, "20000000-0000-4000-8000-000000000002", siteID, "b", "text/plain", 60, sum, nil, 10, 100)
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("second upload did not wait for the first transaction: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrAssetQuotaExceeded) {
		t.Fatalf("second upload error = %v, want ErrAssetQuotaExceeded", err)
	}
	second.Rollback()
	var count int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM site_assets WHERE site_id = $1`, siteID).Scan(&count); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("site has %d asset rows, want 1", count)
	}
}
