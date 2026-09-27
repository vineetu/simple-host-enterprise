package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/lib/pq"
)

// ErrSiteNameTaken is a move or rename whose destination already has a site
// by that name.
var ErrSiteNameTaken = errors.New("destination already has a site with that name")

// MoveDestination is the account or team a site is being handed to.
type MoveDestination struct {
	ID            string
	Username      string
	Kind          string
	Disabled      bool
	ActiveMembers int
}

// IsTeam reports whether the destination is a team.
func (d MoveDestination) IsTeam() bool { return d.Kind == "team" }

const getMoveDestinationQuery = `
	SELECT u.id::text, u.username, u.kind, u.disabled_at IS NOT NULL,
		(SELECT count(*)::int FROM team_members tm
		 JOIN users m ON m.id = tm.user_id
		 WHERE tm.team_id = u.id AND m.disabled_at IS NULL)
	FROM users u
	WHERE u.username = $1
`

// GetMoveDestination resolves the account or team named username, with
// whether it can still sign in (a person) or still has somebody who can (a
// team). sql.ErrNoRows when nobody holds the name.
func GetMoveDestination(ctx context.Context, q Querier, username string) (MoveDestination, error) {
	var d MoveDestination
	err := q.QueryRowContext(ctx, getMoveDestinationQuery, username).Scan(&d.ID, &d.Username, &d.Kind, &d.Disabled, &d.ActiveMembers)
	return d, err
}

const moveSiteQuery = `
	UPDATE sites SET user_id = $2::uuid, name = $3
	WHERE id = $1::uuid AND deleted_at IS NULL
`

const recordSiteRedirectQuery = `
	INSERT INTO site_redirects (owner_label, site_part, site_id)
	VALUES ($1, $2, $3::uuid)
	ON CONFLICT (owner_label, site_part)
	DO UPDATE SET site_id = EXCLUDED.site_id, created_at = now()
`

const clearSiteRedirectQuery = `
	DELETE FROM site_redirects WHERE owner_label = $1 AND site_part = $2
`

// SiteAddress is one site address, as the host gate reads it: the owner's
// host label and the site's own label beneath it.
type SiteAddress struct {
	OwnerLabel string
	SitePart   string
}

// MoveSite gives siteID to toOwnerID under toName, in tx, and records the
// address it had (from) as a redirect to it. The address it now has (to) is
// no longer anybody's redirect: a live site is always served. Files, versions,
// saved data and its history, assets, the access level and named viewers are
// all keyed by the site's id and move with it untouched. The caller holds both
// names' LockSiteCollaboration. ErrSiteNameTaken when toOwnerID already has a
// site named toName; ErrUserNotFound when toOwnerID is gone.
func MoveSite(ctx context.Context, tx *sql.Tx, siteID, toOwnerID, toName string, from, to SiteAddress) error {
	result, err := tx.ExecContext(ctx, moveSiteQuery, siteID, toOwnerID, toName)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) {
			switch pqErr.Code {
			case "23505":
				return ErrSiteNameTaken
			case "23503":
				return ErrUserNotFound
			}
		}
		return fmt.Errorf("move site: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("move site: %w", err)
	} else if n == 0 {
		return sql.ErrNoRows
	}
	if from != to {
		if _, err := tx.ExecContext(ctx, recordSiteRedirectQuery, from.OwnerLabel, from.SitePart, siteID); err != nil {
			return fmt.Errorf("record site redirect: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, clearSiteRedirectQuery, to.OwnerLabel, to.SitePart); err != nil {
		return fmt.Errorf("clear site redirect: %w", err)
	}
	return nil
}

const siteRedirectQuery = `
	SELECT u.username, s.name
	FROM site_redirects r
	JOIN sites s ON s.id = r.site_id
	JOIN users u ON u.id = s.user_id
	WHERE r.owner_label = $1 AND r.site_part = $2 AND s.deleted_at IS NULL
`

// SiteRedirect reports where the site that used to be at address now lives:
// its owner's username and its name. ok is false when the address was never
// a moved site's, or that site has since been deleted (recently deleted
// included: a redirect must not reveal a site that no longer serves).
func SiteRedirect(ctx context.Context, q Querier, address SiteAddress) (owner, site string, ok bool, err error) {
	err = q.QueryRowContext(ctx, siteRedirectQuery, address.OwnerLabel, address.SitePart).Scan(&owner, &site)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return owner, site, true, nil
}

const siteIDsByOwnerQuery = `
	SELECT id::text, name, active_version FROM sites WHERE user_id = $1::uuid AND deleted_at IS NULL ORDER BY name
`

// ListOwnerSites returns every live site ownerID holds, for moving or
// deleting all of them at once. Recently deleted sites stay where they are.
func ListOwnerSites(ctx context.Context, q Querier, ownerID string) ([]TeamSite, error) {
	rows, err := q.QueryContext(ctx, siteIDsByOwnerQuery, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list owner sites: %w", err)
	}
	defer rows.Close()
	var sites []TeamSite
	for rows.Next() {
		var site TeamSite
		if err := rows.Scan(&site.ID, &site.Name, &site.ActiveVersion); err != nil {
			return nil, fmt.Errorf("list owner sites: %w", err)
		}
		sites = append(sites, site)
	}
	return sites, rows.Err()
}
