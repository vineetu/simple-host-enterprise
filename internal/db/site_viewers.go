package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// MaxSiteViewers mirrors MaxSiteEditors: a bounded batch per request, not a
// hard cap on how many people or teams may ever be listed, but a sane limit
// on one grant call.
const MaxSiteViewers = 50

// SiteViewer describes one entry on a site's viewer list, person or team.
type SiteViewer struct {
	PrincipalID string
	Username    string
	Kind        string // "person" or "team", from users.kind
	AddedBy     *string
	CreatedAt   time.Time
}

// ViewerCandidate is a person or team that could be added to a site's viewer
// list, with whether they already are.
type ViewerCandidate struct {
	PrincipalID   string
	Username      string
	Kind          string
	AlreadyViewer bool
	AlreadyEditor bool
}

var ErrViewerLimit = errors.New("site viewer limit reached")

var ErrSiteNotFound = errors.New("site not found")

const siteForServingQuery = `
	SELECT s.id::text, EXISTS (SELECT 1 FROM site_viewers sv WHERE sv.site_id = s.id)
	FROM sites s
	JOIN users u ON u.id = s.user_id
	WHERE u.username = $1 AND s.name = $2
`

// SiteForServing resolves a site's canonical id and restriction status by
// its owner's username and its own name — the pairing the host gate has in
// hand once it has resolved a hostname label to an owner and a site
// directory to a name, both from disk (design.md 5.2a, 7.2). It never
// consults the disk itself: the disk-based resolution stays authoritative
// for "does this site exist at all" (host_gate.resolveOwner), and this is
// only reached once that has already succeeded.
func SiteForServing(ctx context.Context, q Querier, ownerUsername, siteName string) (siteID string, restricted bool, err error) {
	err = q.QueryRowContext(ctx, siteForServingQuery, ownerUsername, siteName).Scan(&siteID, &restricted)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrSiteNotFound
	}
	return siteID, restricted, err
}

const isSiteRestrictedQuery = `SELECT EXISTS (SELECT 1 FROM site_viewers WHERE site_id = $1::uuid)`

// IsSiteRestricted reports whether a site has any viewer rows at all. This is
// the single predicate that decides both viewerAllowed's "no rows -> true"
// branch and which hostname the site is addressed at (handler.HostModel):
// the two can never disagree because both read this same table.
func IsSiteRestricted(ctx context.Context, q Querier, siteID string) (bool, error) {
	var restricted bool
	err := q.QueryRowContext(ctx, isSiteRestrictedQuery, siteID).Scan(&restricted)
	return restricted, err
}

const viewerAllowedQuery = `
	SELECT
		EXISTS (SELECT 1 FROM site_viewers WHERE site_id = $1::uuid) AS restricted,
		EXISTS (SELECT 1 FROM sites s WHERE s.id = $1::uuid AND s.user_id = $2::uuid) AS is_owner,
		EXISTS (
			SELECT 1 FROM sites s
			JOIN team_members tm ON tm.team_id = s.user_id AND tm.user_id = $2::uuid
			WHERE s.id = $1::uuid
		) AS is_owner_team_member,
		EXISTS (
			SELECT 1 FROM site_collaborators sc
			WHERE sc.site_id = $1::uuid AND sc.user_id = $2::uuid AND sc.role = 'editor'
		) AS is_editor,
		EXISTS (
			SELECT 1 FROM site_viewers sv WHERE sv.site_id = $1::uuid AND sv.principal_id = $2::uuid
		) AS is_direct_viewer,
		EXISTS (
			SELECT 1 FROM site_viewers sv
			JOIN team_members tm ON tm.team_id = sv.principal_id AND tm.user_id = $2::uuid
			WHERE sv.site_id = $1::uuid
		) AS is_team_viewer
`

// ViewerAllowed implements design.md 7.2's viewerAllowed(session.user, site)
// rule: a site with no site_viewers rows is open to any signed-in person; a
// restricted site additionally admits its owner, a member of the owner's
// team, an editor, a listed viewer, or a member of a listed team.
func ViewerAllowed(ctx context.Context, q Querier, siteID, userID string) (bool, error) {
	var restricted, isOwner, isOwnerTeamMember, isEditor, isDirectViewer, isTeamViewer bool
	err := q.QueryRowContext(ctx, viewerAllowedQuery, siteID, userID).Scan(
		&restricted, &isOwner, &isOwnerTeamMember, &isEditor, &isDirectViewer, &isTeamViewer,
	)
	if err != nil {
		return false, err
	}
	if !restricted {
		return true, nil
	}
	return isOwner || isOwnerTeamMember || isEditor || isDirectViewer || isTeamViewer, nil
}

