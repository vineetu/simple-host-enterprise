package db

import (
	"context"
	"testing"
	"time"
)

// TestBumpStateWriteCoalescesRepeatedCallsIntoOneRow proves the actual
// upsert behavior (one row per (actor, site, five-minute window) with
// detail.count), against a real Postgres — nothing about a
// partial unique index and an ON CONFLICT DO UPDATE is faithfully
// mockable. Companion to the review finding that the arbiter is NULL-unsafe:
// this test uses real, non-empty ActorID/SiteID, which is exactly the case
// that must keep working correctly while the NULL case (asserted without a
// database in TestBumpStateWriteRejectsEmptyActorOrSite) is refused.
func TestBumpStateWriteCoalescesRepeatedCallsIntoOneRow(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	userID, siteID := mustCreateUserAndSite(t, database, "alice", "demo")

	window := time.Now().UTC().Truncate(5 * time.Minute)
	for i, requestID := range []string{"req-1", "req-2", "req-3"} {
		err := BumpStateWrite(ctx, database, BumpStateWriteParams{
			WindowStart: window,
			ActorID:     userID,
			OwnerID:     userID,
			SiteID:      siteID,
			RequestID:   requestID,
			IP:          "10.0.0." + string(rune('1'+i)),
		})
		if err != nil {
			t.Fatalf("BumpStateWrite call %d: %v", i+1, err)
		}
	}

	page, err := ListAuditEvents(ctx, database, AuditEventFilter{Admin: true, Action: "state_write"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("got %d state_write row(s), want exactly 1 (coalesced): %+v", len(page.Events), page.Events)
	}
	got := page.Events[0]
	if count, _ := got.Detail["count"].(float64); count != 3 {
		t.Fatalf("detail.count = %v, want 3", got.Detail["count"])
	}
	// The first write in the window keeps its request id and address: a
	// later write only bumps the count (migration 0030).
	if got.RequestID != "req-1" || got.IP != "10.0.0.1" {
		t.Fatalf("RequestID = %q, IP = %q, want the first call's req-1 / 10.0.0.1", got.RequestID, got.IP)
	}
}

// audit_events.at is the database's clock (migration 0030): a caller cannot
// back-date a row within the month, and a row asking for another month is
// refused outright.
func TestAuditEventTimeIsServerSide(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	if _, err := database.ExecContext(ctx, `INSERT INTO audit_events (at, action) VALUES (now() - interval '40 days', 'backdated_far')`); err == nil {
		t.Fatal("a row dated 40 days ago was accepted")
	}
	if _, err := database.ExecContext(ctx, `INSERT INTO audit_events (at, action) VALUES (date_trunc('month', now()), 'backdated_near')`); err != nil {
		t.Fatal(err)
	}
	var age time.Duration
	var seconds float64
	if err := database.QueryRowContext(ctx, `SELECT extract(epoch FROM now() - at) FROM audit_events WHERE action = 'backdated_near'`).Scan(&seconds); err != nil {
		t.Fatal(err)
	}
	age = time.Duration(seconds * float64(time.Second))
	if age < 0 || age > time.Minute {
		t.Fatalf("stored at is %v away from now; the caller's timestamp was kept", age)
	}
}

// TestBumpStateWriteRejectsEmptyActorOrSite needs no database: both guards
// run before BumpStateWrite ever touches q (a nil Querier here would panic
// if either check were missing, which is itself part of what this test
// proves).
func TestBumpStateWriteRejectsEmptyActorOrSite(t *testing.T) {
	base := BumpStateWriteParams{WindowStart: time.Now(), ActorID: "actor-1", SiteID: "site-1"}

	withoutActor := base
	withoutActor.ActorID = ""
	if err := BumpStateWrite(context.Background(), nil, withoutActor); err == nil {
		t.Fatal("BumpStateWrite with empty ActorID should be refused")
	}

	withoutSite := base
	withoutSite.SiteID = ""
	if err := BumpStateWrite(context.Background(), nil, withoutSite); err == nil {
		t.Fatal("BumpStateWrite with empty SiteID should be refused")
	}
}
