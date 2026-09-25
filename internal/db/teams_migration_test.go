package db

import (
	"os"
	"strings"
	"testing"
)

const teamsMigrationPath = "../migrate/sql/0019_add_teams.sql"

func readTeamsMigration(t *testing.T) string {
	t.Helper()
	contents, err := os.ReadFile(teamsMigrationPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func TestTeamsMigrationShape(t *testing.T) {
	migration := readTeamsMigration(t)

	for _, required := range []string{
		"BEGIN;",
		"SET LOCAL lock_timeout = '5s';",
		"SET LOCAL statement_timeout = '30s';",
		"ALTER TABLE users ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'person'",
		"CHECK (kind IN ('person', 'team'))",
		"CREATE TABLE IF NOT EXISTS team_members",
		"team_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE",
		"user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE",
		"added_by   uuid REFERENCES users(id) ON DELETE SET NULL",
		"created_at timestamptz NOT NULL DEFAULT now()",
		"CONSTRAINT team_members_pkey PRIMARY KEY (team_id, user_id)",
		"CONSTRAINT team_members_not_self CHECK (team_id <> user_id)",
		"CREATE INDEX IF NOT EXISTS team_members_user_idx ON team_members (user_id, team_id)",
		"CREATE TABLE IF NOT EXISTS team_audit",
		"CREATE INDEX IF NOT EXISTS team_audit_team_idx ON team_audit (team_id, created_at DESC)",
		"COMMIT;",
	} {
		if !strings.Contains(migration, required) {
			t.Errorf("migration does not contain %q", required)
		}
	}
}

// The three triggers are the guards that survive a rolled-back binary and a
// hand-written UPDATE, so each is asserted by function, by DROP and by CREATE:
// a function without its trigger is inert, and a CREATE without the DROP is
// not re-runnable.
func TestTeamsMigrationTriggers(t *testing.T) {
	migration := readTeamsMigration(t)

	for _, trigger := range []struct {
		function string
		name     string
		table    string
	}{
		{"teams_have_no_key", "users_teams_have_no_key", "users"},
		{"team_members_kinds", "team_members_kinds", "team_members"},
		{"users_kind_locked_by_membership", "users_kind_locked", "users"},
	} {
		for _, required := range []string{
			"CREATE OR REPLACE FUNCTION " + trigger.function + "() RETURNS trigger",
			"DROP TRIGGER IF EXISTS " + trigger.name + " ON " + trigger.table + ";",
			"CREATE TRIGGER " + trigger.name,
			"EXECUTE FUNCTION " + trigger.function + "()",
		} {
			if !strings.Contains(migration, required) {
				t.Errorf("trigger %s: migration does not contain %q", trigger.name, required)
			}
		}
	}
}

// Everything must be inside one transaction, and the two timeouts must be set
// before the first DDL or they protect nothing.
func TestTeamsMigrationIsOneGuardedTransaction(t *testing.T) {
	migration := readTeamsMigration(t)

	begin := strings.Index(migration, "BEGIN;")
	lockTimeout := strings.Index(migration, "SET LOCAL lock_timeout")
	statementTimeout := strings.Index(migration, "SET LOCAL statement_timeout")
	firstDDL := strings.Index(migration, "ALTER TABLE users")
	commit := strings.Index(migration, "COMMIT;")

	if begin < 0 || lockTimeout < 0 || statementTimeout < 0 || firstDDL < 0 || commit < 0 {
		t.Fatal("migration is missing one of BEGIN, the two timeouts, the first DDL, or COMMIT")
	}
	if !(begin < lockTimeout && lockTimeout < firstDDL) {
		t.Error("lock_timeout is not set between BEGIN and the first DDL")
	}
	if !(begin < statementTimeout && statementTimeout < firstDDL) {
		t.Error("statement_timeout is not set between BEGIN and the first DDL")
	}
	if commit < firstDDL {
		t.Error("COMMIT precedes the first DDL")
	}
}

// Re-runnability cannot be proven by reading a string — the rollout applies the
// file twice against a real Postgres for that. What this does catch is the
// cheap half: a CREATE that forgot its guard.
func TestTeamsMigrationStatementsAreGuarded(t *testing.T) {
	migration := readTeamsMigration(t)

	for _, line := range strings.Split(migration, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "CREATE TABLE "):
			if !strings.HasPrefix(trimmed, "CREATE TABLE IF NOT EXISTS ") {
				t.Errorf("unguarded statement: %q", trimmed)
			}
		case strings.HasPrefix(trimmed, "CREATE INDEX "):
			if !strings.HasPrefix(trimmed, "CREATE INDEX IF NOT EXISTS ") {
				t.Errorf("unguarded statement: %q", trimmed)
			}
		case strings.HasPrefix(trimmed, "ALTER TABLE ") && strings.Contains(trimmed, "ADD COLUMN"):
			if !strings.Contains(trimmed, "ADD COLUMN IF NOT EXISTS") {
				t.Errorf("unguarded statement: %q", trimmed)
			}
		case strings.HasPrefix(trimmed, "CREATE FUNCTION "):
			t.Errorf("function is not CREATE OR REPLACE: %q", trimmed)
		}
	}

	// An ADD CONSTRAINT has no IF NOT EXISTS form, so it is made re-runnable
	// by dropping first. If one is added without its DROP, this catches it.
	adds := strings.Count(migration, "ADD CONSTRAINT")
	drops := strings.Count(migration, "DROP CONSTRAINT IF EXISTS")
	if adds != drops {
		t.Errorf("%d ADD CONSTRAINT but %d DROP CONSTRAINT IF EXISTS; each added constraint needs its drop to stay re-runnable", adds, drops)
	}
}

// There is no role column, and that is a decision rather than an omission:
// teams have exactly one role. A later reintroduction should be a
// deliberate migration, not a quiet edit to this file.
func TestTeamsMigrationHasNoRoleColumn(t *testing.T) {
	migration := readTeamsMigration(t)

	if strings.Contains(migration, "role       text") || strings.Contains(migration, "team_members_role_check") {
		t.Error("team_members has a role column; teams have one role by design")
	}
}
