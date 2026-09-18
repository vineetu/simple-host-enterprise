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

// CollaborationRole is the access an actor has to a canonical site.
type CollaborationRole string

const (
	CollaborationRoleOwner CollaborationRole = "owner"
	// CollaborationRoleMember is a person who belongs to the team that owns
	// the site. A team is a users row like any other, so membership is
	// resolved by joining team_members on the site's owner. It outranks an
	// editor grant on the same site: see resolveSiteAccessQuery.
	CollaborationRoleMember CollaborationRole = "member"
	CollaborationRoleEditor CollaborationRole = "editor"
	MaxSiteEditors                            = 50
)

// grantsSiteAccess reports whether role is one this server issues. It is the
// single list the resolvers check, so a role added to the CASE expressions
// without being added here fails closed rather than reaching a caller that
// does not know it.
func grantsSiteAccess(role CollaborationRole) bool {
	switch role {
	case CollaborationRoleOwner, CollaborationRoleMember, CollaborationRoleEditor:
		return true
	}
	return false
}

var (
	ErrEditorLimit         = errors.New("site editor limit reached")
	ErrEditorNotFound      = errors.New("one or more editor usernames do not exist")
	ErrOwnerCannotBeEditor = errors.New("site owner cannot be an editor")
)

// SiteAccess is an authorized view of a canonical site. Site.ID is the
// immutable resource identity callers must retain across mutation locks.
type SiteAccess struct {
	Site          Site
	OwnerID       string
	OwnerUsername string
	ActorID       string
	Role          CollaborationRole
}

// AccessibleSite is an owned or shared site in an actor's collaboration list.
type AccessibleSite struct {
	Site          Site
	OwnerUsername string
	AccessRole    CollaborationRole
}

// SiteEditor describes a current editor without exposing account credentials.
type SiteEditor struct {
	UserID    string
	Username  string
	AddedBy   *string
	CreatedAt time.Time
}

// EditorCandidate is the minimal user record returned by the membership picker.
type EditorCandidate struct {
	UserID        string
	Username      string
	AlreadyEditor bool
}

const resolveSiteAccessQuery = `
	SELECT
		s.id::text,
		s.user_id::text,
		s.name,
		s.active_version,
		s.public,
		s.uses_state,
		s.uses_versioned_state,
		s.created_at,
		s.updated_at,
		owner.username,
		$1::uuid::text,
		CASE
			WHEN s.user_id = $1::uuid THEN 'owner'
			WHEN tm.user_id IS NOT NULL THEN 'member'
			ELSE 'editor'
		END
	FROM sites s
	INNER JOIN users owner ON owner.id = s.user_id
	LEFT JOIN site_collaborators sc
		ON sc.site_id = s.id
		AND sc.user_id = $1::uuid
		AND sc.role = 'editor'
	LEFT JOIN team_members tm
		ON tm.team_id = s.user_id
		AND tm.user_id = $1::uuid
	WHERE owner.username = $2
	  AND s.name = $3
	  AND (s.user_id = $1::uuid OR sc.user_id IS NOT NULL OR tm.user_id IS NOT NULL)
`

// ResolveSiteAccess resolves an owner-qualified site and authorizes actorID in
// the same query. Inaccessible, revoked, deleted, and unknown sites all return
// sql.ErrNoRows.
//
// Three ways to have access: owning the site, belonging to the team that owns
// it, or holding an editor grant on it. With no team rows present the team
// join matches nothing and this behaves exactly as it did before teams.
func ResolveSiteAccess(ctx context.Context, q Querier, actorID, ownerUsername, siteName string) (SiteAccess, error) {
	var access SiteAccess
	err := q.QueryRowContext(ctx, resolveSiteAccessQuery, actorID, ownerUsername, siteName).Scan(
		&access.Site.ID,
		&access.Site.UserID,
		&access.Site.Name,
		&access.Site.ActiveVersion,
		&access.Site.Public,
		&access.Site.UsesState,
		&access.Site.UsesVersionedState,
		&access.Site.CreatedAt,
		&access.Site.UpdatedAt,
		&access.OwnerUsername,
		&access.ActorID,
		&access.Role,
	)
	if err != nil {
		return SiteAccess{}, err
	}
	access.OwnerID = access.Site.UserID
	if !grantsSiteAccess(access.Role) {
		return SiteAccess{}, fmt.Errorf("resolve site access: unexpected role %q", access.Role)
	}
	return access, nil
}

