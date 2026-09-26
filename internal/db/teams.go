package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

// MaxTeamMembers bounds one team: a
// membership list is read on every authenticated request against the team's
// namespace, and an unbounded one is an unbounded join.
const MaxTeamMembers = 50

var (
	ErrTeamMemberLimit    = errors.New("team member limit reached")
	ErrTeamMemberNotFound = errors.New("one or more member usernames do not exist")
	ErrLastTeamMember     = errors.New("cannot remove the last member of a team")
	ErrTeamHasSites       = errors.New("team still owns sites")
)

// Team is a namespace an actor belongs to. It is a users row like any other,
// so the identity here is the same identity sites are owned by.
type Team struct {
	ID       string
	Username string
}

// TeamMember is one person in a team. There is no role: membership is the
// whole grant. See internal/migrate/sql/0019_add_teams.sql.
type TeamMember struct {
	UserID    string
	Username  string
	CreatedAt time.Time
}

// NamespaceAccess answers "may this actor create a site under this name",
// which ResolveSiteAccess cannot: that one starts from a site row, and a site
// about to be created has none.
type NamespaceAccess struct {
	OwnerID       string
	OwnerUsername string
	Role          CollaborationRole
}

// grantsNamespaceAccess admits the roles that may act on a whole namespace.
func grantsNamespaceAccess(role CollaborationRole) bool {
	switch role {
	case CollaborationRoleOwner, CollaborationRoleMember:
		return true
	}
	return false
}

const createTeamQuery = `
	INSERT INTO users (username, is_admin, kind)
	VALUES ($1, false, 'team')
	RETURNING id::text, username, is_admin, created_at
`

const addTeamCreatorQuery = `
	INSERT INTO team_members (team_id, user_id, added_by)
	VALUES ($1::uuid, $2::uuid, $2::uuid)
`

// CreateTeam inserts the namespace, its first member, and the audit row.
//
// Requiring *sql.Tx: a team row without a membership
// row is a namespace nobody can reach and nobody can delete, so the two
// inserts must not be separable by a caller. A team holds no credential of
// its own; people act on it with their own keys or sessions.
func CreateTeam(ctx context.Context, tx *sql.Tx, name, creatorID string) (User, error) {
	if tx == nil {
		return User{}, fmt.Errorf("create team: nil transaction")
	}
	normalized := normalizeTeamUsername(name)
	if normalized == "" {
		return User{}, fmt.Errorf("create team: empty name")
	}

	var team User
	if err := tx.QueryRowContext(ctx, createTeamQuery, normalized).Scan(
		&team.ID,
		&team.Username,
		&team.IsAdmin,
		&team.CreatedAt,
	); err != nil {
		return User{}, fmt.Errorf("create team %q: %w", normalized, err)
	}

	if _, err := tx.ExecContext(ctx, addTeamCreatorQuery, team.ID, creatorID); err != nil {
		return User{}, fmt.Errorf("add team creator to %q: %w", normalized, err)
	}
	if err := TeamAudit(ctx, tx, team.ID, creatorID, "create", creatorID, normalized); err != nil {
		return User{}, err
	}
	return team, nil
}

const listTeamsForUserQuery = `
	SELECT
		team.id::text,
		team.username
	FROM team_members tm
	INNER JOIN users team ON team.id = tm.team_id
	WHERE tm.user_id = $1::uuid
	  AND team.kind = 'team'
	ORDER BY lower(team.username), team.username, team.id
`

// ListTeamsForUser returns the namespaces userID may act in besides their own.
func ListTeamsForUser(ctx context.Context, q Querier, userID string) ([]Team, error) {
	rows, err := q.QueryContext(ctx, listTeamsForUserQuery, userID)
	if err != nil {
		return nil, fmt.Errorf("list teams for user: %w", err)
	}
	defer rows.Close()

	var teams []Team
	for rows.Next() {
		var team Team
		if err := rows.Scan(&team.ID, &team.Username); err != nil {
			return nil, fmt.Errorf("list teams for user: %w", err)
		}
		teams = append(teams, team)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list teams for user: %w", err)
	}
	return teams, nil
}

