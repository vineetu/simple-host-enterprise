package handler

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/storage"
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

func TestActionForSiteCommit(t *testing.T) {
	tests := []struct {
		name    string
		outcome siteCommitOutcome
		want    siteCommitAction
	}{
		{name: "applied continues", outcome: siteCommitApplied, want: siteCommitAction{continueSuccess: true}},
		{name: "rolled back compensates", outcome: siteCommitRolledBack, want: siteCommitAction{compensate: true}},
		{name: "unknown quarantines without compensation", outcome: siteCommitUnknown, want: siteCommitAction{quarantine: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := actionForSiteCommit(test.outcome); got != test.want {
				t.Fatalf("action = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestQuarantineSiteCurrentPreservesVersions(t *testing.T) {
	store := newMutationTestStorage(t)
	writeMutationVersion(t, store, 1, "v1")
	writeMutationVersion(t, store, 2, "v2")
	if err := store.SetCurrentVersion("alice", "demo", 2); err != nil {
		t.Fatal(err)
	}

	result := quarantineSiteCurrent(store, "alice", "demo", 2)
	if !result.provenAbsent || !result.failClosed() {
		t.Fatalf("quarantine result = %#v; want proven absence", result)
	}
	if result.hiddenCurrent == "" {
		t.Fatal("quarantine returned an empty repair token")
	}
	if current, exists, err := store.CurrentVersion("alice", "demo"); err != nil || exists {
		t.Fatalf("CurrentVersion after quarantine = %d, %t, %v; want hidden", current, exists, err)
	}
	for _, version := range []int{1, 2} {
		if !store.VersionDirExists("alice", "demo", version) {
			t.Fatalf("quarantine removed v%d", version)
		}
	}
	if err := store.RestoreCurrent("alice", "demo", result.hiddenCurrent); err != nil {
		t.Fatalf("repair token cannot restore current: %v", err)
	}
	if current, exists, err := store.CurrentVersion("alice", "demo"); err != nil || !exists || current != 2 {
		t.Fatalf("restored CurrentVersion = %d, %t, %v; want v2", current, exists, err)
	}
}

func TestQuarantineSiteCurrentFallsBackToExactRemoval(t *testing.T) {
	store := newMutationTestStorage(t)
	writeMutationVersion(t, store, 1, "v1")
	writeMutationVersion(t, store, 2, "v2")
	if err := store.SetCurrentVersion("alice", "demo", 2); err != nil {
		t.Fatal(err)
	}
	forced := &forcedQuarantineFailures{DiskStorage: store, failHide: true}

	result := quarantineSiteCurrent(forced, "alice", "demo", 2)
	if !result.provenAbsent || !result.fallbackRemoved || result.servingLatched || !result.failClosed() {
		t.Fatalf("quarantine result = %#v; want proven fallback removal", result)
	}
	if current, exists, err := store.CurrentVersion("alice", "demo"); err != nil || exists {
		t.Fatalf("CurrentVersion after fallback = %d, %t, %v; want absent", current, exists, err)
	}
	for _, version := range []int{1, 2} {
		if !store.VersionDirExists("alice", "demo", version) {
			t.Fatalf("fallback removed retained v%d", version)
		}
	}
}

func TestQuarantineSiteCurrentLatchesServingWhenRemovalCannotBeProven(t *testing.T) {
	store := newMutationTestStorage(t)
	writeMutationVersion(t, store, 1, "v1")
	writeMutationVersion(t, store, 2, "v2")
	if err := store.SetCurrentVersion("alice", "demo", 2); err != nil {
		t.Fatal(err)
	}
	forced := &forcedQuarantineFailures{DiskStorage: store, failHide: true, failRemove: true}

	result := quarantineSiteCurrent(forced, "alice", "demo", 2)
	if result.provenAbsent || !result.servingLatched || !result.failClosed() {
		t.Fatalf("quarantine result = %#v; want active serving latch", result)
	}
	if _, err := store.OpenCurrent("alice", "demo"); !errors.Is(err, storage.ErrSiteCurrentQuarantined) {
		t.Fatalf("OpenCurrent error = %v, want ErrSiteCurrentQuarantined", err)
	}
	if _, _, err := store.CurrentVersion("alice", "demo"); !errors.Is(err, storage.ErrSiteCurrentQuarantined) {
		t.Fatalf("CurrentVersion error = %v, want ErrSiteCurrentQuarantined", err)
	}
	if target, err := os.Readlink(filepath.Join(store.BasePath(), "alice", "demo", "current")); err != nil || target != "v2" {
		t.Fatalf("retained current target = %q, %v; want v2", target, err)
	}
	for _, version := range []int{1, 2} {
		if !store.VersionDirExists("alice", "demo", version) {
			t.Fatalf("serving latch removed retained v%d", version)
		}
	}
	if action := actionForSiteCommit(siteCommitUnknown); action.compensate || !action.quarantine {
		t.Fatalf("unknown action = %#v; must latch/quarantine without restoration", action)
	}
	if err := store.SetCurrentVersion("alice", "demo", 1); !errors.Is(err, storage.ErrSiteCurrentQuarantined) {
		t.Fatalf("SetCurrentVersion error = %v, want quarantine rejection", err)
	}
	if err := store.WriteFiles("alice", "demo", 3, map[string][]byte{"index.html": []byte("v3")}); !errors.Is(err, storage.ErrSiteCurrentQuarantined) {
		t.Fatalf("WriteFiles error = %v, want quarantine rejection", err)
	}
	if err := store.DeleteSite("alice", "demo"); !errors.Is(err, storage.ErrSiteCurrentQuarantined) {
		t.Fatalf("DeleteSite error = %v, want quarantine rejection", err)
	}
	if store.VersionDirExists("alice", "demo", 3) {
		t.Fatal("quarantined mutation published v3")
	}
}

func TestCompensationFailureQuarantinesIntendedCurrent(t *testing.T) {
	t.Run("verified hide", func(t *testing.T) {
		store := newMutationTestStorage(t)
		writeMutationVersion(t, store, 1, "v1")
		writeMutationVersion(t, store, 2, "v2")
		if err := store.SetCurrentVersion("alice", "demo", 2); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteVersion("alice", "demo", 1); err != nil {
			t.Fatal(err)
		}

		compensationErr := compensatePublishedUpdate(store, "alice", "demo", 1, 2, true)
		if compensationErr == nil {
			t.Fatal("compensation unexpectedly restored missing v1")
		}
		if !store.VersionDirExists("alice", "demo", 2) {
			t.Fatal("failed switch compensation destructively removed intended v2")
		}
		result := quarantineSiteCurrent(store, "alice", "demo", 2)
		if !result.provenAbsent || !result.failClosed() {
			t.Fatalf("quarantine result = %#v; want proven absence", result)
		}
		if current, exists, err := store.CurrentVersion("alice", "demo"); err != nil || exists {
			t.Fatalf("CurrentVersion after compensation quarantine = %d, %t, %v; want absent", current, exists, err)
		}
		if !store.VersionDirExists("alice", "demo", 2) {
			t.Fatal("compensation quarantine removed retained v2")
		}
	})

	t.Run("serving latch", func(t *testing.T) {
		store := newMutationTestStorage(t)
		writeMutationVersion(t, store, 1, "v1")
		writeMutationVersion(t, store, 2, "v2")
		if err := store.SetCurrentVersion("alice", "demo", 2); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteVersion("alice", "demo", 1); err != nil {
			t.Fatal(err)
		}
		if err := compensatePublishedUpdate(store, "alice", "demo", 1, 2, true); err == nil {
			t.Fatal("compensation unexpectedly restored missing v1")
		}

		forced := &forcedQuarantineFailures{DiskStorage: store, failHide: true, failRemove: true}
		result := quarantineSiteCurrent(forced, "alice", "demo", 2)
		if !result.servingLatched || !result.failClosed() {
			t.Fatalf("quarantine result = %#v; want serving latch", result)
		}
		if _, err := store.OpenCurrent("alice", "demo"); !errors.Is(err, storage.ErrSiteCurrentQuarantined) {
			t.Fatalf("OpenCurrent error = %v, want ErrSiteCurrentQuarantined", err)
		}
		if !store.VersionDirExists("alice", "demo", 2) {
			t.Fatal("latched compensation quarantine removed retained v2")
		}
	})
}

type forcedQuarantineFailures struct {
	*storage.DiskStorage
	failHide   bool
	failRemove bool
}

func (f *forcedQuarantineFailures) HideCurrent(username, siteName string) (string, error) {
	if f.failHide {
		return "", errors.New("forced hide failure")
	}
	return f.DiskStorage.HideCurrent(username, siteName)
}

func (f *forcedQuarantineFailures) RemoveCurrentVersion(username, siteName string, version int) error {
	if f.failRemove {
		return errors.New("forced removal failure")
	}
	return f.DiskStorage.RemoveCurrentVersion(username, siteName, version)
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

func TestCompensatePublishedUpdateRestoresPriorCurrent(t *testing.T) {
	store := newMutationTestStorage(t)
	writeMutationVersion(t, store, 1, "v1")
	writeMutationVersion(t, store, 2, "v2")
	if err := store.SetCurrentVersion("alice", "demo", 2); err != nil {
		t.Fatal(err)
	}
	if err := compensatePublishedUpdate(store, "alice", "demo", 1, 2, true); err != nil {
		t.Fatal(err)
	}
	if current, exists, err := store.CurrentVersion("alice", "demo"); err != nil || !exists || current != 1 {
		t.Fatalf("CurrentVersion = %d, %t, %v; want v1", current, exists, err)
	}
	if store.VersionDirExists("alice", "demo", 2) {
		t.Fatal("uncommitted v2 was not deleted")
	}
}

func TestCompensatePublishedCreatePreservesOrphanVersion(t *testing.T) {
	store := newMutationTestStorage(t)
	writeMutationVersion(t, store, 7, "orphan-sentinel")
	writeMutationVersion(t, store, 1, "new-v1")
	if err := store.SetCurrentVersion("alice", "demo", 1); err != nil {
		t.Fatal(err)
	}
	if err := compensatePublishedCreate(store, "alice", "demo", 1, true); err != nil {
		t.Fatal(err)
	}
	if store.VersionDirExists("alice", "demo", 1) {
		t.Fatal("uncommitted v1 was not deleted")
	}
	if !store.VersionDirExists("alice", "demo", 7) {
		t.Fatal("pre-existing orphan v7 was deleted")
	}
	root, err := store.OpenCurrent("alice", "demo")
	if err == nil {
		root.Close()
		t.Fatal("create compensation left an uncommitted current link")
	}
}

func TestPreCreateCurrentCheckDetectsOrphanCurrent(t *testing.T) {
	store := newMutationTestStorage(t)
	writeMutationVersion(t, store, 7, "orphan-sentinel")
	if err := store.SetCurrentVersion("alice", "demo", 7); err != nil {
		t.Fatal(err)
	}
	current, exists, err := store.CurrentVersion("alice", "demo")
	if err != nil || !exists || current != 7 {
		t.Fatalf("CurrentVersion = %d, %t, %v; create must refuse this orphan", current, exists, err)
	}
	root, err := store.OpenCurrent("alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	content, err := fs.ReadFile(root.FS(), "index.html")
	if err != nil || string(content) != "orphan-sentinel" {
		t.Fatalf("orphan current content = %q, %v", content, err)
	}
}

func newMutationTestStorage(t *testing.T) *storage.DiskStorage {
	t.Helper()
	store, err := storage.NewDiskStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store
}

func writeMutationVersion(t *testing.T, store *storage.DiskStorage, version int, content string) {
	t.Helper()
	if err := store.WriteFiles("alice", "demo", version, map[string][]byte{"index.html": []byte(content)}); err != nil {
		t.Fatal(err)
	}
}
