package db

import (
	"os"
	"strings"
	"testing"
)

func TestSiteCollaborationMigrationShape(t *testing.T) {
	contents, err := os.ReadFile("../migrate/sql/0012_add_site_collaboration.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := string(contents)

	for _, required := range []string{
		"BEGIN;",
		"CREATE TABLE site_collaborators",
		"site_id    uuid NOT NULL REFERENCES sites(id) ON DELETE CASCADE",
		"user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE",
		"role       text NOT NULL DEFAULT 'editor'",
		"added_by   uuid REFERENCES users(id) ON DELETE SET NULL",
		"created_at timestamptz NOT NULL DEFAULT now()",
		"PRIMARY KEY (site_id, user_id)",
		"CHECK (role = 'editor')",
		"ON site_collaborators (user_id, site_id)",
		"ADD COLUMN uploaded_by uuid REFERENCES users(id) ON DELETE SET NULL",
		"UPDATE versions AS version",
		"SET uploaded_by = site.user_id",
		"FROM sites AS site",
		"WHERE site.id = version.site_id",
		"COMMIT;",
	} {
		if !strings.Contains(migration, required) {
			t.Errorf("migration does not contain %q", required)
		}
	}

	if strings.Index(migration, "BEGIN;") > strings.Index(migration, "CREATE TABLE site_collaborators") ||
		strings.Index(migration, "COMMIT;") < strings.Index(migration, "UPDATE versions AS version") {
		t.Fatal("collaboration schema and version backfill are not enclosed by the migration transaction")
	}
	if strings.Contains(migration, "uploaded_by uuid NOT NULL") {
		t.Fatal("versions.uploaded_by must remain nullable")
	}
}
