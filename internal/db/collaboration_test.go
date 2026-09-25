package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestResolveSiteAccessIsOwnerQualifiedAndDenyByDefault(t *testing.T) {
	checks := []string{
		"INNER JOIN users owner ON owner.id = s.user_id",
		"owner.username = $2",
		"s.name = $3",
		"tm.team_id = s.user_id",
		"tm.user_id = $1::uuid",
		"s.user_id = $1::uuid OR tm.user_id IS NOT NULL",
		"WHEN s.user_id = $1::uuid THEN 'owner'",
		"ELSE 'member'",
	}
	for _, check := range checks {
		if !strings.Contains(resolveSiteAccessQuery, check) {
			t.Errorf("access query missing %q:\n%s", check, resolveSiteAccessQuery)
		}
	}
	if strings.Contains(resolveSiteAccessQuery, "site_collaborators") || strings.Contains(listAccessibleSitesQuery, "site_collaborators") {
		t.Error("access queries still read per-site editor grants")
	}
}

// The team join must not be able to multiply rows. team_members is keyed on
// (team_id, user_id) and the join fixes both, so at most one row can match.
func TestAccessQueriesJoinTeamMembersOnBothKeyColumns(t *testing.T) {
	for name, query := range map[string]string{
		"resolveSiteAccessQuery":   resolveSiteAccessQuery,
		"listAccessibleSitesQuery": listAccessibleSitesQuery,
	} {
		if !strings.Contains(query, "tm.team_id = s.user_id") || !strings.Contains(query, "tm.user_id = $1::uuid") {
			t.Errorf("%s does not join team_members on both key columns:\n%s", name, query)
		}
		if strings.Contains(query, "tm.role") {
			t.Errorf("%s reads a role from team_members; teams have one role:\n%s", name, query)
		}
	}
}

func TestUsernameBatchIsDeterministicAndStrict(t *testing.T) {
	got, err := normalizeUsernames([]string{" Bob ", "alice", "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "alice,bob" {
		t.Fatalf("normalized usernames = %#v", got)
	}
	if _, err := normalizeUsernames([]string{"alice", " "}); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("blank username error = %v, want ErrUserNotFound", err)
	}
}

func TestAdvisoryLockIsTransactionScopedAndResourceKeyed(t *testing.T) {
	var signature func(context.Context, *sql.Tx, string, string) error = LockSiteCollaboration
	_ = signature
	if !strings.Contains(lockSiteCollaborationQuery, "pg_advisory_xact_lock") {
		t.Fatalf("lock is not transaction scoped:\n%s", lockSiteCollaborationQuery)
	}
	if !strings.Contains(lockSiteCollaborationQuery, "$1::uuid::text") ||
		!strings.Contains(lockSiteCollaborationQuery, "chr(31)") ||
		!strings.Contains(lockSiteCollaborationQuery, "$2") {
		t.Fatalf("lock is not keyed by owner UUID and site name:\n%s", lockSiteCollaborationQuery)
	}
	if err := LockSiteCollaboration(context.Background(), nil, "owner", "site"); err == nil {
		t.Fatal("nil transaction unexpectedly accepted")
	}
}

type captureQueryQuerier struct {
	query string
	args  []any
	err   error
}

func (q *captureQueryQuerier) QueryContext(_ context.Context, query string, args ...any) (*sql.Rows, error) {
	q.query = query
	q.args = args
	return nil, q.err
}

func (q *captureQueryQuerier) QueryRowContext(context.Context, string, ...any) *sql.Row {
	panic("unexpected QueryRowContext")
}

func (q *captureQueryQuerier) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	panic("unexpected ExecContext")
}
