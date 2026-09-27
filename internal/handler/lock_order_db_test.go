package handler

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
)

// Two-connection interleavings: a transaction on its own connection holds a
// site-name lock so that a bulk operation stops between reading its site
// list and locking a later site, while a move runs in that gap.

// holdSiteName takes ownerID/name's site lock in a transaction of its own
// and returns the function that releases it.
func (w *accessWorld) holdSiteName(ownerID, name string) func() {
	w.t.Helper()
	tx, err := w.database.BeginTx(context.Background(), nil)
	if err != nil {
		w.t.Fatal(err)
	}
	if err := db.LockSiteCollaboration(context.Background(), tx, ownerID, name); err != nil {
		_ = tx.Rollback()
		w.t.Fatal(err)
	}
	return func() { _ = tx.Rollback() }
}

// lockWaiters counts this database's backends waiting on a lock.
func (w *accessWorld) lockWaiters() int {
	var n int
	_ = w.database.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n)
	return n
}

// waitFor polls until cond holds or five seconds pass.
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// liveSitesQueuedForRetirement counts sites that still exist while their
// files are queued for the retire sweep: the sweep would empty them.
func (w *accessWorld) liveSitesQueuedForRetirement() int {
	return w.count(`SELECT count(*) FROM sites s JOIN storage_retired r ON r.object_key = 'sites/' || s.id::text || '/'`)
}

// A team being deleted holds its row before its sites' names; a transfer
// out of the team takes the team's row first too, so it can no longer
// commit between the deletion's site list and its name locks and have the
// transferred site's files swept.
func TestTeamDeleteAndTransferOutDoNotRace(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "alice", "mo")
	w.newTeam("dest", "alice")
	w.deploy("alice", "/api/collaboration/sites/team-crew/aaa")
	w.deploy("alice", "/api/collaboration/sites/team-crew/sss")
	var crewID string
	_ = w.database.QueryRow(`SELECT id::text FROM users WHERE username = 'team-crew'`).Scan(&crewID)

	release := w.holdSiteName(crewID, "aaa")
	deleted := make(chan *httptest.ResponseRecorder, 1)
	go func() { deleted <- w.api("alice", http.MethodDelete, "/api/teams/crew?confirm_name=crew", nil) }()
	if !waitFor(func() bool { return w.lockWaiters() >= 1 }) {
		release()
		t.Fatal("team deletion never waited on the held site name")
	}
	moved := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		moved <- w.api("alice", http.MethodPost, "/api/collaboration/sites/team-crew/sss/transfer", map[string]any{"to": "dest"})
	}()
	// The transfer either waits behind the deletion (fixed) or finishes
	// in the gap (the race).
	var early *httptest.ResponseRecorder
	waitFor(func() bool {
		select {
		case early = <-moved:
			return true
		default:
			return w.lockWaiters() >= 2
		}
	})
	release()
	del := <-deleted
	move := early
	if move == nil {
		move = <-moved
	}
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete team = %d %s", del.Code, del.Body)
	}
	if move.Code == http.StatusOK {
		t.Errorf("transfer out of a team being deleted succeeded: %s", move.Body)
	}
	if move.Code >= 500 {
		t.Errorf("transfer = %d %s", move.Code, move.Body)
	}
	if n := w.liveSitesQueuedForRetirement(); n != 0 {
		t.Errorf("%d live sites have their files queued for retirement", n)
	}
	if n := w.count(`SELECT count(*) FROM sites s JOIN users u ON u.id = s.user_id WHERE u.username IN ('team-crew', 'team-dest')`); n != 0 {
		t.Errorf("%d sites survived; want both deleted with the team", n)
	}
}

// A bulk delete of a leaver's sites re-checks each site's owner and name
// under its lock: a site an admin handed to someone else after the list was
// read is theirs, and stays live.
func TestAdminBulkDeleteSkipsSiteMovedAway(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/aaa")
	w.deploy("alice", "/api/sites/sss")
	w.disable("alice")
	movedID := w.siteID("alice", "sss")

	release := w.holdSiteName(w.users["alice"], "aaa")
	deleted := make(chan *httptest.ResponseRecorder, 1)
	go func() { deleted <- w.adminPost("/api/admin/users/alice/delete-sites", "") }()
	if !waitFor(func() bool { return w.lockWaiters() >= 1 }) {
		release()
		t.Fatal("bulk delete never waited on the held site name")
	}
	if rec := w.adminPost("/api/admin/sites/alice/sss/transfer", "to=mo"); rec.Code != http.StatusOK {
		release()
		t.Fatalf("admin transfer during bulk delete = %d %s", rec.Code, rec.Body)
	}
	release()
	if rec := <-deleted; rec.Code != http.StatusOK {
		t.Fatalf("bulk delete = %d %s", rec.Code, rec.Body)
	}
	var owner string
	var deletedAt sql.NullTime
	if err := w.database.QueryRow(`SELECT u.username, s.deleted_at FROM sites s JOIN users u ON u.id = s.user_id WHERE s.id = $1`, movedID).Scan(&owner, &deletedAt); err != nil {
		t.Fatal(err)
	}
	if owner != "mo" || deletedAt.Valid {
		t.Errorf("moved site is %s's, deleted=%v; want mo's and live", owner, deletedAt.Valid)
	}
	if n := w.count(`SELECT count(*) FROM sites WHERE user_id = $1 AND name = 'aaa' AND deleted_at IS NOT NULL`, w.users["alice"]); n != 1 {
		t.Errorf("alice's remaining site not deleted")
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_delete' AND site_id = $1`, movedID); n != 0 {
		t.Errorf("moved site has %d site_delete audit rows", n)
	}
}
