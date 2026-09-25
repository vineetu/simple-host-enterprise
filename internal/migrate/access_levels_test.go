package migrate

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/lib/pq"
)

// TestAccessLevelsMigrationKeepsExistingLinks applies the chain up to 0032,
// creates the three kinds of site that existed before access levels (with
// viewers, listed, and neither), applies 0033, and checks each landed at the
// level that keeps its shared link working, while a new site defaults to
// only_me.
func TestAccessLevelsMigrationKeepsExistingLinks(t *testing.T) {
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
	name := "access_mig_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(`CREATE DATABASE ` + pq.QuoteIdentifier(name)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + pq.QuoteIdentifier(name) + ` WITH (FORCE)`) })
	parsed.Path = "/" + name
	db, err := sql.Open("postgres", parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE schema_migrations (version integer PRIMARY KEY, name text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now(), backward_compatible boolean NOT NULL DEFAULT false)`); err != nil {
		t.Fatal(err)
	}
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	var m33 Migration
	for _, m := range all {
		if m.Version == 33 {
			m33 = m
			break
		}
		if err := applyOne(ctx, conn, m); err != nil {
			t.Fatal(err)
		}
	}
	if m33.Version != 33 {
		t.Fatal("migration 0033 not embedded")
	}

	var alice, bob string
	for username, id := range map[string]*string{"alice": &alice, "bob": &bob} {
		if err := conn.QueryRowContext(ctx, `INSERT INTO users (username, oidc_sub) VALUES ($1, $1) RETURNING id::text`, username).Scan(id); err != nil {
			t.Fatal(err)
		}
	}
	siteIDs := map[string]string{}
	for site, public := range map[string]bool{"restricted": true, "listed": true, "unlisted": false} {
		var id string
		if err := conn.QueryRowContext(ctx, `INSERT INTO sites (user_id, name, public) VALUES ($1, $2, $3) RETURNING id::text`, alice, site, public).Scan(&id); err != nil {
			t.Fatal(err)
		}
		siteIDs[site] = id
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO site_viewers (site_id, principal_id) VALUES ($1, $2)`, siteIDs["restricted"], bob); err != nil {
		t.Fatal(err)
	}

	if err := applyOne(ctx, conn, m33); err != nil {
		t.Fatal(err)
	}

	for site, want := range map[string]struct {
		access string
		public bool
	}{"restricted": {"specific", false}, "listed": {"listed", true}, "unlisted": {"company", false}} {
		var access string
		var public bool
		if err := conn.QueryRowContext(ctx, `SELECT access, public FROM sites WHERE id = $1`, siteIDs[site]).Scan(&access, &public); err != nil {
			t.Fatal(err)
		}
		if access != want.access || public != want.public {
			t.Errorf("%s: access=%q public=%v, want %q %v", site, access, public, want.access, want.public)
		}
	}
	var access string
	if err := conn.QueryRowContext(ctx, `INSERT INTO sites (user_id, name) VALUES ($1, 'fresh') RETURNING access`, alice).Scan(&access); err != nil {
		t.Fatal(err)
	}
	if access != "only_me" {
		t.Errorf("new site access = %q, want only_me", access)
	}
	var editors sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT to_regclass('site_collaborators')::text`).Scan(&editors); err != nil || editors.Valid {
		t.Errorf("site_collaborators still exists (%v, %v)", editors, err)
	}
	if m33.BackwardCompatible() {
		t.Error("0033 drops a table and a column; it must not be marked backward-compatible")
	}
}
