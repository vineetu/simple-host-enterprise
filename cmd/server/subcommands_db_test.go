package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/lib/pq"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/migrate"
)

// operatorTestDB is a fresh, fully migrated database on the server
// MIGRATE_TEST_DSN names; plain `go test` skips.
func operatorTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MIGRATE_TEST_DSN")
	if dsn == "" {
		t.Skip("MIGRATE_TEST_DSN not set")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminDSN := *parsed
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("postgres", adminDSN.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	name := "operator_run_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(`CREATE DATABASE ` + pq.QuoteIdentifier(name)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("postgres", adminDSN.String())
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(`DROP DATABASE IF EXISTS ` + pq.QuoteIdentifier(name) + ` WITH (FORCE)`)
	})
	testDSN := *parsed
	testDSN.Path = "/" + name
	database, err := sql.Open("postgres", testDSN.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := migrate.Apply(context.Background(), database, 0, nil); err != nil {
		t.Fatal(err)
	}
	return database
}

// reencrypt and migrate-storage record one chained system event per run,
// with their counts, like restore.
func TestRecordOperatorRun(t *testing.T) {
	database := operatorTestDB(t)
	ctx := context.Background()
	if err := recordOperatorRun(ctx, database, "storage_reencrypt", map[string]any{"scanned": 7, "rewritten": 5, "failed": 0}); err != nil {
		t.Fatal(err)
	}
	var actorKind, rewritten string
	var actor sql.NullString
	if err := database.QueryRow(`SELECT actor_kind, actor_id::text, detail->>'rewritten' FROM audit_events WHERE action = 'storage_reencrypt'`).Scan(&actorKind, &actor, &rewritten); err != nil {
		t.Fatal(err)
	}
	if actorKind != "system" || actor.Valid || rewritten != "5" {
		t.Fatalf("event = %s %v rewritten=%s; want a system event with the counts", actorKind, actor, rewritten)
	}
	report, err := audit.VerifyChain(ctx, database, audit.ChainExpectation{})
	if err != nil || report.Break != nil || report.Rows != 1 {
		t.Fatalf("chain = %+v, %v; want the event chained", report, err)
	}
	database.Close()
	if err := recordOperatorRun(ctx, database, "storage_migrate", nil); err == nil {
		t.Fatal("a failed audit write must be reported so the command exits non-zero")
	}
}
