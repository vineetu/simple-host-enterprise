package db

import (
	"os"
	"strings"
	"testing"
)

// TestAuditMigrationShape pins the load-bearing pieces of migration
// 0027_add_audit.sql that this package's queries and internal/audit's
// Recorder depend on, the same way TestSiteCollaborationMigrationShape
// pins 0012's. A change here that breaks one of these strings is exactly
// the kind of drift a query written against column names, not the schema
// object, would otherwise only catch against a live database.
func TestAuditMigrationShape(t *testing.T) {
	contents, err := os.ReadFile("../migrate/sql/0027_add_audit.sql")
	if err != nil {
		t.Fatal(err)
	}
	migration := string(contents)

	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS audit_events",
		"PARTITION BY RANGE (at)",
		"CREATE TABLE IF NOT EXISTS audit_events_default PARTITION OF audit_events DEFAULT",
		"CREATE TABLE IF NOT EXISTS access_log",
		"CREATE TABLE IF NOT EXISTS access_log_default PARTITION OF access_log DEFAULT",
		"CREATE OR REPLACE FUNCTION audit_ensure_partitions",
		"CREATE OR REPLACE FUNCTION audit_bump_state_write",
		"SECURITY DEFINER",
		"ON CONFLICT (site_id, actor_id, at) WHERE action = 'state_write'",
		"GRANT SELECT, INSERT ON TABLE audit_events TO simplehost_app",
		"GRANT SELECT, INSERT ON TABLE access_log TO simplehost_app",
		"GRANT EXECUTE ON FUNCTION audit_bump_state_write",
	} {
		if !strings.Contains(migration, required) {
			t.Errorf("migration does not contain %q", required)
		}
	}

	// The application role's grant on these two tables must never widen to
	// UPDATE or DELETE: audit rows are append-only at the database. Guard
	// against a future edit accidentally broadening the plain table grant,
	// as distinct from the narrow SECURITY DEFINER function's own UPDATE.
	for _, forbidden := range []string{
		"GRANT SELECT, INSERT, UPDATE ON TABLE audit_events",
		"GRANT SELECT, INSERT, DELETE ON TABLE audit_events",
		"GRANT ALL ON TABLE audit_events",
		"GRANT ALL ON TABLE access_log",
	} {
		if strings.Contains(migration, forbidden) {
			t.Errorf("migration grants the application role more than SELECT/INSERT via %q", forbidden)
		}
	}

	// team_audit is folded, not dropped, in this migration; the drop lands
	// one release later. See the migration's own comment and
	// docs/security-review.md's Deviated section for why.
	if strings.Contains(migration, "DROP TABLE team_audit") {
		t.Error("migration drops team_audit; internal/db/teams.go expects the drop to land in a later migration")
	}
	if !strings.Contains(migration, "INSERT INTO audit_events (at, actor_id, actor_kind, action, team_id, detail)") {
		t.Error("migration does not fold team_audit into audit_events")
	}
}
