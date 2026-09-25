package migrate

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"
)

func TestEmbeddedMigrationsAreOrderedAndUnique(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 || all[0].Version != 1 {
		t.Fatalf("migrations must start at 0001, got %+v", all)
	}
	for i := 1; i < len(all); i++ {
		if all[i].Version != all[i-1].Version+1 {
			t.Errorf("gap between %s and %s", all[i-1].Name, all[i].Name)
		}
	}
}

func TestVersionOf(t *testing.T) {
	for name, want := range map[string]int{"0001_initial_schema.sql": 1, "0017_add_user_email.sql": 17, "0100.sql": 100} {
		got, err := versionOf(name)
		if err != nil || got != want {
			t.Errorf("versionOf(%q) = %d, %v; want %d", name, got, err, want)
		}
	}
	for _, bad := range []string{"initial.sql", "0000_x.sql", "x_0001.sql"} {
		if _, err := versionOf(bad); err == nil {
			t.Errorf("versionOf(%q) accepted", bad)
		}
	}
}

// TestApplyAgainstPostgres runs the whole chain on a real database when
// MIGRATE_TEST_DSN is set; CI and `make test-db` set it, plain `go test` skips.
func TestApplyAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("MIGRATE_TEST_DSN")
	if dsn == "" {
		t.Skip("MIGRATE_TEST_DSN not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatal(err)
	}
	if err := Check(ctx, db); err == nil {
		t.Fatal("Check passed on an empty database")
	}
	applied, err := Apply(ctx, db, 0, nil)
	if err != nil {
		t.Fatalf("Apply: %v (applied %v)", err, applied)
	}
	all, _ := All()
	if len(applied) != len(all) {
		t.Fatalf("applied %d of %d", len(applied), len(all))
	}
	if err := Check(ctx, db); err != nil {
		t.Fatalf("Check after Apply: %v", err)
	}

	// Migration 0020 creates the app role with no password; the server's own
	// connection (design 9.3) depends on this step to make it usable at all.
	const appPassword = `o'Reilly "quotes" and a backslash \`
	if err := SetAppRolePassword(ctx, db, appPassword); err != nil {
		t.Fatalf("SetAppRolePassword: %v", err)
	}
	appDB, err := sql.Open("postgres", appRoleDSN(t, dsn, appPassword))
	if err != nil {
		t.Fatal(err)
	}
	defer appDB.Close()
	var count int
	if err := appDB.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("app role SELECT on users: %v", err)
	}
	if _, err := appDB.ExecContext(ctx, `CREATE TABLE should_not_be_creatable (id int)`); err == nil {
		t.Fatal("app role was able to CREATE TABLE; least-privilege grant is too broad")
	}
	if _, err := appDB.ExecContext(ctx, `TRUNCATE users`); err == nil {
		t.Fatal("app role was able to TRUNCATE; least-privilege grant is too broad")
	}
	if err := CheckLeastPrivilege(ctx, appDB); err != nil {
		t.Fatalf("CheckLeastPrivilege as the app role: %v", err)
	}
	if err := CheckLeastPrivilege(ctx, db); err == nil {
		t.Fatal("CheckLeastPrivilege accepted the owning role")
	}

	again, err := Apply(ctx, db, 0, nil)
	if err != nil || len(again) != 0 {
		t.Fatalf("second Apply = %v, %v; want nothing to do", again, err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name) VALUES (9999, 'from_the_future.sql')`); err != nil {
		t.Fatal(err)
	}
	// This test runs against MIGRATE_TEST_DSN's own database, not a
	// throwaway one, and leaving this row behind would make any later
	// `simple-host migrate` against that same database (or a person
	// running `-status` by hand) see version 9999 and report the schema
	// current when it is not — a real, previously-unfixed leak (review
	// finding, Phase 4 core). A defer, not t.Cleanup: t.Cleanup callbacks
	// run after this function returns, which is after its own
	// `defer db.Close()` above already ran (defers are LIFO within one
	// function), so a Cleanup here would find db already closed — this
	// defer, declared after that one, runs first.
	defer func() {
		if _, err := db.ExecContext(context.Background(), `DELETE FROM schema_migrations WHERE version = 9999`); err != nil {
			t.Errorf("cleanup: remove injected future-migration row: %v", err)
		}
	}()
	if err := Check(ctx, db); err == nil {
		t.Fatal("Check accepted a schema newer than the binary")
	}
	// A newer migration recorded as backward-compatible is what lets the
	// previous image start again after a rollback.
	if _, err := db.ExecContext(ctx, `UPDATE schema_migrations SET backward_compatible = true WHERE version = 9999`); err != nil {
		t.Fatal(err)
	}
	if err := Check(ctx, db); err != nil {
		t.Fatalf("Check refused a newer, backward-compatible schema: %v", err)
	}

	// A failing file leaves neither its changes nor its record behind.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	bad := Migration{Version: 9998, Name: "9998_bad.sql", body: "CREATE TABLE half_applied (id int);\nSELECT 1/0;"}
	if err := applyOne(ctx, conn, bad); err == nil {
		t.Fatal("applyOne succeeded on a failing file")
	}
	var leftovers int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM schema_migrations WHERE version = 9998) + (SELECT count(*) FROM pg_tables WHERE tablename = 'half_applied')`).Scan(&leftovers); err != nil {
		t.Fatal(err)
	}
	if leftovers != 0 {
		t.Fatal("a failed migration left its table or its record behind")
	}
}

// Every file runs inside the transaction Apply opens, so any transaction
// control left after stripping BEGIN;/COMMIT; would end that transaction early
// and record nothing atomically.
func TestMigrationsCarryNoOtherTransactionControl(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		for _, line := range strings.Split(withoutTransactionControl(m.body), "\n") {
			upper := strings.ToUpper(strings.TrimSpace(line))
			for _, control := range []string{"BEGIN;", "BEGIN TRANSACTION", "START TRANSACTION", "COMMIT", "END;", "END TRANSACTION", "ROLLBACK", "SAVEPOINT", "RELEASE SAVEPOINT"} {
				if strings.HasPrefix(upper, control) && !(control == "END;" && strings.Contains(m.body, "$$")) {
					t.Errorf("%s: %q; Apply already wraps each file in a transaction", m.Name, strings.TrimSpace(line))
				}
			}
		}
	}
}

func TestBackwardCompatibleMarker(t *testing.T) {
	if !(Migration{body: CompatibleMarker + "\nALTER TABLE x ADD COLUMN y int;"}).BackwardCompatible() {
		t.Error("marker on the first line was not recognised")
	}
	if (Migration{body: "ALTER TABLE x DROP COLUMN y;\n" + CompatibleMarker}).BackwardCompatible() {
		t.Error("marker off the first line was recognised")
	}
}

// appRoleDSN swaps the owner credentials in a working test DSN for
// AppRoleName and the given password, so the test can open a second
// connection and prove what that role can and cannot do.
func appRoleDSN(t *testing.T, ownerDSN, password string) string {
	t.Helper()
	parsed, err := url.Parse(ownerDSN)
	if err != nil {
		t.Fatalf("parse MIGRATE_TEST_DSN: %v", err)
	}
	parsed.User = url.UserPassword(AppRoleName, password)
	return parsed.String()
}