const writerAllowedQuery = `
	SELECT
		s.state_write_mode,
		EXISTS (SELECT 1 FROM sites s2 WHERE s2.id = $1::uuid AND s2.user_id = $2::uuid) AS is_owner,
		EXISTS (
			SELECT 1 FROM sites s2
			JOIN team_members tm ON tm.team_id = s2.user_id AND tm.user_id = $2::uuid
			WHERE s2.id = $1::uuid
		) AS is_owner_team_member,
		EXISTS (
			SELECT 1 FROM site_collaborators sc
			WHERE sc.site_id = $1::uuid AND sc.user_id = $2::uuid AND sc.role = 'editor'
		) AS is_editor
	FROM sites s
	WHERE s.id = $1::uuid
`

// WriterAllowed implements design.md 7.3's writerAllowed(session.user, site)
// rule: state_write_mode "anyone" (the default) defers entirely to
// viewerAllowed — the same signed-in-or-listed rule that already governs
// reads; state_write_mode "editors" additionally narrows writes to the
// owner, a member of the owner's team, or an editor, regardless of who may
// view. ErrSiteNotFound covers a siteID that names no row, matching
// ViewerAllowed's caller contract (the site-facing API resolves siteID from
// SiteForServing first, so this is reached only for a site already known to
// exist, but a deleted-between-requests race still needs an answer).
func WriterAllowed(ctx context.Context, q Querier, siteID, userID string) (bool, error) {
	var mode string
	var isOwner, isOwnerTeamMember, isEditor bool
	err := q.QueryRowContext(ctx, writerAllowedQuery, siteID, userID).Scan(&mode, &isOwner, &isOwnerTeamMember, &isEditor)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrSiteNotFound
	}
	if err != nil {
		return false, err
	}
	if mode == "editors" {
		return isOwner || isOwnerTeamMember || isEditor, nil
	}
	return ViewerAllowed(ctx, q, siteID, userID)
}

const listRestrictedSiteIDsQuery = `SELECT DISTINCT site_id::text FROM site_viewers`

// ListRestrictedSiteIDs returns the set of every site id that currently has
// at least one viewer row, for callers that render or link many sites at
// once (the admin dashboard, the showcase, the collaboration list) and would
// otherwise pay one IsSiteRestricted query per row.
func ListRestrictedSiteIDs(ctx context.Context, q Querier) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, listRestrictedSiteIDsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	restricted := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		restricted[id] = true
	}
	return restricted, rows.Err()
}

const listSiteViewersQuery = `
	SELECT u.id::text, u.username, u.kind, sv.added_by::text, sv.created_at
	FROM site_viewers sv
	INNER JOIN users u ON u.id = sv.principal_id
	WHERE sv.site_id = $1::uuid
	ORDER BY lower(u.username), u.username, u.id
`