const listTeamMembersQuery = `
	SELECT
		member.id::text,
		member.username,
		tm.created_at
	FROM team_members tm
	INNER JOIN users member ON member.id = tm.user_id
	WHERE tm.team_id = $1::uuid
	ORDER BY lower(member.username), member.username, member.id
`

// ListTeamMembers returns everyone in the team. There is no role to report:
// every row here carries the same access.
func ListTeamMembers(ctx context.Context, q Querier, teamID string) ([]TeamMember, error) {
	rows, err := q.QueryContext(ctx, listTeamMembersQuery, teamID)
	if err != nil {
		return nil, fmt.Errorf("list team members: %w", err)
	}
	defer rows.Close()

	var members []TeamMember
	for rows.Next() {
		var member TeamMember
		if err := rows.Scan(&member.UserID, &member.Username, &member.CreatedAt); err != nil {
			return nil, fmt.Errorf("list team members: %w", err)
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list team members: %w", err)
	}
	return members, nil
}

const isTeamMemberQuery = `
	SELECT EXISTS (
		SELECT 1
		FROM team_members
		WHERE team_id = $1::uuid
		  AND user_id = $2::uuid
	)
`

func IsTeamMember(ctx context.Context, q Querier, teamID, userID string) (bool, error) {
	var member bool
	if err := q.QueryRowContext(ctx, isTeamMemberQuery, teamID, userID).Scan(&member); err != nil {
		return false, fmt.Errorf("is team member: %w", err)
	}
	return member, nil
}

const countTeamMembersQuery = `
	SELECT count(*)::int
	FROM team_members
	WHERE team_id = $1::uuid
`

const resolveRequestedMembersQuery = `
	SELECT
		count(*)::int,
		count(tm.user_id)::int
	FROM users candidate
	LEFT JOIN team_members tm
		ON tm.team_id = $1::uuid
		AND tm.user_id = candidate.id
	WHERE candidate.username = ANY($2::text[])
	  AND candidate.kind = 'person'
`

const addTeamMembersQuery = `
	INSERT INTO team_members (team_id, user_id, added_by)
	SELECT $1::uuid, candidate.id, $3::uuid
	FROM users candidate
	WHERE candidate.username = ANY($2::text[])
	  AND candidate.kind = 'person'
	ON CONFLICT (team_id, user_id) DO NOTHING
`

// AddTeamMembers resolves and inserts a bounded username batch in one
// statement. The kind = 'person' predicate is the application half of the
// team_members_kinds trigger: a name that belongs to another team resolves to
// nothing here rather than nesting namespaces.
//
// A username that does not resolve to a person is reported as
// ErrTeamMemberNotFound for the whole batch rather than silently skipped —
// the caller asked for a set, not a best effort. Detection is by count: the
// requested names must end up either newly inserted or already present.
//
// The caller is expected to hold LockTeam in the same transaction, so the cap
// check and the insert cannot interleave with a concurrent add.
func AddTeamMembers(ctx context.Context, q Querier, teamID string, usernames []string, addedBy string) error {
	normalized, err := normalizeTeamUsernames(usernames)
	if err != nil {
		return err
	}
	if len(normalized) == 0 {
		return nil
	}
	if len(normalized) > MaxTeamMembers {
		return ErrTeamMemberLimit
	}

	var current int
	if err := q.QueryRowContext(ctx, countTeamMembersQuery, teamID).Scan(&current); err != nil {
		return fmt.Errorf("count team members: %w", err)
	}

	var resolved, alreadyMembers int
	if err := q.QueryRowContext(ctx, resolveRequestedMembersQuery, teamID, pq.Array(normalized)).Scan(&resolved, &alreadyMembers); err != nil {
		return fmt.Errorf("resolve requested team members: %w", err)
	}
	if current+(resolved-alreadyMembers) > MaxTeamMembers {
		return ErrTeamMemberLimit
	}

	result, err := q.ExecContext(ctx, addTeamMembersQuery, teamID, pq.Array(normalized), nullableID(addedBy))
	if err != nil {
		return fmt.Errorf("add team members: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("add team members: %w", err)
	}
	if int(affected)+alreadyMembers != len(normalized) {
		return ErrTeamMemberNotFound
	}
	return nil
}

const removeTeamMemberQuery = `
	DELETE FROM team_members
	WHERE team_id = $1::uuid
	  AND user_id = $2::uuid
	  AND EXISTS (
		SELECT 1
		FROM team_members remaining
		WHERE remaining.team_id = $1::uuid
		  AND remaining.user_id <> $2::uuid
	)
`

// RemoveTeamMember deletes one membership and refuses to empty the team.
//
// The emptiness check is inside the DELETE rather than a separate SELECT so it
// holds even if the caller forgot LockTeam: two concurrent removals of the
// last two members cannot both see a remaining row, because the second one
// reads the first one's committed delete. The lock is still the intended
// serialization; this is the backstop.
//
// Returns ErrLastTeamMember when userID is the only member, and
// ErrTeamMemberNotFound when they were not a member at all.
func RemoveTeamMember(ctx context.Context, q Querier, teamID, userID string) error {
	result, err := q.ExecContext(ctx, removeTeamMemberQuery, teamID, userID)
	if err != nil {
		return fmt.Errorf("remove team member: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("remove team member: %w", err)
	}
	if affected > 1 {
		return fmt.Errorf("remove team member affected %d rows, want at most 1", affected)
	}
	if affected == 1 {
		return nil
	}

	member, err := IsTeamMember(ctx, q, teamID, userID)
	if err != nil {
		return err
	}
	if member {
		return ErrLastTeamMember
	}
	return ErrTeamMemberNotFound
}

const lockTeamQuery = `
	SELECT 1
	FROM users
	WHERE id = $1::uuid
	  AND kind = 'team'
	FOR UPDATE
`

// LockTeam takes a row lock on the team namespace for the rest of tx. Every
// membership-changing operation calls it first, so two concurrent removals
// serialize instead of both reading a pre-removal member count.
//
// Requiring *sql.Tx follows LockSiteCollaboration: a lock taken outside a
// transaction is released before the work it was meant to protect finishes.
// Returns sql.ErrNoRows when teamID is not a team.
func LockTeam(ctx context.Context, tx *sql.Tx, teamID string) error {
	if tx == nil {
		return fmt.Errorf("lock team: nil transaction")
	}
	var one int
	return tx.QueryRowContext(ctx, lockTeamQuery, teamID).Scan(&one)
}

const countTeamSitesQuery = `
	SELECT count(*)::int
	FROM sites
	WHERE user_id = $1::uuid
`

// CountTeamSites counts sites owned by the team namespace. A team owns sites
// the same way a person does — sites.user_id is the namespace, not the author.
func CountTeamSites(ctx context.Context, q Querier, teamID string) (int, error) {
	var count int
	if err := q.QueryRowContext(ctx, countTeamSitesQuery, teamID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count team sites: %w", err)
	}
	return count, nil
}

const countOtherActiveTeamMembersQuery = `
	SELECT count(*)::int
	FROM team_members tm
	INNER JOIN users member ON member.id = tm.user_id
	WHERE tm.team_id = $1::uuid
	  AND tm.user_id IS DISTINCT FROM NULLIF($2, '')::uuid
	  AND member.disabled_at IS NULL
`

// CountOtherActiveTeamMembers counts the team's members other than userID
// whose accounts are not disabled. A disabled account cannot sign in, so it
// does not keep a team alive: when this is zero, userID leaving would leave
// nobody who can act on the team. Pass "" for userID to count every active
// member.
func CountOtherActiveTeamMembers(ctx context.Context, q Querier, teamID, userID string) (int, error) {
	var count int
	if err := q.QueryRowContext(ctx, countOtherActiveTeamMembersQuery, teamID, userID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count active team members: %w", err)
	}
	return count, nil
}

const listTeamSitesQuery = `
	SELECT id::text, name, active_version
	FROM sites
	WHERE user_id = $1::uuid
	ORDER BY name
`

// TeamSite is the part of a team's site that deleting it needs.
type TeamSite struct {
	ID            string
	Name          string
	ActiveVersion int
}

// ListTeamSites returns every site the team owns, so the team's deletion can
// retire each one. The caller holds LockTeam, which keeps the list complete
// (a new site's insert waits on it), and takes each site's
// LockSiteCollaboration itself; row-locking the sites here would take the
// row before the advisory lock a deploy holds first, and deadlock with it.
func ListTeamSites(ctx context.Context, tx *sql.Tx, teamID string) ([]TeamSite, error) {
	rows, err := tx.QueryContext(ctx, listTeamSitesQuery, teamID)
	if err != nil {
		return nil, fmt.Errorf("list team sites: %w", err)
	}
	defer rows.Close()
	var sites []TeamSite
	for rows.Next() {
		var site TeamSite
		if err := rows.Scan(&site.ID, &site.Name, &site.ActiveVersion); err != nil {
			return nil, fmt.Errorf("list team sites: %w", err)
		}
		sites = append(sites, site)
	}
	return sites, rows.Err()
}

const deleteTeamQuery = `
	DELETE FROM users
	WHERE id = $1::uuid
	  AND kind = 'team'
`

// DeleteTeam removes a team namespace that owns no sites. Memberships and
// audit rows go with it through ON DELETE CASCADE.
//
// It refuses with ErrTeamHasSites rather than cascading into sites: the site
// files in the bucket are not in this transaction, so the caller deletes each
// site first and queues its files for retirement (handler.deleteTeamAndSites). Returns sql.ErrNoRows when teamID is not a
// team. The caller is expected to hold LockTeam in the same transaction so a
// site cannot be created between the count and the delete.
func DeleteTeam(ctx context.Context, q Querier, teamID string) error {
	sites, err := CountTeamSites(ctx, q, teamID)
	if err != nil {
		return err
	}
	if sites > 0 {
		return ErrTeamHasSites
	}

	result, err := q.ExecContext(ctx, deleteTeamQuery, teamID)
	if err != nil {
		return fmt.Errorf("delete team: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete team: %w", err)
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

const resolveNamespaceAccessQuery = `
	SELECT
		owner.id::text,
		owner.username,
		CASE
			WHEN owner.id = $1::uuid THEN 'owner'
			ELSE 'member'
		END
	FROM users owner
	LEFT JOIN team_members tm
		ON tm.team_id = owner.id
		AND tm.user_id = $1::uuid
	WHERE owner.username = $2
	  AND (owner.id = $1::uuid OR tm.user_id IS NOT NULL)
`

// ResolveNamespaceAccess answers whether actorID may create a site under
// ownerUsername. Unknown, unowned, and unjoined namespaces all return
// sql.ErrNoRows.
//
// It deliberately does not join sites: the question is asked about a site
// that does not exist yet. The role here can only be owner or member; see
// grantsNamespaceAccess.
func ResolveNamespaceAccess(ctx context.Context, q Querier, actorID, ownerUsername string) (NamespaceAccess, error) {
	var access NamespaceAccess
	err := q.QueryRowContext(ctx, resolveNamespaceAccessQuery, actorID, ownerUsername).Scan(
		&access.OwnerID,
		&access.OwnerUsername,
		&access.Role,
	)
	if err != nil {
		return NamespaceAccess{}, err
	}
	if !grantsNamespaceAccess(access.Role) {
		return NamespaceAccess{}, fmt.Errorf("resolve namespace access: unexpected role %q", access.Role)
	}
	return access, nil
}

const teamAuditQuery = `
	INSERT INTO team_audit (team_id, actor_id, action, subject_id, detail)
	VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5)
`

// TeamAudit records one membership or lifecycle event. actorID and subjectID
// are empty-string-means-NULL, because an admin action has no member actor and
// a lifecycle action has no subject. detail is free text and is written as
// NULL when empty.
func TeamAudit(ctx context.Context, q Querier, teamID, actorID, action, subjectID, detail string) error {
	if _, err := q.ExecContext(
		ctx,
		teamAuditQuery,
		teamID,
		nullableID(actorID),
		action,
		nullableID(subjectID),
		nullableText(detail),
	); err != nil {
		return fmt.Errorf("team audit %q: %w", action, err)
	}
	return nil
}

func nullableID(id string) any {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	return id
}

func nullableText(text string) any {
	if text == "" {
		return nil
	}
	return text
}

// normalizeTeamUsername mirrors the lookup form used by every username
// predicate in this package: stored usernames are lowercase, so a name is
// trimmed and lowercased before it is compared or inserted.
func normalizeTeamUsername(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// normalizeTeamUsernames deduplicates and sorts a requested batch so the same
// request produces the same statement, and rejects a blank name outright
// rather than letting it fail the count check with a confusing message.
func normalizeTeamUsernames(usernames []string) ([]string, error) {
	seen := make(map[string]struct{}, len(usernames))
	normalized := make([]string, 0, len(usernames))
	for _, username := range usernames {
		username = normalizeTeamUsername(username)
		if username == "" {
			return nil, ErrTeamMemberNotFound
		}
		if _, ok := seen[username]; ok {
			continue
		}
		seen[username] = struct{}{}
		normalized = append(normalized, username)
	}
	sort.Strings(normalized)
	return normalized, nil
}

const getTeamByNameQuery = `
	SELECT id::text, username
	FROM users
	WHERE username = $1
	  AND kind = 'team'
`

// GetTeamByName resolves a team namespace by name. It filters on kind rather
// than reusing GetUserByUsername so a person's name can never be mistaken for
// a team's: every team route resolves through here, and a route that resolved
// a person would authorise membership against a namespace that has none.
// Returns sql.ErrNoRows when the name is free or belongs to a person.
func GetTeamByName(ctx context.Context, q Querier, name string) (Team, error) {
	var team Team
	if err := q.QueryRowContext(ctx, getTeamByNameQuery, name).Scan(&team.ID, &team.Username); err != nil {
		return Team{}, err
	}
	return team, nil
}

const searchTeamMemberCandidatesQuery = `
	SELECT
		u.id::text,
		u.username,
		EXISTS (
			SELECT 1
			FROM team_members tm
			WHERE tm.team_id = $1::uuid
			  AND tm.user_id = u.id
		)
	FROM users u
	WHERE u.kind = 'person'
	  AND strpos(lower(u.username), lower($2)) > 0
	ORDER BY lower(u.username), u.username, u.id
	LIMIT $3
`

// SearchTeamMemberCandidates finds people who could be added to a team, by
// case-insensitive literal substring — never a SQL pattern, so a name
// containing % or _ searches for itself.
//
// Teams are excluded by kind, so a team can never be offered as a member of
// another team.
func SearchTeamMemberCandidates(ctx context.Context, q Querier, teamID, search string, limit int) ([]UserCandidate, error) {
	if limit < 1 || limit > 20 {
		limit = 20
	}
	rows, err := q.QueryContext(ctx, searchTeamMemberCandidatesQuery, teamID, search, limit)
	if err != nil {
		return nil, fmt.Errorf("search team member candidates: %w", err)
	}
	defer rows.Close()

	var candidates []UserCandidate
	for rows.Next() {
		var candidate UserCandidate
		if err := rows.Scan(&candidate.UserID, &candidate.Username, &candidate.AlreadyMember); err != nil {
			return nil, fmt.Errorf("scan team member candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search team member candidates: %w", err)
	}
	return candidates, nil
}

// TeamPrefix begins every team name from v1.3 on, so a team can never take a
// name a person might sign in with. Migration 0041 renamed older teams.
const TeamPrefix = "team-"

const legacyTeamLabelQuery = `
	SELECT t.username
	FROM users t
	WHERE t.kind = 'team' AND t.username = 'team-' || $1
	  AND NOT EXISTS (SELECT 1 FROM users u WHERE lower(replace(u.username, '.', '-')) = $1)
`

// LegacyTeamName reports the team a pre-v1.3 team address now belongs to:
// label names no account, and a team called "team-<label>" exists. Once a
// person takes the old name, the old address is theirs and this reports
// nothing.
func LegacyTeamName(ctx context.Context, q Querier, label string) (string, bool, error) {
	if strings.HasPrefix(label, TeamPrefix) {
		return "", false, nil
	}
	var name string
	err := q.QueryRowContext(ctx, legacyTeamLabelQuery, label).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}
