package handler

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"log"
	"sync"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
)

const siteMutationLockStripes = 256
const siteCommitReconcileTimeout = 2 * time.Second

type siteCommitOutcome uint8

const (
	siteCommitUnknown siteCommitOutcome = iota
	siteCommitApplied
	siteCommitRolledBack
)

func (o siteCommitOutcome) String() string {
	switch o {
	case siteCommitApplied:
		return "applied"
	case siteCommitRolledBack:
		return "rolled_back"
	default:
		return "unknown"
	}
}

type siteCommitSnapshot struct {
	exists        bool
	siteID        string
	activeVersion int
}

type siteCommitExpectation struct {
	applied    siteCommitSnapshot
	rolledBack siteCommitSnapshot
}

type siteCommitReconciliation struct {
	outcome  siteCommitOutcome
	observed siteCommitSnapshot
	queryErr error
}

func existingSiteCommitSnapshot(siteID string, activeVersion int) siteCommitSnapshot {
	return siteCommitSnapshot{exists: true, siteID: siteID, activeVersion: activeVersion}
}

func classifySiteCommit(site db.Site, err error, expectation siteCommitExpectation) siteCommitReconciliation {
	result := siteCommitReconciliation{queryErr: err}
	switch {
	case err == nil:
		result.observed = existingSiteCommitSnapshot(site.ID, site.ActiveVersion)
	case errors.Is(err, sql.ErrNoRows):
		result.observed = siteCommitSnapshot{}
		result.queryErr = nil
	default:
		result.outcome = siteCommitUnknown
		return result
	}

	// When applied and rolled-back states are indistinguishable (for example,
	// rolling back to the already-active version), the intended state exists
	// and it is safe to finish the request as applied.
	switch result.observed {
	case expectation.applied:
		result.outcome = siteCommitApplied
	case expectation.rolledBack:
		result.outcome = siteCommitRolledBack
	default:
		result.outcome = siteCommitUnknown
	}
	return result
}

// reconcileSiteCommit deliberately uses the connection pool and a fresh,
// bounded background context. A request cancellation or Rollback result after
// Commit cannot establish whether Postgres committed the transaction.
func reconcileSiteCommit(database db.Querier, userID, siteName string, expectation siteCommitExpectation) siteCommitReconciliation {
	ctx, cancel := context.WithTimeout(context.Background(), siteCommitReconcileTimeout)
	defer cancel()
	site, err := db.GetSite(ctx, database, userID, siteName)
	return classifySiteCommit(site, err, expectation)
}

// logSiteCommitOutcome records a commit whose result had to be looked up.
// There is nothing to repair either way: the database is the only record of
// what is live, so whatever it holds is what every replica serves.
func logSiteCommitOutcome(operation, ownerID, siteName string, previousVersion, intendedVersion int, commitErr error, result siteCommitReconciliation) {
	log.Printf(
		"site commit reconciliation operation=%q owner_id=%q site=%q previous_version=%d intended_version=%d outcome=%q observed_exists=%t observed_active_version=%d commit_error=%v query_error=%v",
		operation, ownerID, siteName, previousVersion, intendedVersion, result.outcome.String(),
		result.observed.exists, result.observed.activeVersion, commitErr, result.queryErr,
	)
}

// siteMutationLocks queues same-site mutations inside one process before
// they take a database connection. Serialization itself is the advisory lock
// every mutation takes in its transaction (db.LockSiteCollaboration), which
// holds across replicas; this only keeps a burst of requests for one site
// from each holding a pooled connection while they wait for it. Hash
// collisions only reduce concurrency.
type siteMutationLocks struct {
	stripes [siteMutationLockStripes]sync.Mutex
}

func (l *siteMutationLocks) lock(userID, siteName string) func() {
	mutex := &l.stripes[l.index(userID, siteName)]
	mutex.Lock()
	return mutex.Unlock
}

func (l *siteMutationLocks) index(userID, siteName string) uint32 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(userID))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(siteName))
	return hash.Sum32() % siteMutationLockStripes
}
