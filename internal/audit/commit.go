package audit

import (
	"database/sql"
	"log/slog"
	"sync"
)

// held is the SIEM lines RecordTx wrote inside one transaction, waiting for
// Commit.
type held struct {
	mu    sync.Mutex
	lines []heldLine
}

type heldLine struct {
	stream *Stream
	attrs  []slog.Attr
}

// heldByTx maps a *sql.Tx to its *held lines.
var heldByTx sync.Map

// holdUntilCommit keeps line until tx ends: Commit streams it, Rollback
// drops it. Keyed to the transaction, not to a context, so a committed
// event's line is never lost to a context that ended first.
func holdUntilCommit(tx *sql.Tx, stream *Stream, attrs []slog.Attr) {
	v, _ := heldByTx.LoadOrStore(tx, &held{})
	h := v.(*held)
	h.mu.Lock()
	h.lines = append(h.lines, heldLine{stream, attrs})
	h.mu.Unlock()
}

// Commit commits tx and then streams the SIEM lines RecordTx wrote in it,
// so the SIEM never receives an event that was rolled back. Every caller
// that records through RecordTx commits through here instead of
// tx.Commit, and rolls back through Rollback.
func Commit(tx *sql.Tx) error {
	v, ok := heldByTx.LoadAndDelete(tx)
	if err := tx.Commit(); err != nil {
		return err
	}
	if !ok {
		return nil
	}
	h := v.(*held)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, line := range h.lines {
		line.stream.emit(line.attrs)
	}
	return nil
}

// Rollback rolls tx back and drops the SIEM lines RecordTx held for it. It
// is what a RecordTx caller defers; after Commit it is a no-op, as
// tx.Rollback is.
func Rollback(tx *sql.Tx) error {
	heldByTx.Delete(tx)
	return tx.Rollback()
}
