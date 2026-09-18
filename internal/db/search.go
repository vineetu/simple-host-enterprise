package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const maxSiteSearchQueueErrorBytes = 2 << 10

// SiteSearchOperation is the desired state represented by one coalescing queue
// row. A new enqueue replaces the desired operation and advances Generation.
type SiteSearchOperation string

const (
	SiteSearchReconcile SiteSearchOperation = "reconcile"
	SiteSearchDelete    SiteSearchOperation = "delete"
)

// SiteSearchWork is one leased queue generation. LeaseToken is supplied by the
// caller and is the fence that prevents an expired, reclaimed worker from
// changing or acknowledging newer work.
type SiteSearchWork struct {
	SiteID      string
	Operation   SiteSearchOperation
	Generation  int64
	LeaseToken  string
	AvailableAt time.Time
	LockedUntil time.Time
	Attempts    int
}

// SiteSearchSnapshot identifies the immutable version an extractor must open.
type SiteSearchSnapshot struct {
	SiteID        string
	OwnerName     string
	SiteName      string
	ActiveVersion int
}

// SearchDocument is the bounded plain-text representation accepted by the DB
// layer. It intentionally does not depend on the internal/search package.
type SearchDocument struct {
	PagePath    string
	URLPath     string
	Title       string
	Description string
	Headings    string
	BodyText    string
}

type SiteSearchClaimOutcome uint8

const (
	SiteSearchClaimUnknown SiteSearchClaimOutcome = iota
	SiteSearchClaimEmpty
	SiteSearchClaimed
)

type SiteSearchSnapshotOutcome uint8

const (
	SiteSearchSnapshotUnknown SiteSearchSnapshotOutcome = iota
	SiteSearchSnapshotMissing
	SiteSearchSnapshotFound
)

// SiteSearchFenceOutcome separates obsolete/idempotent work from database
// errors. SiteMissing is specific to reconcile publication and tells the worker
// to follow its idempotent deletion path.
type SiteSearchFenceOutcome uint8

const (
	SiteSearchFenceUnknown SiteSearchFenceOutcome = iota
	SiteSearchFenceApplied
	SiteSearchFenceStale
	SiteSearchFenceMissing
	SiteSearchFenceSiteMissing
)

const enqueueSiteSearchQuery = `
	WITH authoritative_site AS MATERIALIZED (
		SELECT id
		FROM sites
		WHERE id = $1
		FOR UPDATE
	)
	INSERT INTO site_search_queue (
		site_id, operation, generation, lease_token, available_at,
		locked_until, attempts, last_error, updated_at
	)
	SELECT id, $2, 1, NULL, now(), NULL, 0, NULL, now()
	FROM authoritative_site
	ON CONFLICT (site_id) DO UPDATE SET
		operation = EXCLUDED.operation,
		generation = site_search_queue.generation + 1,
		lease_token = NULL,
		available_at = now(),
		locked_until = NULL,
		attempts = 0,
		last_error = NULL,
		updated_at = now()
`

// EnqueueSiteSearch must be called through the same authoritative transaction
// that changes the active version or deletes the site. Locking the site before
// the queue establishes the same lock order used by reconcile publication.
func EnqueueSiteSearch(ctx context.Context, q Querier, siteID string, operation SiteSearchOperation) error {
	if q == nil {
		return errors.New("enqueue site search: nil querier")
	}
	if siteID == "" {
		return errors.New("enqueue site search: empty site ID")
	}
	if !operation.valid() {
		return fmt.Errorf("enqueue site search: invalid operation %q", operation)
	}

	result, err := q.ExecContext(ctx, enqueueSiteSearchQuery, siteID, string(operation))
	if err != nil {
		return fmt.Errorf("enqueue site search: %w", err)
	}
	return requireSiteSearchRows(result, 1, "enqueue site search")
}

const enqueueMissingSiteSearchQuery = `
	INSERT INTO site_search_queue (
		site_id, operation, generation, lease_token, available_at,
		locked_until, attempts, last_error, updated_at
	)
	SELECT
		s.id, 'reconcile', 1, NULL, now(), NULL, 0, NULL, now()
	FROM sites AS s
	LEFT JOIN site_search_index_status AS status
		ON status.site_id = s.id
	LEFT JOIN site_search_queue AS queued
		ON queued.site_id = s.id
	WHERE queued.site_id IS NULL
		AND (
			status.site_id IS NULL
			OR status.version_number <> s.active_version
			OR status.extractor_version <> $1
		)
	ON CONFLICT (site_id) DO NOTHING
`

