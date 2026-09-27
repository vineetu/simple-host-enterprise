package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// SiteManifestData is what a site's bucket manifest records
// (storage.SiteManifest): its owner (id, name, kind, and a person's sign-in
// subject, which the caller hashes with the issuer), its name and live
// version, whether it is deleted or restricted by an admin, and its
// uploaded files that are not deleted.
type SiteManifestData struct {
	OwnerID          string
	Owner            string
	OwnerKind        string
	OwnerSubject     string
	Site             string
	LiveVersion      int
	DeletedAt        *time.Time
	PurgeAt          *time.Time
	Restricted       bool
	RestrictedReason string
	Assets           []SiteManifestAsset
}

// SiteManifestAsset is one uploaded file's row as the manifest keeps it.
type SiteManifestAsset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// LoadSiteManifestData reads one site's manifest data, deleted sites
// included. sql.ErrNoRows when the site is gone.
func LoadSiteManifestData(ctx context.Context, q Querier, siteID string) (SiteManifestData, error) {
	var out SiteManifestData
	var assets []byte
	var deleted, purge sql.NullTime
	err := q.QueryRowContext(ctx, `
		SELECT u.id::text, u.username, u.kind, COALESCE(u.oidc_sub, ''), s.name, s.active_version,
		       s.deleted_at, s.purge_at, COALESCE(s.access_decision = 'restricted', false), COALESCE(s.access_decision_reason, ''),
		       COALESCE((SELECT json_agg(json_build_object('id', a.id::text, 'name', a.name, 'content_type', a.content_type,
		                                                   'size', a.size, 'sha256', encode(a.sha256, 'hex')) ORDER BY a.created_at, a.id)
		                 FROM site_assets a WHERE a.site_id = s.id AND a.deleted_at IS NULL), '[]')
		FROM sites s JOIN users u ON u.id = s.user_id
		WHERE s.id = $1::uuid`, siteID).Scan(&out.OwnerID, &out.Owner, &out.OwnerKind, &out.OwnerSubject, &out.Site, &out.LiveVersion,
		&deleted, &purge, &out.Restricted, &out.RestrictedReason, &assets)
	if err != nil {
		return out, err
	}
	if deleted.Valid {
		at := deleted.Time.UTC()
		out.DeletedAt = &at
	}
	if purge.Valid && deleted.Valid {
		at := purge.Time.UTC()
		out.PurgeAt = &at
	}
	err = json.Unmarshal(assets, &out.Assets)
	return out, err
}

// LockSiteManifest serialises manifest writes for one site until tx ends,
// so a later write never lands before an earlier one.
func LockSiteManifest(ctx context.Context, tx *sql.Tx, siteID string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('site_manifest:' || $1))`, siteID)
	return err
}

// PersonWithSubject is a person account and its sign-in subject.
type PersonWithSubject struct {
	ID       string
	Username string
	Subject  string
}

// ListPeopleWithSubjects lists every person account that has a sign-in
// subject: rebuild-index matches a manifest's owner identity against them.
func ListPeopleWithSubjects(ctx context.Context, q Querier) ([]PersonWithSubject, error) {
	rows, err := q.QueryContext(ctx, `SELECT id::text, username, oidc_sub FROM users WHERE kind = 'person' AND oidc_sub IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PersonWithSubject
	for rows.Next() {
		var p PersonWithSubject
		if err := rows.Scan(&p.ID, &p.Username, &p.Subject); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
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
	// DeletedAt and PurgeAt put the site back in Recently deleted.
	DeletedAt *time.Time
	PurgeAt   time.Time
	// Restricted puts an admin's restriction back, with its reason.
	Restricted       bool
	RestrictedReason string
	// VersionKey names a version's object (storage.VersionKey), recorded
	// as the version row's prefix the way a restore records it.
	VersionKey func(version int) string
}

// InsertRebuiltSite recreates a site row, its version rows and its asset
// rows inside tx. The site comes back at the default access level (only
// its owner or team), with no saved data, viewers or history: those were
// only ever in the database. A site that was deleted comes back deleted,
// one an admin restricted comes back restricted.
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
	if s.Restricted {
		if _, err := q.ExecContext(ctx, `
			UPDATE sites SET access = 'only_me', access_decision = 'restricted', access_decision_at = now(),
			       access_decision_reason = $2, access_decision_previous = 'only_me'
			WHERE id = $1::uuid`, s.SiteID, s.RestrictedReason); err != nil {
			return err
		}
	}
	if s.DeletedAt != nil {
		if _, err := q.ExecContext(ctx, `UPDATE sites SET deleted_at = $2, purge_at = $3 WHERE id = $1::uuid`,
			s.SiteID, *s.DeletedAt, s.PurgeAt); err != nil {
			return err
		}
	}
	return nil
}
