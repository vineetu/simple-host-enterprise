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
		"sc.user_id = $1::uuid",
		"tm.team_id = s.user_id",
		"tm.user_id = $1::uuid",
		"s.user_id = $1::uuid OR sc.user_id IS NOT NULL OR tm.user_id IS NOT NULL",
		"WHEN s.user_id = $1::uuid THEN 'owner'",
		"WHEN tm.user_id IS NOT NULL THEN 'member'",
		"ELSE 'editor'",
	}
	for _, check := range checks {
		if !strings.Contains(resolveSiteAccessQuery, check) {
			t.Errorf("access query missing %q:\n%s", check, resolveSiteAccessQuery)
		}
	}
}

// Membership must be tested before the editor fallback, so that a person who
// is both a member of the owning team and an editor on one of its sites
// resolves as a member. Ordering inside a SQL CASE is the whole rule, and it
// is invisible unless something asserts on it.
func TestResolveSiteAccessPrefersMembershipOverEditorGrant(t *testing.T) {
	member := strings.Index(resolveSiteAccessQuery, "THEN 'member'")
	editor := strings.Index(resolveSiteAccessQuery, "ELSE 'editor'")
	if member < 0 || editor < 0 {
		t.Fatalf("access query does not branch on both member and editor:\n%s", resolveSiteAccessQuery)
	}
	if member > editor {
		t.Errorf("editor is decided before member, so a member who is also an editor resolves as an editor:\n%s", resolveSiteAccessQuery)
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

func TestListAccessibleSitesContainsOwnedAndSharedWithoutLegacyQueryChanges(t *testing.T) {
	checks := []string{
		"s.user_id = $1::uuid OR sc.user_id IS NOT NULL OR tm.user_id IS NOT NULL",
		"sc.site_id = s.id",
		"sc.user_id = $1::uuid",
		"owner.username",
		"WHEN s.user_id = $1::uuid THEN 'owner'",
		"WHEN tm.user_id IS NOT NULL THEN 'member'",
		"ELSE 'editor'",
	}
	for _, check := range checks {
		if !strings.Contains(listAccessibleSitesQuery, check) {
			t.Errorf("accessible-sites query missing %q:\n%s", check, listAccessibleSitesQuery)
		}
	}
}

func TestCandidateSearchIsLiteralOwnerExcludedAndCapped(t *testing.T) {
	if strings.Contains(strings.ToUpper(searchEditorCandidatesQuery), " ILIKE ") ||
		strings.Contains(strings.ToUpper(searchEditorCandidatesQuery), " LIKE ") {
		t.Fatalf("candidate search treats user input as a SQL pattern:\n%s", searchEditorCandidatesQuery)
	}
	checks := []string{
		"strpos(lower(u.username), lower($2)) > 0",
		"u.id <> s.user_id",
		"LIMIT $3",
		"EXISTS (",
	}
	for _, check := range checks {
		if !strings.Contains(searchEditorCandidatesQuery, check) {
			t.Errorf("candidate query missing %q:\n%s", check, searchEditorCandidatesQuery)
		}
	}
}

func TestCandidateLimitNormalization(t *testing.T) {
	for _, input := range []int{-1, 0, 21, 200} {
		q := &captureQueryQuerier{err: sql.ErrConnDone}
		_, _ = SearchEditorCandidates(context.Background(), q, "site-id", "term", input)
		if len(q.args) != 3 || q.args[2] != 20 {
			t.Fatalf("limit %d produced args %#v, want cap 20", input, q.args)
		}
	}

	q := &captureQueryQuerier{err: sql.ErrConnDone}
	_, _ = SearchEditorCandidates(context.Background(), q, "site-id", "100%_literal", 7)
	if len(q.args) != 3 || q.args[1] != "100%_literal" || q.args[2] != 7 {
		t.Fatalf("bounded literal search args = %#v", q.args)
	}
}

func TestMembershipQueriesPreserveInvariants(t *testing.T) {
	if !strings.Contains(grantSiteEditorsQuery, "ON CONFLICT (site_id, user_id) DO NOTHING") {
		t.Fatalf("grant is not idempotent:\n%s", grantSiteEditorsQuery)
	}
	if !strings.Contains(grantSiteEditorsQuery, "unnest($2::uuid[])") {
		t.Fatalf("grant is not a bounded batch insert:\n%s", grantSiteEditorsQuery)
	}
	if !strings.Contains(revokeSiteEditorQuery, "site_id = $1::uuid") ||
		!strings.Contains(revokeSiteEditorQuery, "member.username = $2") {
		t.Fatalf("revoke is not resource and editor scoped:\n%s", revokeSiteEditorQuery)
	}
	if !strings.Contains(countSiteCollaboratorsQuery, "site_id = $1::uuid") {
		t.Fatalf("count is not site scoped:\n%s", countSiteCollaboratorsQuery)
	}
}

func TestMembershipMutationsRequireTransactions(t *testing.T) {
	var grantSignature func(context.Context, *sql.Tx, string, string, string, *string, []string) ([]SiteEditor, error) = GrantSiteEditors
	var revokeSignature func(context.Context, *sql.Tx, string, string, string, string) (bool, error) = RevokeSiteEditor
	_, _ = grantSignature, revokeSignature

	if _, err := GrantSiteEditors(context.Background(), nil, "owner", "site", "site-id", nil, []string{"editor"}); err == nil {
		t.Fatal("nil grant transaction unexpectedly accepted")
	}
	if _, err := RevokeSiteEditor(context.Background(), nil, "owner", "site", "site-id", "editor"); err == nil {
		t.Fatal("nil revoke transaction unexpectedly accepted")
	}
}

func TestEditorUsernameBatchIsDeterministicAndStrict(t *testing.T) {
	got, err := normalizeEditorUsernames([]string{" Bob ", "alice", "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "alice,bob" {
		t.Fatalf("normalized usernames = %#v", got)
	}
	if _, err := normalizeEditorUsernames([]string{"alice", " "}); !errors.Is(err, ErrEditorNotFound) {
		t.Fatalf("blank username error = %v, want ErrEditorNotFound", err)
	}
	if MaxSiteEditors != 50 {
		t.Fatalf("MaxSiteEditors = %d, want 50", MaxSiteEditors)
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
