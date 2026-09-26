package audit

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"log/slog"
	"time"

	dbstore "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/reqlog"
)

// coalesceWindow is the five-minute window for state_write.
// time.Time.Truncate anchors to an absolute clock boundary (not to the
// first call's timestamp), so two processes computing this for the same
// instant always agree on the same window, which is what lets the upsert
// in migration 0027's audit_bump_state_write coalesce correctly.
const coalesceWindow = 5 * time.Minute

// DBRecorder is the Recorder that persists to audit_events through
// internal/db (migration 0027). Both Recorder methods write synchronously —
// unlike AccessWriter, this needs to run inside the mutation's transaction
// where one exists, which only works if the write happens on the caller's
// own goroutine, and a single-row INSERT or upsert
// costs no more than the session/key-table writes every one of these
// actions already makes synchronously today.
type DBRecorder struct {
	db *sql.DB
	// stream, when set, gets one JSON line per event written (see
	// SetStream). nil writes nothing.
	stream *slog.Logger
	// retryDelays are the waits before Record's second and later
	// attempts. A field so a test can shorten them.
	retryDelays []time.Duration
}

// NewDBRecorder builds a Recorder backed by database. database must not be
// nil; use NoOp{} for a deployment or test with no audit sink configured.
func NewDBRecorder(database *sql.DB) *DBRecorder {
	if database == nil {
		panic("audit: NewDBRecorder requires a non-nil database")
	}
	return &DBRecorder{db: database, retryDelays: []time.Duration{100 * time.Millisecond, 400 * time.Millisecond}}
}

// SetStream makes the recorder also write every event it persists to
// logger as one JSON line with "type":"audit" — the SIEM stream (see
// docs/configuration.md). cmd/server passes the request log's own stdout
// JSON logger, so the two share one writer and one lock and their lines
// never interleave. It must be called before the recorder is used.
func (r *DBRecorder) SetStream(logger *slog.Logger) {
	r.stream = logger
}

// Record persists event outside any caller transaction, using its own
// implicit one, for a call site with no transaction to share (see
// docs/security-review.md for which those are). A failure is retried and
// then logged, not returned, so a sign-out or an access refusal is never
// blocked by the audit sink being unavailable — the "must not block the
// request path" half of the Recorder contract. RecordTx exists for the
// opposite guarantee.
//
// The write ignores the request's cancellation (a client that hangs up
// right after its sign-out must not take the audit row with it), bounded
// by a timeout of its own.
func (r *DBRecorder) Record(ctx context.Context, event Event) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var err error
	for attempt := 0; ; attempt++ {
		if err = r.write(ctx, r.db, event); err == nil {
			return
		}
		if attempt >= len(r.retryDelays) || ctx.Err() != nil {
			break
		}
		time.Sleep(r.retryDelays[attempt])
	}
	log.Printf("AUDIT WRITE FAILED: action=%s actor=%s owner=%s site=%s team=%s request=%s: %v",
		event.Action, event.ActorID, event.OwnerID, event.SiteID, event.TeamID, event.RequestID, err)
}

// RecordTx persists event inside tx, so a caller whose mutation and audit
// row must commit together can roll both back on either
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
	if err := r.persist(ctx, q, event, now); err != nil {
		return err
	}
	r.emit(event, now)
	return nil
}

func (r *DBRecorder) persist(ctx context.Context, q dbstore.Querier, event Event, now time.Time) error {
	if event.Action == "state_write" {
		// The coalescing arbiter is (site_id, actor_id, at); Postgres never
		// considers one NULL equal to another, so a state_write with either
		// missing would insert a fresh row every time instead of coalescing
		// (one row per window) — checked here, at the one
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

// emit writes event to the SIEM stream, after the database write
// succeeded. For RecordTx the caller's transaction can still roll back
// after this, so a streamed line can describe a change that never
// committed: the database (and its hash chain) is authoritative. "at" is
// this server's clock; the row's own at is the database's.
func (r *DBRecorder) emit(event Event, now time.Time) {
	if r.stream == nil {
		return
	}
	kind := event.ActorKind
	if kind == "" {
		kind = "person"
	}
	detail := event.detailJSON()
	if detail == nil {
		detail = map[string]any{}
	}
	r.stream.LogAttrs(context.Background(), slog.LevelInfo, "audit",
		slog.String("type", "audit"),
		slog.String("at", now.UTC().Format(time.RFC3339Nano)),
		slog.String("action", event.Action),
		slog.String("actor_id", event.ActorID),
		slog.String("actor_kind", kind),
		slog.String("key_id", event.KeyID),
		slog.String("owner_id", event.OwnerID),
		slog.String("site_id", event.SiteID),
		slog.String("team_id", event.TeamID),
		slog.String("ip", event.IP),
		slog.String("user_agent", event.UserAgent),
		slog.String("request_id", event.RequestID),
		slog.Any("detail", detail),
	)
}
