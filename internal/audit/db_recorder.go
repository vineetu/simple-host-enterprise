package audit

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	dbstore "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/reqlog"
)

// coalesceWindow is design 8.1's "five-minute window" for state_write.
// time.Time.Truncate anchors to an absolute clock boundary (not to the
// first call's timestamp), so two processes computing this for the same
// instant always agree on the same window, which is what lets the upsert
// in migration 0027's audit_bump_state_write coalesce correctly.
const coalesceWindow = 5 * time.Minute

// DBRecorder is the Phase 4 Recorder: it persists to audit_events through
// internal/db (migration 0027). Both Recorder methods write synchronously —
// unlike AccessWriter, design 8.1 calls for this to run "inside the
// mutation's transaction where one exists," which only works if the write
// happens on the caller's own goroutine, and a single-row INSERT or upsert
// costs no more than the session/key-table writes every one of these
// actions already makes synchronously today.
type DBRecorder struct {
	db *sql.DB
}

// NewDBRecorder builds a Recorder backed by database. database must not be
// nil; use NoOp{} for a deployment or test with no audit sink configured.
func NewDBRecorder(database *sql.DB) *DBRecorder {
	if database == nil {
		panic("audit: NewDBRecorder requires a non-nil database")
	}
	return &DBRecorder{db: database}
}

// Record persists event outside any caller transaction, using its own
// implicit one. This is what Phase 1's existing call sites use (they call
// Record, not RecordTx), and what a caller with no transaction to share
// should keep using; a failure is logged, not returned, matching Phase 1's
// audit.NoOp shape so a sign-in or key mint is never blocked by the audit
// sink being unavailable — the "must not block the request path" half of
// the Recorder contract. RecordTx exists for the opposite guarantee.
func (r *DBRecorder) Record(ctx context.Context, event Event) {
	if err := r.write(ctx, r.db, event); err != nil {
		log.Printf("audit: record %s failed (actor=%s owner=%s site=%s): %v",
			event.Action, event.ActorID, event.OwnerID, event.SiteID, err)
	}
}

// RecordTx persists event inside tx, so a caller whose mutation and audit
// row must commit together (design 8.1) can roll both back on either
// failing. A nil tx falls back to Record's own implicit transaction and
// never returns an error, so a caller written before a transaction existed
// at its call site does not have to change.
func (r *DBRecorder) RecordTx(ctx context.Context, tx *sql.Tx, event Event) error {
	if tx == nil {
		r.Record(ctx, event)
		return nil
	}
	return r.write(ctx, tx, event)
}

func (r *DBRecorder) write(ctx context.Context, q dbstore.Querier, event Event) error {
	event = withRequestInfo(ctx, event)
	now := time.Now()
	if event.Action == "state_write" {
		// The coalescing arbiter is (site_id, actor_id, at); Postgres never
		// considers one NULL equal to another, so a state_write with either
		// missing would insert a fresh row every time instead of coalescing
		// (design 8.1's "one row per window") — checked here, at the one
		// call site that ever constructs a state_write BumpStateWriteParams,
		// so the failure names the event rather than a generic db error.
		if event.ActorID == "" || event.SiteID == "" {
			return errors.New("audit: state_write requires both ActorID and SiteID to coalesce correctly")
		}
		return dbstore.BumpStateWrite(ctx, q, dbstore.BumpStateWriteParams{
			WindowStart: now.UTC().Truncate(coalesceWindow),
			ActorID:     event.ActorID,
			OwnerID:     event.OwnerID,
			SiteID:      event.SiteID,
			RequestID:   event.RequestID,
			ActorKind:   event.ActorKind,
			KeyID:       event.KeyID,
			IP:          event.IP,
			UserAgent:   event.UserAgent,
		})
	}
	return dbstore.InsertAuditEvent(ctx, q, event.row(now))
}

// withRequestInfo fills the request id, client address and User-Agent from
// the request log's record when the caller did not set them, so every
// audit row carries them without each call site having to.
func withRequestInfo(ctx context.Context, event Event) Event {
	record := reqlog.FromContext(ctx)
	if record == nil {
		return event
	}
	if event.RequestID == "" {
		event.RequestID = record.ID
	}
	if event.IP == "" {
		event.IP = record.IP
	}
	if event.UserAgent == "" {
		event.UserAgent = record.UserAgent
	}
	return event
}
