package db

import (
	"context"
	"database/sql"
)

// OwnerUsage is what one owner (a person's or a team's namespace) holds:
// its sites, and the stored bytes of every retained version of them plus
// every live asset. A version whose size is not known yet (size_bytes NULL,
// migration 0037) counts as zero until the background fill records it.
type OwnerUsage struct {
	Sites int64
	Bytes int64
}

const lockOwnerQuotaQuery = `
	SELECT pg_advisory_xact_lock(hashtextextended('owner-quota' || chr(31) || $1::uuid::text, 0))
`

// LockOwnerQuota serializes, until tx ends, every transaction that checks
// ownerID's quota: two parallel deploys (on any replica) cannot both pass a
// check only one of them fits. Take it after the transaction's own inserts
// and immediately before OwnerUsageOf, so the count sees them and everything
// the previous holder committed.
func LockOwnerQuota(ctx context.Context, tx *sql.Tx, ownerID string) error {
	_, err := tx.ExecContext(ctx, lockOwnerQuotaQuery, ownerID)
	return err
}

// OwnerUsageOf returns ownerID's site count and stored bytes.
func OwnerUsageOf(ctx context.Context, q Querier, ownerID string) (OwnerUsage, error) {
	const query = `
		SELECT
			(SELECT count(*) FROM sites WHERE user_id = $1),
			(SELECT COALESCE(sum(v.size_bytes), 0) FROM versions v JOIN sites s ON s.id = v.site_id WHERE s.user_id = $1)
			+ (SELECT COALESCE(sum(a.size), 0) FROM site_assets a JOIN sites s ON s.id = a.site_id
			   WHERE s.user_id = $1 AND a.deleted_at IS NULL)
	`
	var usage OwnerUsage
	err := q.QueryRowContext(ctx, query, ownerID).Scan(&usage.Sites, &usage.Bytes)
	return usage, err
}

// SetVersionSize records a version's stored archive size.
func SetVersionSize(ctx context.Context, q Querier, versionID string, size int64) error {
	_, err := q.ExecContext(ctx, `UPDATE versions SET size_bytes = $2 WHERE id = $1`, versionID, size)
	return err
}

// PrunedVersion is one version row PruneVersions deleted.
type PrunedVersion struct {
	VersionNumber int
	SizeBytes     int64
}

// PruneVersions deletes siteID's versions beyond the newest keep, never the
// active one (the version the site serves, which is also the only one a
// rollback points at), and returns what it deleted so the caller can retire
// their objects in the same transaction. Run it under the site's lock.
func PruneVersions(ctx context.Context, tx *sql.Tx, siteID string, activeVersion, keep int) ([]PrunedVersion, error) {
	const query = `
		WITH doomed AS (
			SELECT id FROM versions
			WHERE site_id = $1
			ORDER BY version_number DESC
			OFFSET $3
		)
		DELETE FROM versions v
		USING doomed
		WHERE v.id = doomed.id AND v.version_number <> $2
		RETURNING v.version_number, COALESCE(v.size_bytes, 0)
	`
	rows, err := tx.QueryContext(ctx, query, siteID, activeVersion, keep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PrunedVersion
	for rows.Next() {
		var p PrunedVersion
		if err := rows.Scan(&p.VersionNumber, &p.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// VersionSizeUnknown is a version whose stored size has not been recorded.
type VersionSizeUnknown struct {
	ID            string
	SiteID        string
	VersionNumber int
}

// ListVersionsWithoutSize returns up to limit versions with no size_bytes.
func ListVersionsWithoutSize(ctx context.Context, q Querier, limit int) ([]VersionSizeUnknown, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, site_id, version_number FROM versions WHERE size_bytes IS NULL LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VersionSizeUnknown
	for rows.Next() {
		var v VersionSizeUnknown
		if err := rows.Scan(&v.ID, &v.SiteID, &v.VersionNumber); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
