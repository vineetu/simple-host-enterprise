package audit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/vsriram/simple-host/internal/migrate"
)

// openPruneTestDB applies the full embedded migration chain to a fresh,
// throwaway database on the server MIGRATE_TEST_DSN names, and returns a
// connection to it. CI and `make test-db` set this env var; plain `go test`
// skips these cases the same way internal/migrate's own
// TestApplyAgainstPostgres does. Prune needs a real Postgres because it
// reads pg_inherits and calls a plpgsql function — nothing here is
// faithfully mockable.
//
// A dedicated database per test, not internal/migrate's own
// DROP-SCHEMA-public-on-MIGRATE_TEST_DSN's-own-database convention:
// internal/db/assets_db_test.go's assetsTestDB found that running more than
// one MIGRATE_TEST_DSN-gated package's tests together races that shared
// DROP SCHEMA and corrupts both. This mirrors that fix exactly rather than
// reintroducing the same hazard here.
func openPruneTestDB(t *testing.T) *sql.DB {
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

	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("generate random suffix: %v", err)
	}
	dbName := "audit_prune_test_" + hex.EncodeToString(suffix)
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

// createHistoricalPartition adds a monthly partition audit_ensure_partitions
// would never create on its own (it only ever creates the current month and
// a few ahead), standing in for a partition old enough to have aged out of
// retention by the time a test's fixed Now runs.
func createHistoricalPartition(t *testing.T, db *sql.DB, parent, name string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`CREATE TABLE `+name+` PARTITION OF `+parent+` FOR VALUES FROM ('2020-01-01 00:00:00+00') TO ('2020-02-01 00:00:00+00')`)
	if err != nil {
		t.Fatalf("create historical partition %s: %v", name, err)
	}
}

func partitionExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRowContext(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func containsPartition(partitions []PrunedPartition, table, name string) bool {
	for _, p := range partitions {
		if p.Table == table && p.Partition == name {
			return true
		}
	}
	return false
}

func TestPruneDryRunListsWithoutDropping(t *testing.T) {
	db := openPruneTestDB(t)
	createHistoricalPartition(t, db, "audit_events", "audit_events_p2020_01")
	createHistoricalPartition(t, db, "access_log", "access_log_p2020_01")

	now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	result, err := Prune(context.Background(), db, PruneOptions{
		AuditRetentionDays: 400, AccessRetentionDays: 90, DryRun: true, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !containsPartition(result.WouldDrop, "audit_events", "audit_events_p2020_01") {
		t.Errorf("WouldDrop = %#v, want audit_events_p2020_01", result.WouldDrop)
	}
	if !containsPartition(result.WouldDrop, "access_log", "access_log_p2020_01") {
		t.Errorf("WouldDrop = %#v, want access_log_p2020_01", result.WouldDrop)
	}
	if len(result.Dropped) != 0 {
		t.Errorf("DryRun reported %d dropped partition(s), want 0", len(result.Dropped))
	}
	if !partitionExists(t, db, "audit_events_p2020_01") {
		t.Error("DryRun dropped audit_events_p2020_01; it should still exist")
	}
}

func TestPruneDropsExpiredPartitionsAndKeepsCurrentOnes(t *testing.T) {
	db := openPruneTestDB(t)
	createHistoricalPartition(t, db, "audit_events", "audit_events_p2020_01")

	now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	result, err := Prune(context.Background(), db, PruneOptions{
		AuditRetentionDays: 400, AccessRetentionDays: 90, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !containsPartition(result.Dropped, "audit_events", "audit_events_p2020_01") {
		t.Fatalf("Dropped = %#v, want audit_events_p2020_01", result.Dropped)
	}
	if partitionExists(t, db, "audit_events_p2020_01") {
		t.Error("audit_events_p2020_01 still exists after Prune")
	}

	// Prune's own audit_ensure_partitions bootstrap must have kept the
	// current month and the next two rolling forward, matching migration
	// 0027's own bootstrap call.
	for _, name := range []string{"audit_events_p2026_09", "audit_events_p2026_10", "audit_events_p2026_11"} {
		if !partitionExists(t, db, name) {
			t.Errorf("expected rolling partition %s to exist after Prune", name)
		}
	}
	if !partitionExists(t, db, "audit_events_default") {
		t.Error("the default partition must never be dropped by Prune")
	}
}

func TestPruneRequiresPositiveRetention(t *testing.T) {
	db := openPruneTestDB(t)
	if _, err := Prune(context.Background(), db, PruneOptions{AuditRetentionDays: 0, AccessRetentionDays: 90}); err == nil {
		t.Fatal("Prune accepted a zero AuditRetentionDays")
	}
	if _, err := Prune(context.Background(), db, PruneOptions{AuditRetentionDays: 400, AccessRetentionDays: -1}); err == nil {
		t.Fatal("Prune accepted a negative AccessRetentionDays")
	}
}

func TestPruneRequiresDatabase(t *testing.T) {
	if _, err := Prune(context.Background(), nil, PruneOptions{AuditRetentionDays: 400, AccessRetentionDays: 90}); err == nil {
		t.Fatal("Prune(nil db) did not error")
	}
}
