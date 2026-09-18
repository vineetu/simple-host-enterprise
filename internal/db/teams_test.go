package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// teamQueries is every statement this file ships. The shape assertions below
// run over the whole set rather than a hand-picked few, so a query added later
// inherits the same invariants instead of quietly opting out of them.
var teamQueries = map[string]string{
	"createTeamQuery":              createTeamQuery,
	"addTeamCreatorQuery":          addTeamCreatorQuery,
	"listTeamsForUserQuery":        listTeamsForUserQuery,
	"listTeamMembersQuery":         listTeamMembersQuery,
	"isTeamMemberQuery":            isTeamMemberQuery,
	"countTeamMembersQuery":        countTeamMembersQuery,
	"resolveRequestedMembersQuery": resolveRequestedMembersQuery,
	"addTeamMembersQuery":          addTeamMembersQuery,
	"removeTeamMemberQuery":        removeTeamMemberQuery,
	"lockTeamQuery":                lockTeamQuery,
	"countTeamSitesQuery":          countTeamSitesQuery,
	"deleteTeamQuery":              deleteTeamQuery,
	"resolveNamespaceAccessQuery":  resolveNamespaceAccessQuery,
	"teamAuditQuery":               teamAuditQuery,
}

func TestTeamQueriesNameTheirColumns(t *testing.T) {
	for name, query := range teamQueries {
		if strings.Contains(query, "SELECT *") || strings.Contains(query, "select *") {
			t.Errorf("%s selects every column; name them so a schema change is a compile-time decision:\n%s", name, query)
		}
	}
}

// There is no role column on team_members, and nothing in this package may
// behave as though there is. A query that starts filtering or returning one is
// the first half of a second source of truth.
func TestTeamQueriesHaveNoMembershipRole(t *testing.T) {
	for name, query := range teamQueries {
		if strings.Contains(strings.ToLower(query), "role") {
			t.Errorf("%s references a role; teams have exactly one role by design:\n%s", name, query)
		}
	}
}

// A membership row must link a team to a person. The trigger enforces it, but
// resolving a name that is not a person would turn a refused insert into a
// batch-wide ErrTeamMemberNotFound for the wrong reason, and a team resolved
// as a member would be a nested namespace.
func TestAddTeamMembersResolvesPeopleOnly(t *testing.T) {
	for _, query := range []string{addTeamMembersQuery, resolveRequestedMembersQuery} {
		if !strings.Contains(query, "candidate.kind = 'person'") {
			t.Errorf("membership resolution does not restrict to people:\n%s", query)
		}
		if !strings.Contains(query, "candidate.username = ANY($2::text[])") {
			t.Errorf("membership resolution is not a bounded username batch:\n%s", query)
		}
	}
	if !strings.Contains(addTeamMembersQuery, "ON CONFLICT (team_id, user_id) DO NOTHING") {
		t.Errorf("add is not idempotent:\n%s", addTeamMembersQuery)
	}
}

// The namespace question is asked about a site that does not exist yet, so a
// join on sites could only answer it wrongly. In particular an editor grant on
// some other site in the namespace must not leak in.
func TestResolveNamespaceAccessDoesNotConsultSites(t *testing.T) {
	lowered := strings.ToLower(resolveNamespaceAccessQuery)
	for _, forbidden := range []string{"sites", "site_collaborators", "'editor'"} {
		if strings.Contains(lowered, forbidden) {
			t.Errorf("namespace access query references %q:\n%s", forbidden, resolveNamespaceAccessQuery)
		}
	}
	for _, required := range []string{
		"owner.username = $2",
		"tm.team_id = owner.id",
		"tm.user_id = $1::uuid",
		"owner.id = $1::uuid OR tm.user_id IS NOT NULL",
		"WHEN owner.id = $1::uuid THEN 'owner'",
		"ELSE 'member'",
	} {
		if !strings.Contains(resolveNamespaceAccessQuery, required) {
			t.Errorf("namespace access query missing %q:\n%s", required, resolveNamespaceAccessQuery)
		}
	}
}

func TestNamespaceAccessRejectsEditor(t *testing.T) {
	if grantsNamespaceAccess(CollaborationRoleEditor) {
		t.Fatal("an editor grant is per-site and must not answer a namespace question")
	}
	if !grantsNamespaceAccess(CollaborationRoleOwner) || !grantsNamespaceAccess(CollaborationRoleMember) {
		t.Fatal("owner and member must both grant namespace access")
	}
	if grantsNamespaceAccess(CollaborationRole("admin")) {
		t.Fatal("an unknown role must fail closed")
	}
}

// Without FOR UPDATE this is a plain read and two concurrent removals both see
// a member that the other is about to delete.
func TestLockTeamTakesRowLock(t *testing.T) {
	for _, required := range []string{"FROM users", "id = $1::uuid", "kind = 'team'", "FOR UPDATE"} {
		if !strings.Contains(lockTeamQuery, required) {
			t.Errorf("lock query missing %q:\n%s", required, lockTeamQuery)
		}
	}
}

// The lock and the two-statement create are meaningless outside a transaction,
// so both refuse a nil one rather than silently doing half the work.
func TestTeamMutationsRequireTransactions(t *testing.T) {
	var createSignature func(context.Context, *sql.Tx, string, string) (User, error) = CreateTeam
	var lockSignature func(context.Context, *sql.Tx, string) error = LockTeam
	_, _ = createSignature, lockSignature

	if _, err := CreateTeam(context.Background(), nil, "platform", "creator"); err == nil {
		t.Error("nil create transaction unexpectedly accepted")
	}
	if err := LockTeam(context.Background(), nil, "team-id"); err == nil {
		t.Error("nil lock transaction unexpectedly accepted")
	}
}

