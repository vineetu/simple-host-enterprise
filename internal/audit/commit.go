package audit

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
)

// held is the SIEM lines RecordTx wrote inside one transaction, waiting for
// Commit.
type held struct {
	mu    sync.Mutex
	lines []heldLine
	stop  func() bool
}

type heldLine struct {
	stream *Stream
	attrs  []slog.Attr
}

// heldByTx maps a *sql.Tx to its *held lines.
var heldByTx sync.Map

// holdUntilCommit keeps line until tx is committed through Commit. If ctx
// ends first (the request is over, so a transaction begun on it has been
// rolled back or will never be committed through Commit), the lines are
// dropped with it.
func holdUntilCommit(ctx context.Context, tx *sql.Tx, stream *Stream, attrs []slog.Attr) {
	v, loaded := heldByTx.LoadOrStore(tx, &held{})
	h := v.(*held)
	h.mu.Lock()
	h.lines = append(h.lines, heldLine{stream, attrs})
	if !loaded {
		h.stop = context.AfterFunc(ctx, func() { heldByTx.CompareAndDelete(tx, h) })
	}
	h.mu.Unlock()
}

// Commit commits tx and then streams the SIEM lines RecordTx wrote in it,
// so the SIEM never receives an event that was rolled back. Every caller
// that records through RecordTx commits through here instead of
// tx.Commit; a transaction that rolls back just drops its lines.
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
	if h.stop != nil {
		h.stop()
	}
	for _, line := range h.lines {
		line.stream.emit(line.attrs)
	}
	return nil
}
