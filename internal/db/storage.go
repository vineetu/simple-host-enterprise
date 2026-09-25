package db

import (
	"context"
	"database/sql"
	"time"
)

// ServingSite returns the id and active version of ownerUsername's site, the
// two things serving needs to find the site's current files in the bucket.
// sql.ErrNoRows means there is no such site.
func ServingSite(ctx context.Context, q Querier, ownerUsername, siteName string) (string, int, error) {
	const query = `
		SELECT s.id, s.active_version
		FROM sites s
		JOIN users u ON u.id = s.user_id
		WHERE u.username = $1 AND s.name = $2
	`
	var siteID string
	var version int
	err := q.QueryRowContext(ctx, query, ownerUsername, siteName).Scan(&siteID, &version)
	return siteID, version, err
}

// ListSiteOwnerUsernames returns every username that owns at least one site.
func ListSiteOwnerUsernames(ctx context.Context, q Querier) ([]string, error) {
	const query = `
		SELECT DISTINCT u.username
		FROM users u
		JOIN sites s ON s.user_id = u.id
		ORDER BY u.username
	`
	return queryStrings(ctx, q, query)
}

// ListSiteNamesByOwnerUsername returns the names of every site ownerUsername owns.
func ListSiteNamesByOwnerUsername(ctx context.Context, q Querier, ownerUsername string) ([]string, error) {
	const query = `
		SELECT s.name
		FROM sites s
		JOIN users u ON u.id = s.user_id
		WHERE u.username = $1
		ORDER BY s.name
	`
	return queryStrings(ctx, q, query, ownerUsername)
}

func queryStrings(ctx context.Context, q Querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

// RetireObjects queues a bucket key, or a key prefix ending in "/", for
// deletion once grace has passed (migration 0029). Call it in the same
// transaction that stops the database referring to the objects, so the queue
// and the reference can never disagree. The grace period is what lets another
// replica finish serving a version it resolved just before the change.
func RetireObjects(ctx context.Context, q Querier, key string, grace time.Duration) error {
	const query = `
		INSERT INTO storage_retired (object_key, retire_after)
		VALUES ($1, now() + make_interval(secs => $2))
	`
	_, err := q.ExecContext(ctx, query, key, grace.Seconds())
	return err
}

// RetiredObject is one due entry of the retirement queue.
type RetiredObject struct {
	ID  int64
	Key string
}

// ClaimDueRetiredObjects locks up to limit due entries for the calling
// transaction. SKIP LOCKED lets every replica run the sweep without two of
// them working the same entry.
func ClaimDueRetiredObjects(ctx context.Context, tx *sql.Tx, limit int) ([]RetiredObject, error) {
	const query = `
		SELECT id, object_key
		FROM storage_retired
		WHERE retire_after <= now()
		ORDER BY retire_after
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`
	rows, err := tx.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetiredObject
	for rows.Next() {
		var entry RetiredObject
		if err := rows.Scan(&entry.ID, &entry.Key); err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// DeleteRetiredObject removes a queue entry whose objects are gone.
func DeleteRetiredObject(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM storage_retired WHERE id = $1`, id)
	return err
}

// DeferRetiredObject pushes an entry whose deletion failed to the back of the
// queue, so one stubborn entry cannot hold up the ones behind it.
func DeferRetiredObject(ctx context.Context, tx *sql.Tx, id int64, by time.Duration) error {
	_, err := tx.ExecContext(ctx, `UPDATE storage_retired SET retire_after = now() + make_interval(secs => $2) WHERE id = $1`, id, by.Seconds())
	return err
}