// EnqueueMissingSiteSearch schedules startup reconciliation only when the
// active/extractor version lacks a matching success marker and no work is
// already pending. The conflict clause closes the race with live mutations.
func EnqueueMissingSiteSearch(ctx context.Context, q Querier, extractorVersion int) (int64, error) {
	if q == nil {
		return 0, errors.New("enqueue missing site search: nil querier")
	}
	if extractorVersion < 1 {
		return 0, fmt.Errorf("enqueue missing site search: invalid extractor version %d", extractorVersion)
	}

	result, err := q.ExecContext(ctx, enqueueMissingSiteSearchQuery, extractorVersion)
	if err != nil {
		return 0, fmt.Errorf("enqueue missing site search: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("enqueue missing site search rows affected: %w", err)
	}
	if rows < 0 {
		return 0, fmt.Errorf("enqueue missing site search affected %d rows", rows)
	}
	return rows, nil
}

const claimSiteSearchQuery = `
	WITH candidate AS MATERIALIZED (
		SELECT site_id
		FROM site_search_queue
		WHERE available_at <= now()
			AND (locked_until IS NULL OR locked_until <= now())
		ORDER BY available_at, updated_at, site_id
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	)
	UPDATE site_search_queue AS queued
	SET lease_token = $1,
		locked_until = now() + ($2::bigint * interval '1 millisecond'),
		attempts = queued.attempts + 1,
		updated_at = now()
	FROM candidate
	WHERE queued.site_id = candidate.site_id
	RETURNING
		queued.site_id::text,
		queued.operation,
		queued.generation,
		queued.lease_token,
		queued.available_at,
		queued.locked_until,
		queued.attempts
`

// ClaimSiteSearch atomically leases at most one due generation. Attempts count
// deliveries, so an expired lease reclaimed by another replica increments it
// again. Expiry permits reclaim; a token is invalidated when a later claim or
// enqueue replaces it, which avoids a clock-skew fence at publication time.
func ClaimSiteSearch(ctx context.Context, q Querier, leaseToken string, leaseDuration time.Duration) (SiteSearchWork, SiteSearchClaimOutcome, error) {
	if q == nil {
		return SiteSearchWork{}, SiteSearchClaimUnknown, errors.New("claim site search: nil querier")
	}
	if leaseToken == "" {
		return SiteSearchWork{}, SiteSearchClaimUnknown, errors.New("claim site search: empty lease token")
	}
	if leaseDuration <= 0 {
		return SiteSearchWork{}, SiteSearchClaimUnknown, fmt.Errorf("claim site search: invalid lease duration %s", leaseDuration)
	}
	leaseMilliseconds := int64(leaseDuration / time.Millisecond)
	if leaseDuration%time.Millisecond != 0 {
		leaseMilliseconds++
	}

	var work SiteSearchWork
	var operation string
	err := q.QueryRowContext(ctx, claimSiteSearchQuery, leaseToken, leaseMilliseconds).Scan(
		&work.SiteID,
		&operation,
		&work.Generation,
		&work.LeaseToken,
		&work.AvailableAt,
		&work.LockedUntil,
		&work.Attempts,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SiteSearchWork{}, SiteSearchClaimEmpty, nil
	}
	if err != nil {
		return SiteSearchWork{}, SiteSearchClaimUnknown, fmt.Errorf("claim site search: %w", err)
	}
	work.Operation = SiteSearchOperation(operation)
	if err := validateSiteSearchWork(work); err != nil {
		return SiteSearchWork{}, SiteSearchClaimUnknown, fmt.Errorf("claim site search result: %w", err)
	}
	return work, SiteSearchClaimed, nil
}

const loadSiteSearchSnapshotQuery = `
	SELECT s.id::text, owner.username, s.name, s.active_version
	FROM sites AS s
	JOIN users AS owner ON owner.id = s.user_id
	WHERE s.id = $1
`

func LoadSiteSearchSnapshot(ctx context.Context, q Querier, siteID string) (SiteSearchSnapshot, SiteSearchSnapshotOutcome, error) {
	if q == nil {
		return SiteSearchSnapshot{}, SiteSearchSnapshotUnknown, errors.New("load site search snapshot: nil querier")
	}
	if siteID == "" {
		return SiteSearchSnapshot{}, SiteSearchSnapshotUnknown, errors.New("load site search snapshot: empty site ID")
	}

	var snapshot SiteSearchSnapshot
	err := q.QueryRowContext(ctx, loadSiteSearchSnapshotQuery, siteID).Scan(
		&snapshot.SiteID,
		&snapshot.OwnerName,
		&snapshot.SiteName,
		&snapshot.ActiveVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SiteSearchSnapshot{}, SiteSearchSnapshotMissing, nil
	}
	if err != nil {
		return SiteSearchSnapshot{}, SiteSearchSnapshotUnknown, fmt.Errorf("load site search snapshot: %w", err)
	}
	if snapshot.ActiveVersion < 1 {
		return SiteSearchSnapshot{}, SiteSearchSnapshotUnknown, fmt.Errorf("load site search snapshot: invalid active version %d", snapshot.ActiveVersion)
	}
	return snapshot, SiteSearchSnapshotFound, nil
}

const lockSiteSearchSiteQuery = `
	SELECT owner.username, s.name, s.active_version
	FROM sites AS s
	JOIN users AS owner ON owner.id = s.user_id
	WHERE s.id = $1
	FOR UPDATE OF s
`

const lockSiteSearchQueueQuery = `
	SELECT operation, generation, lease_token
	FROM site_search_queue
	WHERE site_id = $1
	FOR UPDATE
`

const deleteSiteSearchDocumentsQuery = `
	DELETE FROM site_search_documents
	WHERE site_id = $1
`

const upsertSiteSearchStatusQuery = `
	INSERT INTO site_search_index_status (
		site_id, version_number, extractor_version, document_count, partial, indexed_at
	)
	VALUES ($1, $2, $3, $4, $5, now())
	ON CONFLICT (site_id) DO UPDATE SET
		version_number = EXCLUDED.version_number,
		extractor_version = EXCLUDED.extractor_version,
		document_count = EXCLUDED.document_count,
		partial = EXCLUDED.partial,
		indexed_at = EXCLUDED.indexed_at
`

const acknowledgeSiteSearchQuery = `
	DELETE FROM site_search_queue
	WHERE site_id = $1
		AND operation = $2
		AND generation = $3
		AND lease_token = $4
`

// PublishSiteSearchReconcile performs the authoritative publication fence in a
// fresh transaction. Site is locked before queue, matching EnqueueSiteSearch's
// lock order for active-version mutations and deletes.
func PublishSiteSearchReconcile(
	ctx context.Context,
	database *sql.DB,
	work SiteSearchWork,
	snapshot SiteSearchSnapshot,
	extractorVersion int,
	documents []SearchDocument,
	partial bool,
) (SiteSearchFenceOutcome, error) {
	if database == nil {
		return SiteSearchFenceUnknown, errors.New("publish site search: nil database")
	}
	if err := validateSiteSearchWork(work); err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("publish site search: %w", err)
	}
	if work.Operation != SiteSearchReconcile {
		return SiteSearchFenceUnknown, fmt.Errorf("publish site search: operation %q is not reconcile", work.Operation)
	}
	if snapshot.SiteID == "" || snapshot.SiteID != work.SiteID {
		return SiteSearchFenceUnknown, errors.New("publish site search: snapshot does not match work site")
	}
	if snapshot.ActiveVersion < 1 {
		return SiteSearchFenceUnknown, fmt.Errorf("publish site search: invalid active version %d", snapshot.ActiveVersion)
	}
	if extractorVersion < 1 {
		return SiteSearchFenceUnknown, fmt.Errorf("publish site search: invalid extractor version %d", extractorVersion)
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("publish site search begin: %w", err)
	}
	defer tx.Rollback()

	var lockedOwner, lockedSite string
	var lockedVersion int
	err = tx.QueryRowContext(ctx, lockSiteSearchSiteQuery, work.SiteID).Scan(
		&lockedOwner,
		&lockedSite,
		&lockedVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SiteSearchFenceSiteMissing, nil
	}
	if err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("publish site search lock site: %w", err)
	}

	lockedWork, found, err := lockSiteSearchQueue(ctx, tx, work.SiteID)
	if err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("publish site search lock queue: %w", err)
	}
	if !found {
		return SiteSearchFenceMissing, nil
	}
	if !sameSiteSearchFence(lockedWork, work) ||
		lockedVersion != snapshot.ActiveVersion ||
		lockedOwner != snapshot.OwnerName ||
		lockedSite != snapshot.SiteName {
		return SiteSearchFenceStale, nil
	}

	if _, err := tx.ExecContext(ctx, deleteSiteSearchDocumentsQuery, work.SiteID); err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("publish site search delete old documents: %w", err)
	}
	if err := insertSiteSearchDocuments(ctx, tx, work.SiteID, lockedVersion, lockedOwner, lockedSite, documents); err != nil {
		return SiteSearchFenceUnknown, err
	}

	statusResult, err := tx.ExecContext(
		ctx,
		upsertSiteSearchStatusQuery,
		work.SiteID,
		lockedVersion,
		extractorVersion,
		len(documents),
		partial,
	)
	if err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("publish site search status: %w", err)
	}
	if err := requireSiteSearchRows(statusResult, 1, "publish site search status"); err != nil {
		return SiteSearchFenceUnknown, err
	}

	ackResult, err := tx.ExecContext(
		ctx,
		acknowledgeSiteSearchQuery,
		work.SiteID,
		string(work.Operation),
		work.Generation,
		work.LeaseToken,
	)
	if err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("publish site search acknowledge: %w", err)
	}
	ackOutcome, err := siteSearchFenceRows(ackResult, "publish site search acknowledge")
	if err != nil {
		return SiteSearchFenceUnknown, err
	}
	if ackOutcome != SiteSearchFenceApplied {
		return ackOutcome, nil
	}

	if err := tx.Commit(); err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("publish site search commit: %w", err)
	}
	return SiteSearchFenceApplied, nil
}

