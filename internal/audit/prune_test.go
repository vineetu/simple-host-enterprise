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
	// current month and the months after it rolling forward.
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

// TestPruneRecoversRowsStrandedInTheDefaultPartition: when prune has not run
// for long enough that rows landed in the default partitions (this month's
// partitions missing, and a past month that never had one), the next prune
// moves them into their months' partitions unchanged instead of failing
// forever, the audit chain still verifies, and those months then age out
// on schedule.
func TestPruneRecoversRowsStrandedInTheDefaultPartition(t *testing.T) {
	db := openPruneTestDB(t)
	ctx := context.Background()
	month := time.Now().UTC().Format("2006_01")
	mustExec(t, db, `DROP TABLE audit_events_p`+month)
	mustExec(t, db, `DROP TABLE access_log_p`+month)

	mustExec(t, db, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_server_time`)
	mustExec(t, db, `INSERT INTO audit_events (at, actor_kind, action) VALUES ('2021-03-15 12:00:00+00', 'system', 'old')`)
	mustExec(t, db, `ALTER TABLE audit_events ENABLE TRIGGER audit_events_server_time`)
	r := newChainRecorder(db)
	r.Record(ctx, Event{ActorID: chainActor, Action: "key_mint"})
	r.Record(ctx, Event{ActorID: chainActor, Action: "key_revoke"})
	for _, at := range []string{"'2021-03-10 08:00:00+00'", "now()"} {
		mustExec(t, db, `INSERT INTO access_log (at, owner_label, site_name, path, method, status) VALUES (`+at+`, 'alice', 'demo', '/', 'GET', 200)`)
	}
	count := func(table string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("audit_events_default") != 3 || count("access_log_default") != 2 {
		t.Fatalf("setup: default partitions hold %d audit and %d access rows, want 3 and 2", count("audit_events_default"), count("access_log_default"))
	}

	keepAll := PruneOptions{AuditRetentionDays: 100000, AccessRetentionDays: 100000}
	for run := 0; run < 2; run++ {
		if _, err := Prune(ctx, db, keepAll); err != nil {
			t.Fatalf("prune run %d with rows in the default partitions: %v", run+1, err)
		}
	}
	if count("audit_events_default") != 0 || count("access_log_default") != 0 {
		t.Fatalf("default partitions still hold %d audit and %d access rows", count("audit_events_default"), count("access_log_default"))
	}
	if count("audit_events_p"+month) != 2 || count("audit_events_p2021_03") != 1 || count("access_log_p"+month) != 1 || count("access_log_p2021_03") != 1 {
		t.Fatal("stranded rows did not land in their months' partitions")
	}
	var at time.Time
	if err := db.QueryRow(`SELECT at FROM audit_events WHERE action = 'old'`).Scan(&at); err != nil || !at.Equal(time.Date(2021, 3, 15, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("moved row's at = %v, %v; want it unchanged", at, err)
	}
	ahead := time.Now().UTC().AddDate(0, partitionMonthsAhead, 0).Format("2006_01")
	if !partitionExists(t, db, "audit_events_p"+ahead) || !partitionExists(t, db, "access_log_p"+ahead) {
		t.Errorf("partitions %d months ahead (%s) were not created", partitionMonthsAhead, ahead)
	}
	if report := mustVerify(t, db); report.Break != nil || report.Rows != 3 {
		t.Fatalf("chain after the move: %+v (break %v), want 3 clean rows", report, report.Break)
	}
	// New rows still route to the moved month's partition.
	r.Record(ctx, Event{ActorID: chainActor, Action: "key_mint"})
	if count("audit_events_p"+month) != 3 || count("audit_events_default") != 0 {
		t.Fatal("a new event did not land in this month's partition")
	}

	// The recovered past month now ages out like any other.
	result, err := Prune(ctx, db, PruneOptions{AuditRetentionDays: 400, AccessRetentionDays: 90})
	if err != nil {
		t.Fatal(err)
	}
	if !containsPartition(result.Dropped, "audit_events", "audit_events_p2021_03") || !containsPartition(result.Dropped, "access_log", "access_log_p2021_03") || result.ChainTrimmed != 1 {
		t.Fatalf("prune after recovery = %+v, want both 2021-03 partitions dropped and 1 chain row trimmed", result)
	}
	if report := mustVerify(t, db); report.Break != nil || report.Rows != 3 {
		t.Fatalf("chain after pruning the recovered month: %+v (break %v)", report, report.Break)
	}
}
