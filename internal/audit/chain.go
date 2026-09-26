package audit

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// ChainBreak is the first place VerifyChain found the audit chain broken
// (migration 0036).
type ChainBreak struct {
	Seq     int64
	EventID int64
	At      time.Time
	Reason  string
}

func (b ChainBreak) String() string {
	return fmt.Sprintf("seq %d (event %d at %s): %s", b.Seq, b.EventID, b.At.UTC().Format(time.RFC3339Nano), b.Reason)
}

// ChainReport is VerifyChain's answer. Break is nil when every row checked
// out.
type ChainReport struct {
	Rows     int64
	FirstSeq int64
	HeadSeq  int64
	HeadHash string
	Break    *ChainBreak
	// ExpectPruned is true when an expected row was asked for but is older
	// than the first remaining row, so it could not be compared.
	ExpectPruned bool
}

// ChainExpectation is a (seq, hash) pair an earlier audit-verify printed and
// someone kept outside the database: the chain must still pass through it.
// The zero value checks nothing.
type ChainExpectation struct {
	Seq  int64
	Hash []byte
}

// verifyChainQuery walks audit_chain in seq order with the event each row
// names, recomputing the hash in SQL through the same
// audit_event_canonical the trigger used, so the Go side only compares.
const verifyChainQuery = `
	SELECT c.seq, c.event_id, c.event_at, c.prev_hash, c.hash,
	       e.id IS NOT NULL,
	       CASE WHEN e.id IS NULL THEN NULL ELSE sha256(c.prev_hash || convert_to(audit_event_canonical(
	           e.id, e.at, e.request_id, e.actor_id, e.actor_kind, e.key_id,
	           e.action, e.owner_id, e.site_id, e.team_id, e.via_site_label,
	           e.via_site_name, e.via_site_observed, e.ip, e.user_agent, e.detail
	       ), 'UTF8')) END
	FROM audit_chain c
	LEFT JOIN audit_events e ON e.id = c.event_id AND e.at = c.event_at
	ORDER BY c.seq
`

// VerifyChain checks the audit hash chain end to end: consecutive seqs,
// each prev_hash equal to the previous row's hash, each event still
// present and hashing to the stored value, and the last row matching
// audit_chain_head. The first remaining row's prev_hash is trusted as the
// anchor, because `simple-host prune` deletes the chain's oldest rows with
// their partitions. It runs in one read-only REPEATABLE READ transaction so
// rows inserted while it runs cannot make the head look ahead of the chain.
// db must connect as the owning role: the application role cannot read the
// chain tables.
//
// expect, when its Seq is set, is a row an earlier run reported as the
// head: its hash must be unchanged, which catches the owner rewriting and
// recomputing the whole chain since then.
func VerifyChain(ctx context.Context, db *sql.DB, expect ChainExpectation) (ChainReport, error) {
	var report ChainReport
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return report, err
	}
	defer tx.Rollback()

	var headHash []byte
	if err := tx.QueryRowContext(ctx, `SELECT seq, hash FROM audit_chain_head WHERE id`).Scan(&report.HeadSeq, &headHash); err != nil {
		return report, fmt.Errorf("read chain head: %w", err)
	}
	report.HeadHash = hex.EncodeToString(headHash)

	rows, err := tx.QueryContext(ctx, verifyChainQuery)
	if err != nil {
		return report, err
	}
	defer rows.Close()

	var (
		lastSeq  int64
		lastHash []byte
		lastID   int64
		lastAt   time.Time
	)
	for rows.Next() {
		var (
			seq, eventID     int64
			at               time.Time
			prev, hash, calc []byte
			present          bool
		)
		if err := rows.Scan(&seq, &eventID, &at, &prev, &hash, &present, &calc); err != nil {
			return report, err
		}
		fail := func(reason string) (ChainReport, error) {
			report.Break = &ChainBreak{Seq: seq, EventID: eventID, At: at, Reason: reason}
			return report, nil
		}
		if report.Rows == 0 {
			report.FirstSeq = seq
		} else {
			if seq != lastSeq+1 {
				return fail(fmt.Sprintf("chain rows %d to %d are missing", lastSeq+1, seq-1))
			}
			if !bytes.Equal(prev, lastHash) {
				return fail("prev_hash does not match the previous row's hash")
			}
		}
		if !present {
			return fail("event is missing from audit_events")
		}
		if !bytes.Equal(calc, hash) {
			return fail("event does not match its hash (changed after it was written)")
		}
		if expect.Seq != 0 && seq == expect.Seq && !bytes.Equal(hash, expect.Hash) {
			return fail(fmt.Sprintf("hash %x is not the expected %x: the chain was rewritten", hash, expect.Hash))
		}
		report.Rows++
		lastSeq, lastHash, lastID, lastAt = seq, hash, eventID, at
	}
	if err := rows.Err(); err != nil {
		return report, err
	}
	if expect.Seq != 0 && (report.Rows == 0 || expect.Seq < report.FirstSeq) {
		report.ExpectPruned = expect.Seq <= report.HeadSeq
	}
	if expect.Seq > report.HeadSeq {
		report.Break = &ChainBreak{Seq: expect.Seq, Reason: fmt.Sprintf("the expected row is beyond the head (seq %d): rows were deleted", report.HeadSeq)}
		return report, nil
	}
	if report.Rows == 0 {
		if report.HeadSeq != 0 {
			report.Break = &ChainBreak{Seq: report.HeadSeq, Reason: "the head names a chain row but the chain is empty"}
		}
		return report, nil
	}
	if lastSeq != report.HeadSeq || !bytes.Equal(lastHash, headHash) {
		report.Break = &ChainBreak{Seq: lastSeq, EventID: lastID, At: lastAt,
			Reason: fmt.Sprintf("the last chain row is not the head (head is seq %d); rows after it were deleted", report.HeadSeq)}
	}
	return report, nil
}

// trimChain deletes the chain rows for events Prune has just dropped: every
// row up to the last one whose event is older than the oldest remaining
// monthly audit partition. It deletes a prefix, not individual rows, so the
// chain that remains is still contiguous and VerifyChain anchors on its
// first row. A row whose event sits in the default partition with an older
// timestamp leaves the chain with that prefix; it would have been pruned by
// now had its month had a partition.
func trimChain(ctx context.Context, db *sql.DB, oldestKept time.Time) (int64, error) {
	result, err := db.ExecContext(ctx, `
		DELETE FROM audit_chain
		WHERE seq <= (SELECT max(seq) FROM audit_chain WHERE event_at < $1)`, oldestKept)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
