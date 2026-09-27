package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// MaxSiteViewers bounds a site's viewer list.
const MaxSiteViewers = 50

// SiteViewer describes one entry on a site's viewer list, person or team.
type SiteViewer struct {
	PrincipalID string
	Username    string
	Kind        string // "person" or "team", from users.kind
	AddedBy     *string
	CreatedAt   time.Time
	// Pending marks a person named by email who has not signed in yet:
	// Username is then the address and PrincipalID is empty.
	Pending bool
}

// ViewerCandidate is a person or team that could be added to a site's viewer
// list, with whether they already are.
type ViewerCandidate struct {
	PrincipalID   string
	Username      string
	Kind          string
	AlreadyViewer bool
}

var ErrViewerLimit = errors.New("site viewer limit reached")

var ErrSiteNotFound = errors.New("site not found")

const siteForServingQuery = `
	SELECT s.id::text, s.access = 'specific'
	FROM sites s
	JOIN users u ON u.id = s.user_id
	WHERE u.username = $1 AND s.name = $2 AND s.deleted_at IS NULL
`

// SiteForServing resolves a site's canonical id and whether it is served on
// its own host by its owner's username and its own name — the pairing the
// host gate has in hand once it has resolved a hostname label to an owner
// and a site directory to a name. restricted is true exactly when the
// site's access level is AccessSpecific (named viewers).
func SiteForServing(ctx context.Context, q Querier, ownerUsername, siteName string) (siteID string, restricted bool, err error) {
	err = q.QueryRowContext(ctx, siteForServingQuery, ownerUsername, siteName).Scan(&siteID, &restricted)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrSiteNotFound
	}
	return siteID, restricted, err
}

const isSiteRestrictedQuery = `SELECT access = 'specific' FROM sites WHERE id = $1::uuid`

