package db

import (
	"context"
	"encoding/json"
	"time"
)

// SiteManifestData is what a site's bucket manifest records
// (storage.SiteManifest): its owner's label and name, its live version, and
// its uploaded files that are not deleted.
type SiteManifestData struct {
	Owner       string
	Site        string
	LiveVersion int
	Assets      []SiteManifestAsset
}

// SiteManifestAsset is one uploaded file's row as the manifest keeps it.
type SiteManifestAsset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// LoadSiteManifestData reads one site's manifest data. sql.ErrNoRows when
// the site is gone.
func LoadSiteManifestData(ctx context.Context, q Querier, siteID string) (SiteManifestData, error) {
	var out SiteManifestData
	var assets []byte
	err := q.QueryRowContext(ctx, `
		SELECT u.username, s.name, s.active_version,
		       COALESCE((SELECT json_agg(json_build_object('id', a.id::text, 'name', a.name, 'content_type', a.content_type,
		                                                   'size', a.size, 'sha256', encode(a.sha256, 'hex')) ORDER BY a.created_at, a.id)
		                 FROM site_assets a WHERE a.site_id = s.id AND a.deleted_at IS NULL), '[]')
		FROM sites s JOIN users u ON u.id = s.user_id
		WHERE s.id = $1::uuid`, siteID).Scan(&out.Owner, &out.Site, &out.LiveVersion, &assets)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(assets, &out.Assets)
	return out, err
}

// RebuiltSite is one site `simple-host rebuild-index` recreates from the
// bucket: the same id (so its objects' keys still match), the versions whose
// archives are there, and the uploaded files the manifest lists whose
// objects are there.
type RebuiltSite struct {
	SiteID      string
	OwnerID     string
	Name        string
	Versions    []int
	LiveVersion int
	Assets      []SiteManifestAsset
	// VersionKey names a version's object (storage.VersionKey), recorded
	// as the version row's prefix the way a restore records it.
	VersionKey func(version int) string
}

// InsertRebuiltSite recreates a site row, its version rows and its asset
// rows inside tx. The site comes back at the default access level (only
// its owner or team), with no saved data, viewers or history: those were
// only ever in the database.
func InsertRebuiltSite(ctx context.Context, q Querier, s RebuiltSite) error {
	if _, err := q.ExecContext(ctx, `
		INSERT INTO sites (id, user_id, name, active_version) VALUES ($1::uuid, $2::uuid, $3, $4)`,
		s.SiteID, s.OwnerID, s.Name, s.LiveVersion); err != nil {
		return err
	}
	for _, v := range s.Versions {
		row, err := CreateVersion(ctx, q, s.SiteID, v, s.VersionKey(v), nil)
		if err != nil {
			return err
		}
		if err := ActivateVersion(ctx, q, row.ID); err != nil {
			return err
		}
	}
	for _, a := range s.Assets {
		if _, err := q.ExecContext(ctx, `
			INSERT INTO site_assets (id, site_id, name, content_type, size, sha256, created_at)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, decode($6, 'hex'), $7)`,
			a.ID, s.SiteID, a.Name, a.ContentType, a.Size, a.SHA256, time.Now()); err != nil {
			return err
		}
	}
	return nil
}
