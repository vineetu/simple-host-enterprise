package migrate

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 0057 holds renamed-away labels for the application role, which may read,
// add and release them but not update them (so the rename path must not
// use ON CONFLICT DO UPDATE); a hold survives the person's row; the label
// lock function is callable by the role.
func TestOwnerLabelRenameMigration(t *testing.T) {
	conn, m := freshThrough(t, 57)
	ctx := context.Background()
	if err := applyOne(ctx, conn, m); err != nil {
		t.Fatal(err)
	}
	var privileges string
	if err := conn.QueryRowContext(ctx, `SELECT string_agg(privilege_type, ',' ORDER BY privilege_type) FROM information_schema.role_table_grants
		WHERE table_name = 'renamed_owner_labels' AND grantee = 'simplehost_app'`).Scan(&privileges); err != nil || privileges != "DELETE,INSERT,SELECT" {
		t.Errorf("simplehost_app privileges = %q (%v)", privileges, err)
	}
	var canLock bool
	if err := conn.QueryRowContext(ctx, `SELECT has_function_privilege('simplehost_app', 'owner_label_lock(text)', 'EXECUTE')`).Scan(&canLock); err != nil || !canLock {
		t.Errorf("simplehost_app cannot run owner_label_lock (%v)", err)
	}
	// A hold outlives its person (an older binary's erasure) and still
	// refuses the label to everyone.
	alice := insertUser(t, conn, "alice", "person")
	if _, err := conn.ExecContext(ctx, `INSERT INTO renamed_owner_labels (owner_label, user_id) VALUES ('old-alice', $1)`, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, alice); err != nil {
		t.Fatal(err)
	}
	var held int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM renamed_owner_labels WHERE owner_label = 'old-alice' AND user_id IS NULL`).Scan(&held); err != nil || held != 1 {
		t.Fatalf("hold after the person's row went = %d (%v)", held, err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO users (username, kind) VALUES ('old-alice', 'person')`); err == nil || !strings.Contains(err.Error(), "held after a rename") {
		t.Fatalf("a new person took a held label: %v", err)
	}
	if !m.BackwardCompatible() {
		t.Error("0057 must be marked backward-compatible")
	}
	if _, err := conn.ExecContext(ctx, m.body); err != nil {
		t.Errorf("re-run: %v", err)
	}
}

// Postgres checks UPDATE privilege for every INSERT ... ON CONFLICT DO
// UPDATE, whether or not a row conflicts. Every such statement in the
// server's code must name a table the application role may update, or it
// fails in production (DB_APP_USER) while passing as the owner.
func TestUpsertTablesGrantUpdateToAppRole(t *testing.T) {
	conn, last := freshThrough(t, lastVersion(t))
	ctx := context.Background()
	if err := applyOne(ctx, conn, last); err != nil {
		t.Fatal(err)
	}
	upsert := regexp.MustCompile("(?is)INSERT\\s+INTO\\s+([a-z_]+)[^`]*?ON\\s+CONFLICT[^`]*?DO\\s+UPDATE")
	tables := map[string]string{}
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range upsert.FindAllStringSubmatch(string(body), -1) {
				tables[strings.ToLower(m[1])] = path
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(tables) == 0 {
		t.Fatal("found no upserts: the pattern is broken")
	}
	for table, path := range tables {
		var exists, ok bool
		if err := conn.QueryRowContext(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			continue // a temporary or partition-maintenance table, not the app's
		}
		if err := conn.QueryRowContext(ctx, `SELECT has_table_privilege('simplehost_app', 'public.' || $1, 'UPDATE')`, table).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Errorf("%s upserts into %s with DO UPDATE, but simplehost_app has no UPDATE on it", path, table)
		}
	}
}

func lastVersion(t *testing.T) int {
	t.Helper()
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	return all[len(all)-1].Version
}
