package db

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
)

// approveAs is one admin's approval in its own transaction.
func approveAs(t *testing.T, database *sql.DB, siteID, adminID string, required int) (NetworkApproval, error) {
	t.Helper()
	var out NetworkApproval
	err := inTx(t, database, func(tx *sql.Tx) error {
		var err error
		out, err = ApproveNetworkAccess(context.Background(), tx, siteID, adminID, required)
		return err
	})
	return out, err
}

func requestNetwork(t *testing.T, database *sql.DB, siteID, requesterID string) {
	t.Helper()
	if err := inTx(t, database, func(tx *sql.Tx) error {
		return RequestNetworkAccess(context.Background(), tx, siteID, requesterID, "event page")
	}); err != nil {
		t.Fatal(err)
	}
}

func approvalRows(t *testing.T, database *sql.DB, siteID string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT count(*) FROM network_access_approvals WHERE site_id = $1`, siteID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestNetworkApprovalOneAdminRefusesSelf: with one approval required, an
// admin who made the request (here, as a member of a team site) cannot
// approve it; any other admin opens the site at once.
func TestNetworkApprovalOneAdminRefusesSelf(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	requesterID, siteID := mustCreateUserAndSite(t, database, "ann", "demo")
	otherID, _ := mustCreateUserAndSite(t, database, "bob", "unused")
	mustSetAccess(t, database, siteID, AccessCompany)
	requestNetwork(t, database, siteID, requesterID)

	if _, err := approveAs(t, database, siteID, requesterID, 1); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self-approval = %v, want ErrSelfApproval", err)
	}
	if open, _ := NetworkOpen(ctx, database, siteID); open {
		t.Fatal("self-approval opened the site")
	}
	got, err := approveAs(t, database, siteID, otherID, 1)
	if err != nil || !got.Approved || got.Approvals != 1 || got.Previous != AccessCompany {
		t.Fatalf("approval = %+v, %v", got, err)
	}
	if open, _ := NetworkOpen(ctx, database, siteID); !open {
		t.Fatal("approval did not open the site")
	}
	if n := approvalRows(t, database, siteID); n != 0 {
		t.Fatalf("%d approval rows left after the site opened", n)
	}
}

// TestNetworkApprovalTwoAdmins: with two required, the requester is
// refused, the first admin leaves the request pending with one approval,
// the same admin again is not counted twice, and a second admin opens the
// site. A disabled admin's earlier approval still counts.
func TestNetworkApprovalTwoAdmins(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	requesterID, siteID := mustCreateUserAndSite(t, database, "ann", "demo")
	firstID, _ := mustCreateUserAndSite(t, database, "bob", "unused")
	secondID, _ := mustCreateUserAndSite(t, database, "cy", "unused")
	mustSetAccess(t, database, siteID, AccessCompany)
	requestNetwork(t, database, siteID, requesterID)

	if _, err := approveAs(t, database, siteID, requesterID, 2); !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self-approval = %v, want ErrSelfApproval", err)
	}
	got, err := approveAs(t, database, siteID, firstID, 2)
	if err != nil || got.Approved || got.Duplicate || got.Approvals != 1 {
		t.Fatalf("first approval = %+v, %v", got, err)
	}
	got, err = approveAs(t, database, siteID, firstID, 2)
	if err != nil || got.Approved || !got.Duplicate || got.Approvals != 1 {
		t.Fatalf("same admin again = %+v, %v", got, err)
	}
	if open, _ := NetworkOpen(ctx, database, siteID); open {
		t.Fatal("one of two approvals opened the site")
	}
	entries, err := ListNetworkAccess(ctx, database)
	if err != nil || len(entries) != 1 || len(entries[0].ApprovedBy) != 1 ||
		entries[0].ApprovedBy[0].AdminID != firstID || entries[0].ApprovedBy[0].Name != "bob@example.com" ||
		entries[0].RequestedByID != requesterID {
		t.Fatalf("pending list = %+v, %v", entries, err)
	}
	access, err := ResolveSiteAccess(ctx, database, requesterID, "ann", "demo")
	if err != nil || access.Site.NetworkApprovals != 1 {
		t.Fatalf("owner's view: approvals = %d, %v", access.Site.NetworkApprovals, err)
	}

	// The first admin is disabled: their approval was valid when given.
	if err := SetUserDisabled(ctx, database, firstID, true); err != nil {
		t.Fatal(err)
	}
	got, err = approveAs(t, database, siteID, secondID, 2)
	if err != nil || !got.Approved || got.Approvals != 2 {
		t.Fatalf("second approval = %+v, %v", got, err)
	}
	if open, _ := NetworkOpen(ctx, database, siteID); !open {
		t.Fatal("second approval did not open the site")
	}
	if _, err := approveAs(t, database, siteID, firstID, 2); !errors.Is(err, ErrNoPendingRequest) {
		t.Fatalf("approval after opening = %v, want ErrNoPendingRequest", err)
	}
}

// TestNetworkApprovalClearedByDeclineWithdrawAndNewRequest: a decline, the
// owner changing the level, or a replacement request each start the count
// from zero.
func TestNetworkApprovalClearedByDeclineWithdrawAndNewRequest(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	requesterID, siteID := mustCreateUserAndSite(t, database, "ann", "demo")
	adminID, _ := mustCreateUserAndSite(t, database, "bob", "unused")
	mustSetAccess(t, database, siteID, AccessCompany)

	cases := map[string]func(){
		"decline": func() {
			if err := inTx(t, database, func(tx *sql.Tx) error { return DeclineNetworkAccess(ctx, tx, siteID) }); err != nil {
				t.Fatal(err)
			}
		},
		"withdraw":   func() { mustSetAccess(t, database, siteID, AccessCompany) },
		"re-request": func() { requestNetwork(t, database, siteID, requesterID) },
	}
	for name, end := range cases {
		requestNetwork(t, database, siteID, requesterID)
		if got, err := approveAs(t, database, siteID, adminID, 2); err != nil || got.Approvals != 1 {
			t.Fatalf("%s: first approval = %+v, %v", name, got, err)
		}
		end()
		if n := approvalRows(t, database, siteID); n != 0 {
			t.Fatalf("%s: %d approval rows left", name, n)
		}
		if name != "re-request" {
			requestNetwork(t, database, siteID, requesterID)
		}
		if got, err := approveAs(t, database, siteID, adminID, 2); err != nil || got.Approvals != 1 || got.Duplicate {
			t.Fatalf("%s: approval of the new request = %+v, %v; want a fresh count", name, got, err)
		}
		mustSetAccess(t, database, siteID, AccessCompany)
	}

	// An approval of an older request (left by code that does not clear the
	// table) never counts toward the current one.
	requestNetwork(t, database, siteID, requesterID)
	if _, err := database.Exec(`INSERT INTO network_access_approvals (site_id, requested_at, admin_id) VALUES ($1, now() - interval '1 day', $2)`, siteID, adminID); err != nil {
		t.Fatal(err)
	}
	if got, err := approveAs(t, database, siteID, adminID, 2); err != nil || got.Approvals != 1 || got.Duplicate {
		t.Fatalf("stale approval counted: %+v, %v", got, err)
	}
}

// TestNetworkApprovalConcurrent: two admins approving at the same instant
// with one approval already given are serialized: exactly one opens the
// site, and the other finds nothing pending. Two admins approving from zero
// at the same instant both count.
func TestNetworkApprovalConcurrent(t *testing.T) {
	database := assetsTestDB(t)
	ctx := context.Background()
	requesterID, siteID := mustCreateUserAndSite(t, database, "ann", "demo")
	admins := make([]string, 3)
	for i, name := range []string{"bob", "cy", "dee"} {
		admins[i], _ = mustCreateUserAndSite(t, database, name, "unused")
	}
	mustSetAccess(t, database, siteID, AccessCompany)

	race := func(ids ...string) (opened, pending, other int) {
		var wg sync.WaitGroup
		var mu sync.Mutex
		start := make(chan struct{})
		for _, id := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				tx, err := database.BeginTx(ctx, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer tx.Rollback()
				got, err := ApproveNetworkAccess(ctx, tx, siteID, id, 2)
				if err == nil {
					err = tx.Commit()
				}
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil && got.Approved:
					opened++
				case err == nil:
					pending++
				case errors.Is(err, ErrNoPendingRequest):
					other++
				default:
					t.Errorf("approve: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
		return
	}

	// From zero, two at once: one records 1 of 2, the other opens the site.
	requestNetwork(t, database, siteID, requesterID)
	if opened, pending, other := race(admins[0], admins[1]); opened != 1 || pending != 1 || other != 0 {
		t.Fatalf("race from zero: opened=%d pending=%d other=%d", opened, pending, other)
	}
	if open, _ := NetworkOpen(ctx, database, siteID); !open {
		t.Fatal("two approvals did not open the site")
	}

	// From one approval, two at once: exactly one opens it.
	mustSetAccess(t, database, siteID, AccessCompany)
	requestNetwork(t, database, siteID, requesterID)
	if _, err := approveAs(t, database, siteID, admins[0], 2); err != nil {
		t.Fatal(err)
	}
	if opened, pending, other := race(admins[1], admins[2]); opened != 1 || pending != 0 || other != 1 {
		t.Fatalf("race from one: opened=%d pending=%d other=%d", opened, pending, other)
	}
}
