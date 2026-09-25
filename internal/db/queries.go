package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lib/pq"
)

// GetUserByUsername takes a Querier, not a concrete *sql.DB, so a caller
// already holding a *sql.Tx (site_api.go's auditEvent, resolving an
// owner's username to its id from inside the same transaction as the
// write it is auditing) can pass that instead of a second, separate
// connection — required, not just tidier: a caller with MaxOpenConns(1)
// deadlocks outright if this runs against h.database while a transaction
// on that same *sql.DB is already open and unfinished.
func GetUserByUsername(ctx context.Context, db Querier, username string) (User, error) {
	const query = `
		SELECT id, username, is_admin, created_at, kind
		FROM users
		WHERE username = $1
	`

	var user User
	err := db.QueryRowContext(ctx, query, username).Scan(
		&user.ID,
		&user.Username,
		&user.IsAdmin,
		&user.CreatedAt,
		&user.Kind,
	)
	return user, err
}

// GetUserByEmail finds the one account registered to an address.
//
// The address is how a person identifies themselves at sign-in, while the
// username is only what the address was turned into when the account was
// made. Those agree for every account created by registration, and they stop
// agreeing the moment a name is corrected, so looking the address up directly
// is what lets a renamed account still be found by its owner.
//
// It deliberately refuses to answer when two accounts share an address. The
// column is not unique, because a typo account and its owner may legitimately
// both carry one, and handing back an arbitrary one of them would be a way to
// reach somebody else's account. The caller falls back to the username rule.
func GetUserByEmail(ctx context.Context, db *sql.DB, email string) (User, error) {
	const query = `
		SELECT id, username, is_admin, created_at, kind
		FROM users
		WHERE email = $1
	`

	rows, err := db.QueryContext(ctx, query, email)
	if err != nil {
		return User{}, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return User{}, err
		}
		return User{}, sql.ErrNoRows
	}
	var user User
	if err := rows.Scan(&user.ID, &user.Username, &user.IsAdmin, &user.CreatedAt, &user.Kind); err != nil {
		return User{}, err
	}
	if rows.Next() {
		// Ambiguous: say so rather than pick one.
		return User{}, sql.ErrNoRows
	}
	return user, rows.Err()
}

// Querier abstracts *sql.DB and *sql.Tx for transaction-safe queries.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func CreateSite(ctx context.Context, q Querier, userID, name string) (Site, error) {
	const query = `
		INSERT INTO sites (user_id, name)
		VALUES ($1, $2)
		RETURNING id, user_id, name, active_version, public, created_at, updated_at
	`

	var site Site

	err := q.QueryRowContext(ctx, query, userID, name).Scan(
		&site.ID,
		&site.UserID,
		&site.Name,
		&site.ActiveVersion,
		&site.Public,
		&site.CreatedAt,
		&site.UpdatedAt,
	)
	if err != nil {
		return Site{}, err
	}

	return site, nil
}

func GetSite(ctx context.Context, q Querier, userID, name string) (Site, error) {
	const query = `
		SELECT id, user_id, name, active_version, public, created_at, updated_at
		FROM sites
		WHERE user_id = $1 AND name = $2
	`

	var site Site

	err := q.QueryRowContext(ctx, query, userID, name).Scan(
		&site.ID,
		&site.UserID,
		&site.Name,
		&site.ActiveVersion,
		&site.Public,
		&site.CreatedAt,
		&site.UpdatedAt,
	)
	if err != nil {
		return Site{}, err
	}

	return site, nil
}

func GetSiteState(ctx context.Context, db *sql.DB, username, sitename string) (json.RawMessage, string, error) {
	const query = `
		SELECT s.id::text, s.state
		FROM sites s
		INNER JOIN users u ON u.id = s.user_id
		WHERE u.username = $1 AND s.name = $2
	`

	var siteID string
	var state json.RawMessage
	err := db.QueryRowContext(ctx, query, username, sitename).Scan(&siteID, &state)
	if err != nil {
		return nil, "", err
	}

	return state, siteID, nil
}

