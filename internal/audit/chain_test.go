package audit

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	chainActor = "11111111-1111-1111-1111-111111111111"
	chainSite  = "22222222-2222-2222-2222-222222222222"
)

func newChainRecorder(db *sql.DB) *DBRecorder {
	r := NewDBRecorder(db)
	r.retryDelays = nil
	return r
}

func mustVerify(t *testing.T, db *sql.DB) ChainReport {
	t.Helper()
	report, err := VerifyChain(context.Background(), db, ChainExpectation{})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func chainSeqForEvent(t *testing.T, db *sql.DB, n int) (seq, eventID int64) {
	t.Helper()
	err := db.QueryRow(`SELECT seq, event_id FROM audit_chain ORDER BY seq OFFSET $1 LIMIT 1`, n).Scan(&seq, &eventID)
	if err != nil {
		t.Fatal(err)
	}
	return seq, eventID
}

func TestChainLinksAcrossManyInserts(t *testing.T) {
	db := openPruneTestDB(t)
	r := newChainRecorder(db)
	for i := 0; i < 40; i++ {
		r.Record(context.Background(), Event{ActorID: chainActor, Action: "sign_in", Detail: fmt.Sprintf("session %d", i)})
	}
	report := mustVerify(t, db)
	if report.Break != nil {
		t.Fatalf("unexpected break: %s", report.Break)
	}
	if report.Rows != 40 || report.FirstSeq != 1 || report.HeadSeq != 40 || len(report.HeadHash) != 64 {
		t.Fatalf("report = %+v, want 40 rows, seq 1..40, a 32-byte head", report)
	}
	var genesis []byte
	if err := db.QueryRow(`SELECT prev_hash FROM audit_chain WHERE seq = 1`).Scan(&genesis); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(genesis, make([]byte, 32)) {
		t.Fatalf("genesis prev_hash = %x, want 32 zero bytes", genesis)
	}
}

func TestChainStaysOneChainUnderConcurrentInserts(t *testing.T) {
	db := openPruneTestDB(t)
	db.SetMaxOpenConns(8)
	r := newChainRecorder(db)
	const workers, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				tx, err := db.Begin()
				if err != nil {
					errs <- err
					return
				}
				if err := r.RecordTx(context.Background(), tx, Event{ActorID: chainActor, Action: "member_add", Detail: fmt.Sprintf("w%d-%d", w, i)}); err != nil {
					tx.Rollback()
					errs <- err
					return
				}
				if err := tx.Commit(); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	report := mustVerify(t, db)
	if report.Break != nil {
		t.Fatalf("unexpected break: %s", report.Break)
	}
	if report.Rows != workers*each || report.HeadSeq != workers*each {
		t.Fatalf("report = %+v, want %d rows", report, workers*each)
	}
}

func TestChainRolledBackInsertLeavesNoTrace(t *testing.T) {
	db := openPruneTestDB(t)
	r := newChainRecorder(db)
	r.Record(context.Background(), Event{ActorID: chainActor, Action: "sign_in"})
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RecordTx(context.Background(), tx, Event{ActorID: chainActor, Action: "site_delete"}); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	r.Record(context.Background(), Event{ActorID: chainActor, Action: "sign_out"})
	report := mustVerify(t, db)
	if report.Break != nil || report.Rows != 2 || report.HeadSeq != 2 {
		t.Fatalf("report = %+v (break %v), want 2 clean rows", report, report.Break)
	}
}

func TestChainStateWriteBumpDoesNotBreakIt(t *testing.T) {
	db := openPruneTestDB(t)
	r := newChainRecorder(db)
	r.Record(context.Background(), Event{ActorID: chainActor, Action: "sign_in"})
	for i := 0; i < 5; i++ {
		r.Record(context.Background(), Event{ActorID: chainActor, SiteID: chainSite, OwnerID: chainActor, Action: "state_write"})
	}
	r.Record(context.Background(), Event{ActorID: chainActor, Action: "sign_out"})

	var count int
	if err := db.QueryRow(`SELECT (detail->>'count')::int FROM audit_events WHERE action = 'state_write'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("state_write count = %d, want 5 (one coalesced row)", count)
	}
	report := mustVerify(t, db)
	if report.Break != nil {
		t.Fatalf("unexpected break: %s", report.Break)
	}
	if report.Rows != 3 {
		t.Fatalf("chain rows = %d, want 3: only the window's first state_write is inserted", report.Rows)
	}
}

func TestChainReportsTamperingAtTheRightSeq(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(t *testing.T, db *sql.DB, eventID int64)
		// offset is where the break is reported, relative to the tampered
		// row's seq.
		offset int64
		reason string
	}{
		{"detail changed", func(t *testing.T, db *sql.DB, id int64) {
			mustExec(t, db, `UPDATE audit_events SET detail = '{"note":"forged"}' WHERE id = $1`, id)
		}, 0, "does not match its hash"},
		{"actor changed", func(t *testing.T, db *sql.DB, id int64) {
			mustExec(t, db, `UPDATE audit_events SET actor_id = NULL WHERE id = $1`, id)
		}, 0, "does not match its hash"},
		{"event deleted", func(t *testing.T, db *sql.DB, id int64) {
			mustExec(t, db, `DELETE FROM audit_events WHERE id = $1`, id)
		}, 0, "missing from audit_events"},
		{"chain row deleted", func(t *testing.T, db *sql.DB, id int64) {
			mustExec(t, db, `DELETE FROM audit_chain WHERE event_id = $1`, id)
		}, 1, "missing"},
		{"chain row hash rewritten", func(t *testing.T, db *sql.DB, id int64) {
			mustExec(t, db, `UPDATE audit_chain SET hash = sha256(hash) WHERE event_id = $1`, id)
		}, 0, "does not match its hash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openPruneTestDB(t)
			r := newChainRecorder(db)
			for i := 0; i < 10; i++ {
				r.Record(context.Background(), Event{ActorID: chainActor, Action: "key_mint", Detail: fmt.Sprintf("k%d", i)})
			}
			seq, eventID := chainSeqForEvent(t, db, 4)
			tc.tamper(t, db, eventID)
			report := mustVerify(t, db)
			if report.Break == nil {
				t.Fatal("tampering not detected")
			}
			if report.Break.Seq != seq+tc.offset || !strings.Contains(report.Break.Reason, tc.reason) {
				t.Fatalf("break = %s, want seq %d with %q", report.Break, seq+tc.offset, tc.reason)
			}
		})
	}

	t.Run("tail deleted", func(t *testing.T) {
		db := openPruneTestDB(t)
		r := newChainRecorder(db)
		for i := 0; i < 5; i++ {
			r.Record(context.Background(), Event{ActorID: chainActor, Action: "key_mint"})
		}
		mustExec(t, db, `DELETE FROM audit_chain WHERE seq = 5`)
		report := mustVerify(t, db)
		if report.Break == nil || report.Break.Seq != 4 || !strings.Contains(report.Break.Reason, "not the head") {
			t.Fatalf("break = %v, want the head mismatch at seq 4", report.Break)
		}
	})
}

// The application role inserts audit events (and so extends the chain
// through the SECURITY DEFINER trigger) but cannot touch the chain itself.
func TestChainAppRoleCannotWriteChain(t *testing.T) {
	db := openPruneTestDB(t)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET ROLE simplehost_app`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO audit_events (actor_kind, action) VALUES ('system', 'sign_in')`); err != nil {
		t.Fatalf("app role insert into audit_events: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO audit_chain (seq, event_id, event_at, prev_hash, hash) VALUES (99, 1, now(), '\x00', '\x00')`,
		`UPDATE audit_chain_head SET seq = 0`,
		`DELETE FROM audit_chain`,
		`SELECT 1 FROM audit_chain`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("app role %q: err = %v, want permission denied", stmt, err)
		}
	}
	if _, err := conn.ExecContext(ctx, `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	report := mustVerify(t, db)
	if report.Break != nil || report.Rows != 1 {
		t.Fatalf("report = %+v (break %v), want the app role's row chained", report, report.Break)
	}
}

func TestPruneTrimsChainAndVerifyStillPasses(t *testing.T) {
	db := openPruneTestDB(t)
	createHistoricalPartition(t, db, "audit_events", "audit_events_p2020_01")
	r := newChainRecorder(db)

	// Old events: the server-time trigger is what stops anyone back-dating
	// a row, so the test lifts it (as the owner) to place three in 2020.
	mustExec(t, db, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_server_time`)
	for i := 0; i < 3; i++ {
		mustExec(t, db, `INSERT INTO audit_events (at, actor_kind, action) VALUES ('2020-01-15 00:00:00+00', 'system', 'old')`)
	}
	mustExec(t, db, `ALTER TABLE audit_events ENABLE TRIGGER audit_events_server_time`)
	for i := 0; i < 4; i++ {
		r.Record(context.Background(), Event{ActorID: chainActor, Action: "key_mint"})
	}
	if report := mustVerify(t, db); report.Break != nil || report.Rows != 7 {
		t.Fatalf("before prune: %+v (break %v)", report, report.Break)
	}

	result, err := Prune(context.Background(), db, PruneOptions{
		AuditRetentionDays: 400, AccessRetentionDays: 90, Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsPartition(result.Dropped, "audit_events", "audit_events_p2020_01") || result.ChainTrimmed != 3 {
		t.Fatalf("prune result = %+v, want the 2020 partition dropped and 3 chain rows trimmed", result)
	}
	report := mustVerify(t, db)
	if report.Break != nil {
		t.Fatalf("after prune: unexpected break: %s", report.Break)
	}
	if report.Rows != 4 || report.FirstSeq != 4 || report.HeadSeq != 7 {
		t.Fatalf("after prune: %+v, want 4 rows from seq 4 to 7", report)
	}
}

func TestRecordStreamsOneLineAfterTheWrite(t *testing.T) {
	db := openPruneTestDB(t)
	var buf bytes.Buffer
	r := newChainRecorder(db)
	stream := NewStream(slog.New(slog.NewJSONHandler(&buf, nil)), 0)
	r.SetStream(stream)

	r.Record(context.Background(), Event{
		ActorID: chainActor, Action: "key_mint", Detail: "abc123", SubjectID: chainSite,
		IP: "10.0.0.9", UserAgent: "test-agent", RequestID: "req-9",
	})
	stream.Close(time.Second)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %q", len(lines), buf.String())
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"type": "audit", "action": "key_mint", "actor_id": chainActor, "actor_kind": "person",
		"ip": "10.0.0.9", "user_agent": "test-agent", "request_id": "req-9", "key_id": "", "site_id": "",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	detail, _ := got["detail"].(map[string]any)
	if detail["note"] != "abc123" || detail["subject_id"] != chainSite {
		t.Errorf("detail = %v", got["detail"])
	}
	if _, err := time.Parse(time.RFC3339Nano, fmt.Sprint(got["at"])); err != nil {
		t.Errorf("at = %v: %v", got["at"], err)
	}

	// A write that fails is not streamed.
	buf.Reset()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := r.RecordTx(context.Background(), tx, Event{Action: "state_write"}); err == nil {
		t.Fatal("state_write without actor/site was accepted")
	}
	if buf.Len() != 0 {
		t.Fatalf("a failed write was streamed: %q", buf.String())
	}
}

func TestRecordRetriesThenLogsLoudly(t *testing.T) {
	db := openPruneTestDB(t)
	r := NewDBRecorder(db)
	r.retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	var buf bytes.Buffer
	stream := NewStream(slog.New(slog.NewJSONHandler(&buf, nil)), 0)
	r.SetStream(stream)
	db.Close()

	var logged bytes.Buffer
	restore := captureLog(&logged)
	defer restore()
	r.Record(context.Background(), Event{ActorID: chainActor, Action: "sign_out"})
	if !strings.Contains(logged.String(), "AUDIT WRITE FAILED: action=sign_out actor="+chainActor) {
		t.Fatalf("log = %q, want the loud failure line", logged.String())
	}
	stream.Close(time.Second)
	if buf.Len() != 0 {
		t.Fatal("a failed write was streamed")
	}
}

func TestChainMigrationIsMarkedCompatible(t *testing.T) {
	contents, err := os.ReadFile("../migrate/sql/0036_audit_chain.sql")
	if err != nil {
		t.Fatal(err)
	}
	s := string(contents)
	if !strings.HasPrefix(s, "-- simple-host: backward-compatible\n") {
		t.Error("0036 only adds tables, functions and a trigger; it must be marked backward-compatible")
	}
	for _, forbidden := range []string{"TO simplehost_app"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("0036 grants the application role something (%q); the chain is written only by the trigger", forbidden)
		}
	}
	if !strings.Contains(s, "AFTER INSERT ON audit_events") {
		t.Error("the chain trigger must be AFTER INSERT (a BEFORE trigger also fires for an upsert that updates)")
	}
}

func captureLog(w *bytes.Buffer) func() {
	prev := log.Writer()
	log.SetOutput(w)
	return func() { log.SetOutput(prev) }
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// An owner who edits an event and recomputes every later hash passes a plain
// verification; the head an earlier run printed is what catches it.
func TestChainExpectationCatchesARecomputedChain(t *testing.T) {
	db := openPruneTestDB(t)
	r := newChainRecorder(db)
	for i := 0; i < 5; i++ {
		r.Record(context.Background(), Event{ActorID: chainActor, Action: "key_mint", Detail: fmt.Sprintf("k%d", i)})
	}
	before := mustVerify(t, db)
	headHash, err := hex.DecodeString(before.HeadHash)
	if err != nil {
		t.Fatal(err)
	}
	expect := ChainExpectation{Seq: before.HeadSeq, Hash: headHash}

	mustExec(t, db, `UPDATE audit_events SET detail = '{"note":"forged"}' WHERE id = (SELECT event_id FROM audit_chain WHERE seq = 3)`)
	mustExec(t, db, `
		DO $$
		DECLARE c record; prev bytea; h bytea;
		BEGIN
			SELECT hash INTO prev FROM audit_chain WHERE seq = 2;
			FOR c IN SELECT ch.seq, e.* FROM audit_chain ch JOIN audit_events e ON e.id = ch.event_id AND e.at = ch.event_at WHERE ch.seq >= 3 ORDER BY ch.seq LOOP
				h := sha256(prev || convert_to(audit_event_canonical(c.id, c.at, c.request_id, c.actor_id, c.actor_kind, c.key_id, c.action, c.owner_id, c.site_id, c.team_id, c.via_site_label, c.via_site_name, c.via_site_observed, c.ip, c.user_agent, c.detail), 'UTF8'));
				UPDATE audit_chain SET prev_hash = prev, hash = h WHERE seq = c.seq;
				prev := h;
			END LOOP;
			UPDATE audit_chain_head SET hash = prev;
		END $$`)

	if report := mustVerify(t, db); report.Break != nil {
		t.Fatalf("a fully recomputed chain should verify on its own, got %s", report.Break)
	}
	report, err := VerifyChain(context.Background(), db, expect)
	if err != nil {
		t.Fatal(err)
	}
	if report.Break == nil || report.Break.Seq != 5 || !strings.Contains(report.Break.Reason, "rewritten") {
		t.Fatalf("break = %v, want the rewrite caught at seq 5", report.Break)
	}

	beyond, err := VerifyChain(context.Background(), db, ChainExpectation{Seq: 99, Hash: headHash})
	if err != nil {
		t.Fatal(err)
	}
	if beyond.Break == nil || !strings.Contains(beyond.Break.Reason, "beyond the head") {
		t.Fatalf("break = %v, want an expected row beyond the head reported", beyond.Break)
	}
}