const listAccessibleSitesQuery = `
	SELECT
		s.id::text,
		s.user_id::text,
		s.name,
		s.active_version,
		s.public,
		s.uses_state,
		s.uses_versioned_state,
		s.created_at,
		s.updated_at,
		owner.username,
		CASE
			WHEN s.user_id = $1::uuid THEN 'owner'
			WHEN tm.user_id IS NOT NULL THEN 'member'
			ELSE 'editor'
		END
	FROM sites s
	INNER JOIN users owner ON owner.id = s.user_id
	LEFT JOIN site_collaborators sc
		ON sc.site_id = s.id
		AND sc.user_id = $1::uuid
		AND sc.role = 'editor'
	LEFT JOIN team_members tm
		ON tm.team_id = s.user_id
		AND tm.user_id = $1::uuid
	WHERE s.user_id = $1::uuid OR sc.user_id IS NOT NULL OR tm.user_id IS NOT NULL
	ORDER BY
		CASE WHEN s.user_id = $1::uuid THEN 0 ELSE 1 END,
		owner.username,
		s.name,
		s.id
`

// ListAccessibleSites returns canonical sites owned by actorID, owned by a
// team they belong to, or shared with them. A site appears at most once even
// if invalid duplicate membership data exists: team_members is keyed on
// (team_id, user_id) and site_collaborators on (site_id, user_id), so neither
// join can multiply rows.
func ListAccessibleSites(ctx context.Context, q Querier, actorID string) ([]AccessibleSite, error) {
	rows, err := q.QueryContext(ctx, listAccessibleSitesQuery, actorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sites []AccessibleSite
	for rows.Next() {
		var accessible AccessibleSite
		if err := rows.Scan(
			&accessible.Site.ID,
			&accessible.Site.UserID,
			&accessible.Site.Name,
			&accessible.Site.ActiveVersion,
			&accessible.Site.Public,
			&accessible.Site.UsesState,
			&accessible.Site.UsesVersionedState,
			&accessible.Site.CreatedAt,
			&accessible.Site.UpdatedAt,
			&accessible.OwnerUsername,
			&accessible.AccessRole,
		); err != nil {
			return nil, err
		}
		if !grantsSiteAccess(accessible.AccessRole) {
			return nil, fmt.Errorf("list accessible sites: unexpected role %q", accessible.AccessRole)
		}
		sites = append(sites, accessible)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sites, nil
}

const listSiteEditorsQuery = `
	SELECT
		u.id::text,
		u.username,
		sc.added_by::text,
		sc.created_at
	FROM site_collaborators sc
	INNER JOIN users u ON u.id = sc.user_id
	WHERE sc.site_id = $1::uuid
	  AND sc.role = 'editor'
	ORDER BY lower(u.username), u.username, u.id
`

func ListSiteEditors(ctx context.Context, q Querier, siteID string) ([]SiteEditor, error) {
	rows, err := q.QueryContext(ctx, listSiteEditorsQuery, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var editors []SiteEditor
	for rows.Next() {
		var editor SiteEditor
		var addedBy sql.NullString
		if err := rows.Scan(&editor.UserID, &editor.Username, &addedBy, &editor.CreatedAt); err != nil {
			return nil, err
		}
		if addedBy.Valid {
			editor.AddedBy = &addedBy.String
		}
		editors = append(editors, editor)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return editors, nil
}

const searchEditorCandidatesQuery = `
	SELECT
		u.id::text,
		u.username,
		EXISTS (
			SELECT 1
			FROM site_collaborators sc
			WHERE sc.site_id = $1::uuid
			  AND sc.user_id = u.id
			  AND sc.role = 'editor'
		)
	FROM users u
	INNER JOIN sites s ON s.id = $1::uuid
	WHERE u.id <> s.user_id
	  AND strpos(lower(u.username), lower($2)) > 0
	ORDER BY lower(u.username), u.username, u.id
	LIMIT $3
`

// SearchEditorCandidates performs a case-insensitive literal substring match.
// limit is normalized to the API maximum so every invocation is bounded.
func SearchEditorCandidates(ctx context.Context, q Querier, siteID, search string, limit int) ([]EditorCandidate, error) {
	if limit < 1 || limit > 20 {
		limit = 20
	}
	rows, err := q.QueryContext(ctx, searchEditorCandidatesQuery, siteID, search, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []EditorCandidate
	for rows.Next() {
		var candidate EditorCandidate
		if err := rows.Scan(&candidate.UserID, &candidate.Username, &candidate.AlreadyEditor); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}

const grantSiteEditorsQuery = `
	INSERT INTO site_collaborators (site_id, user_id, role, added_by)
	SELECT $1::uuid, requested.user_id, 'editor', $3::uuid
	FROM unnest($2::uuid[]) AS requested(user_id)
	ON CONFLICT (site_id, user_id) DO NOTHING
`

const resolveRequestedEditorsQuery = `
	SELECT id::text, username
	FROM users
	WHERE username = ANY($1::text[])
	ORDER BY username, id
`

// GrantSiteEditors resolves and grants a bounded username batch atomically.
// Requiring *sql.Tx and acquiring the namespace advisory lock here prevents a
// caller from separating cap enforcement from insertion.
func GrantSiteEditors(ctx context.Context, tx *sql.Tx, ownerID, siteName, siteID string, addedByID *string, usernames []string) ([]SiteEditor, error) {
	if tx == nil {
		return nil, fmt.Errorf("grant site editors: nil transaction")
	}
	if err := LockSiteCollaboration(ctx, tx, ownerID, siteName); err != nil {
		return nil, err
	}
	if err := requireSiteIncarnation(ctx, tx, ownerID, siteName, siteID); err != nil {
		return nil, err
	}

	normalized, err := normalizeEditorUsernames(usernames)
	if err != nil {
		return nil, err
	}
	if len(normalized) == 0 {
		return ListSiteEditors(ctx, tx, siteID)
	}
	if len(normalized) > MaxSiteEditors {
		return nil, ErrEditorLimit
	}

	rows, err := tx.QueryContext(ctx, resolveRequestedEditorsQuery, pq.Array(normalized))
	if err != nil {
		return nil, err
	}
	resolvedIDs := make([]string, 0, len(normalized))
	for rows.Next() {
		var userID, username string
		if err := rows.Scan(&userID, &username); err != nil {
			rows.Close()
			return nil, err
		}
		if userID == ownerID {
			rows.Close()
			return nil, ErrOwnerCannotBeEditor
		}
		resolvedIDs = append(resolvedIDs, userID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(resolvedIDs) != len(normalized) {
		return nil, ErrEditorNotFound
	}

	current, err := ListSiteEditors(ctx, tx, siteID)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]struct{}, len(current))
	for _, editor := range current {
		existing[editor.UserID] = struct{}{}
	}
	newEditors := 0
	for _, userID := range resolvedIDs {
		if _, ok := existing[userID]; !ok {
			newEditors++
		}
	}
	if len(current)+newEditors > MaxSiteEditors {
		return nil, ErrEditorLimit
	}

	if _, err := tx.ExecContext(ctx, grantSiteEditorsQuery, siteID, pq.Array(resolvedIDs), addedByID); err != nil {
		return nil, err
	}
	return ListSiteEditors(ctx, tx, siteID)
}

func normalizeEditorUsernames(usernames []string) ([]string, error) {
	seen := make(map[string]struct{}, len(usernames))
	normalized := make([]string, 0, len(usernames))
	for _, username := range usernames {
		username = strings.ToLower(strings.TrimSpace(username))
		if username == "" {
			return nil, ErrEditorNotFound
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

const revokeSiteEditorQuery = `
	DELETE FROM site_collaborators AS collaborator
	USING users AS member
	WHERE collaborator.site_id = $1::uuid
	  AND collaborator.user_id = member.id
	  AND member.username = $2
	  AND collaborator.role = 'editor'
`

// RevokeSiteEditor uses the same transaction-only namespace lock as deploys.
// It returns false when the username was not currently an editor.
func RevokeSiteEditor(ctx context.Context, tx *sql.Tx, ownerID, siteName, siteID, username string) (bool, error) {
	if tx == nil {
		return false, fmt.Errorf("revoke site editor: nil transaction")
	}
	if err := LockSiteCollaboration(ctx, tx, ownerID, siteName); err != nil {
		return false, err
	}
	if err := requireSiteIncarnation(ctx, tx, ownerID, siteName, siteID); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, revokeSiteEditorQuery, siteID, strings.ToLower(strings.TrimSpace(username)))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected > 1 {
		return false, fmt.Errorf("revoke site editor affected %d rows, want at most 1", affected)
	}
	return affected == 1, nil
}

func requireSiteIncarnation(ctx context.Context, tx *sql.Tx, ownerID, siteName, siteID string) error {
	const query = `
		SELECT 1
		FROM sites
		WHERE id = $1::uuid AND user_id = $2::uuid AND name = $3
	`
	var one int
	return tx.QueryRowContext(ctx, query, siteID, ownerID, siteName).Scan(&one)
}

const countSiteCollaboratorsQuery = `
	SELECT count(*)
	FROM site_collaborators
	WHERE site_id = $1::uuid
	  AND role = 'editor'
`

func CountSiteCollaborators(ctx context.Context, q Querier, siteID string) (int, error) {
	var count int
	err := q.QueryRowContext(ctx, countSiteCollaboratorsQuery, siteID).Scan(&count)
	return count, err
}

const listSiteCollaboratorCountsQuery = `
	SELECT site_id::text, count(*)::int
	FROM site_collaborators
	WHERE role = 'editor'
	GROUP BY site_id
`

// ListSiteCollaboratorCounts returns editor count per site, for every site that
// has at least one. Sites absent from the map have none. Admin-wide aggregate;
// use CountSiteCollaborators for a single site.
func ListSiteCollaboratorCounts(ctx context.Context, q Querier) (map[string]int, error) {
	rows, err := q.QueryContext(ctx, listSiteCollaboratorCountsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var siteID string
		var count int
		if err := rows.Scan(&siteID, &count); err != nil {
			return nil, err
		}
		counts[siteID] = count
	}
	return counts, rows.Err()
}

const listEditorGrantCountsByUserQuery = `
	SELECT user_id::text, count(DISTINCT site_id)::int
	FROM site_collaborators
	WHERE role = 'editor'
	GROUP BY user_id
`

// ListEditorGrantCountsByUser returns, per user, how many sites they have been
// granted editor access to. This is the mirror of ListSiteCollaboratorCounts:
// that one counts people per site, this one counts sites per person.
func ListEditorGrantCountsByUser(ctx context.Context, q Querier) (map[string]int, error) {
	rows, err := q.QueryContext(ctx, listEditorGrantCountsByUserQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var userID string
		var count int
		if err := rows.Scan(&userID, &count); err != nil {
			return nil, err
		}
		counts[userID] = count
	}
	return counts, rows.Err()
}

const lockSiteCollaborationQuery = `
	SELECT pg_advisory_xact_lock(
		hashtextextended($1::uuid::text || chr(31) || $2, 0)
	)
`

// LockSiteCollaboration serializes transaction-scoped work for one canonical
// owner/site namespace. Requiring *sql.Tx prevents a session-level call that
// could release the lock before the protected transaction finishes.
func LockSiteCollaboration(ctx context.Context, tx *sql.Tx, ownerID, siteName string) error {
	if tx == nil {
		return fmt.Errorf("lock site collaboration: nil transaction")
	}
	_, err := tx.ExecContext(ctx, lockSiteCollaborationQuery, ownerID, siteName)
	return err
}
