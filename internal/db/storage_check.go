package db

import "context"

// StoredObject is one object the database says the bucket holds and a site
// needs: a site's live version, or one of its live uploaded files.
// Deleted is true for a site in Recently deleted, which a restore needs whole.
type StoredObject struct {
	SiteID  string
	Owner   string
	Site    string
	Version int    // the live version, when AssetID is ""
	AssetID string // an uploaded file
	Deleted bool
}

// ListStoredObjects lists every object the sites in use and in their
// recovery window depend on, grouped by site, for `simple-host
// verify-storage`.
func ListStoredObjects(ctx context.Context, q Querier) ([]StoredObject, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT s.id::text, u.username, s.name, s.active_version, '', s.deleted_at IS NOT NULL
		FROM sites s JOIN users u ON u.id = s.user_id
		WHERE s.active_version > 0
		UNION ALL
		SELECT s.id::text, u.username, s.name, 0, a.id::text, s.deleted_at IS NOT NULL
		FROM site_assets a JOIN sites s ON s.id = a.site_id JOIN users u ON u.id = s.user_id
		WHERE a.deleted_at IS NULL
		ORDER BY 1, 5`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredObject
	for rows.Next() {
		var o StoredObject
		if err := rows.Scan(&o.SiteID, &o.Owner, &o.Site, &o.Version, &o.AssetID, &o.Deleted); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