// AcknowledgeSiteSearchDelete acknowledges an idempotent backend deletion. It
// accepts a claimed reconcile operation too, because a missing authoritative
// site is intentionally handled through the same deletion path.
func AcknowledgeSiteSearchDelete(ctx context.Context, database *sql.DB, work SiteSearchWork) (SiteSearchFenceOutcome, error) {
	if database == nil {
		return SiteSearchFenceUnknown, errors.New("acknowledge site search delete: nil database")
	}
	if err := validateSiteSearchWork(work); err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("acknowledge site search delete: %w", err)
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("acknowledge site search delete begin: %w", err)
	}
	defer tx.Rollback()

	lockedWork, found, err := lockSiteSearchQueue(ctx, tx, work.SiteID)
	if err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("acknowledge site search delete lock queue: %w", err)
	}
	if !found {
		return SiteSearchFenceMissing, nil
	}
	if !sameSiteSearchFence(lockedWork, work) {
		return SiteSearchFenceStale, nil
	}

	result, err := tx.ExecContext(
		ctx,
		acknowledgeSiteSearchQuery,
		work.SiteID,
		string(work.Operation),
		work.Generation,
		work.LeaseToken,
	)
	if err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("acknowledge site search delete: %w", err)
	}
	outcome, err := siteSearchFenceRows(result, "acknowledge site search delete")
	if err != nil || outcome != SiteSearchFenceApplied {
		return outcome, err
	}
	if err := tx.Commit(); err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("acknowledge site search delete commit: %w", err)
	}
	return SiteSearchFenceApplied, nil
}