// UpdateSiteState takes a Querier, not a concrete *sql.DB, so a caller that
// wants the write and its audit_events row in one all-or-nothing commit
// (design 8.1) can pass a *sql.Tx (site_api.go's PutState does).
func UpdateSiteState(ctx context.Context, db Querier, username, sitename string, state json.RawMessage) error {
	const query = `
		UPDATE sites s
		SET state = $3::jsonb, state_version = state_version + 1, uses_state = true, updated_at = now()
		FROM users u
		WHERE s.user_id = u.id
		  AND u.username = $1
		  AND s.name = $2
	`

	result, err := db.ExecContext(ctx, query, username, sitename, string(state))
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return sql.ErrNoRows
	}

	return nil
}

// MarkSiteUsesState idempotently flags that a site has used the given state
// backend variant. It's the read-path companion to the flag-setting folded
// into UpdateSiteState / UpdateSiteStateCAS: state GETs would otherwise leave
// no trace, so a site that only ever reads its state wouldn't show as a user.
// The caller coalesces and deduplicates successful process-local work. The
// immutable site ID keeps a deleted-and-recreated site from inheriting the old
// site's process-local marker. A failure here must never break the state read.
func MarkSiteUsesState(ctx context.Context, db Querier, siteID string, versioned bool) error {
	query := `
		UPDATE sites
		SET uses_state = true
		WHERE id = $1
	`
	if versioned {
		query = `
			UPDATE sites
			SET uses_versioned_state = true
			WHERE id = $1
		`
	}

	result, err := db.ExecContext(ctx, query, siteID)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	switch rowsAffected {
	case 0:
		return sql.ErrNoRows
	case 1:
		return nil
	default:
		return fmt.Errorf("mark site state usage: expected 1 affected row, got %d", rowsAffected)
	}
}

// ErrVersionConflict is returned by UpdateSiteStateCAS when the site exists
// but its state_version no longer matches the caller's expected version.
var ErrVersionConflict = errors.New("state version conflict")

func GetSiteStateVersioned(ctx context.Context, db *sql.DB, username, sitename string) (json.RawMessage, int64, string, error) {
	const query = `
		SELECT s.id::text, s.state, s.state_version
		FROM sites s
		INNER JOIN users u ON u.id = s.user_id
		WHERE u.username = $1 AND s.name = $2
	`

	var siteID string
	var state json.RawMessage
	var version int64
	err := db.QueryRowContext(ctx, query, username, sitename).Scan(&siteID, &state, &version)
	if err != nil {
		return nil, 0, "", err
	}

	return state, version, siteID, nil
}

