package migrate

import (
	"context"
	"testing"
)

// 0045 adds the pending-grant tables for the application role, and is
// additive only.
func TestPendingGrantsMigration(t *testing.T) {
	conn, m := freshThrough(t, 45)
	ctx := context.Background()
	if err := applyOne(ctx, conn, m); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"pending_site_viewers", "pending_team_members"} {
		var privileges string
		if err := conn.QueryRowContext(ctx, `SELECT string_agg(privilege_type, ',' ORDER BY privilege_type) FROM information_schema.role_table_grants
			WHERE table_name = $1 AND grantee = 'simplehost_app'`, table).Scan(&privileges); err != nil || privileges != "DELETE,INSERT,SELECT" {
			t.Errorf("%s: simplehost_app privileges = %q (%v)", table, privileges, err)
		}
	}
	if !m.BackwardCompatible() {
		t.Error("0045 only adds tables; it must be marked backward-compatible")
	}
	if _, err := conn.ExecContext(ctx, m.body); err != nil {
		t.Errorf("re-run: %v", err)
	}
}
