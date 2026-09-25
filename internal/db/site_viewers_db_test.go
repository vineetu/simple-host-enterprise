package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func mustSetAccess(t *testing.T, database *sql.DB, siteID, level string) string {
	t.Helper()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	previous, err := SetSiteAccess(context.Background(), tx, siteID, level)
	if err != nil {
		t.Fatalf("SetSiteAccess(%s): %v", level, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return previous
}

func inTx(t *testing.T, database *sql.DB, fn func(tx *sql.Tx) error) error {
	t.Helper()
	tx, err := database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// TestAccessLevelMatrix is who can open a site at each level: the owner, a
// member of the owning team, a named viewer, and any other signed-in person.
// Writing saved data follows opening exactly.
func TestAccessLevelMatrix(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	ownerID, siteID := mustCreateUserAndSite(t, database, "alice", "demo")
	viewerID, _ := mustCreateUserAndSite(t, database, "vera", "v")
	otherID, _ := mustCreateUserAndSite(t, database, "olly", "o")
	memberID, _ := mustCreateUserAndSite(t, database, "mo", "m")

	var team User
	if err := inTx(t, database, func(tx *sql.Tx) error {
		var err error
		team, err = CreateTeam(ctx, tx, "crew", memberID)
		return err
	}); err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	teamSite, err := CreateSite(ctx, database, team.ID, "board")
	if err != nil {
		t.Fatal(err)
	}

	var access string
	if err := database.QueryRow(`SELECT access FROM sites WHERE id = $1`, siteID).Scan(&access); err != nil || access != AccessOnlyMe {
		t.Fatalf("new site access = %q (%v), want only_me", access, err)
	}

	if err := inTx(t, database, func(tx *sql.Tx) error {
		_, err := GrantSiteViewers(ctx, tx, ownerID, "demo", siteID, &ownerID, []string{"vera"})
		return err
	}); err != nil {
		t.Fatalf("GrantSiteViewers: %v", err)
	}
	if err := database.QueryRow(`SELECT access FROM sites WHERE id = $1`, siteID).Scan(&access); err != nil || access != AccessSpecific {
		t.Fatalf("access after a viewer grant = %q (%v), want specific", access, err)
	}

	type who struct {
		name string
		id   string
	}
	people := []who{{"owner", ownerID}, {"viewer", viewerID}, {"other", otherID}}
	want := map[string]map[string]bool{
		AccessOnlyMe:   {"owner": true, "viewer": false, "other": false},
		AccessSpecific: {"owner": true, "viewer": true, "other": false},
		AccessCompany:  {"owner": true, "viewer": true, "other": true},
		AccessListed:   {"owner": true, "viewer": true, "other": true},
	}
	for _, level := range []string{AccessOnlyMe, AccessSpecific, AccessCompany, AccessListed} {
		mustSetAccess(t, database, siteID, level)
		for _, p := range people {
			view, err := ViewerAllowed(ctx, database, siteID, p.id)
			if err != nil {
				t.Fatal(err)
			}
			write, err := WriterAllowed(ctx, database, siteID, p.id)
			if err != nil {
				t.Fatal(err)
			}
			if view != want[level][p.name] || write != view {
				t.Errorf("%s/%s: view=%v write=%v, want %v", level, p.name, view, write, want[level][p.name])
			}
		}
		restricted, err := IsSiteRestricted(ctx, database, siteID)
		if err != nil || restricted != (level == AccessSpecific) {
			t.Errorf("%s: restricted=%v (%v)", level, restricted, err)
		}
		var public bool
		_ = database.QueryRow(`SELECT public FROM sites WHERE id = $1`, siteID).Scan(&public)
		if public != (level == AccessListed) {
			t.Errorf("%s: public=%v", level, public)
		}
	}

	// A team site at only_me: its members open it, nobody else does.
	for id, ok := range map[string]bool{memberID: true, ownerID: false} {
		view, err := ViewerAllowed(ctx, database, teamSite.ID, id)
		if err != nil || view != ok {
			t.Errorf("team site only_me: view(%s)=%v (%v), want %v", id, view, err, ok)
		}
	}
	if view, err := ViewerAllowed(ctx, database, "00000000-0000-4000-8000-000000000000", ownerID); err != nil || view {
		t.Errorf("unknown site: view=%v (%v), want false", view, err)
	}
}

// TestNetworkAccessRequestFlow: a request leaves the level alone until an
// admin approves; decline clears it; the owner lowering the level revokes an
// approval.
func TestNetworkAccessRequestFlow(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	ownerID, siteID := mustCreateUserAndSite(t, database, "alice", "demo")
	mustSetAccess(t, database, siteID, AccessCompany)

	tx, _ := database.Begin()
	if _, err := SetSiteAccess(ctx, tx, siteID, AccessNetwork); err == nil {
		t.Fatal("SetSiteAccess accepted network without approval")
	}
	tx.Rollback()

	if err := inTx(t, database, func(tx *sql.Tx) error { return DeclineNetworkAccess(ctx, tx, siteID) }); !errors.Is(err, ErrNoPendingRequest) {
		t.Fatalf("decline with nothing pending = %v", err)
	}
	if err := inTx(t, database, func(tx *sql.Tx) error { return RequestNetworkAccess(ctx, tx, siteID, ownerID, "event page") }); err != nil {
		t.Fatal(err)
	}
	if open, _ := NetworkOpen(ctx, database, siteID); open {
		t.Fatal("a pending request opened the site")
	}
	entries, err := ListNetworkAccess(ctx, database)
	if err != nil || len(entries) != 1 || entries[0].Reason != "event page" || entries[0].RequestedBy != "alice" || entries[0].Access != AccessCompany {
		t.Fatalf("pending list = %+v (%v)", entries, err)
	}
	if err := inTx(t, database, func(tx *sql.Tx) error { return DeclineNetworkAccess(ctx, tx, siteID) }); err != nil {
		t.Fatal(err)
	}
	if entries, _ := ListNetworkAccess(ctx, database); len(entries) != 0 {
		t.Fatalf("declined request still listed: %+v", entries)
	}

	if err := inTx(t, database, func(tx *sql.Tx) error { return RequestNetworkAccess(ctx, tx, siteID, ownerID, "again") }); err != nil {
		t.Fatal(err)
	}
	if err := inTx(t, database, func(tx *sql.Tx) error { _, err := ApproveNetworkAccess(ctx, tx, siteID); return err }); err != nil {
		t.Fatal(err)
	}
	if open, _ := NetworkOpen(ctx, database, siteID); !open {
		t.Fatal("approval did not open the site")
	}
	if err := inTx(t, database, func(tx *sql.Tx) error { return RequestNetworkAccess(ctx, tx, siteID, ownerID, "x") }); !errors.Is(err, ErrAlreadyNetwork) {
		t.Fatalf("request on a network site = %v", err)
	}
	if previous := mustSetAccess(t, database, siteID, AccessListed); previous != AccessNetwork {
		t.Fatalf("previous = %q", previous)
	}
	if open, _ := NetworkOpen(ctx, database, siteID); open {
		t.Fatal("lowering the level did not revoke network access")
	}
	if err := inTx(t, database, func(tx *sql.Tx) error { _, err := ApproveNetworkAccess(ctx, tx, siteID); return err }); !errors.Is(err, ErrNoPendingRequest) {
		t.Fatalf("approve after lowering = %v, want ErrNoPendingRequest", err)
	}
}

// TestStateHistoryKeepsTwentyAndRestores writes 25 states, checks only the
// newest 20 remain, and restores an older one as a new write.
func TestStateHistoryKeepsTwentyAndRestores(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	ownerID, siteID := mustCreateUserAndSite(t, database, "alice", "demo")
	for i := 1; i <= 25; i++ {
		if err := inTx(t, database, func(tx *sql.Tx) error {
			if err := UpdateSiteState(ctx, tx, "alice", "demo", json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
				return err
			}
			return RecordStateHistory(ctx, tx, siteID, ownerID)
		}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := ListStateHistory(ctx, database, siteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != MaxStateHistory || entries[0].StateVersion != 25 || entries[19].StateVersion != 6 || entries[0].WrittenBy != "alice" {
		t.Fatalf("history = %d entries, newest v%d, oldest v%d", len(entries), entries[0].StateVersion, entries[len(entries)-1].StateVersion)
	}
	oldest := entries[19]
	var version int64
	if err := inTx(t, database, func(tx *sql.Tx) error {
		var err error
		version, err = RestoreStateHistory(ctx, tx, siteID, oldest.ID, ownerID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	state, current, _, err := GetSiteStateVersioned(ctx, database, "alice", "demo")
	if err != nil || current != 26 || version != 26 || string(state) != `{"n": 6}` {
		t.Fatalf("after restore: state=%s version=%d/%d (%v)", state, current, version, err)
	}
	entries, _ = ListStateHistory(ctx, database, siteID)
	if len(entries) != MaxStateHistory || entries[0].StateVersion != 26 {
		t.Fatalf("restore not recorded in history: %+v", entries[0])
	}

	_, otherSite := mustCreateUserAndSite(t, database, "bob", "other")
	if err := inTx(t, database, func(tx *sql.Tx) error {
		_, err := RestoreStateHistory(ctx, tx, otherSite, entries[0].ID, ownerID)
		return err
	}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("restoring another site's entry = %v, want ErrNoRows", err)
	}
}
