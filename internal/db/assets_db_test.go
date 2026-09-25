package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/lib/pq"

	"github.com/vsriram/simple-host/internal/migrate"
)

// assetsTestDB applies the full embedded migration chain to a fresh,
// throwaway database on the same server MIGRATE_TEST_DSN names, and
// returns a connection to it. CI and `make test-db` set this env var
// (internal/migrate's own TestApplyAgainstPostgres uses it too); plain
// `go test` skips these cases the same way that test does.
//
// A dedicated database per test, not the shared "public" schema
// TestApplyAgainstPostgres uses: `go test ./...` runs different packages'
// test binaries concurrently, so an assets test and internal/migrate's own
// test racing DROP SCHEMA public against the same live database would
// otherwise corrupt each other. A fresh CREATE DATABASE also sidesteps a
// subtler problem a shared-schema approach would hit: pgcrypto (migration
// 0022) is installed once per database, so a second isolated schema on an
// already-migrated database would find CREATE EXTENSION IF NOT EXISTS a
// no-op and its digest() function unreachable outside the schema it was
// first installed into.
func assetsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MIGRATE_TEST_DSN")
	if dsn == "" {
		t.Skip("MIGRATE_TEST_DSN not set")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse MIGRATE_TEST_DSN: %v", err)
	}

	adminDSN := *parsed
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("postgres", adminDSN.String())
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()

	dbName := "assets_test_" + randomHex(t, 8)
	if _, err := admin.Exec(`CREATE DATABASE ` + pq.QuoteIdentifier(dbName)); err != nil {
		t.Fatalf("create test database %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		cleanupAdmin, err := sql.Open("postgres", adminDSN.String())
		if err != nil {
			t.Logf("cleanup: open admin connection: %v", err)
			return
		}
		defer cleanupAdmin.Close()
		if _, err := cleanupAdmin.Exec(`DROP DATABASE IF EXISTS ` + pq.QuoteIdentifier(dbName) + ` WITH (FORCE)`); err != nil {
			t.Logf("cleanup: drop test database %s: %v", dbName, err)
		}
	})

	testDSN := *parsed
	testDSN.Path = "/" + dbName
	database, err := sql.Open("postgres", testDSN.String())
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	ctx := context.Background()
	if _, err := migrate.Apply(ctx, database, 0, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return database
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generate random suffix: %v", err)
	}
	return hex.EncodeToString(b)
}

func mustCreateUserAndSite(t *testing.T, database *sql.DB, username, siteName string) (userID, siteID string) {
	t.Helper()
	ctx := context.Background()
	// Phase 2 dropped users.api_key and db.CreateUser along with it; every
	// account is now OIDC-bound from creation (identity.go's CreateOIDCUser
	// doc comment). The sub and email are test-only fixtures with no bearing
	// on what this file actually exercises (asset rows and their site_id/
	// created_by foreign keys).
	user, err := CreateOIDCUser(ctx, database, username, "sub-"+username, username+"@example.com", false)
	if err != nil {
		t.Fatalf("CreateOIDCUser: %v", err)
	}
	site, err := CreateSite(ctx, database, user.ID, siteName)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	return user.ID, site.ID
}

