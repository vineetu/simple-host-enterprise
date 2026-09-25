package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// MaxPublicSiteSearchQueryRunes, MaxPublicSiteSearchLimit, and
	// MaxPublicSiteSearchOffset keep the DB boundary bounded even when it is
	// called outside an HTTP handler.
	MaxPublicSiteSearchQueryRunes = 200
	MaxPublicSiteSearchLimit      = 50
	MaxPublicSiteSearchOffset     = 1_000

	// MaxSiteSearchTelemetryImpressions matches the largest public result page.
	// MaxSiteSearchTelemetryDeleteBatch bounds one cascading retention delete.
	MaxSiteSearchTelemetryImpressions = MaxPublicSiteSearchLimit
	MaxSiteSearchTelemetryDeleteBatch = 1_000
)

// PublicSiteSearchDocument is one ranked, live, public page returned by the
// PostgreSQL backend. SnippetSource is an untrusted plain-text fragment selected
// from normalized extractor output; it is never markup.
type PublicSiteSearchDocument struct {
	SiteID        string
	VersionNumber int
	OwnerName     string
	SiteName      string
	PagePath      string
	URLPath       string
	Title         string
	SnippetSource string
}

const publicSiteSearchQuery = `
	WITH parsed_input AS MATERIALIZED (
		SELECT candidate.parsed_query, candidate.exact_query
		FROM (
			SELECT
			websearch_to_tsquery('english'::regconfig, $1::text) AS parsed_query,
				lower($1::text) AS exact_query
		) AS candidate
		WHERE querytree(candidate.parsed_query) NOT IN ('', 'T')
	),
	ranked_pages AS (
		SELECT
			document.id AS document_id,
			document.site_id,
			document.owner_name,
			document.site_name,
			document.page_path,
			(
				ts_rank_cd(
					ARRAY[0.05, 0.10, 0.40, 1.00]::real[],
					document.search_vector,
					parsed_input.parsed_query,
					32
				)
				+ CASE
					WHEN lower(document.site_name) = parsed_input.exact_query
					THEN 0.05::real
					ELSE 0::real
				END
				+ CASE
					WHEN lower(document.title) = parsed_input.exact_query
					THEN 0.025::real
					ELSE 0::real
				END
			) AS rank
		FROM site_search_documents AS document
		JOIN sites AS live_site
			ON live_site.id = document.site_id
			AND live_site.public = true
			AND live_site.active_version = document.version_number
			-- A restricted site is not public, whatever its own public flag
			-- says: search must not surface it to anyone who is not
			-- already on its viewer list.
			AND NOT EXISTS (SELECT 1 FROM site_viewers sv WHERE sv.site_id = live_site.id)
		CROSS JOIN parsed_input
		WHERE document.search_vector @@ parsed_input.parsed_query
	),
	windowed_pages AS (
		SELECT
			document_id,
			site_id,
			owner_name,
			site_name,
			page_path,
			rank,
			row_number() OVER (
				PARTITION BY site_id
				ORDER BY
					rank DESC,
					lower(owner_name) COLLATE "C", owner_name COLLATE "C",
					lower(site_name) COLLATE "C", site_name COLLATE "C",
					lower(page_path) COLLATE "C", page_path COLLATE "C",
					site_id
			) AS site_page_number
		FROM ranked_pages
	),
	paged_sites AS MATERIALIZED (
		SELECT
			document_id,
			site_id,
			owner_name,
			site_name,
			page_path,
			rank
		FROM windowed_pages
		WHERE site_page_number = 1
		ORDER BY
			rank DESC,
			lower(owner_name) COLLATE "C", owner_name COLLATE "C",
			lower(site_name) COLLATE "C", site_name COLLATE "C",
			lower(page_path) COLLATE "C", page_path COLLATE "C",
			site_id
		LIMIT $2
		OFFSET $3
	)
	SELECT
		document.site_id::text,
		document.version_number,
		document.owner_name,
		document.site_name,
		document.page_path,
		document.url_path,
		document.title,
		left(
			ts_headline(
				'english'::regconfig,
				concat_ws(
					' ',
					NULLIF(document.description, ''),
					NULLIF(document.headings, ''),
					NULLIF(document.body_text, '')
				),
				parsed_input.parsed_query,
				'StartSel="", StopSel="", MaxFragments=1, MinWords=12, MaxWords=48, ShortWord=0'
			),
			1024
		) AS snippet_source
	FROM paged_sites
	JOIN site_search_documents AS document
		ON document.id = paged_sites.document_id
	CROSS JOIN parsed_input
	ORDER BY
		paged_sites.rank DESC,
		lower(paged_sites.owner_name) COLLATE "C", paged_sites.owner_name COLLATE "C",
		lower(paged_sites.site_name) COLLATE "C", paged_sites.site_name COLLATE "C",
		lower(paged_sites.page_path) COLLATE "C", paged_sites.page_path COLLATE "C",
		paged_sites.site_id
`

