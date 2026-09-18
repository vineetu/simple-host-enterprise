package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"sync"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/storage"
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

type siteCommitAction struct {
	continueSuccess bool
	compensate      bool
	quarantine      bool
}

type siteCurrentQuarantiner interface {
	HideCurrent(username, siteName string) (string, error)
	RemoveCurrentVersion(username, siteName string, version int) error
	CurrentVersion(username, siteName string) (int, bool, error)
	QuarantineCurrentServing(username, siteName string) error
}

type siteCurrentQuarantine struct {
	hiddenCurrent   string
	fallbackRemoved bool
	provenAbsent    bool
	observedVersion int
	observedExists  bool
	hideErr         error
	firstCheckErr   error
	removeErr       error
	finalCheckErr   error
	servingLatched  bool
	latchErr        error
}

func (q siteCurrentQuarantine) failClosed() bool {
	return q.provenAbsent || q.servingLatched
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

func actionForSiteCommit(outcome siteCommitOutcome) siteCommitAction {
	switch outcome {
	case siteCommitApplied:
		return siteCommitAction{continueSuccess: true}
	case siteCommitRolledBack:
		return siteCommitAction{compensate: true}
	default:
		return siteCommitAction{quarantine: true}
	}
}

func quarantineSiteCurrent(disk siteCurrentQuarantiner, username, siteName string, intendedVersion int) siteCurrentQuarantine {
	result := siteCurrentQuarantine{}
	result.hiddenCurrent, result.hideErr = disk.HideCurrent(username, siteName)
	result.observedVersion, result.observedExists, result.firstCheckErr = disk.CurrentVersion(username, siteName)
	if result.firstCheckErr == nil && !result.observedExists {
		result.provenAbsent = true
		return result
	}

	result.removeErr = disk.RemoveCurrentVersion(username, siteName, intendedVersion)
	result.fallbackRemoved = result.removeErr == nil
	result.observedVersion, result.observedExists, result.finalCheckErr = disk.CurrentVersion(username, siteName)
	result.provenAbsent = result.finalCheckErr == nil && !result.observedExists
	if !result.provenAbsent {
		result.latchErr = disk.QuarantineCurrentServing(username, siteName)
		result.servingLatched = result.latchErr == nil
	}
	return result
}

func logSiteCommitReconciliation(operation, ownerID, username, siteName, expectedSiteID string, previousVersion, intendedVersion int, commitErr error, result siteCommitReconciliation, quarantine siteCurrentQuarantine) {
	prefix := "site commit reconciliation"
	if result.outcome == siteCommitUnknown && quarantine.servingLatched {
		prefix = "CRITICAL site commit serving latch active; DO NOT RESTART OR HOT DEPLOY UNTIL REPAIRED"
	} else if result.outcome == siteCommitUnknown && !quarantine.failClosed() {
		prefix = "CRITICAL site commit fail-closed quarantine FAILED; REMOVE TRAFFIC IMMEDIATELY"
	}
	log.Printf(
		"%s operation=%q owner_id=%q username=%q site=%q expected_site_id=%q previous_version=%d intended_version=%d outcome=%q observed_exists=%t observed_site_id=%q observed_active_version=%d hidden_current=%q quarantine_fail_closed=%t quarantine_proven_absent=%t fallback_removed=%t serving_latched=%t quarantine_observed_exists=%t quarantine_observed_version=%d commit_error_type=%T query_error_type=%T hide_error_type=%T first_check_error_type=%T remove_error_type=%T final_check_error_type=%T latch_error_type=%T",
		prefix,
		operation,
		ownerID,
		username,
		siteName,
		expectedSiteID,
		previousVersion,
		intendedVersion,
		result.outcome.String(),
		result.observed.exists,
		result.observed.siteID,
		result.observed.activeVersion,
		quarantine.hiddenCurrent,
		quarantine.failClosed(),
		quarantine.provenAbsent,
		quarantine.fallbackRemoved,
		quarantine.servingLatched,
		quarantine.observedExists,
		quarantine.observedVersion,
		commitErr,
		result.queryErr,
		quarantine.hideErr,
		quarantine.firstCheckErr,
		quarantine.removeErr,
		quarantine.finalCheckErr,
		quarantine.latchErr,
	)
}

func logSiteCompensationQuarantine(operation, ownerID, username, siteName string, previousVersion, intendedVersion int, compensationErr error, quarantine siteCurrentQuarantine) {
	prefix := "site compensation failure quarantined"
	if quarantine.servingLatched {
		prefix = "CRITICAL site compensation serving latch active; DO NOT RESTART OR HOT DEPLOY UNTIL REPAIRED"
	} else if !quarantine.failClosed() {
		prefix = "CRITICAL site compensation fail-closed quarantine FAILED; REMOVE TRAFFIC IMMEDIATELY"
	}
	log.Printf(
		"%s operation=%q owner_id=%q username=%q site=%q previous_version=%d intended_version=%d hidden_current=%q quarantine_fail_closed=%t quarantine_proven_absent=%t fallback_removed=%t serving_latched=%t quarantine_observed_exists=%t quarantine_observed_version=%d compensation_error_type=%T hide_error_type=%T first_check_error_type=%T remove_error_type=%T final_check_error_type=%T latch_error_type=%T",
		prefix,
		operation,
		ownerID,
		username,
		siteName,
		previousVersion,
		intendedVersion,
		quarantine.hiddenCurrent,
		quarantine.failClosed(),
		quarantine.provenAbsent,
		quarantine.fallbackRemoved,
		quarantine.servingLatched,
		quarantine.observedExists,
		quarantine.observedVersion,
		compensationErr,
		quarantine.hideErr,
		quarantine.firstCheckErr,
		quarantine.removeErr,
		quarantine.finalCheckErr,
		quarantine.latchErr,
	)
}

// siteMutationLocks bounds lock state while serializing the database+disk
// critical section for one stable owner ID and validated site name. Hash
// collisions only reduce concurrency; they cannot weaken serialization.
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

func compensatePublishedUpdate(disk *storage.DiskStorage, username, siteName string, previousVersion, newVersion int, switched bool) error {
	if switched {
		if previousVersion < 1 {
			return fmt.Errorf("cannot restore invalid previous version %d; preserved v%d", previousVersion, newVersion)
		}
		if err := disk.SetCurrentVersion(username, siteName, previousVersion); err != nil {
			// Do not remove newVersion if current might still reference it.
			return fmt.Errorf("restore current to v%d (preserved v%d): %w", previousVersion, newVersion, err)
		}
	}
	if err := disk.DeleteVersion(username, siteName, newVersion); err != nil {
		return fmt.Errorf("delete uncommitted v%d: %w", newVersion, err)
	}
	return nil
}

func compensatePublishedCreate(disk *storage.DiskStorage, username, siteName string, version int, switched bool) error {
	if switched {
		if err := disk.RemoveCurrentVersion(username, siteName, version); err != nil {
			return fmt.Errorf("remove uncommitted current v%d (preserved version): %w", version, err)
		}
	}
	if err := disk.DeleteVersion(username, siteName, version); err != nil {
		return fmt.Errorf("delete uncommitted v%d: %w", version, err)
	}
	return nil
}

func restorePriorCurrent(disk *storage.DiskStorage, username, siteName string, previousVersion int) error {
	if previousVersion < 1 {
		return fmt.Errorf("cannot restore invalid previous version %d", previousVersion)
	}
	if err := disk.SetCurrentVersion(username, siteName, previousVersion); err != nil {
		return fmt.Errorf("restore current to v%d: %w", previousVersion, err)
	}
	return nil
}
