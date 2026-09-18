package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// AuditEvent is one row of audit_events (design.md 8.1). String fields that
// are optional on the table (RequestID, ActorID, KeyID, OwnerID, SiteID,
// TeamID, ViaSiteLabel, ViaSiteName, IP, UserAgent) use "" for NULL; every
// insert path below turns "" back into NULL with NULLIF, the same
// convention CreateSession already uses for its own optional ip column.
type AuditEvent struct {
	ID              int64
	At              time.Time
	RequestID       string
	ActorID         string
	ActorKind       string
	KeyID           string
	Action          string
	OwnerID         string
	SiteID          string
	TeamID          string
	ViaSiteLabel    string
	ViaSiteName     string
	ViaSiteObserved bool
	IP              string
	UserAgent       string
	Detail          map[string]any
}

func marshalDetail(detail map[string]any) ([]byte, error) {
	if len(detail) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(detail)
}

// InsertAuditEvent writes one row. q may be *sql.DB or *sql.Tx: design 8.1
// calls for "audit.Record(ctx, tx, event), called inside the mutation's
// transaction where one exists," and Querier is what lets this function
// serve both that caller and the plain non-transactional one Phase 1's
// handlers already use.
func InsertAuditEvent(ctx context.Context, q Querier, e AuditEvent) error {
	if e.Action == "" {
		return errors.New("db: InsertAuditEvent requires an action")
	}
	if e.ActorKind == "" {
		e.ActorKind = "person"
	}
	detail, err := marshalDetail(e.Detail)
	if err != nil {
		return fmt.Errorf("marshal audit detail: %w", err)
	}
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	const query = `
		INSERT INTO audit_events (
			at, request_id, actor_id, actor_kind, key_id, action,
			owner_id, site_id, team_id, via_site_label, via_site_name,
			via_site_observed, ip, user_agent, detail
		) VALUES (
			$1, NULLIF($2, ''), NULLIF($3, '')::uuid, $4, NULLIF($5, '')::uuid, $6,
			NULLIF($7, '')::uuid, NULLIF($8, '')::uuid, NULLIF($9, '')::uuid, NULLIF($10, ''), NULLIF($11, ''),
			$12, NULLIF($13, '')::inet, NULLIF($14, ''), $15::jsonb
		)
	`
	_, err = q.ExecContext(ctx, query,
		at, e.RequestID, e.ActorID, e.ActorKind, e.KeyID, e.Action,
		e.OwnerID, e.SiteID, e.TeamID, e.ViaSiteLabel, e.ViaSiteName,
		e.ViaSiteObserved, e.IP, e.UserAgent, detail,
	)
	return err
}

// BumpStateWriteParams is one state_write coalescing call (design 8.1: "one
// row per (actor, site, five-minute window) with detail.count, upserted").
// WindowStart is the deterministic window key the caller computes (the
// event time truncated to the coalescing interval); it is also the row's
// `at`, so two calls in the same window always target the same row.
type BumpStateWriteParams struct {
	WindowStart time.Time
	ActorID     string
	OwnerID     string
	SiteID      string
	RequestID   string
	ActorKind   string
	KeyID       string
	IP          string
	UserAgent   string
}

// BumpStateWrite creates or increments the coalesced state_write row for one
// window, through the SECURITY DEFINER function migration 0027 grants the
// application role EXECUTE on (design 9.3): the app role has no general
// UPDATE on audit_events, only this one narrow upsert.
func BumpStateWrite(ctx context.Context, q Querier, p BumpStateWriteParams) error {
	if p.WindowStart.IsZero() {
		return errors.New("db: BumpStateWrite requires a WindowStart")
	}
	// The coalescing arbiter is (site_id, actor_id, at); Postgres treats
	// every NULL as distinct from every other NULL, so a row with either
	// column NULL would never conflict with itself and every state_write
	// for that (missing) actor or site would insert a fresh row instead of
	// coalescing — silently defeating design 8.1's "one row per window."
	// Every real state_write has both, so refuse before it ever reaches
	// the upsert rather than let it coalesce incorrectly.
	if p.ActorID == "" || p.SiteID == "" {
		return errors.New("db: BumpStateWrite requires both ActorID and SiteID (the coalescing key would not coalesce otherwise)")
	}
	if p.ActorKind == "" {
		p.ActorKind = "person"
	}
	const query = `
		SELECT audit_bump_state_write($1, NULLIF($2, '')::uuid, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, NULLIF($5, ''), $6, NULLIF($7, '')::uuid, NULLIF($8, '')::inet, NULLIF($9, ''))
	`
	_, err := q.ExecContext(ctx, query,
		p.WindowStart, p.ActorID, p.OwnerID, p.SiteID, p.RequestID, p.ActorKind, p.KeyID, p.IP, p.UserAgent,
	)
	return err
}