// SearchPublicSites returns the best matching page for each live public site.
// The query text, limit, and offset are always bound parameters; limit and
// offset are applied only after the per-site window has retained one page.
func SearchPublicSites(ctx context.Context, q Querier, query string, limit, offset int) ([]PublicSiteSearchDocument, error) {
	if q == nil {
		return nil, errors.New("search public sites: nil querier")
	}
	query = normalizePublicSiteSearchText(query)
	if query == "" {
		return nil, errors.New("search public sites: empty query")
	}
	if utf8.RuneCountInString(query) > MaxPublicSiteSearchQueryRunes {
		return nil, fmt.Errorf("search public sites: query exceeds %d runes", MaxPublicSiteSearchQueryRunes)
	}
	if limit < 1 || limit > MaxPublicSiteSearchLimit {
		return nil, fmt.Errorf("search public sites: limit %d is outside 1..%d", limit, MaxPublicSiteSearchLimit)
	}
	if offset < 0 {
		return nil, fmt.Errorf("search public sites: negative offset %d", offset)
	}
	if offset > MaxPublicSiteSearchOffset {
		return nil, fmt.Errorf("search public sites: offset %d exceeds %d", offset, MaxPublicSiteSearchOffset)
	}

	rows, err := q.QueryContext(ctx, publicSiteSearchQuery, query, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("search public sites: %w", err)
	}
	defer rows.Close()

	documents := make([]PublicSiteSearchDocument, 0, limit)
	for rows.Next() {
		var document PublicSiteSearchDocument
		if err := rows.Scan(
			&document.SiteID,
			&document.VersionNumber,
			&document.OwnerName,
			&document.SiteName,
			&document.PagePath,
			&document.URLPath,
			&document.Title,
			&document.SnippetSource,
		); err != nil {
			return nil, fmt.Errorf("search public sites scan: %w", err)
		}
		documents = append(documents, document)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("search public sites rows: %w", err)
	}
	return documents, nil
}

// SiteSearchTelemetryImpression is the immutable result snapshot stored for
// one displayed search result. It intentionally does not reference a mutable
// site_search_documents row.
type SiteSearchTelemetryImpression struct {
	ResultPosition int
	SiteID         string
	VersionNumber  int
	PagePath       string
}

// SiteSearchTelemetryRecord contains caller-generated IDs committed for one
// query and its impressions. ImpressionIDs preserve the input result order.
type SiteSearchTelemetryRecord struct {
	QueryID       string
	ImpressionIDs []string
}

const insertSiteSearchTelemetryQuery = `
	INSERT INTO site_search_queries (
		id, normalized_query, result_count, session_digest, created_at
	)
	VALUES ($1, $2, $3, $4, now())
`

const insertSiteSearchTelemetryImpressionsPrefix = `
	INSERT INTO site_search_impressions (
		id, query_id, result_position, site_id, version_number, page_path, created_at
	)
	VALUES
`

