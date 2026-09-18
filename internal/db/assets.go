package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Asset is one row of site_assets (design.md 7.3, migration 0026): the
// metadata for a file uploaded to a site's asset store. The bytes
// themselves live on disk at <site>/assets/<ID> (internal/storage); this
// row is what a listing, the audit trail, and the per-site quota query
// read without touching the filesystem.
type Asset struct {
	ID          string
	SiteID      string
	Name        string
	ContentType string
	Size        int64
	SHA256      []byte
	CreatedBy   *string
	CreatedAt   time.Time
	DeletedAt   *time.Time
}

// ErrAssetNotFound is returned by GetAsset and SoftDeleteAsset when no
// live (non-deleted) row matches the given site and id.
var ErrAssetNotFound = errors.New("asset not found")

// CreateAsset inserts one asset row. id comes from the storage layer,
// which already used it as the on-disk filename before this is called, so
// the row and the file always agree on what to call it — this does not
// default it from the column's own DEFAULT gen_random_uuid().
func CreateAsset(ctx context.Context, q Querier, id, siteID, name, contentType string, size int64, sha256Sum []byte, createdBy *string) (Asset, error) {
	const query = `
		INSERT INTO site_assets (id, site_id, name, content_type, size, sha256, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, site_id, name, content_type, size, sha256, created_by, created_at, deleted_at
	`
	var a Asset
	err := q.QueryRowContext(ctx, query, id, siteID, name, contentType, size, sha256Sum, createdBy).Scan(
		&a.ID, &a.SiteID, &a.Name, &a.ContentType, &a.Size, &a.SHA256, &a.CreatedBy, &a.CreatedAt, &a.DeletedAt,
	)
	return a, err
}

// GetAsset returns one live asset scoped to its site, so a caller cannot
// fetch another site's asset by guessing an id. ErrAssetNotFound covers
// "does not exist", "belongs to a different site", and "soft-deleted"
// alike — the caller never needs to distinguish them.
func GetAsset(ctx context.Context, q Querier, siteID, id string) (Asset, error) {
	const query = `
		SELECT id, site_id, name, content_type, size, sha256, created_by, created_at, deleted_at
		FROM site_assets
		WHERE id = $1 AND site_id = $2 AND deleted_at IS NULL
	`
	var a Asset
	err := q.QueryRowContext(ctx, query, id, siteID).Scan(
		&a.ID, &a.SiteID, &a.Name, &a.ContentType, &a.Size, &a.SHA256, &a.CreatedBy, &a.CreatedAt, &a.DeletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Asset{}, ErrAssetNotFound
	}
	return a, err
}

// ListAssets returns a site's live assets, newest first, for the assets
// list API (design.md 7.3: GET /api/sites/{site}/assets).
func ListAssets(ctx context.Context, q Querier, siteID string) ([]Asset, error) {
	const query = `
		SELECT id, site_id, name, content_type, size, sha256, created_by, created_at, deleted_at
		FROM site_assets
		WHERE site_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC
	`
	rows, err := q.QueryContext(ctx, query, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Asset
	for rows.Next() {
		var a Asset
		if err := rows.Scan(&a.ID, &a.SiteID, &a.Name, &a.ContentType, &a.Size, &a.SHA256, &a.CreatedBy, &a.CreatedAt, &a.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SoftDeleteAsset marks one live asset deleted, scoped to its site.
// ErrAssetNotFound covers the same three cases GetAsset's does. The
// caller (the site-facing API's DELETE handler, and storage's own
// DeleteAsset for the on-disk bytes) is responsible for removing the
// underlying file; this only stops the row from listing or counting
// against quota.
func SoftDeleteAsset(ctx context.Context, q Querier, siteID, id string) error {
	const query = `
		UPDATE site_assets SET deleted_at = now()
		WHERE id = $1 AND site_id = $2 AND deleted_at IS NULL
	`
	result, err := q.ExecContext(ctx, query, id, siteID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrAssetNotFound
	}
	return nil
}

// AssetUsage is a site's current live-asset footprint: how many rows count
// against MaxSiteCount and how many bytes count against MaxSiteBytes
// (design.md 7.3's per-site limits). Storage's own quota check computes
// the same numbers by walking disk rather than querying this table
// (internal/storage.CreateAsset's usage check) so an outage or drift in
// either store cannot let the other one be bypassed — see
// docs/security-review.md.
type AssetUsage struct {
	Count int64
	Bytes int64
}

// SumAssetUsage returns a site's live-asset count and total bytes.
func SumAssetUsage(ctx context.Context, q Querier, siteID string) (AssetUsage, error) {
	const query = `
		SELECT count(*), COALESCE(sum(size), 0)
		FROM site_assets
		WHERE site_id = $1 AND deleted_at IS NULL
	`
	var usage AssetUsage
	err := q.QueryRowContext(ctx, query, siteID).Scan(&usage.Count, &usage.Bytes)
	return usage, err
}
