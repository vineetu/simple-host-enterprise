// Package audit defines the recording interface every mutation handler calls
// and a real implementation of it: a partitioned audit_events table, an
// access log, and the query shapes /api/audit and /api/access read. Every
// call site that must audit an action already calls through this package's
// Recorder, so it replaces one implementation (NoOp -> DBRecorder) rather
// than touching every handler — see docs/security-review.md.
package audit

import (
	"context"
	"database/sql"
	"log"
	"time"

	dbstore "github.com/vsriram/simple-host/internal/db"
)

// Event is one recordable action. Fields mirror the audit_events table's shape:
// actor, action, and the subject(s) the action concerned. Every field
// beyond ActorID/Action/Detail is optional and "" (or the zero value) when
// it does not apply to a given action, matching the table's nullable
// columns.
type Event struct {
	// ActorID is the authenticated user performing the action. Empty for an
	// event with no authenticated actor.
	ActorID string
	// ActorKind is "person", "key", or "system". Defaults to
	// "person" when empty, which is every call site that predates this
	// field.
	ActorKind string
	// KeyID is the api_keys row the actor authenticated with, when the
	// action was taken over an API key rather than a session.
	KeyID string
	// Action is a short, stable verb: "sign_in",
	// "sign_out", "key_mint", "key_revoke", "session_revoke", "state_write",
	// and so on.
	Action string
	// SubjectID is who or what the action was taken on, when it differs
	// from the actor — an admin disabling somebody else, for instance.
	// audit_events has no subject_id column; the
	// DBRecorder folds this into detail.subject_id rather than dropping it,
	// which is also what the team_audit fold in migration 0027 does for
	// team_audit's own subject_id column.
	SubjectID string
	// OwnerID, SiteID, TeamID are the namespace(s) the action concerns.
	OwnerID string
	SiteID  string
	TeamID  string
	// ViaSiteLabel and ViaSiteName record the host a request arrived on
	// when that differs from the site the action concerns (a restricted
	// site's own hostname, for instance); ViaSiteObserved is true when the
	// caller actually inspected the request's host to fill these in, so a
	// false does not read as "definitely not via a site."
	ViaSiteLabel    string
	ViaSiteName     string
	ViaSiteObserved bool
	// IP and UserAgent are the request's, when known.
	IP        string
	UserAgent string
	// RequestID ties this event back to the structured request log
	// sharing the same id.
	RequestID string
	// Detail is a short, human-readable note: a session id, a key's
	// prefix, nothing secret. Persisted as detail.note in the jsonb column;
	// Extra, below, is for a caller that wants to set other jsonb keys
	// directly.
	Detail string
	// Extra adds additional keys to the persisted detail object alongside
	// Detail's "note" and SubjectID's "subject_id". Nil for every call site
	// that predates this field.
	Extra map[string]any
}

// detailJSON assembles the jsonb object InsertAuditEvent persists: Extra's
// keys first, then SubjectID and Detail layered on top under their fixed
// names so a caller's Extra cannot silently shadow them.
func (e Event) detailJSON() map[string]any {
	if len(e.Extra) == 0 && e.SubjectID == "" && e.Detail == "" {
		return nil
	}
	out := make(map[string]any, len(e.Extra)+2)
	for k, v := range e.Extra {
		out[k] = v
	}
	if e.SubjectID != "" {
		out["subject_id"] = e.SubjectID
	}
	if e.Detail != "" {
		out["note"] = e.Detail
	}
	return out
}

func (e Event) row(at time.Time) dbstore.AuditEvent {
	kind := e.ActorKind
	if kind == "" {
		kind = "person"
	}
	return dbstore.AuditEvent{
		At:              at,
		RequestID:       e.RequestID,
		ActorID:         e.ActorID,
		ActorKind:       kind,
		KeyID:           e.KeyID,
		Action:          e.Action,
		OwnerID:         e.OwnerID,
		SiteID:          e.SiteID,
		TeamID:          e.TeamID,
		ViaSiteLabel:    e.ViaSiteLabel,
		ViaSiteName:     e.ViaSiteName,
		ViaSiteObserved: e.ViaSiteObserved,
		IP:              e.IP,
		UserAgent:       e.UserAgent,
		Detail:          e.detailJSON(),
	}
}

// Recorder records an audit event. Record must not block the request path
// on a slow sink; a Recorder that cannot keep up should drop and log rather
// than stall the caller. RecordTx is for a caller that already holds the
// mutation's transaction and wants the stronger guarantee ("a
// mutation without its audit row cannot commit"): it returns an error so
// the caller can roll the transaction back, rather than only logging one.
//
// Both methods exist on every implementation so that widening this
// interface does not require touching the existing
// call sites, which only ever call Record.
type Recorder interface {
	Record(ctx context.Context, event Event)
	RecordTx(ctx context.Context, tx *sql.Tx, event Event) error
}

// NoOp is a recorder that logs at info level and keeps nothing. It
// still exists (rather than being deleted now that DBRecorder can replace
// it) for a deployment with no database migrated yet, and for tests that
// construct a handler without wiring a real audit sink.
type NoOp struct{}

func (NoOp) Record(_ context.Context, event Event) {
	log.Printf("audit (no-op recorder, not persisted): actor=%s action=%s subject=%s detail=%q",
		event.ActorID, event.Action, event.SubjectID, event.Detail)
}

func (n NoOp) RecordTx(ctx context.Context, _ *sql.Tx, event Event) error {
	n.Record(ctx, event)
	return nil
}
