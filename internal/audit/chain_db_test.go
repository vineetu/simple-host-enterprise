package audit

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Go's canonical form must match migration 0036's audit_event_canonical
// byte for byte on every shape of row, or audit-verify would report a
// healthy chain as broken.
func TestGoCanonicalMatchesDatabase(t *testing.T) {
	db := openPruneTestDB(t)
	details := []string{
		`{}`,
		`{"note": "plain"}`,
		`{"note": "unicode: héllo wörld 日本語 🙂 \u2028 end"}`,
		`{"note": "escapes: \" \\ / \b \f \n \r \t \u0001 \u001f \u007f <>&"}`,
		`{"b": 1, "a": 2, "aa": 3, "B": 4, "ab": {"z": [1, 2, {"y": null}], "": true}, "long key": false}`,
		`{"n": [0, -0, -0.0, 1.0, 1.50, 1e3, 1E+2, 1.5e-3, -2.5E-10, 123456789012345678901234567890, 0.000001, 1.50e1, 3.14159]}`,
		`{"nested": {"deeper": {"deepest": ["x", {"k": "v"}, [], {}]}}, "empty": "", "nul": null}`,
		`[1, "two", null]`,
		`"a scalar"`,
		`{"count": 7, "note": "state_write drops count"}`,
		`{"count": 3}`,
	}
	for _, d := range details {
		action := "key_mint"
		if strings.Contains(d, "count") {
			action = "state_write"
		}
		mustExec(t, db, `INSERT INTO audit_events (actor_kind, action, detail) VALUES ('system', $1, $2::jsonb)`, action, d)
	}
	// Every nullable column set, with v4 and v6 addresses and masks.
	mustExec(t, db, `INSERT INTO audit_events (request_id, actor_id, actor_kind, key_id, action, owner_id, site_id, team_id,
		via_site_label, via_site_name, via_site_observed, ip, user_agent, detail)
		VALUES ('req-1', NULL, 'key', NULL, 'site_delete', NULL, NULL, NULL, 'lbl', 'name "q"', true, '10.0.0.9', 'Mozilla/5.0 (ü)', '{"x": 1}')`)
	mustExec(t, db, `INSERT INTO audit_events (actor_kind, action, ip) VALUES ('person', 'sign_in', '2001:db8::1')`)
	mustExec(t, db, `INSERT INTO audit_events (actor_kind, action, ip) VALUES ('person', 'sign_in', '10.1.0.0/16')`)
	r := newChainRecorder(db)
	r.Record(context.Background(), Event{ActorID: chainActor, SiteID: chainSite, OwnerID: chainActor, Action: "state_write", IP: "192.168.1.1"})
	r.Record(context.Background(), Event{ActorID: chainActor, Action: "key_mint", Detail: "ké\ty", Extra: map[string]any{"f": 1.25, "big": 1 << 60, "list": []string{"a"}}})

	rows, err := db.Query(`
		SELECT e.id, e.at, e.request_id, e.actor_id::text, e.actor_kind, e.key_id::text,
		       e.action, e.owner_id::text, e.site_id::text, e.team_id::text, e.via_site_label,
		       e.via_site_name, e.via_site_observed, e.ip::text, e.user_agent, e.detail::text,
		       audit_event_canonical(e.id, e.at, e.request_id, e.actor_id, e.actor_kind, e.key_id,
		           e.action, e.owner_id, e.site_id, e.team_id, e.via_site_label,
		           e.via_site_name, e.via_site_observed, e.ip, e.user_agent, e.detail)
		FROM audit_events e ORDER BY e.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var e chainedEvent
		var want string
		if err := rows.Scan(&e.ID, &e.At, &e.RequestID, &e.ActorID, &e.ActorKind, &e.KeyID,
			&e.Action, &e.OwnerID, &e.SiteID, &e.TeamID, &e.ViaSiteLabel,
			&e.ViaSiteName, &e.ViaSiteObserved, &e.IP, &e.UserAgent, &e.Detail, &want); err != nil {
			t.Fatal(err)
		}
		got, err := e.canonical()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("event %d:\n go: %s\n db: %s", e.ID, got, want)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != len(details)+5 {
		t.Fatalf("checked %d rows, want %d", n, len(details)+5)
	}
	if report := mustVerify(t, db); report.Break != nil || report.Rows != int64(n) {
		t.Fatalf("report = %+v (break %v), want %d clean rows", report, report.Break, n)
	}

	// Tampering is still caught by the Go recomputation.
	mustExec(t, db, `UPDATE audit_events SET detail = '{"note": "unicode: héllo wörld 日本語 🙂 \u2028 End"}' WHERE id = (SELECT event_id FROM audit_chain WHERE seq = 3)`)
	report := mustVerify(t, db)
	if report.Break == nil || report.Break.Seq != 3 || !strings.Contains(report.Break.Reason, "does not match its hash") {
		t.Fatalf("break = %v, want the change caught at seq 3", report.Break)
	}
}

// audit-verify does not call the database's canonicalisation function, so an
// owner who redefines it cannot change the verdict.
func TestVerifyIgnoresARedefinedCanonicalFunction(t *testing.T) {
	db := openPruneTestDB(t)
	r := newChainRecorder(db)
	for i := 0; i < 3; i++ {
		r.Record(context.Background(), Event{ActorID: chainActor, Action: "key_mint"})
	}
	mustExec(t, db, `UPDATE audit_events SET detail = '{"note":"forged"}' WHERE id = (SELECT event_id FROM audit_chain WHERE seq = 2)`)
	// A function that makes any row hash like the original would have.
	mustExec(t, db, `ALTER FUNCTION audit_event_canonical RENAME TO audit_event_canonical_orig`)
	mustExec(t, db, `CREATE FUNCTION audit_event_canonical(p_id bigint, p_at timestamptz, p_request_id text, p_actor_id uuid,
		p_actor_kind text, p_key_id uuid, p_action text, p_owner_id uuid, p_site_id uuid, p_team_id uuid,
		p_via_site_label text, p_via_site_name text, p_via_site_observed boolean, p_ip inet, p_user_agent text, p_detail jsonb)
		RETURNS text LANGUAGE sql AS $$ SELECT audit_event_canonical_orig(p_id, p_at, p_request_id, p_actor_id, p_actor_kind,
		p_key_id, p_action, p_owner_id, p_site_id, p_team_id, p_via_site_label, p_via_site_name, p_via_site_observed, p_ip,
		p_user_agent, '{}'::jsonb) $$`)
	report := mustVerify(t, db)
	if report.Break == nil || report.Break.Seq != 2 {
		t.Fatalf("break = %v, want the forged row caught at seq 2 despite the redefined function", report.Break)
	}
}

// A line for an event recorded in a transaction reaches the stream only
// once Commit has committed it, carries the chain's seq and hash, and a
// rolled-back event is never streamed.
func TestRecordTxStreamsOnlyAfterCommit(t *testing.T) {
	db := openPruneTestDB(t)
	var buf syncBuffer
	r := newChainRecorder(db)
	stream := NewStream(slog.New(slog.NewJSONHandler(&buf, nil)), 0)
	r.SetStream(stream)
	ctx := context.Background()

	rolled, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RecordTx(ctx, rolled, Event{ActorID: chainActor, Action: "site_delete"}); err != nil {
		t.Fatal(err)
	}
	rolled.Rollback()

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"key_mint", "key_revoke"} {
		if err := r.RecordTx(ctx, tx, Event{ActorID: chainActor, Action: action}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.RecordTx(ctx, tx, Event{ActorID: chainActor, SiteID: chainSite, Action: "state_write"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if buf.Len() != 0 {
		t.Fatalf("streamed before commit: %q", buf.String())
	}
	if err := Commit(tx); err != nil {
		t.Fatal(err)
	}
	stream.Close(time.Second)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (the rolled-back event must not stream): %q", len(lines), buf.String())
	}
	for i, line := range lines {
		var got struct {
			Action string `json:"action"`
			Seq    int64  `json:"seq"`
			Hash   string `json:"hash"`
		}
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatal(err)
		}
		var hash []byte
		if err := db.QueryRow(`SELECT hash FROM audit_chain WHERE seq = $1`, got.Seq).Scan(&hash); err != nil {
			t.Fatalf("line %d (%s) seq %d: %v", i, got.Action, got.Seq, err)
		}
		if got.Seq != int64(i+1) || got.Hash != hex.EncodeToString(hash) {
			t.Errorf("line %d = %+v, want seq %d with hash %x", i, got, i+1, hash)
		}
	}
	if _, ok := heldByTx.Load(tx); ok {
		t.Error("committed transaction still held")
	}
}

// A request that rolls back drops its held lines when its context ends.
func TestHeldLinesDroppedOnRollback(t *testing.T) {
	db := openPruneTestDB(t)
	r := newChainRecorder(db)
	stream := NewStream(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), 0)
	defer stream.Close(time.Second)
	r.SetStream(stream)
	// A context that never ends: only Rollback can drop the lines.
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RecordTx(context.Background(), tx, Event{ActorID: chainActor, Action: "key_mint"}); err != nil {
		t.Fatal(err)
	}
	if err := Rollback(tx); err != nil {
		t.Fatal(err)
	}
	if _, ok := heldByTx.Load(tx); ok {
		t.Fatal("held lines survived Rollback")
	}
}

// A committed event is streamed even when the context RecordTx was given
// ended before the commit.
func TestCommittedLineSurvivesEndedRecordContext(t *testing.T) {
	db := openPruneTestDB(t)
	r := newChainRecorder(db)
	buf := &syncBuffer{}
	stream := NewStream(slog.New(slog.NewJSONHandler(buf, nil)), 0)
	r.SetStream(stream)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer Rollback(tx)
	recordCtx, cancel := context.WithCancel(context.Background())
	if err := r.RecordTx(recordCtx, tx, Event{ActorID: chainActor, Action: "key_mint"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(20 * time.Millisecond)
	if err := Commit(tx); err != nil {
		t.Fatal(err)
	}
	stream.Close(time.Second)
	if !strings.Contains(buf.String(), `"key_mint"`) {
		t.Fatalf("committed event not streamed: %q", buf.String())
	}
}

// Every caller that records through RecordTx must commit through Commit, or
// its SIEM lines are never written.
func TestRecordTxCallersCommitThroughAudit(t *testing.T) {
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(path, "internal/audit/") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(src, []byte("RecordTx(")) && bytes.Contains(src, []byte(".Commit()")) {
				t.Errorf("%s records through RecordTx but commits with tx.Commit(); use audit.Commit(tx)", path)
			}
			if bytes.Contains(src, []byte("RecordTx(")) && bytes.Contains(src, []byte(".Rollback()")) {
				t.Errorf("%s records through RecordTx but rolls back with tx.Rollback(); use audit.Rollback(tx)", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// syncBuffer is a bytes.Buffer the stream goroutine can write while the
// test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