// RecordSiteSearchTelemetry atomically records one query and all displayed
// impressions. Telemetry is best-effort at the service boundary: callers should
// report this error separately and must not discard otherwise valid results.
func RecordSiteSearchTelemetry(
	ctx context.Context,
	database *sql.DB,
	normalizedQuery string,
	sessionDigest string,
	impressions []SiteSearchTelemetryImpression,
) (SiteSearchTelemetryRecord, error) {
	if database == nil {
		return SiteSearchTelemetryRecord{}, errors.New("record site search telemetry: nil database")
	}
	normalizedQuery = normalizePublicSiteSearchText(normalizedQuery)
	if normalizedQuery == "" {
		return SiteSearchTelemetryRecord{}, errors.New("record site search telemetry: empty normalized query")
	}
	if utf8.RuneCountInString(normalizedQuery) > MaxPublicSiteSearchQueryRunes {
		return SiteSearchTelemetryRecord{}, fmt.Errorf(
			"record site search telemetry: normalized query exceeds %d runes",
			MaxPublicSiteSearchQueryRunes,
		)
	}
	if sessionDigest == "" {
		return SiteSearchTelemetryRecord{}, errors.New("record site search telemetry: empty session digest")
	}
	if len(impressions) > MaxSiteSearchTelemetryImpressions {
		return SiteSearchTelemetryRecord{}, fmt.Errorf(
			"record site search telemetry: %d impressions exceeds %d",
			len(impressions),
			MaxSiteSearchTelemetryImpressions,
		)
	}
	if err := validateSiteSearchTelemetryImpressions(impressions); err != nil {
		return SiteSearchTelemetryRecord{}, fmt.Errorf("record site search telemetry: %w", err)
	}

	record := SiteSearchTelemetryRecord{ImpressionIDs: make([]string, len(impressions))}
	var err error
	record.QueryID, err = newSiteSearchUUID()
	if err != nil {
		return SiteSearchTelemetryRecord{}, fmt.Errorf("record site search telemetry query ID: %w", err)
	}
	for index := range impressions {
		record.ImpressionIDs[index], err = newSiteSearchUUID()
		if err != nil {
			return SiteSearchTelemetryRecord{}, fmt.Errorf("record site search telemetry impression ID: %w", err)
		}
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return SiteSearchTelemetryRecord{}, fmt.Errorf("record site search telemetry begin: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(
		ctx,
		insertSiteSearchTelemetryQuery,
		record.QueryID,
		normalizedQuery,
		len(impressions),
		sessionDigest,
	)
	if err != nil {
		return SiteSearchTelemetryRecord{}, fmt.Errorf("record site search telemetry query: %w", err)
	}
	if err := requireSiteSearchRows(result, 1, "record site search telemetry query"); err != nil {
		return SiteSearchTelemetryRecord{}, err
	}

	if err := insertSiteSearchTelemetryImpressions(ctx, tx, record, impressions); err != nil {
		return SiteSearchTelemetryRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return SiteSearchTelemetryRecord{}, fmt.Errorf("record site search telemetry commit: %w", err)
	}
	return record, nil
}

func normalizePublicSiteSearchText(value string) string {
	validUTF8 := strings.ToValidUTF8(value, "\uFFFD")
	return strings.Join(strings.Fields(validUTF8), " ")
}

func validateSiteSearchTelemetryImpressions(impressions []SiteSearchTelemetryImpression) error {
	positions := make(map[int]struct{}, len(impressions))
	for index, impression := range impressions {
		if impression.ResultPosition < 1 {
			return fmt.Errorf("impression %d has invalid position %d", index, impression.ResultPosition)
		}
		if _, duplicate := positions[impression.ResultPosition]; duplicate {
			return fmt.Errorf("impression %d repeats position %d", index, impression.ResultPosition)
		}
		positions[impression.ResultPosition] = struct{}{}
		if impression.SiteID == "" {
			return fmt.Errorf("impression %d has empty site ID", index)
		}
		if impression.VersionNumber < 1 {
			return fmt.Errorf("impression %d has invalid version %d", index, impression.VersionNumber)
		}
		if impression.PagePath == "" {
			return fmt.Errorf("impression %d has empty page path", index)
		}
	}
	return nil
}

func insertSiteSearchTelemetryImpressions(
	ctx context.Context,
	tx *sql.Tx,
	record SiteSearchTelemetryRecord,
	impressions []SiteSearchTelemetryImpression,
) error {
	if len(impressions) == 0 {
		return nil
	}

	var query strings.Builder
	query.WriteString(insertSiteSearchTelemetryImpressionsPrefix)
	args := make([]any, 0, len(impressions)*6)
	for index, impression := range impressions {
		if index > 0 {
			query.WriteByte(',')
		}
		base := len(args) + 1
		fmt.Fprintf(
			&query,
			"($%d, $%d, $%d, $%d, $%d, $%d, now())",
			base,
			base+1,
			base+2,
			base+3,
			base+4,
			base+5,
		)
		args = append(
			args,
			record.ImpressionIDs[index],
			record.QueryID,
			impression.ResultPosition,
			impression.SiteID,
			impression.VersionNumber,
			impression.PagePath,
		)
	}

	result, err := tx.ExecContext(ctx, query.String(), args...)
	if err != nil {
		return fmt.Errorf("record site search telemetry impressions: %w", err)
	}
	return requireSiteSearchRows(result, int64(len(impressions)), "record site search telemetry impressions")
}

const insertSiteSearchClickQuery = `
	INSERT INTO site_search_clicks (impression_id, session_digest, created_at)
	SELECT impression.id, $2, now()
	FROM site_search_impressions AS impression
	JOIN site_search_queries AS search_query
		ON search_query.id = impression.query_id
	WHERE impression.id = $1
		AND search_query.session_digest = $2
	ON CONFLICT (impression_id, session_digest) DO NOTHING
`

// RecordSiteSearchClick records at most one click for an impression and session.
// A false result means either the impression was absent/not from the same
// session, or that the same session had already recorded the click.
func RecordSiteSearchClick(ctx context.Context, q Querier, impressionID, sessionDigest string) (bool, error) {
	if q == nil {
		return false, errors.New("record site search click: nil querier")
	}
	if impressionID == "" {
		return false, errors.New("record site search click: empty impression ID")
	}
	if sessionDigest == "" {
		return false, errors.New("record site search click: empty session digest")
	}

	result, err := q.ExecContext(ctx, insertSiteSearchClickQuery, impressionID, sessionDigest)
	if err != nil {
		return false, fmt.Errorf("record site search click: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record site search click rows affected: %w", err)
	}
	switch rows {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, fmt.Errorf("record site search click affected %d rows, want at most 1", rows)
	}
}

const deleteExpiredSiteSearchTelemetryQuery = `
	WITH expired_queries AS MATERIALIZED (
		SELECT id
		FROM site_search_queries
		WHERE created_at < now() - interval '180 days'
		ORDER BY created_at, id
		FOR UPDATE SKIP LOCKED
		LIMIT $1
	)
	DELETE FROM site_search_queries AS search_query
	USING expired_queries
	WHERE search_query.id = expired_queries.id
`

// DeleteExpiredSiteSearchTelemetry deletes a bounded batch of query rows older
// than 180 days. Foreign-key cascades remove their impressions and clicks in the
// same statement.
func DeleteExpiredSiteSearchTelemetry(ctx context.Context, q Querier, batchSize int) (int64, error) {
	if q == nil {
		return 0, errors.New("delete expired site search telemetry: nil querier")
	}
	if batchSize < 1 || batchSize > MaxSiteSearchTelemetryDeleteBatch {
		return 0, fmt.Errorf(
			"delete expired site search telemetry: batch size %d is outside 1..%d",
			batchSize,
			MaxSiteSearchTelemetryDeleteBatch,
		)
	}

	result, err := q.ExecContext(ctx, deleteExpiredSiteSearchTelemetryQuery, batchSize)
	if err != nil {
		return 0, fmt.Errorf("delete expired site search telemetry: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired site search telemetry rows affected: %w", err)
	}
	if rows < 0 || rows > int64(batchSize) {
		return 0, fmt.Errorf(
			"delete expired site search telemetry affected %d rows, want 0..%d",
			rows,
			batchSize,
		)
	}
	return rows, nil
}

func newSiteSearchUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	// RFC 4122 version 4 and the RFC 4122 variant (binary 10xxxxxx).
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80

	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
