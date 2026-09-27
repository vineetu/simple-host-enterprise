package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// CollaborationRole is the access an actor has to a canonical site.
type CollaborationRole string

const (
	CollaborationRoleOwner CollaborationRole = "owner"
	// CollaborationRoleMember is a person who belongs to the team that owns
	// the site. A team is a users row like any other, so membership is
	// resolved by joining team_members on the site's owner.
	CollaborationRoleMember CollaborationRole = "member"
)

// grantsSiteAccess reports whether role is one this server issues. It is the
// single list the resolvers check, so a role added to the CASE expressions
// without being added here fails closed rather than reaching a caller that
// does not know it.
func grantsSiteAccess(role CollaborationRole) bool {
	switch role {
	case CollaborationRoleOwner, CollaborationRoleMember:
		return true
	}
	return false
}

// ErrUserNotFound is a requested username that names nobody.
var ErrUserNotFound = errors.New("one or more usernames do not exist")

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

// UserCandidate is the minimal user record returned by the member picker.
type UserCandidate struct {
	UserID        string
	Username      string
	AlreadyMember bool
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
			ELSE 'member'
		END,
		s.access,
		s.network_requested_at,
		COALESCE(s.network_request_reason, ''),
		(SELECT count(*) FROM network_access_approvals a
		 WHERE a.site_id = s.id AND a.requested_at = s.network_requested_at),
		COALESCE(s.access_decision, ''),
		s.access_decision_at,
		COALESCE(s.access_decision_reason, '')
	FROM sites s
	INNER JOIN users owner ON owner.id = s.user_id
	LEFT JOIN team_members tm
		ON tm.team_id = s.user_id
		AND tm.user_id = $1::uuid
	WHERE owner.username = $2
	  AND s.name = $3
	  AND s.deleted_at IS NULL
	  AND (s.user_id = $1::uuid OR tm.user_id IS NOT NULL)
`

// ResolveSiteAccess resolves an owner-qualified site and authorizes actorID in
// the same query. Inaccessible, revoked, deleted, and unknown sites all return
// sql.ErrNoRows.
//
// Two ways to have access: owning the site, or belonging to the team that
// owns it.
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
		&access.Site.Access,
		&access.Site.NetworkRequestedAt,
		&access.Site.NetworkRequestReason,
		&access.Site.NetworkApprovals,
		&access.Site.AccessDecision,
		&access.Site.AccessDecisionAt,
		&access.Site.AccessDecisionReason,
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
			ELSE 'member'
		END,
		s.access,
		s.network_requested_at,
		COALESCE(s.network_request_reason, ''),
		(SELECT count(*) FROM network_access_approvals a
		 WHERE a.site_id = s.id AND a.requested_at = s.network_requested_at),
		COALESCE(s.access_decision, ''),
		s.access_decision_at,
		COALESCE(s.access_decision_reason, '')
	FROM sites s
	INNER JOIN users owner ON owner.id = s.user_id
	LEFT JOIN team_members tm
		ON tm.team_id = s.user_id
		AND tm.user_id = $1::uuid
	WHERE (s.user_id = $1::uuid OR tm.user_id IS NOT NULL) AND s.deleted_at IS NULL
	ORDER BY
		CASE WHEN s.user_id = $1::uuid THEN 0 ELSE 1 END,
		owner.username,
		s.name,
		s.id
`

// ListAccessibleSites returns canonical sites owned by actorID or by a team
// they belong to. team_members is keyed on (team_id, user_id), so the join
// cannot multiply rows.
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
			&accessible.Site.Access,
			&accessible.Site.NetworkRequestedAt,
			&accessible.Site.NetworkRequestReason,
			&accessible.Site.NetworkApprovals,
			&accessible.Site.AccessDecision,
			&accessible.Site.AccessDecisionAt,
			&accessible.Site.AccessDecisionReason,
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

func normalizeUsernames(usernames []string) ([]string, error) {
	seen := make(map[string]struct{}, len(usernames))
	normalized := make([]string, 0, len(usernames))
	for _, username := range usernames {
		username = strings.ToLower(strings.TrimSpace(username))
		if username == "" {
			return nil, ErrUserNotFound
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

func requireSiteIncarnation(ctx context.Context, tx *sql.Tx, ownerID, siteName, siteID string) error {
	const query = `
		SELECT 1
		FROM sites
		WHERE id = $1::uuid AND user_id = $2::uuid AND name = $3 AND deleted_at IS NULL
	`
	var one int
	return tx.QueryRowContext(ctx, query, siteID, ownerID, siteName).Scan(&one)
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

// SharedSite is a site the actor is a named viewer of, directly or through a
// team they are in, without owning it or belonging to its team.
type SharedSite struct {
	Site          Site
	OwnerUsername string
	// SharedVia is the team the viewer grant names, or "" when it names the
	// actor directly (a direct grant wins when both exist).
	SharedVia string
}

const listSharedSitesQuery = `
	WITH principals AS (
		SELECT $1::uuid AS id
		UNION
		SELECT team_id FROM team_members WHERE user_id = $1::uuid
	), grants AS (
		SELECT DISTINCT ON (sv.site_id)
			sv.site_id,
			CASE WHEN sv.principal_id = $1::uuid THEN '' ELSE via.username END AS via
		FROM site_viewers sv
		JOIN principals p ON p.id = sv.principal_id
		JOIN users via ON via.id = sv.principal_id
		ORDER BY sv.site_id, (sv.principal_id = $1::uuid) DESC, via.username
	)
	SELECT s.id::text, s.user_id::text, s.name, s.active_version, s.public,
		s.created_at, s.updated_at, owner.username, s.access, g.via
	FROM grants g
	JOIN sites s ON s.id = g.site_id
	JOIN users owner ON owner.id = s.user_id
	WHERE s.deleted_at IS NULL
	  AND s.access <> 'only_me'
	  AND s.user_id <> $1::uuid
	  AND NOT EXISTS (SELECT 1 FROM team_members tm WHERE tm.team_id = s.user_id AND tm.user_id = $1::uuid)
	ORDER BY owner.username, s.name, s.id
`

// ListSharedSites returns the sites actorID can open because they, or a team
// they are in, are named as a viewer. A site set back to only_me is left
// out: the grant is kept but opens nothing until the level changes.
func ListSharedSites(ctx context.Context, q Querier, actorID string) ([]SharedSite, error) {
	rows, err := q.QueryContext(ctx, listSharedSitesQuery, actorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sites []SharedSite
	for rows.Next() {
		var s SharedSite
		if err := rows.Scan(&s.Site.ID, &s.Site.UserID, &s.Site.Name, &s.Site.ActiveVersion, &s.Site.Public,
			&s.Site.CreatedAt, &s.Site.UpdatedAt, &s.OwnerUsername, &s.Site.Access, &s.SharedVia); err != nil {
			return nil, err
		}
		sites = append(sites, s)
	}
	return sites, rows.Err()
}