func TestCreateTeamWritesNoAPIKey(t *testing.T) {
	// users.api_key was dropped outright by migration 0025, so a team row
	// (which never had one) is inserted with no api_key column at all now,
	// not even an explicit NULL.
	if !strings.Contains(createTeamQuery, "VALUES ($1, false, 'team')") {
		t.Errorf("create does not insert a team the way the post-0025 schema expects:\n%s", createTeamQuery)
	}
	if strings.Contains(createTeamQuery, "api_key") {
		t.Errorf("create query still references the dropped api_key column:\n%s", createTeamQuery)
	}
	if _, err := CreateTeam(context.Background(), &sql.Tx{}, "   ", "creator"); err == nil {
		t.Error("blank team name unexpectedly accepted")
	}
}

// Emptying a team orphans its sites, so the remaining-member check lives in
// the DELETE itself and not in a SELECT the caller could race past.
func TestRemoveTeamMemberGuardsTheLastMember(t *testing.T) {
	for _, required := range []string{
		"DELETE FROM team_members",
		"team_id = $1::uuid",
		"user_id = $2::uuid",
		"AND EXISTS (",
		"remaining.user_id <> $2::uuid",
	} {
		if !strings.Contains(removeTeamMemberQuery, required) {
			t.Errorf("remove query missing %q:\n%s", required, removeTeamMemberQuery)
		}
	}

	if err := RemoveTeamMember(context.Background(), &teamExecQuerier{affected: 1}, "team", "member"); err != nil {
		t.Errorf("successful removal returned %v", err)
	}
	if err := RemoveTeamMember(context.Background(), &teamExecQuerier{affected: 2}, "team", "member"); err == nil {
		t.Error("a multi-row delete on a primary-keyed table was not reported")
	}
}

func TestDeleteTeamIsScopedToTeams(t *testing.T) {
	if !strings.Contains(deleteTeamQuery, "kind = 'team'") {
		t.Errorf("delete is not restricted to team namespaces:\n%s", deleteTeamQuery)
	}
	if !strings.Contains(countTeamSitesQuery, "FROM sites") || !strings.Contains(countTeamSitesQuery, "user_id = $1::uuid") {
		t.Errorf("site count is not namespace scoped:\n%s", countTeamSitesQuery)
	}
}

func TestTeamAuditNullsEmptyActorAndSubject(t *testing.T) {
	q := &teamExecQuerier{affected: 1}
	if err := TeamAudit(context.Background(), q, "team", "", "remove", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(q.args) != 5 {
		t.Fatalf("audit args = %#v", q.args)
	}
	if q.args[1] != nil || q.args[3] != nil || q.args[4] != nil {
		t.Fatalf("empty actor, subject and detail were not written as NULL: %#v", q.args)
	}

	q = &teamExecQuerier{affected: 1}
	if err := TeamAudit(context.Background(), q, "team", "actor", "add", "subject", "note"); err != nil {
		t.Fatal(err)
	}
	if q.args[1] != "actor" || q.args[3] != "subject" || q.args[4] != "note" {
		t.Fatalf("populated audit args = %#v", q.args)
	}
}

func TestTeamMemberBatchIsDeterministicAndStrict(t *testing.T) {
	got, err := normalizeTeamUsernames([]string{" Bob ", "alice", "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "alice,bob" {
		t.Fatalf("normalized usernames = %#v", got)
	}
	if _, err := normalizeTeamUsernames([]string{"alice", " "}); !errors.Is(err, ErrTeamMemberNotFound) {
		t.Fatalf("blank username error = %v, want ErrTeamMemberNotFound", err)
	}
	if MaxTeamMembers != 50 {
		t.Fatalf("MaxTeamMembers = %d, want 50", MaxTeamMembers)
	}
	if err := AddTeamMembers(context.Background(), &teamExecQuerier{}, "team", nil, "actor"); err != nil {
		t.Fatalf("empty batch returned %v", err)
	}

	oversized := make([]string, 0, MaxTeamMembers+1)
	for i := 0; i <= MaxTeamMembers; i++ {
		oversized = append(oversized, string(rune('a'+i%26))+string(rune('a'+i/26))+"user")
	}
	if err := AddTeamMembers(context.Background(), &teamExecQuerier{}, "team", oversized, "actor"); !errors.Is(err, ErrTeamMemberLimit) {
		t.Fatalf("oversized batch error = %v, want ErrTeamMemberLimit", err)
	}
}

// teamExecQuerier records ExecContext and reports a fixed RowsAffected. The
// read paths panic: a test that reaches one is asserting something this stub
// cannot honestly answer without a database.
type teamExecQuerier struct {
	query    string
	args     []any
	affected int64
}

func (q *teamExecQuerier) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	q.query = query
	q.args = args
	return teamExecResult{affected: q.affected}, nil
}

func (q *teamExecQuerier) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	panic("unexpected QueryContext")
}

func (q *teamExecQuerier) QueryRowContext(context.Context, string, ...any) *sql.Row {
	panic("unexpected QueryRowContext")
}

type teamExecResult struct {
	affected int64
}

func (r teamExecResult) LastInsertId() (int64, error) { return 0, errors.New("not supported") }
func (r teamExecResult) RowsAffected() (int64, error) { return r.affected, nil }
