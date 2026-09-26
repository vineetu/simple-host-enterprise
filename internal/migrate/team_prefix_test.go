package migrate

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/lib/pq"
)

// freshThrough creates a throwaway database, applies every migration before
// version, and returns a connection to it and that migration.
func freshThrough(t *testing.T, version int) (*sql.Conn, Migration) {
	t.Helper()
	dsn := os.Getenv("MIGRATE_TEST_DSN")
	if dsn == "" {
		t.Skip("MIGRATE_TEST_DSN not set")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	name := "team_prefix_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(`CREATE DATABASE ` + pq.QuoteIdentifier(name)); err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	database, err := sql.Open("postgres", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := database.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
		database.Close()
		c, err := sql.Open("postgres", dsn)
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Exec(`DROP DATABASE IF EXISTS ` + pq.QuoteIdentifier(name) + ` WITH (FORCE)`)
	})
	if _, err := conn.ExecContext(ctx, `CREATE TABLE schema_migrations (version integer PRIMARY KEY, name text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now(), backward_compatible boolean NOT NULL DEFAULT false)`); err != nil {
		t.Fatal(err)
	}
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Version == version {
			return conn, m
		}
		if err := applyOne(ctx, conn, m); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatalf("migration %d not embedded", version)
	return nil, Migration{}
}

func insertUser(t *testing.T, conn *sql.Conn, username, kind string) string {
	t.Helper()
	var id string
	sub := sql.NullString{String: username, Valid: kind == "person"}
	if err := conn.QueryRowContext(context.Background(), `INSERT INTO users (username, oidc_sub, kind) VALUES ($1, $2, $3) RETURNING id::text`, username, sub, kind).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// 0041 renames every older team "team-<name>", leaves people and teams
// already prefixed alone, and keeps search documents' owner name in step.
func TestTeamPrefixMigrationRenamesTeams(t *testing.T) {
	conn, m41 := freshThrough(t, 41)
	ctx := context.Background()
	insertUser(t, conn, "alice", "person")
	insertUser(t, conn, "team-bob", "person")
	sales := insertUser(t, conn, "sales", "team")
	insertUser(t, conn, "team-ops", "team")
	var siteID string
	if err := conn.QueryRowContext(ctx, `INSERT INTO sites (user_id, name) VALUES ($1, 'board') RETURNING id::text`, sales).Scan(&siteID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO site_search_documents (site_id, version_number, owner_name, site_name, page_path, url_path, title, description, headings, body_text, indexed_at)
		VALUES ($1, 1, 'sales', 'board', 'index.html', '/', 't', '', '', '', now())`, siteID); err != nil {
		t.Fatal(err)
	}

	if err := applyOne(ctx, conn, m41); err != nil {
		t.Fatal(err)
	}

	for before, after := range map[string]string{"alice": "alice", "team-bob": "team-bob", "sales": "team-sales", "team-ops": "team-ops"} {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE username = $1`, after).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s: want exactly one %q after 0041 (%d, %v)", before, after, n, err)
		}
	}
	var owner string
	if err := conn.QueryRowContext(ctx, `SELECT owner_name FROM site_search_documents WHERE site_id = $1`, siteID).Scan(&owner); err != nil || owner != "team-sales" {
		t.Errorf("search owner_name = %q (%v), want team-sales", owner, err)
	}
	if !m41.BackwardCompatible() {
		t.Error("0041 only renames rows; it must be marked backward-compatible")
	}
}

// A team whose new name another account already holds stops the migration,
// naming both, and changes nothing.
func TestTeamPrefixMigrationRefusesCollision(t *testing.T) {
	conn, m41 := freshThrough(t, 41)
	insertUser(t, conn, "team-sales", "person")
	insertUser(t, conn, "sales", "team")
	err := applyOne(context.Background(), conn, m41)
	if err == nil || !strings.Contains(err.Error(), "sales -> team-sales") {
		t.Fatalf("applyOne = %v, want the collision named", err)
	}
	var n int
	if err := conn.QueryRowContext(context.Background(), `SELECT count(*) FROM users WHERE username = 'sales' AND kind = 'team'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("team renamed despite refusal (%d, %v)", n, err)
	}
}

// 0042 adds owner_hosts for the application role, and is additive only.
func TestOwnerHostsMigration(t *testing.T) {
	conn, m42 := freshThrough(t, 42)
	ctx := context.Background()
	if err := applyOne(ctx, conn, m42); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO owner_hosts (owner_label) VALUES ('alice')`); err != nil {
		t.Fatal(err)
	}
	var ready bool
	if err := conn.QueryRowContext(ctx, `SELECT ready FROM owner_hosts WHERE owner_label = 'alice'`).Scan(&ready); err != nil || ready {
		t.Errorf("default ready = %t (%v), want false", ready, err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO owner_hosts (owner_label) VALUES ('alice')`); err == nil {
		t.Error("duplicate owner label accepted")
	}
	var privileges string
	if err := conn.QueryRowContext(ctx, `SELECT string_agg(privilege_type, ',' ORDER BY privilege_type) FROM information_schema.role_table_grants
		WHERE table_name = 'owner_hosts' AND grantee = 'simplehost_app'`).Scan(&privileges); err != nil || privileges != "DELETE,INSERT,SELECT,UPDATE" {
		t.Errorf("simplehost_app privileges = %q (%v)", privileges, err)
	}
	if !m42.BackwardCompatible() {
		t.Error("0042 only adds a table; it must be marked backward-compatible")
	}
	// Re-running it is harmless.
	if _, err := conn.ExecContext(ctx, m42.body); err != nil {
		t.Errorf("re-run: %v", err)
	}
}