// UpdateSiteStateCAS is the conditional save: a single atomic UPDATE that only
// applies when state_version still equals expectedVersion. Returns the new
// version on success, sql.ErrNoRows if the site doesn't exist, or
// ErrVersionConflict if the version moved (caller re-reads for the 409 body).
// UpdateSiteStateCAS takes a Querier for the same reason UpdateSiteState
// does: PutStateVersioned runs it inside a *sql.Tx shared with the
// audit_events row (design 8.1).
func UpdateSiteStateCAS(ctx context.Context, db Querier, username, sitename string, state json.RawMessage, expectedVersion int64) (int64, error) {
	const query = `
		UPDATE sites s
		SET state = $3::jsonb, state_version = state_version + 1, uses_versioned_state = true, updated_at = now()
		FROM users u
		WHERE s.user_id = u.id
		  AND u.username = $1
		  AND s.name = $2
		  AND s.state_version = $4
		RETURNING s.state_version
	`

	var newVersion int64
	err := db.QueryRowContext(ctx, query, username, sitename, string(state), expectedVersion).Scan(&newVersion)
	if err == nil {
		return newVersion, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	// 0 rows: distinguish "site doesn't exist" from "version moved".
	const exists = `
		SELECT 1
		FROM sites s
		INNER JOIN users u ON u.id = s.user_id
		WHERE u.username = $1 AND s.name = $2
	`
	var one int
	if err := db.QueryRowContext(ctx, exists, username, sitename).Scan(&one); err != nil {
		return 0, err
	}

	return 0, ErrVersionConflict
}

// kind comes back with every row because a team and a person render
// differently everywhere this is used (the admin dashboard, rankings). The
// member count rides along on the same query — one LEFT JOIN rather than a
// lookup per row.
//
// One thing about the shape that looks wrong and is not, so nobody "fixes"
// it into a slower version: GROUP BY u.id alone is legal even though
// username, is_admin, created_at and kind are neither grouped nor
// aggregated. users.id is the primary key, so Postgres knows every other
// users column is functionally dependent on it and allows them ungrouped
// (this is standard SQL and has worked since Postgres 9.1). Listing the
// rest changes nothing but noise. count(m.user_id) is 0, never NULL, for a
// person with no membership rows: count() over zero rows returns 0, and the
// LEFT JOIN's NULL m columns are not counted. So MemberCount scans into a
// plain int with no null guard. It would be NULL if this were a bare column
// instead.
const listAllUsersQuery = `
	SELECT u.id, u.username, u.is_admin, u.created_at,
	       u.kind, count(m.user_id), u.disabled_at
	FROM users u
	LEFT JOIN team_members m ON m.team_id = u.id
	GROUP BY u.id
	ORDER BY u.created_at ASC
`

func ListAllUsers(ctx context.Context, db *sql.DB) ([]User, error) {
	rows, err := db.QueryContext(ctx, listAllUsersQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.IsAdmin, &u.CreatedAt, &u.Kind, &u.MemberCount, &u.DisabledAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func ListAllSites(ctx context.Context, db *sql.DB) ([]Site, error) {
	const query = `
		SELECT id, user_id, name, active_version, public, uses_state, uses_versioned_state, created_at, updated_at, access
		FROM sites
		ORDER BY created_at ASC, name ASC
	`

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return scanSiteRows(rows)
}

func ListSitesByUser(ctx context.Context, db *sql.DB, userID string) ([]Site, error) {
	const query = `
		SELECT id, user_id, name, active_version, public, uses_state, uses_versioned_state, created_at, updated_at, access
		FROM sites
		WHERE user_id = $1
		ORDER BY created_at ASC, name ASC
	`

	rows, err := db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	return scanSiteRows(rows)
}

func ListSitesByUsername(ctx context.Context, db *sql.DB, username string) ([]Site, error) {
	const query = `
		SELECT s.id, s.user_id, s.name, s.active_version, s.public, s.uses_state, s.uses_versioned_state, s.created_at, s.updated_at, s.access
		FROM sites s
		INNER JOIN users u ON u.id = s.user_id
		WHERE u.username = $1
		ORDER BY s.created_at ASC, s.name ASC
	`

	rows, err := db.QueryContext(ctx, query, username)
	if err != nil {
		return nil, err
	}
	return scanSiteRows(rows)
}

func scanSiteRows(rows *sql.Rows) ([]Site, error) {
	defer rows.Close()

	var sites []Site
	for rows.Next() {
		var site Site

		if err := rows.Scan(
			&site.ID,
			&site.UserID,
			&site.Name,
			&site.ActiveVersion,
			&site.Public,
			&site.UsesState,
			&site.UsesVersionedState,
			&site.CreatedAt,
			&site.UpdatedAt,
			&site.Access,
		); err != nil {
			return nil, err
		}

		sites = append(sites, site)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return sites, nil
}

// RecordSiteDailyAnalytics counts one page view against a site's daily rollup.
//
// isBot routes the hit to the bot columns instead of the headline ones. Nothing
// is discarded: agent traffic is recorded separately so a number can always be
// explained, and so a misclassification is visible rather than silent.
func RecordSiteDailyAnalytics(ctx context.Context, q Querier, username, siteName string, countVisit, isBot bool) error {
	const query = `
		WITH target_site AS (
			SELECT s.id
			FROM sites s
			INNER JOIN users u ON u.id = s.user_id
			WHERE u.username = $1 AND s.name = $2
		)
		INSERT INTO site_daily_analytics (
			site_id, day, pageviews, visits, bot_pageviews, bot_visits, first_seen_at, last_seen_at
		)
		SELECT
			id,
			timezone('UTC', now())::date,
			CASE WHEN $4::boolean THEN 0 ELSE 1 END,
			CASE WHEN $3::boolean AND NOT $4::boolean THEN 1 ELSE 0 END,
			CASE WHEN $4::boolean THEN 1 ELSE 0 END,
			CASE WHEN $3::boolean AND $4::boolean THEN 1 ELSE 0 END,
			now(),
			now()
		FROM target_site
		ON CONFLICT (site_id, day) DO UPDATE
		SET pageviews     = site_daily_analytics.pageviews + EXCLUDED.pageviews,
		    visits        = site_daily_analytics.visits + EXCLUDED.visits,
		    bot_pageviews = site_daily_analytics.bot_pageviews + EXCLUDED.bot_pageviews,
		    bot_visits    = site_daily_analytics.bot_visits + EXCLUDED.bot_visits,
		    last_seen_at  = now()
	`

	result, err := q.ExecContext(ctx, query, username, siteName, countVisit, isBot)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return sql.ErrNoRows
	}

	return nil
}

const listSiteAnalyticsSummariesQuery = `
	SELECT
		site_id::text,
		COALESCE(SUM(CASE WHEN day = timezone('UTC', now())::date THEN pageviews ELSE 0 END), 0)::bigint AS today_pageviews,
		COALESCE(SUM(CASE WHEN day = timezone('UTC', now())::date THEN visits ELSE 0 END), 0)::bigint AS today_visits,
		COALESCE(SUM(pageviews), 0)::bigint AS last7_pageviews,
		COALESCE(SUM(visits), 0)::bigint AS last7_visits,
		COALESCE(SUM(bot_pageviews), 0)::bigint AS last7_bot_pageviews,
		COALESCE(SUM(bot_visits), 0)::bigint AS last7_bot_visits
	FROM site_daily_analytics
	WHERE day >= timezone('UTC', now())::date - ($1::int - 1)
	  AND day <= timezone('UTC', now())::date
	GROUP BY site_id
`

const listSiteAnalyticsSummariesForSitesQuery = `
	SELECT
		site_id::text,
		COALESCE(SUM(CASE WHEN day = timezone('UTC', now())::date THEN pageviews ELSE 0 END), 0)::bigint AS today_pageviews,
		COALESCE(SUM(CASE WHEN day = timezone('UTC', now())::date THEN visits ELSE 0 END), 0)::bigint AS today_visits,
		COALESCE(SUM(pageviews), 0)::bigint AS last7_pageviews,
		COALESCE(SUM(visits), 0)::bigint AS last7_visits,
		COALESCE(SUM(bot_pageviews), 0)::bigint AS last7_bot_pageviews,
		COALESCE(SUM(bot_visits), 0)::bigint AS last7_bot_visits
	FROM site_daily_analytics
	WHERE day >= timezone('UTC', now())::date - ($1::int - 1)
	  AND day <= timezone('UTC', now())::date
	  AND site_id = ANY($2::uuid[])
	GROUP BY site_id
`

func ListSiteAnalyticsSummaries(ctx context.Context, db *sql.DB, days int) (map[string]SiteAnalyticsSummary, error) {
	if days < 1 {
		days = 1
	}

	rows, err := db.QueryContext(ctx, listSiteAnalyticsSummariesQuery, days)
	if err != nil {
		return nil, err
	}
	return scanSiteAnalyticsSummaries(rows)
}

func ListSiteAnalyticsSummariesForSites(ctx context.Context, db *sql.DB, days int, siteIDs []string) (map[string]SiteAnalyticsSummary, error) {
	if days < 1 {
		days = 1
	}
	summaries := make(map[string]SiteAnalyticsSummary)
	if len(siteIDs) == 0 {
		return summaries, nil
	}

	rows, err := db.QueryContext(ctx, listSiteAnalyticsSummariesForSitesQuery, days, pq.Array(siteIDs))
	if err != nil {
		return nil, err
	}
	return scanSiteAnalyticsSummaries(rows)
}

func scanSiteAnalyticsSummaries(rows *sql.Rows) (map[string]SiteAnalyticsSummary, error) {
	defer rows.Close()

	summaries := make(map[string]SiteAnalyticsSummary)
	for rows.Next() {
		var summary SiteAnalyticsSummary
		if err := rows.Scan(
			&summary.SiteID,
			&summary.TodayPageviews,
			&summary.TodayVisits,
			&summary.Last7Pageviews,
			&summary.Last7Visits,
			&summary.BotPageviews,
			&summary.BotVisits,
		); err != nil {
			return nil, err
		}
		summaries[summary.SiteID] = summary
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return summaries, nil
}

func RecordSiteFileDownload(ctx context.Context, q Querier, username, siteName, path string) error {
	const query = `
		WITH target_site AS (
			SELECT s.id
			FROM sites s
			INNER JOIN users u ON u.id = s.user_id
			WHERE u.username = $1 AND s.name = $2
		)
		INSERT INTO site_file_downloads (site_id, path, day, downloads, first_seen_at, last_seen_at)
		SELECT
			id,
			$3,
			timezone('UTC', now())::date,
			1,
			now(),
			now()
		FROM target_site
		ON CONFLICT (site_id, path, day) DO UPDATE
		SET downloads = site_file_downloads.downloads + 1,
		    last_seen_at = now()
	`

	result, err := q.ExecContext(ctx, query, username, siteName, path)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return sql.ErrNoRows
	}

	return nil
}

const listSiteFileDownloadSummariesQuery = `
	SELECT
		site_id::text,
		path,
		COALESCE(SUM(downloads), 0)::bigint AS total_downloads,
		COALESCE(SUM(CASE WHEN day >= timezone('UTC', now())::date - ($1::int - 1) THEN downloads ELSE 0 END), 0)::bigint AS last7_downloads
	FROM site_file_downloads
	WHERE site_id = ANY($2::uuid[])
	GROUP BY site_id, path
`

func ListSiteFileDownloadSummaries(ctx context.Context, db *sql.DB, days int, siteIDs []string) (map[string]map[string]FileDownloadStat, error) {
	if days < 1 {
		days = 1
	}
	summaries := make(map[string]map[string]FileDownloadStat)
	if len(siteIDs) == 0 {
		return summaries, nil
	}

	rows, err := db.QueryContext(ctx, listSiteFileDownloadSummariesQuery, days, pq.Array(siteIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var siteID, path string
		var stat FileDownloadStat
		if err := rows.Scan(&siteID, &path, &stat.Total, &stat.Last7); err != nil {
			return nil, err
		}
		if summaries[siteID] == nil {
			summaries[siteID] = make(map[string]FileDownloadStat)
		}
		summaries[siteID][path] = stat
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return summaries, nil
}

const listSiteDownloadTotalsQuery = `
	SELECT
		site_id::text,
		COALESCE(SUM(downloads), 0)::bigint AS total_downloads,
		COALESCE(SUM(CASE WHEN day >= timezone('UTC', now())::date - ($1::int - 1) THEN downloads ELSE 0 END), 0)::bigint AS range_downloads
	FROM site_file_downloads
	GROUP BY site_id
`

// ListSiteDownloadTotals rolls every file's downloads up to one row per site,
// across all sites. ListSiteFileDownloadSummaries answers "which files on this
// site were downloaded"; this answers "which sites get downloaded from".
//
// No caller today. The admin ranking that used it was removed: a per-site total
// mixes a person taking an installer with a dashboard fetching its own data, so
// the two cannot be ranked against each other. Kept because the per-file
// summary is the number a site publishing a real download wants to read, and
// this is the cross-site view of it.
func ListSiteDownloadTotals(ctx context.Context, db *sql.DB, days int) (map[string]FileDownloadStat, error) {
	if days < 1 {
		days = 1
	}

	rows, err := db.QueryContext(ctx, listSiteDownloadTotalsQuery, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	totals := make(map[string]FileDownloadStat)
	for rows.Next() {
		var siteID string
		var stat FileDownloadStat
		if err := rows.Scan(&siteID, &stat.Total, &stat.Last7); err != nil {
			return nil, err
		}
		totals[siteID] = stat
	}
	return totals, rows.Err()
}

const listSiteAnalyticsDailySeriesQuery = `
	SELECT
		site_id::text,
		day,
		pageviews,
		visits
	FROM site_daily_analytics
	WHERE day >= timezone('UTC', now())::date - ($1::int - 1)
	  AND day <= timezone('UTC', now())::date
	ORDER BY site_id, day
`

const listSiteAnalyticsDailySeriesForSitesQuery = `
	SELECT
		site_id::text,
		day,
		pageviews,
		visits
	FROM site_daily_analytics
	WHERE day >= timezone('UTC', now())::date - ($1::int - 1)
	  AND day <= timezone('UTC', now())::date
	  AND site_id = ANY($2::uuid[])
	ORDER BY site_id, day
`

func ListSiteAnalyticsDailySeries(ctx context.Context, db *sql.DB, days int) (map[string][]SiteAnalyticsDay, error) {
	if days < 1 {
		days = 1
	}

	rows, err := db.QueryContext(ctx, listSiteAnalyticsDailySeriesQuery, days)
	if err != nil {
		return nil, err
	}
	return scanSiteAnalyticsDailySeries(rows)
}

func ListSiteAnalyticsDailySeriesForSites(ctx context.Context, db *sql.DB, days int, siteIDs []string) (map[string][]SiteAnalyticsDay, error) {
	if days < 1 {
		days = 1
	}
	seriesBySiteID := make(map[string][]SiteAnalyticsDay)
	if len(siteIDs) == 0 {
		return seriesBySiteID, nil
	}

	rows, err := db.QueryContext(ctx, listSiteAnalyticsDailySeriesForSitesQuery, days, pq.Array(siteIDs))
	if err != nil {
		return nil, err
	}
	return scanSiteAnalyticsDailySeries(rows)
}

func scanSiteAnalyticsDailySeries(rows *sql.Rows) (map[string][]SiteAnalyticsDay, error) {
	defer rows.Close()

	seriesBySiteID := make(map[string][]SiteAnalyticsDay)
	for rows.Next() {
		var day SiteAnalyticsDay
		if err := rows.Scan(&day.SiteID, &day.Day, &day.Pageviews, &day.Visits); err != nil {
			return nil, err
		}
		seriesBySiteID[day.SiteID] = append(seriesBySiteID[day.SiteID], day)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return seriesBySiteID, nil
}

func DeleteSite(ctx context.Context, q Querier, userID, siteName string) error {
	const query = `
		DELETE FROM sites
		WHERE user_id = $1 AND name = $2
	`

	result, err := q.ExecContext(ctx, query, userID, siteName)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	if rows != 1 {
		return fmt.Errorf("delete site affected %d rows, want 1", rows)
	}
	return nil
}

func UpdateSiteActiveVersion(ctx context.Context, db Querier, siteID string, version int) error {
	const query = `
		UPDATE sites
		SET active_version = $2, updated_at = now()
		WHERE id = $1
	`

	_, err := db.ExecContext(ctx, query, siteID, version)
	return err
}

func DeleteVersion(ctx context.Context, q Querier, versionID string) error {
	const query = `
		DELETE FROM versions
		WHERE id = $1
	`

	_, err := q.ExecContext(ctx, query, versionID)
	return err
}

const createVersionQuery = `
	WITH inserted_version AS (
		INSERT INTO versions (site_id, version_number, s3_prefix, status, uploaded_by)
		VALUES ($1, $2, $3, 'uploading', $4)
		RETURNING id, site_id, version_number, s3_prefix, status, uploaded_by, created_at
	)
	SELECT inserted_version.id, inserted_version.site_id, inserted_version.version_number,
		inserted_version.s3_prefix, inserted_version.status, inserted_version.uploaded_by,
		uploader.username, inserted_version.created_at
	FROM inserted_version
	LEFT JOIN users AS uploader ON uploader.id = inserted_version.uploaded_by
`

func CreateVersion(ctx context.Context, db Querier, siteID string, versionNumber int, s3Prefix string, uploadedBy *string) (Version, error) {
	var version Version
	err := db.QueryRowContext(ctx, createVersionQuery, siteID, versionNumber, s3Prefix, uploadedBy).Scan(
		&version.ID,
		&version.SiteID,
		&version.VersionNumber,
		&version.S3Prefix,
		&version.Status,
		&version.UploadedBy,
		&version.UploaderUsername,
		&version.CreatedAt,
	)
	return version, err
}

func ActivateVersion(ctx context.Context, db Querier, versionID string) error {
	const query = `
		UPDATE versions
		SET status = 'active'
		WHERE id = $1
	`

	_, err := db.ExecContext(ctx, query, versionID)
	return err
}

func GetMaxVersionNumber(ctx context.Context, db Querier, siteID string) (int, error) {
	const query = `
		SELECT COALESCE(MAX(version_number), 0)
		FROM versions
		WHERE site_id = $1
	`

	var maxVersion int
	err := db.QueryRowContext(ctx, query, siteID).Scan(&maxVersion)
	return maxVersion, err
}

const listVersionsQuery = `
	SELECT v.id, v.site_id, v.version_number, v.s3_prefix, v.status,
		v.uploaded_by, uploader.username, v.created_at
	FROM versions AS v
	LEFT JOIN users AS uploader ON uploader.id = v.uploaded_by
	WHERE v.site_id = $1
	ORDER BY v.version_number DESC
`

func ListVersions(ctx context.Context, q Querier, siteID string) ([]Version, error) {
	rows, err := q.QueryContext(ctx, listVersionsQuery, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var versions []Version
	for rows.Next() {
		var version Version
		if err := rows.Scan(
			&version.ID,
			&version.SiteID,
			&version.VersionNumber,
			&version.S3Prefix,
			&version.Status,
			&version.UploadedBy,
			&version.UploaderUsername,
			&version.CreatedAt,
		); err != nil {
			return nil, err
		}

		versions = append(versions, version)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return versions, nil
}

const getActiveSiteVersionQuery = `
	SELECT v.id, v.site_id, v.version_number, v.s3_prefix, v.status,
		v.uploaded_by, uploader.username, v.created_at
	FROM versions v
	INNER JOIN sites s ON s.id = v.site_id
	LEFT JOIN users AS uploader ON uploader.id = v.uploaded_by
	WHERE v.site_id = $1
	  AND v.version_number = s.active_version
	  AND v.status = 'active'
`

func GetActiveSiteVersion(ctx context.Context, db *sql.DB, siteID string) (Version, error) {
	var version Version
	err := db.QueryRowContext(ctx, getActiveSiteVersionQuery, siteID).Scan(
		&version.ID,
		&version.SiteID,
		&version.VersionNumber,
		&version.S3Prefix,
		&version.Status,
		&version.UploadedBy,
		&version.UploaderUsername,
		&version.CreatedAt,
	)
	return version, err
}
