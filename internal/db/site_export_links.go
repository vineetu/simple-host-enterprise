package db

import (
	"context"
	"time"
)

// UseSiteExportLink records that the whole-site download link with this
// signature has started its download, so it works once. It reports false
// when the link was already used. Links past their expiry are dropped on the
// way (they are refused before they get here anyway).
func UseSiteExportLink(ctx context.Context, q Querier, signature string, expires time.Time) (bool, error) {
	if _, err := q.ExecContext(ctx, `DELETE FROM site_export_links_used WHERE expires_at < now()`); err != nil {
		return false, err
	}
	result, err := q.ExecContext(ctx, `
		INSERT INTO site_export_links_used (signature, expires_at) VALUES ($1, $2)
		ON CONFLICT (signature) DO NOTHING`, signature, expires)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
