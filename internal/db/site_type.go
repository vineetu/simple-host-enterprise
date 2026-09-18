package db

import (
	"context"
	"database/sql"
)

// SiteTypeCandidate is one public site awaiting classification, together with
// the already-extracted text of its active version. The text comes from
// site_search_documents, which the search indexer maintains — classification
// never reads the filesystem.
type SiteTypeCandidate struct {
	SiteID        string
	SiteName      string
	Username      string
	ActiveVersion int
	Title         string
	Description   string
	Headings      string
	BodyText      string
}

// listSiteTypeCandidatesQuery finds public sites whose classification is
// missing or older than the version currently being served.
//
// The join is on the root document of the active version: the page a visitor
// lands on is what decides the type. DISTINCT ON keeps one row per site even
// if several paths tie for shortest.
const listSiteTypeCandidatesQuery = `
	SELECT DISTINCT ON (s.id)
		s.id::text,
		s.name,
		u.username,
		s.active_version,
		COALESCE(d.title, ''),
		COALESCE(d.description, ''),
		COALESCE(d.headings, ''),
		COALESCE(d.body_text, '')
	FROM sites s
	JOIN users u ON u.id = s.user_id
	JOIN site_search_documents d
		ON d.site_id = s.id AND d.version_number = s.active_version
	WHERE s.public = true
	  AND (s.site_type IS NULL OR s.site_type_version IS NULL OR s.site_type_version < s.active_version)
	ORDER BY s.id, length(d.page_path), d.page_path
	LIMIT $1
`

// ListSiteTypeCandidates returns up to limit sites needing classification.
func ListSiteTypeCandidates(ctx context.Context, db *sql.DB, limit int) ([]SiteTypeCandidate, error) {
	if limit < 1 {
		limit = 1
	}
	rows, err := db.QueryContext(ctx, listSiteTypeCandidatesQuery, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []SiteTypeCandidate
	for rows.Next() {
		var c SiteTypeCandidate
		if err := rows.Scan(&c.SiteID, &c.SiteName, &c.Username, &c.ActiveVersion,
			&c.Title, &c.Description, &c.Headings, &c.BodyText); err != nil {
			return nil, err
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

// setSiteTypeQuery writes a label, fenced on the version it was derived from.
//
// Three things this deliberately does:
//   - keys on the immutable site id, so a site deleted and recreated under the
//     same name cannot inherit an in-flight label;
//   - refuses to move the type backwards, so two passes finishing out of order
//     cannot leave the older answer in place;
//   - re-checks public, so a site made private between selection and write is
//     not labelled.
//
// It does not touch updated_at. That column orders the showcase's "Recently
// updated" view, and bumping it here would silently reorder the page.
const setSiteTypeQuery = `
	UPDATE sites
	SET site_type = $2, site_type_version = $3
	WHERE id = $1
	  AND public = true
	  AND (site_type_version IS NULL OR site_type_version < $3)
`

// SetSiteType stores a classification. It reports whether the row was written;
// false means the fence rejected it, which is a normal outcome and not an error.
func SetSiteType(ctx context.Context, db *sql.DB, siteID, siteType string, version int) (bool, error) {
	result, err := db.ExecContext(ctx, setSiteTypeQuery, siteID, siteType, version)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// CountSitesByType returns the number of public sites of each type, plus the
// count of unclassified ones under the empty key.
//
// Counted over every public site, never over a filtered subset: the showcase
// shows these next to each chip while a type is selected, and a count that
// shrank to match the current view would be wrong.
const countSitesByTypeQuery = `
	SELECT COALESCE(site_type, ''), count(*)
	FROM sites
	WHERE public = true
	GROUP BY COALESCE(site_type, '')
`

func CountSitesByType(ctx context.Context, db *sql.DB) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, countSitesByTypeQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int)
	for rows.Next() {
		var key string
		var n int
		if err := rows.Scan(&key, &n); err != nil {
			return nil, err
		}
		counts[key] = n
	}
	return counts, rows.Err()
}

// listPublicSiteTypesQuery reads types as a side table keyed by site id.
//
// Deliberately not folded into ListAllSites: that query is scanned by
// scanSiteRows, which four call sites share, and widening it would change the
// shape of queries that have nothing to do with the showcase.
const listPublicSiteTypesQuery = `
	SELECT id::text, site_type
	FROM sites
	WHERE public = true AND site_type IS NOT NULL
`

// ListPublicSiteTypes returns siteID -> type for every classified public site.
func ListPublicSiteTypes(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, listPublicSiteTypesQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	types := make(map[string]string)
	for rows.Next() {
		var siteID, siteType string
		if err := rows.Scan(&siteID, &siteType); err != nil {
			return nil, err
		}
		types[siteID] = siteType
	}
	return types, rows.Err()
}