// ListSiteViewers returns a site's current viewer list, people and teams
// together, for the dashboard's viewer-list page.
func ListSiteViewers(ctx context.Context, q Querier, siteID string) ([]SiteViewer, error) {
	rows, err := q.QueryContext(ctx, listSiteViewersQuery, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var viewers []SiteViewer
	for rows.Next() {
		var v SiteViewer
		var addedBy sql.NullString
		if err := rows.Scan(&v.PrincipalID, &v.Username, &v.Kind, &addedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		if addedBy.Valid {
			v.AddedBy = &addedBy.String
		}
		viewers = append(viewers, v)
	}
	return viewers, rows.Err()
}

const searchViewerCandidatesQuery = `
	SELECT
		u.id::text,
		u.username,
		u.kind,
		EXISTS (SELECT 1 FROM site_viewers sv WHERE sv.site_id = $1::uuid AND sv.principal_id = u.id),
		EXISTS (SELECT 1 FROM site_collaborators sc WHERE sc.site_id = $1::uuid AND sc.user_id = u.id AND sc.role = 'editor')
	FROM users u
	INNER JOIN sites s ON s.id = $1::uuid
	WHERE u.id <> s.user_id
	  AND strpos(lower(u.username), lower($2)) > 0
	ORDER BY lower(u.username), u.username, u.id
	LIMIT $3
`

// SearchViewerCandidates mirrors SearchEditorCandidates but over every
// principal (person or team), since design.md 5.1's site_viewers table
// admits both.
func SearchViewerCandidates(ctx context.Context, q Querier, siteID, search string, limit int) ([]ViewerCandidate, error) {
	if limit < 1 || limit > 20 {
		limit = 20
	}
	rows, err := q.QueryContext(ctx, searchViewerCandidatesQuery, siteID, search, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []ViewerCandidate
	for rows.Next() {
		var c ViewerCandidate
		if err := rows.Scan(&c.PrincipalID, &c.Username, &c.Kind, &c.AlreadyViewer, &c.AlreadyEditor); err != nil {
			return nil, err
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

const grantSiteViewersQuery = `
	INSERT INTO site_viewers (site_id, principal_id, added_by)
	SELECT $1::uuid, requested.principal_id, $3::uuid
	FROM unnest($2::uuid[]) AS requested(principal_id)
	ON CONFLICT (site_id, principal_id) DO NOTHING
`

const resolveRequestedViewersQuery = `
	SELECT id::text, username
	FROM users
	WHERE username = ANY($1::text[])
	ORDER BY username, id
`

// GrantSiteViewers resolves a bounded username batch and adds them to a
// site's viewer list, restricting the site the moment the first row lands.
// tx must already hold whatever lock the caller uses to serialize collab
// changes for this site; it reuses LockSiteCollaboration for that, the same
// advisory lock GrantSiteEditors takes, since both mutate access to the same
// site and must not interleave.
func GrantSiteViewers(ctx context.Context, tx *sql.Tx, ownerID, siteName, siteID string, addedByID *string, usernames []string) ([]SiteViewer, error) {
	if tx == nil {
		return nil, fmt.Errorf("grant site viewers: nil transaction")
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
		return ListSiteViewers(ctx, tx, siteID)
	}

	resolvedRows, err := tx.QueryContext(ctx, resolveRequestedViewersQuery, pq.Array(normalized))
	if err != nil {
		return nil, err
	}
	var ids []string
	seen := map[string]bool{}
	for resolvedRows.Next() {
		var id, username string
		if err := resolvedRows.Scan(&id, &username); err != nil {
			resolvedRows.Close()
			return nil, err
		}
		ids = append(ids, id)
		seen[username] = true
	}
	if err := resolvedRows.Close(); err != nil {
		return nil, err
	}
	if err := resolvedRows.Err(); err != nil {
		return nil, err
	}
	for _, u := range normalized {
		if !seen[u] {
			return nil, ErrEditorNotFound
		}
	}

	current, err := ListSiteViewers(ctx, tx, siteID)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]struct{}, len(current))
	for _, v := range current {
		existing[v.PrincipalID] = struct{}{}
	}
	newViewers := 0
	for _, id := range ids {
		if _, ok := existing[id]; !ok {
			newViewers++
		}
	}
	if len(current)+newViewers > MaxSiteViewers {
		return nil, ErrViewerLimit
	}

	if len(ids) > 0 {
		if _, err := tx.ExecContext(ctx, grantSiteViewersQuery, siteID, pq.Array(ids), addedByID); err != nil {
			return nil, err
		}
	}
	return ListSiteViewers(ctx, tx, siteID)
}

const revokeSiteViewerQuery = `
	DELETE FROM site_viewers
	WHERE site_id = $1::uuid
	  AND principal_id = (SELECT id FROM users WHERE username = $2)
`

// RevokeSiteViewer removes one principal from a site's viewer list. It
// reports whether a row was actually removed.
func RevokeSiteViewer(ctx context.Context, tx *sql.Tx, ownerID, siteName, siteID, username string) (bool, error) {
	if err := LockSiteCollaboration(ctx, tx, ownerID, siteName); err != nil {
		return false, err
	}
	if err := requireSiteIncarnation(ctx, tx, ownerID, siteName, siteID); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, revokeSiteViewerQuery, siteID, username)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}
