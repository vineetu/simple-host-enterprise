package db

import (
	"context"
	"database/sql"
	"time"
)

// DeletedSiteRetention is how long a deleted site stays recoverable. Its row
// (saved data, history, access level, viewers, asset records) and its bucket
// objects are kept untouched for this long; then the sweeper purges the row
// and queues the objects for the usual retire sweep, after which only the
// bucket's own versioning keeps them. Thirty days matches the noncurrent-
// version retention docs/storage.md recommends.
const DeletedSiteRetention = 30 * 24 * time.Hour

// DeletedSite is a site in its recovery window.
type DeletedSite struct {
	ID            string
	OwnerID       string
	Owner         string
	Name          string
	ActiveVersion int
	Access        string
	DeletedAt     time.Time
	// DeletedBy is the username of whoever deleted it, "" when unknown.
	DeletedBy string
}

// PurgeAt is when the site stops being recoverable.
func (d DeletedSite) PurgeAt() time.Time { return d.DeletedAt.Add(DeletedSiteRetention) }

const deletedSiteColumns = `
	s.id::text, s.user_id::text, owner.username, s.name, s.active_version, s.access,
	s.deleted_at, COALESCE(deleter.username, '')
	FROM sites s
	JOIN users owner ON owner.id = s.user_id
	LEFT JOIN users deleter ON deleter.id = s.deleted_by`

func scanDeletedSites(rows *sql.Rows) ([]DeletedSite, error) {
	defer rows.Close()
	var out []DeletedSite
	for rows.Next() {
		var d DeletedSite
		if err := rows.Scan(&d.ID, &d.OwnerID, &d.Owner, &d.Name, &d.ActiveVersion, &d.Access, &d.DeletedAt, &d.DeletedBy); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SoftDeleteSite marks a live site deleted. It stops serving and leaves every
// listing at once; nothing else about the row changes.
func SoftDeleteSite(ctx context.Context, q Querier, siteID, actorID string) error {
	result, err := q.ExecContext(ctx, `
		UPDATE sites SET deleted_at = now(), deleted_by = NULLIF($2, '')::uuid
		WHERE id = $1::uuid AND deleted_at IS NULL`, siteID, actorID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetDeletedSite returns ownerID's deleted site of that name, locking its row
// for the caller's transaction. sql.ErrNoRows when there is none (or it has
// been purged).
func GetDeletedSite(ctx context.Context, tx *sql.Tx, ownerID, name string) (DeletedSite, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+deletedSiteColumns+`
		WHERE s.user_id = $1::uuid AND s.name = $2 AND s.deleted_at IS NOT NULL
		FOR UPDATE OF s`, ownerID, name)
	if err != nil {
		return DeletedSite{}, err
	}
	sites, err := scanDeletedSites(rows)
	if err != nil {
		return DeletedSite{}, err
	}
	if len(sites) == 0 {
		return DeletedSite{}, sql.ErrNoRows
	}
	return sites[0], nil
}

// ListDeletedSitesForActor returns the deleted sites in every namespace
// actorID may delete from: their own, and every team they belong to. Newest
// deletion first.
func ListDeletedSitesForActor(ctx context.Context, q Querier, actorID string) ([]DeletedSite, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+deletedSiteColumns+`
		WHERE s.deleted_at IS NOT NULL
		  AND (s.user_id = $1::uuid OR EXISTS (
			SELECT 1 FROM team_members tm WHERE tm.team_id = s.user_id AND tm.user_id = $1::uuid))
		ORDER BY s.deleted_at DESC, owner.username, s.name`, actorID)
	if err != nil {
		return nil, err
	}
	return scanDeletedSites(rows)
}

// ListAllDeletedSites returns every deleted site still in its window, for
// the admin page. Newest deletion first.
func ListAllDeletedSites(ctx context.Context, q Querier) ([]DeletedSite, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+deletedSiteColumns+`
		WHERE s.deleted_at IS NOT NULL
		ORDER BY s.deleted_at DESC, owner.username, s.name`)
	if err != nil {
		return nil, err
	}
	return scanDeletedSites(rows)
}

// UndeleteSite brings a deleted site back exactly as it was.
func UndeleteSite(ctx context.Context, q Querier, siteID string) error {
	result, err := q.ExecContext(ctx, `
		UPDATE sites SET deleted_at = NULL, deleted_by = NULL, updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NOT NULL`, siteID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SiteStoredBytes is what one site adds to its owner's storage quota: its
// versions' recorded sizes plus its live assets.
func SiteStoredBytes(ctx context.Context, q Querier, siteID string) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, `
		SELECT (SELECT COALESCE(sum(size_bytes), 0) FROM versions WHERE site_id = $1::uuid)
		     + (SELECT COALESCE(sum(size), 0) FROM site_assets WHERE site_id = $1::uuid AND deleted_at IS NULL)`,
		siteID).Scan(&n)
	return n, err
}

// ClaimExpiredDeletedSites locks up to limit deleted sites whose recovery
// window has ended, for the calling transaction to purge. SKIP LOCKED lets
// every replica's sweeper run at once.
func ClaimExpiredDeletedSites(ctx context.Context, tx *sql.Tx, retention time.Duration, limit int) ([]DeletedSite, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+deletedSiteColumns+`
		WHERE s.deleted_at IS NOT NULL AND s.deleted_at <= now() - make_interval(secs => $1)
		ORDER BY s.deleted_at
		LIMIT $2
		FOR UPDATE OF s SKIP LOCKED`, retention.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	return scanDeletedSites(rows)
}

// PurgeDeletedSite removes a deleted site's row for good; its saved data,
// history, viewers and asset records go with it (ON DELETE CASCADE). The
// caller queues the bucket objects for retirement in the same transaction.
func PurgeDeletedSite(ctx context.Context, tx *sql.Tx, siteID string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM sites WHERE id = $1::uuid AND deleted_at IS NOT NULL`, siteID)
	return err
}

// DeletedSiteHoldsName reports whether ownerID has a deleted site of that
// name still in its recovery window: the name stays held until it is purged.
func DeletedSiteHoldsName(ctx context.Context, q Querier, ownerID, name string) (bool, error) {
	var held bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM sites WHERE user_id = $1::uuid AND name = $2 AND deleted_at IS NOT NULL)`,
		ownerID, name).Scan(&held)
	return held, err
}
