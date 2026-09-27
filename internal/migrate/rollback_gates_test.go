package migrate

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"
)

// 0058 records 0047 and 0048 as not backward-compatible (a binary without
// them would not enforce an admin's restriction or an erasure), takes
// owner_label_lock away from PUBLIC while leaving it to the app role, and
// makes partition upkeep take its own lock before it reads anything.
func TestRollbackGatesAndUpkeepLockMigration(t *testing.T) {
	conn, m := freshThrough(t, 58)
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `UPDATE schema_migrations SET backward_compatible = true WHERE version IN (47, 48)`); err != nil {
		t.Fatal(err)
	}
	if err := applyOne(ctx, conn, m); err != nil {
		t.Fatal(err)
	}
	var compatible int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version IN (47, 48) AND backward_compatible`).Scan(&compatible); err != nil || compatible != 0 {
		t.Fatalf("0047/0048 still recorded compatible: %d (%v)", compatible, err)
	}
	var public, app bool
	if err := conn.QueryRowContext(ctx, `SELECT has_function_privilege('public', 'owner_label_lock(text)', 'EXECUTE'), has_function_privilege('simplehost_app', 'owner_label_lock(text)', 'EXECUTE')`).Scan(&public, &app); err != nil || public || !app {
		t.Fatalf("owner_label_lock EXECUTE: public=%v app=%v (%v)", public, app, err)
	}
	for _, name := range []string{"0047_site_access_decision.sql", "0048_person_erasure.sql"} {
		all, _ := All()
		for _, mig := range all {
			if mig.Name == name && mig.BackwardCompatible() {
				t.Errorf("%s is still marked backward-compatible", name)
			}
		}
	}
	if !m.BackwardCompatible() {
		t.Error("0058 must be marked backward-compatible")
	}

	// Upkeep holds its lock for the rest of its transaction, so a second
	// run waits before it reads a default partition.
	var name string
	if err := conn.QueryRowContext(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(os.Getenv("MIGRATE_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	second, err := sql.Open("postgres", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT audit_ensure_partitions(0)`); err != nil {
		t.Fatal(err)
	}
	var got bool
	if err := second.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtext('simple-host audit partition upkeep'))`).Scan(&got); err != nil || got {
		t.Fatalf("a second session got the upkeep lock while a run held it: %v (%v)", got, err)
	}
}
