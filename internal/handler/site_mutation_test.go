package handler

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
)

func TestClassifySiteCommitExactOutcomes(t *testing.T) {
	created := siteCommitExpectation{
		applied:    existingSiteCommitSnapshot("site-1", 1),
		rolledBack: siteCommitSnapshot{},
	}
	updated := siteCommitExpectation{
		applied:    existingSiteCommitSnapshot("site-1", 4),
		rolledBack: existingSiteCommitSnapshot("site-1", 3),
	}
	deleted := siteCommitExpectation{
		applied:    siteCommitSnapshot{},
		rolledBack: existingSiteCommitSnapshot("site-1", 3),
	}
	sameVersionRollback := siteCommitExpectation{
		applied:    existingSiteCommitSnapshot("site-1", 3),
		rolledBack: existingSiteCommitSnapshot("site-1", 3),
	}

	tests := []struct {
		name        string
		site        db.Site
		err         error
		expectation siteCommitExpectation
		want        siteCommitOutcome
	}{
		{name: "create applied exact", site: db.Site{ID: "site-1", ActiveVersion: 1}, expectation: created, want: siteCommitApplied},
		{name: "create rolled back absence", err: sql.ErrNoRows, expectation: created, want: siteCommitRolledBack},
		{name: "create wrong id unknown", site: db.Site{ID: "site-2", ActiveVersion: 1}, expectation: created, want: siteCommitUnknown},
		{name: "create wrong version unknown", site: db.Site{ID: "site-1", ActiveVersion: 2}, expectation: created, want: siteCommitUnknown},
		{name: "update applied exact", site: db.Site{ID: "site-1", ActiveVersion: 4}, expectation: updated, want: siteCommitApplied},
		{name: "update rolled back exact", site: db.Site{ID: "site-1", ActiveVersion: 3}, expectation: updated, want: siteCommitRolledBack},
		{name: "update absence unknown", err: sql.ErrNoRows, expectation: updated, want: siteCommitUnknown},
		{name: "update other version unknown", site: db.Site{ID: "site-1", ActiveVersion: 2}, expectation: updated, want: siteCommitUnknown},
		{name: "update other id unknown", site: db.Site{ID: "site-2", ActiveVersion: 4}, expectation: updated, want: siteCommitUnknown},
		{name: "delete applied absence", err: sql.ErrNoRows, expectation: deleted, want: siteCommitApplied},
		{name: "delete rolled back exact", site: db.Site{ID: "site-1", ActiveVersion: 3}, expectation: deleted, want: siteCommitRolledBack},
		{name: "delete changed version unknown", site: db.Site{ID: "site-1", ActiveVersion: 4}, expectation: deleted, want: siteCommitUnknown},
		{name: "query failure unknown", err: errors.New("database unavailable"), expectation: updated, want: siteCommitUnknown},
		{name: "indistinguishable rollback is applied", site: db.Site{ID: "site-1", ActiveVersion: 3}, expectation: sameVersionRollback, want: siteCommitApplied},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := classifySiteCommit(test.site, test.err, test.expectation)
			if result.outcome != test.want {
				t.Fatalf("outcome = %d, want %d", result.outcome, test.want)
			}
		})
	}
}

func TestSiteMutationLocksSerializeSameSite(t *testing.T) {
	var locks siteMutationLocks
	unlock := locks.lock("stable-user-id", "demo")
	acquired := make(chan struct{})
	go func() {
		release := locks.lock("stable-user-id", "demo")
		close(acquired)
		release()
	}()
	select {
	case <-acquired:
		t.Fatal("same-site lock was acquired concurrently")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("same-site lock was not released")
	}
}