// AuditEventFilter narrows ListAuditEvents. Every field beyond Admin and
// OwnerScope is optional; a zero value means "no restriction on this
// field." OwnerScope, when non-empty, restricts results to rows whose
// owner_id or team_id is one of these ids (design 8.3: "scope: the
// caller's namespace and every team they belong to"). Admin must be true
// for a caller that passes OwnerScope empty — ListAuditEvents refuses
// !Admin with an empty OwnerScope rather than silently returning every
// owner's events, the same fail-closed rule ListAccessLog already applies
// (review finding, Phase 4 core: this filter had no such guard).
type AuditEventFilter struct {
	Admin      bool
	OwnerScope []string
	Owner      string
	Site       string
	Actor      string
	Action     string
	From       time.Time
	To         time.Time
}

// auditCursor is the opaque pagination key: the (at, id) of the last row a
// page ended on, so the next page's WHERE clause can resume a strict
// descending (at, id) order without re-scanning skipped rows or drifting
// under concurrent inserts (unlike an OFFSET).
type auditCursor struct {
	At time.Time
	ID int64
}

func encodeAuditCursor(c auditCursor) string {
	raw := c.At.UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(c.ID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeAuditCursor(s string) (auditCursor, error) {
	var zero auditCursor
	if s == "" {
		return zero, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return zero, fmt.Errorf("invalid cursor: %w", err)
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return zero, errors.New("invalid cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return zero, fmt.Errorf("invalid cursor: %w", err)
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return zero, fmt.Errorf("invalid cursor: %w", err)
	}
	return auditCursor{At: at, ID: id}, nil
}

// auditPageSize is the fixed page size for both /api/audit and /api/access
// (design 8.3). One constant so the two endpoints paginate identically.
const auditPageSize = 100

// AuditEventPage is one page of ListAuditEvents, newest first.
type AuditEventPage struct {
	Events     []AuditEvent
	NextCursor string
}

// listAuditEventsQuery is the shape both the owner-scoped and admin reads of
// /api/audit run: newest first, keyset-paginated on (at, id), every filter
// applied only when the caller supplied it. Exported as a package-level
// string (rather than built fresh per call) so a test can assert on its
// shape the way queries_test.go already does for other list queries.
const listAuditEventsQuery = `
	SELECT id, at, COALESCE(request_id, ''), COALESCE(actor_id::text, ''), actor_kind, COALESCE(key_id::text, ''),
	       action, COALESCE(owner_id::text, ''), COALESCE(site_id::text, ''), COALESCE(team_id::text, ''),
	       COALESCE(via_site_label, ''), COALESCE(via_site_name, ''), via_site_observed,
	       COALESCE(host(ip), ''), COALESCE(user_agent, ''), detail
	FROM audit_events
	WHERE ($1::uuid[] IS NULL OR owner_id = ANY($1::uuid[]) OR team_id = ANY($1::uuid[]))
	  AND ($2 = '' OR owner_id = $2::uuid)
	  AND ($3 = '' OR site_id = $3::uuid)
	  AND ($4 = '' OR actor_id = $4::uuid)
	  AND ($5 = '' OR action = $5)
	  AND ($6::timestamptz IS NULL OR at >= $6)
	  AND ($7::timestamptz IS NULL OR at <= $7)
	  AND ($8::timestamptz IS NULL OR (at, id) < ($8, $9))
	ORDER BY at DESC, id DESC
	LIMIT $10
`

// ListAuditEvents runs one page of /api/audit (design 8.3). db is a *sql.DB
// because a read endpoint has no transaction to share.
func ListAuditEvents(ctx context.Context, database *sql.DB, filter AuditEventFilter, cursor string) (AuditEventPage, error) {
	if !filter.Admin && len(filter.OwnerScope) == 0 {
		return AuditEventPage{}, errors.New("db: ListAuditEvents requires OwnerScope unless Admin")
	}
	if database == nil {
		return AuditEventPage{}, nil
	}
	cur, err := decodeAuditCursor(cursor)
	if err != nil {
		return AuditEventPage{}, err
	}
	var ownerScope any
	if len(filter.OwnerScope) > 0 {
		ownerScope = pq.Array(filter.OwnerScope)
	}
	var from, to, curAt any
	if !filter.From.IsZero() {
		from = filter.From
	}
	if !filter.To.IsZero() {
		to = filter.To
	}
	if !cur.At.IsZero() {
		curAt = cur.At
	}
	rows, err := database.QueryContext(ctx, listAuditEventsQuery,
		ownerScope, filter.Owner, filter.Site, filter.Actor, filter.Action,
		from, to, curAt, cur.ID, auditPageSize+1,
	)
	if err != nil {
		return AuditEventPage{}, err
	}
	defer rows.Close()

	var page AuditEventPage
	for rows.Next() {
		var e AuditEvent
		var detail []byte
		if err := rows.Scan(
			&e.ID, &e.At, &e.RequestID, &e.ActorID, &e.ActorKind, &e.KeyID,
			&e.Action, &e.OwnerID, &e.SiteID, &e.TeamID,
			&e.ViaSiteLabel, &e.ViaSiteName, &e.ViaSiteObserved,
			&e.IP, &e.UserAgent, &detail,
		); err != nil {
			return AuditEventPage{}, err
		}
		if len(detail) > 0 {
			if err := json.Unmarshal(detail, &e.Detail); err != nil {
				return AuditEventPage{}, fmt.Errorf("unmarshal audit detail: %w", err)
			}
		}
		page.Events = append(page.Events, e)
	}
	if err := rows.Err(); err != nil {
		return AuditEventPage{}, err
	}
	if len(page.Events) > auditPageSize {
		last := page.Events[auditPageSize-1]
		page.NextCursor = encodeAuditCursor(auditCursor{At: last.At, ID: last.ID})
		page.Events = page.Events[:auditPageSize]
	}
	return page, nil
}

// AccessLogEvent is one row to write to access_log (design.md 8.2), as
// internal/audit.AccessWriter batches them. Distinct from AccessLogEntry
// below (a row read back) so a write-side caller is not tempted to fill in
// ID or the fields ListAccessLog redacts for an owner-scoped read.
type AccessLogEvent struct {
	At         time.Time
	UserID     string
	SessionID  string
	OwnerLabel string
	SiteName   string
	Path       string
	Method     string
	Status     int
	Bytes      int64
	IP         string
	UserAgent  string
	ClientKind string
}

// InsertAccessLogBatch writes many access_log rows in one statement. Called
// only by internal/audit.AccessWriter's background worker, never on the
// request path directly (design 8.2: "written... through a batching
// writer"). Empty entries is a no-op, not an error, since a flush timer can
// legitimately fire with nothing queued.
func InsertAccessLogBatch(ctx context.Context, q Querier, entries []AccessLogEvent) error {
	if len(entries) == 0 {
		return nil
	}
	const cols = 12
	var b strings.Builder
	b.WriteString(`INSERT INTO access_log (at, user_id, session_id, owner_label, site_name, path, method, status, bytes, ip, user_agent, client_kind) VALUES `)
	args := make([]any, 0, len(entries)*cols)
	for i, e := range entries {
		if i > 0 {
			b.WriteString(",")
		}
		base := i * cols
		fmt.Fprintf(&b, "($%d, NULLIF($%d,'')::uuid, NULLIF($%d,'')::uuid, $%d, $%d, $%d, $%d, $%d, $%d, NULLIF($%d,'')::inet, NULLIF($%d,''), $%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10, base+11, base+12)
		at := e.At
		if at.IsZero() {
			at = time.Now()
		}
		args = append(args, at, e.UserID, e.SessionID, e.OwnerLabel, e.SiteName, e.Path, e.Method, e.Status, e.Bytes, e.IP, e.UserAgent, e.ClientKind)
	}
	_, err := q.ExecContext(ctx, b.String(), args...)
	return err
}

// AccessLogEntry is one row of access_log (design.md 8.2). IP and UserAgent
// are populated by ListAccessLog only for an admin-scoped read; an
// owner-scoped read leaves them "" regardless of what the row holds (design
// 8.3: "ip and user_agent only to admins").
type AccessLogEntry struct {
	ID         int64
	At         time.Time
	UserID     string
	SessionID  string
	OwnerLabel string
	SiteName   string
	Path       string
	Method     string
	Status     int
	Bytes      int64
	IP         string
	UserAgent  string
	ClientKind string
}

// AccessLogFilter narrows ListAccessLog. Admin, when false, both restricts
// to Owner/Site (a caller with owner scope always names the site it is
// asking about; the handler that isn't built yet enforces that it names one
// this caller may see) and redacts IP/UserAgent from the result.
type AccessLogFilter struct {
	Admin bool
	Owner string
	Site  string
	From  time.Time
	To    time.Time
}

// AccessLogPage is one page of ListAccessLog, newest first.
type AccessLogPage struct {
	Entries    []AccessLogEntry
	NextCursor string
}

const listAccessLogQuery = `
	SELECT id, at, COALESCE(user_id::text, ''), COALESCE(session_id::text, ''), owner_label, site_name,
	       path, method, status, bytes, COALESCE(host(ip), ''), COALESCE(user_agent, ''), client_kind
	FROM access_log
	WHERE ($1 = '' OR owner_label = $1)
	  AND ($2 = '' OR site_name = $2)
	  AND ($3::timestamptz IS NULL OR at >= $3)
	  AND ($4::timestamptz IS NULL OR at <= $4)
	  AND ($5::timestamptz IS NULL OR (at, id) < ($5, $6))
	ORDER BY at DESC, id DESC
	LIMIT $7
`

// ListAccessLog runs one page of /api/access (design 8.3).
func ListAccessLog(ctx context.Context, database *sql.DB, filter AccessLogFilter, cursor string) (AccessLogPage, error) {
	if !filter.Admin && filter.Owner == "" {
		return AccessLogPage{}, errors.New("db: ListAccessLog requires Owner unless Admin")
	}
	if database == nil {
		return AccessLogPage{}, nil
	}
	cur, err := decodeAuditCursor(cursor)
	if err != nil {
		return AccessLogPage{}, err
	}
	var from, to, curAt any
	if !filter.From.IsZero() {
		from = filter.From
	}
	if !filter.To.IsZero() {
		to = filter.To
	}
	if !cur.At.IsZero() {
		curAt = cur.At
	}
	rows, err := database.QueryContext(ctx, listAccessLogQuery,
		filter.Owner, filter.Site, from, to, curAt, cur.ID, auditPageSize+1,
	)
	if err != nil {
		return AccessLogPage{}, err
	}
	defer rows.Close()

	var page AccessLogPage
	for rows.Next() {
		var e AccessLogEntry
		if err := rows.Scan(
			&e.ID, &e.At, &e.UserID, &e.SessionID, &e.OwnerLabel, &e.SiteName,
			&e.Path, &e.Method, &e.Status, &e.Bytes, &e.IP, &e.UserAgent, &e.ClientKind,
		); err != nil {
			return AccessLogPage{}, err
		}
		if !filter.Admin {
			e.IP = ""
			e.UserAgent = ""
		}
		page.Entries = append(page.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return AccessLogPage{}, err
	}
	if len(page.Entries) > auditPageSize {
		last := page.Entries[auditPageSize-1]
		page.NextCursor = encodeAuditCursor(auditCursor{At: last.At, ID: last.ID})
		page.Entries = page.Entries[:auditPageSize]
	}
	return page, nil
}
