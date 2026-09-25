package db

import (
	"os"
	"strings"
	"testing"
)

const siteAssetsMigrationPath = "../migrate/sql/0026_add_site_assets.sql"

func readSiteAssetsMigration(t *testing.T) string {
	t.Helper()
	contents, err := os.ReadFile(siteAssetsMigrationPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func TestSiteAssetsMigrationShape(t *testing.T) {
	migration := readSiteAssetsMigration(t)

	for _, required := range []string{
		"BEGIN;",
		"SET LOCAL lock_timeout = '5s';",
		"SET LOCAL statement_timeout = '30s';",
		"CREATE TABLE IF NOT EXISTS site_assets",
		"id           uuid PRIMARY KEY DEFAULT gen_random_uuid()",
		"site_id      uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE",
		"name         text NOT NULL",
		"content_type text NOT NULL",
		"size         bigint NOT NULL CHECK (size >= 0)",
		"sha256       bytea NOT NULL",
		"created_by   uuid REFERENCES users(id) ON DELETE SET NULL",
		"created_at   timestamptz NOT NULL DEFAULT now()",
		"deleted_at   timestamptz",
		"CREATE INDEX IF NOT EXISTS site_assets_site_idx ON site_assets (site_id, created_at DESC)",
		"WHERE deleted_at IS NULL",
		"GRANT SELECT, INSERT, UPDATE ON TABLE site_assets TO simplehost_app",
		"COMMIT;",
	} {
		if !strings.Contains(migration, required) {
			t.Errorf("migration does not contain %q", required)
		}
	}

	if strings.Index(migration, "BEGIN;") > strings.Index(migration, "CREATE TABLE IF NOT EXISTS site_assets") ||
		strings.Index(migration, "COMMIT;") < strings.Index(migration, "GRANT SELECT, INSERT, UPDATE ON TABLE site_assets") {
		t.Fatal("site_assets schema and its grant are not enclosed by the migration transaction")
	}

	// Least privilege, per this migration's own comment: the app role must
	// never get DELETE on this table, only SELECT/INSERT/UPDATE — deletion
	// is soft (an UPDATE of deleted_at).
	if strings.Contains(migration, "GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE site_assets") {
		t.Fatal("site_assets must not grant DELETE to simplehost_app")
	}
}