const retrySiteSearchQuery = `
	UPDATE site_search_queue
	SET lease_token = NULL,
		available_at = $5,
		locked_until = NULL,
		last_error = $6,
		updated_at = now()
	WHERE site_id = $1
		AND operation = $2
		AND generation = $3
		AND lease_token = $4
`

// RetrySiteSearch releases a matching lease at the caller's chosen retry time.
// Attempts is deliberately not reset; the next successful claim increments it.
func RetrySiteSearch(
	ctx context.Context,
	database *sql.DB,
	work SiteSearchWork,
	availableAt time.Time,
	failure error,
) (SiteSearchFenceOutcome, error) {
	if database == nil {
		return SiteSearchFenceUnknown, errors.New("retry site search: nil database")
	}
	if err := validateSiteSearchWork(work); err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("retry site search: %w", err)
	}
	if availableAt.IsZero() {
		return SiteSearchFenceUnknown, errors.New("retry site search: zero available time")
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("retry site search begin: %w", err)
	}
	defer tx.Rollback()

	lockedWork, found, err := lockSiteSearchQueue(ctx, tx, work.SiteID)
	if err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("retry site search lock queue: %w", err)
	}
	if !found {
		return SiteSearchFenceMissing, nil
	}
	if !sameSiteSearchFence(lockedWork, work) {
		return SiteSearchFenceStale, nil
	}

	var lastError any
	if failure != nil {
		lastError = boundSiteSearchError(failure.Error())
	}
	result, err := tx.ExecContext(
		ctx,
		retrySiteSearchQuery,
		work.SiteID,
		string(work.Operation),
		work.Generation,
		work.LeaseToken,
		availableAt.UTC(),
		lastError,
	)
	if err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("retry site search: %w", err)
	}
	outcome, err := siteSearchFenceRows(result, "retry site search")
	if err != nil || outcome != SiteSearchFenceApplied {
		return outcome, err
	}
	if err := tx.Commit(); err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("retry site search commit: %w", err)
	}
	return SiteSearchFenceApplied, nil
}

