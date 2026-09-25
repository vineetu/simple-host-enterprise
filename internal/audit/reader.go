package audit

import (
	"context"
	"database/sql"
	"time"

	dbstore "github.com/vsriram/simple-host/internal/db"
)

// Reader answers /api/audit and /api/access. It is deliberately
// thin — internal/db/audit.go already carries the query shapes and the
// owner-scope/ip-redaction rules — so that the handler wiring up
// the two routes has one obvious seam to call into, and so a handler test
// can fake this interface without a database.
type Reader struct {
	db *sql.DB
}

// NewReader builds a Reader over database. database must not be nil.
func NewReader(database *sql.DB) *Reader {
	if database == nil {
		panic("audit: NewReader requires a non-nil database")
	}
	return &Reader{db: database}
}

// AuditQuery is GET /api/audit's parameters. OwnerScope is the
// caller's own namespace plus every team they belong to; the handler
// resolves that list (from session + team membership, neither of which
// this package knows about) and passes it here. Admin must be true for a
// caller that leaves OwnerScope empty — ListAuditEvents refuses the
// opposite (not Admin, empty OwnerScope) rather than silently returning
// every owner's events; Reader itself still does not decide who is an
// admin, so a handler that sets Admin wrong is still the one hole this
// guard cannot close.
type AuditQuery struct {
	Admin      bool
	OwnerScope []string
	Owner      string
	Site       string
	Actor      string
	Action     string
	From       time.Time
	To         time.Time
	Cursor     string
}

// AuditPage is one page of audit events, newest first, plus the cursor for
// the next page (empty when this is the last one).
type AuditPage struct {
	Events     []dbstore.AuditEvent
	NextCursor string
}

// ListAuditEvents runs one page of q.
func (r *Reader) ListAuditEvents(ctx context.Context, q AuditQuery) (AuditPage, error) {
	page, err := dbstore.ListAuditEvents(ctx, r.db, dbstore.AuditEventFilter{
		Admin:      q.Admin,
		OwnerScope: q.OwnerScope,
		Owner:      q.Owner,
		Site:       q.Site,
		Actor:      q.Actor,
		Action:     q.Action,
		From:       q.From,
		To:         q.To,
	}, q.Cursor)
	if err != nil {
		return AuditPage{}, err
	}
	return AuditPage{Events: page.Events, NextCursor: page.NextCursor}, nil
}

// AccessQuery is GET /api/access's parameters. Owner scope
// (Admin false) must name exactly the one site it is asking about — the
// handler is what checks the caller may see that owner/site at all, the
// same division of responsibility as AuditQuery.OwnerScope above. Admin
// scope may leave Owner and Site empty to see every site.
type AccessQuery struct {
	Admin  bool
	Owner  string
	Site   string
	From   time.Time
	To     time.Time
	Cursor string
}

// AccessPage is one page of access log entries, newest first. Every entry's
// IP and UserAgent are already "" unless q.Admin was true — ip and
// user_agent go only to admins — so a handler rendering this page
// never has to apply that rule itself.
type AccessPage struct {
	Entries    []dbstore.AccessLogEntry
	NextCursor string
}

// ListAccess runs one page of q. Owner-scoped (q.Admin false) queries
// require q.Owner; ACCESS_LOG_VISIBILITY=admin is the
// handler's decision to make by only ever constructing an Admin query in
// that mode, not something this package reads from the environment itself.
func (r *Reader) ListAccess(ctx context.Context, q AccessQuery) (AccessPage, error) {
	page, err := dbstore.ListAccessLog(ctx, r.db, dbstore.AccessLogFilter{
		Admin: q.Admin,
		Owner: q.Owner,
		Site:  q.Site,
		From:  q.From,
		To:    q.To,
	}, q.Cursor)
	if err != nil {
		return AccessPage{}, err
	}
	return AccessPage{Entries: page.Entries, NextCursor: page.NextCursor}, nil
}
