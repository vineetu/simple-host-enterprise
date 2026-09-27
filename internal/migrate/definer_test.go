package migrate

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"testing"
)

const safeSearchPath = "search_path=pg_catalog, public, pg_temp"

// latestFresh is a throwaway database with every migration applied, and the
// application role able to log in to it with password.
func latestFresh(t *testing.T, password string) (owner *sql.Conn, app *sql.DB) {
	t.Helper()
	latest, err := Latest()
	if err != nil {
		t.Fatal(err)
	}
	conn, last := freshThrough(t, latest)
	ctx := context.Background()
	if err := applyOne(ctx, conn, last); err != nil {
		t.Fatal(err)
	}
	verifier, err := scramSHA256Verifier(password)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "ALTER ROLE "+AppRoleName+" WITH LOGIN PASSWORD '"+verifier+"'"); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := conn.QueryRowContext(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(os.Getenv("MIGRATE_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	parsed.User = url.UserPassword(AppRoleName, password)
	parsed.Path = "/" + name
	app, err = sql.Open("postgres", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	// One connection, so the temporary objects below live in one session.
	app.SetMaxOpenConns(1)
	t.Cleanup(func() { app.Close() })
	return conn, app
}

// Every SECURITY DEFINER function pins a search_path with pg_temp last.
// Left out, pg_temp is searched first and a caller's temporary table
// shadows the function's own.
func TestDefinerFunctionsPinSearchPath(t *testing.T) {
	conn, _ := latestFresh(t, "definer-test-password")
	rows, err := conn.QueryContext(context.Background(), `
		SELECT p.proname, COALESCE(array_to_string(p.proconfig, ';'), '')
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public' AND p.prosecdef`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var name, config string
		if err := rows.Scan(&name, &config); err != nil {
			t.Fatal(err)
		}
		seen++
		if config != safeSearchPath {
			t.Errorf("%s: proconfig = %q, want %q", name, config, safeSearchPath)
		}
	}
	if seen < 4 {
		t.Errorf("found %d SECURITY DEFINER functions, want at least 4", seen)
	}
}

// The attack a review reproduced: the application role shadows access_log
// with a temporary table whose trigger runs as the function's owner inside
// access_log_erase_visitor, and deletes a table the role cannot touch.
func TestDefinerFunctionCannotBeHijackedThroughPgTemp(t *testing.T) {
	owner, app := latestFresh(t, "definer-test-password")
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE TABLE canary (id int)`,
		`INSERT INTO canary VALUES (1)`,
		`REVOKE ALL ON canary FROM PUBLIC`,
	} {
		if _, err := owner.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := app.ExecContext(ctx, `DELETE FROM canary`); err == nil {
		t.Fatal("the application role could delete the owner-only canary directly")
	}

	// First line: the application role cannot create temporary objects.
	if _, err := app.ExecContext(ctx, `CREATE TEMP TABLE access_log (user_id uuid)`); err == nil {
		t.Error("the application role could create a temporary table")
	}

	attack := func() error {
		for _, stmt := range []string{
			`CREATE TEMP TABLE IF NOT EXISTS access_log (user_id uuid)`,
			`CREATE OR REPLACE FUNCTION pg_temp.hijack() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN DELETE FROM public.canary; RETURN OLD; END $$`,
			`DROP TRIGGER IF EXISTS hijack ON pg_temp.access_log`,
			`CREATE TRIGGER hijack BEFORE DELETE ON pg_temp.access_log FOR EACH ROW EXECUTE FUNCTION pg_temp.hijack()`,
			`DELETE FROM pg_temp.access_log`,
			`INSERT INTO pg_temp.access_log VALUES ('00000000-0000-0000-0000-00000000abcd')`,
			`SELECT access_log_erase_visitor('00000000-0000-0000-0000-00000000abcd')`,
		} {
			if _, err := app.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	}
	canary := func() int {
		var n int
		if err := owner.QueryRowContext(ctx, `SELECT count(*) FROM canary`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Second line, tested on its own: with TEMP granted back (an install
	// whose database kept it some other way), the pinned search_path still
	// sends the function to public.access_log.
	if _, err := owner.ExecContext(ctx, `DO $$ BEGIN EXECUTE format('GRANT TEMPORARY ON DATABASE %I TO simplehost_app', current_database()); END $$`); err != nil {
		t.Fatal(err)
	}
	if err := attack(); err != nil {
		t.Fatalf("attack setup: %v", err)
	}
	if canary() != 1 {
		t.Fatal("the pg_temp trigger ran as the function owner and deleted the canary")
	}

	// The test would have caught the bug: against the function as 0048 first
	// shipped it (search_path = public, unqualified table) the same attack
	// succeeds.
	if _, err := owner.ExecContext(ctx, `CREATE OR REPLACE FUNCTION access_log_erase_visitor(p_user_id uuid)
		RETURNS bigint LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
		DECLARE removed bigint;
		BEGIN
			DELETE FROM access_log WHERE user_id = p_user_id;
			GET DIAGNOSTICS removed = ROW_COUNT;
			RETURN removed;
		END $$`); err != nil {
		t.Fatal(err)
	}
	// A new session, so the function is planned with the old setting.
	if _, err := app.ExecContext(ctx, `DISCARD ALL`); err != nil {
		t.Fatal(err)
	}
	if err := attack(); err != nil {
		t.Fatalf("attack against the old search_path: %v", err)
	}
	if canary() != 0 {
		t.Error("the attack did not reproduce against search_path = public; this test proves nothing")
	}
}

// access_log_erase_visitor deletes only an erased account's visits.
func TestAccessLogEraserRefusesALiveAccount(t *testing.T) {
	owner, app := latestFresh(t, "definer-test-password")
	ctx := context.Background()
	id := insertUser(t, owner, "alice", "person")
	if _, err := owner.ExecContext(ctx, `INSERT INTO access_log (user_id, owner_label, site_name, path, method, status) VALUES ($1, 'vera', 'notes', '/', 'GET', 200)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := app.ExecContext(ctx, `SELECT access_log_erase_visitor($1)`, id); err == nil {
		t.Fatal("the eraser deleted a live account's visits")
	}
	if _, err := owner.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var removed int
	if err := app.QueryRowContext(ctx, `SELECT access_log_erase_visitor($1)`, id).Scan(&removed); err != nil || removed != 1 {
		t.Fatalf("eraser after the account went = %d, %v; want 1", removed, err)
	}
}