// IsSiteRestricted reports whether a site is at the named-viewers level, the
// single predicate that decides which hostname the site is addressed at
// (handler.HostModel). A site that no longer exists reports false.
func IsSiteRestricted(ctx context.Context, q Querier, siteID string) (bool, error) {
	var restricted bool
	err := q.QueryRowContext(ctx, isSiteRestrictedQuery, siteID).Scan(&restricted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return restricted, err
}

const viewerAllowedQuery = `
	SELECT
		s.access,
		s.user_id = $2::uuid OR EXISTS (
			SELECT 1 FROM team_members tm WHERE tm.team_id = s.user_id AND tm.user_id = $2::uuid
		) AS is_owner_or_member,
		EXISTS (
			SELECT 1 FROM site_viewers sv WHERE sv.site_id = s.id AND sv.principal_id = $2::uuid
		) OR EXISTS (
			SELECT 1 FROM site_viewers sv
			JOIN team_members tm ON tm.team_id = sv.principal_id AND tm.user_id = $2::uuid
			WHERE sv.site_id = s.id
		) AS is_viewer
	FROM sites s
	WHERE s.id = $1::uuid AND s.deleted_at IS NULL
`

// ViewerAllowed decides whether a signed-in person may open a site, by its
// access level: only_me admits the owner (for a team site, its members);
// specific adds the named viewers, people or members of a named team; every
// wider level admits any signed-in person. A site that no longer exists
// admits nobody. Anonymous visitors never reach this: see NetworkOpen.
func ViewerAllowed(ctx context.Context, q Querier, siteID, userID string) (bool, error) {
	var access string
	var isOwnerOrMember, isViewer bool
	err := q.QueryRowContext(ctx, viewerAllowedQuery, siteID, userID).Scan(&access, &isOwnerOrMember, &isViewer)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	switch access {
	case AccessOnlyMe:
		return isOwnerOrMember, nil
	case AccessSpecific:
		return isOwnerOrMember || isViewer, nil
	case AccessCompany, AccessListed, AccessNetwork:
		return true, nil
	}
	return false, nil
}

// WriterAllowed is who may write a site's saved data and upload or delete
// its assets as a signed-in person: whoever may open it. Saved data follows
// opening; anonymous visitors to a network site are refused before this is
// ever asked.
func WriterAllowed(ctx context.Context, q Querier, siteID, userID string) (bool, error) {
	return ViewerAllowed(ctx, q, siteID, userID)
}

const listRestrictedSiteIDsQuery = `SELECT id::text FROM sites WHERE access = 'specific'`

// ListRestrictedSiteIDs returns the set of every site id at the named-viewers
// level, for callers that render or link many sites at once (the admin
// dashboard, the showcase, the owner index) and would otherwise pay one
// IsSiteRestricted query per row.
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
	SELECT principal_id, username, kind, added_by, created_at, pending
	FROM (
		SELECT u.id::text AS principal_id, u.username, u.kind, sv.added_by::text AS added_by, sv.created_at, false AS pending
		FROM site_viewers sv
		INNER JOIN users u ON u.id = sv.principal_id
		WHERE sv.site_id = $1::uuid
		UNION ALL
		SELECT '', p.email, 'person', p.added_by::text, p.created_at, true
		FROM pending_site_viewers p
		WHERE p.site_id = $1::uuid
	) v
	ORDER BY pending, lower(username), username, principal_id
`

// ListSiteViewers returns a site's current viewer list, people and teams
// together, then the people named by email who have not signed in yet
// (Pending), for the dashboard's viewer-list page.
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
		if err := rows.Scan(&v.PrincipalID, &v.Username, &v.Kind, &addedBy, &v.CreatedAt, &v.Pending); err != nil {
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
		EXISTS (SELECT 1 FROM site_viewers sv WHERE sv.site_id = $1::uuid AND sv.principal_id = u.id)
	FROM users u
	INNER JOIN sites s ON s.id = $1::uuid
	WHERE u.id <> s.user_id
	  AND strpos(lower(u.username), lower($2)) > 0
	ORDER BY lower(u.username), u.username, u.id
	LIMIT $3
`

// SearchViewerCandidates is a case-insensitive literal substring match over
// every principal (person or team), since site_viewers admits both.
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
		if err := rows.Scan(&c.PrincipalID, &c.Username, &c.Kind, &c.AlreadyViewer); err != nil {
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
// site's viewer list. An email names the person whose account carries it,
// or, when nobody has signed in with it yet, is kept as a pending grant
// (counted against MaxSiteViewers) until they do. The caller has checked
// each email's shape and domain. It restricts the site the moment the first row lands.
// Granting also moves the site to the named-viewers level (AccessSpecific):
// a viewer list means nothing at any other level. It takes the same
// LockSiteCollaboration advisory lock deploys and access changes take. A site
// an admin restricted is refused with a SiteRestrictedError.
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
	// Granting raises the level to AccessSpecific, which an admin's
	// restriction forbids until an admin lifts it.
	if err := checkNotRestricted(ctx, tx, siteID); err != nil {
		return nil, err
	}

	requested, err := normalizeUsernames(usernames)
	if err != nil {
		return nil, err
	}
	if len(requested) == 0 {
		return ListSiteViewers(ctx, tx, siteID)
	}
	normalized, pending, err := resolveEmailNames(ctx, tx, requested)
	if err != nil {
		return nil, err
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
			return nil, ErrUserNotFound
		}
	}

	current, err := ListSiteViewers(ctx, tx, siteID)
	if err != nil {
		return nil, err
	}
	existing := make(map[string]struct{}, len(current))
	for _, v := range current {
		if v.Pending {
			existing["@"+v.Username] = struct{}{}
		} else {
			existing[v.PrincipalID] = struct{}{}
		}
	}
	newViewers := 0
	for _, id := range ids {
		if _, ok := existing[id]; !ok {
			newViewers++
		}
	}
	for _, email := range pending {
		if _, ok := existing["@"+email]; !ok {
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
	if len(pending) > 0 {
		if _, err := tx.ExecContext(ctx, addPendingSiteViewersQuery, siteID, pq.Array(pending), addedByID); err != nil {
			return nil, err
		}
	}
	if _, err := setSiteAccess(ctx, tx, siteID, AccessSpecific); err != nil {
		return nil, err
	}
	return ListSiteViewers(ctx, tx, siteID)
}

const revokeSiteViewerQuery = `
	DELETE FROM site_viewers
	WHERE site_id = $1::uuid
	  AND principal_id = (SELECT id FROM users WHERE username = $2)
`

// RevokeSiteViewer removes one principal from a site's viewer list. It
// reports whether a row was actually removed, and whether that row was a
// pending grant. An email removes the pending grant for it, or else the
// viewer whose account carries it.
func RevokeSiteViewer(ctx context.Context, tx *sql.Tx, ownerID, siteName, siteID, username string) (removed, pending bool, err error) {
	if err := LockSiteCollaboration(ctx, tx, ownerID, siteName); err != nil {
		return false, false, err
	}
	if err := requireSiteIncarnation(ctx, tx, ownerID, siteName, siteID); err != nil {
		return false, false, err
	}
	if IsEmailName(username) {
		removed, err := execAffected(ctx, tx, removePendingSiteViewerQuery, siteID, username)
		if err != nil || removed {
			return removed, removed, err
		}
		byEmail, _, err := resolveEmails(ctx, tx, []string{username})
		if err != nil {
			return false, false, err
		}
		if byEmail[username] == "" {
			return false, false, nil
		}
		username = byEmail[username]
	}
	removed, err = execAffected(ctx, tx, revokeSiteViewerQuery, siteID, username)
	return removed, false, err
}

func execAffected(ctx context.Context, tx *sql.Tx, query string, args ...any) (bool, error) {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}