type lockedSiteSearchWork struct {
	operation  SiteSearchOperation
	generation int64
	leaseToken sql.NullString
}

func lockSiteSearchQueue(ctx context.Context, tx *sql.Tx, siteID string) (lockedSiteSearchWork, bool, error) {
	var locked lockedSiteSearchWork
	var operation string
	err := tx.QueryRowContext(ctx, lockSiteSearchQueueQuery, siteID).Scan(
		&operation,
		&locked.generation,
		&locked.leaseToken,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return lockedSiteSearchWork{}, false, nil
	}
	if err != nil {
		return lockedSiteSearchWork{}, false, err
	}
	locked.operation = SiteSearchOperation(operation)
	return locked, true, nil
}

func sameSiteSearchFence(locked lockedSiteSearchWork, work SiteSearchWork) bool {
	return locked.operation == work.Operation &&
		locked.generation == work.Generation &&
		locked.leaseToken.Valid &&
		locked.leaseToken.String == work.LeaseToken
}

func insertSiteSearchDocuments(
	ctx context.Context,
	tx *sql.Tx,
	siteID string,
	version int,
	ownerName string,
	siteName string,
	documents []SearchDocument,
) error {
	if len(documents) == 0 {
		return nil
	}

	var query strings.Builder
	query.WriteString(`
		INSERT INTO site_search_documents (
			site_id, version_number, owner_name, site_name, page_path,
			url_path, title, description, headings, body_text, indexed_at
		)
		VALUES
	`)
	args := make([]any, 0, len(documents)*10)
	for i, document := range documents {
		if i > 0 {
			query.WriteString(",")
		}
		base := len(args) + 1
		fmt.Fprintf(
			&query,
			"($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, now())",
			base,
			base+1,
			base+2,
			base+3,
			base+4,
			base+5,
			base+6,
			base+7,
			base+8,
			base+9,
		)
		args = append(
			args,
			siteID,
			version,
			ownerName,
			siteName,
			document.PagePath,
			document.URLPath,
			document.Title,
			document.Description,
			document.Headings,
			document.BodyText,
		)
	}

	result, err := tx.ExecContext(ctx, query.String(), args...)
	if err != nil {
		return fmt.Errorf("publish site search insert documents: %w", err)
	}
	if err := requireSiteSearchRows(result, int64(len(documents)), "publish site search insert documents"); err != nil {
		return err
	}
	return nil
}

func (operation SiteSearchOperation) valid() bool {
	return operation == SiteSearchReconcile || operation == SiteSearchDelete
}

func validateSiteSearchWork(work SiteSearchWork) error {
	if work.SiteID == "" {
		return errors.New("empty site ID")
	}
	if !work.Operation.valid() {
		return fmt.Errorf("invalid operation %q", work.Operation)
	}
	if work.Generation < 1 {
		return fmt.Errorf("invalid generation %d", work.Generation)
	}
	if work.LeaseToken == "" {
		return errors.New("empty lease token")
	}
	return nil
}

func requireSiteSearchRows(result sql.Result, want int64, action string) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", action, err)
	}
	if rows != want {
		return fmt.Errorf("%s affected %d rows, want %d", action, rows, want)
	}
	return nil
}

func siteSearchFenceRows(result sql.Result, action string) (SiteSearchFenceOutcome, error) {
	rows, err := result.RowsAffected()
	if err != nil {
		return SiteSearchFenceUnknown, fmt.Errorf("%s rows affected: %w", action, err)
	}
	switch rows {
	case 0:
		return SiteSearchFenceStale, nil
	case 1:
		return SiteSearchFenceApplied, nil
	default:
		return SiteSearchFenceUnknown, fmt.Errorf("%s affected %d rows, want at most 1", action, rows)
	}
}

func boundSiteSearchError(message string) string {
	message = strings.ToValidUTF8(message, "\uFFFD")
	if len(message) <= maxSiteSearchQueueErrorBytes {
		return message
	}
	message = message[:maxSiteSearchQueueErrorBytes]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return message
}