func TestAssetsCRUD(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	userID, siteID := mustCreateUserAndSite(t, database, "alice", "demo")

	sum := make([]byte, 32)
	for i := range sum {
		sum[i] = byte(i)
	}

	created, err := CreateAsset(ctx, database, "11111111-1111-4111-8111-111111111111", siteID, "logo.png", "image/png", 1234, sum, &userID)
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	if created.ID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("CreateAsset did not preserve the caller-supplied id: got %q", created.ID)
	}
	if created.Name != "logo.png" || created.ContentType != "image/png" || created.Size != 1234 {
		t.Fatalf("CreateAsset row mismatch: %+v", created)
	}
	if created.CreatedBy == nil || *created.CreatedBy != userID {
		t.Fatalf("CreateAsset CreatedBy = %v, want %s", created.CreatedBy, userID)
	}
	if created.DeletedAt != nil {
		t.Fatalf("freshly created asset has DeletedAt set: %v", created.DeletedAt)
	}

	got, err := GetAsset(ctx, database, siteID, created.ID)
	if err != nil {
		t.Fatalf("GetAsset: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("GetAsset returned a different row: %+v", got)
	}

	// A site cannot fetch another site's asset by id.
	_, otherSiteID := mustCreateUserAndSite(t, database, "bob", "demo")
	if _, err := GetAsset(ctx, database, otherSiteID, created.ID); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("cross-site GetAsset error = %v, want ErrAssetNotFound", err)
	}

	second, err := CreateAsset(ctx, database, "22222222-2222-4222-8222-222222222222", siteID, "data.csv", "text/csv", 42, sum, nil)
	if err != nil {
		t.Fatalf("second CreateAsset: %v", err)
	}

	list, err := ListAssets(ctx, database, siteID)
	if err != nil {
		t.Fatalf("ListAssets: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListAssets returned %d rows, want 2: %+v", len(list), list)
	}
	// Newest first.
	if list[0].ID != second.ID || list[1].ID != created.ID {
		t.Fatalf("ListAssets order = [%s, %s], want [%s, %s]", list[0].ID, list[1].ID, second.ID, created.ID)
	}

	usage, err := SumAssetUsage(ctx, database, siteID)
	if err != nil {
		t.Fatalf("SumAssetUsage: %v", err)
	}
	if usage.Count != 2 || usage.Bytes != 1234+42 {
		t.Fatalf("SumAssetUsage = %+v, want Count=2 Bytes=%d", usage, 1234+42)
	}

	if err := SoftDeleteAsset(ctx, database, siteID, created.ID); err != nil {
		t.Fatalf("SoftDeleteAsset: %v", err)
	}
	if _, err := GetAsset(ctx, database, siteID, created.ID); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("GetAsset after soft-delete error = %v, want ErrAssetNotFound", err)
	}
	if err := SoftDeleteAsset(ctx, database, siteID, created.ID); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("second SoftDeleteAsset error = %v, want ErrAssetNotFound", err)
	}

	list, err = ListAssets(ctx, database, siteID)
	if err != nil {
		t.Fatalf("ListAssets after soft-delete: %v", err)
	}
	if len(list) != 1 || list[0].ID != second.ID {
		t.Fatalf("ListAssets after soft-delete = %+v, want only %s", list, second.ID)
	}

	usage, err = SumAssetUsage(ctx, database, siteID)
	if err != nil {
		t.Fatalf("SumAssetUsage after soft-delete: %v", err)
	}
	if usage.Count != 1 || usage.Bytes != 42 {
		t.Fatalf("SumAssetUsage after soft-delete = %+v, want Count=1 Bytes=42", usage)
	}
}

func TestSumAssetUsageOnSiteWithNoAssets(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	_, siteID := mustCreateUserAndSite(t, database, "alice", "demo")

	usage, err := SumAssetUsage(ctx, database, siteID)
	if err != nil {
		t.Fatalf("SumAssetUsage: %v", err)
	}
	if usage.Count != 0 || usage.Bytes != 0 {
		t.Fatalf("SumAssetUsage on an empty site = %+v, want zero", usage)
	}
}

func TestAssetSiteCascadeDelete(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	_, siteID := mustCreateUserAndSite(t, database, "alice", "demo")

	sum := make([]byte, 32)
	if _, err := CreateAsset(ctx, database, "33333333-3333-4333-8333-333333333333", siteID, "logo.png", "image/png", 10, sum, nil); err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	if _, err := database.ExecContext(ctx, `DELETE FROM sites WHERE id = $1`, siteID); err != nil {
		t.Fatalf("delete site: %v", err)
	}
	var count int
	if err := database.QueryRowContext(ctx, `SELECT count(*) FROM site_assets WHERE site_id = $1`, siteID).Scan(&count); err != nil {
		t.Fatalf("count remaining assets: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected ON DELETE CASCADE to remove asset rows with their site, found %d", count)
	}
}
