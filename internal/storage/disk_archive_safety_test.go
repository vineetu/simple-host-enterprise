package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// Independent checks of the two properties that decide whether archiving is
// safe to run against real user data. Written separately from the archive
// implementation's own tests so the behaviour is verified rather than assumed.

func newArchiveTestStorage(t *testing.T) (*DiskStorage, string) {
	t.Helper()
	base := t.TempDir()
	store, err := NewDiskStorage(base)
	if err != nil {
		t.Fatalf("new storage: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, base
}

func writeVersion(t *testing.T, store *DiskStorage, user, site string, version int, files map[string][]byte) {
	t.Helper()
	if err := store.WriteFiles(user, site, version, files); err != nil {
		t.Fatalf("write v%d: %v", version, err)
	}
}

// readTree returns every regular file under root, keyed by relative path, so
// two versions can be compared byte for byte.
func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// A user's site must come back byte-identical after a compress/decompress
// cycle. Anything less means rollback hands them something they did not deploy.
func TestArchiveRoundTripPreservesContentExactly(t *testing.T) {
	store, base := newArchiveTestStorage(t)
	const user, site = "alice", "portfolio"

	files := map[string][]byte{
		"index.html":         []byte("<!doctype html><h1>hello</h1>"),
		"assets/app.css":     []byte("body{margin:0}"),
		"assets/deep/x.json": []byte(`{"a":[1,2,3]}`),
		"empty.txt":          {},
		"binary.bin":         {0x00, 0x01, 0xff, 0xfe, 0x00},
	}
	writeVersion(t, store, user, site, 1, files)
	writeVersion(t, store, user, site, 2, map[string][]byte{"index.html": []byte("v2")})
	if err := store.SetCurrentVersion(user, site, 2); err != nil {
		t.Fatalf("set current: %v", err)
	}

	versionPath := filepath.Join(base, user, site, "v1")
	before := readTree(t, versionPath)
	if len(before) != len(files) {
		t.Fatalf("setup: read %d files, wrote %d", len(before), len(files))
	}

	if err := store.ArchiveVersion(user, site, 1); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := os.Stat(versionPath); !os.IsNotExist(err) {
		t.Errorf("raw directory still present after archiving: %v", err)
	}
	if _, err := os.Stat(versionPath + ".tar.gz"); err != nil {
		t.Fatalf("archive missing: %v", err)
	}
	if !store.VersionExists(user, site, 1) {
		t.Error("archived version reported as not existing")
	}

	if err := store.MaterializeVersion(user, site, 1); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	after := readTree(t, versionPath)

	if len(after) != len(before) {
		t.Fatalf("file count changed: %d -> %d", len(before), len(after))
	}
	for name, want := range before {
		got, ok := after[name]
		if !ok {
			t.Errorf("file lost in round trip: %s", name)
			continue
		}
		if got != want {
			t.Errorf("content changed for %s", name)
		}
	}
	if _, err := os.Stat(versionPath + ".tar.gz"); !os.IsNotExist(err) {
		t.Errorf("archive still present after materialize: %v", err)
	}
}

// The live version must never be archived — serving follows `current` to a real
// directory.
func TestArchiveRefusesTheLiveVersion(t *testing.T) {
	store, base := newArchiveTestStorage(t)
	const user, site = "alice", "portfolio"

	writeVersion(t, store, user, site, 1, map[string][]byte{"index.html": []byte("live")})
	if err := store.SetCurrentVersion(user, site, 1); err != nil {
		t.Fatalf("set current: %v", err)
	}

	if err := store.ArchiveVersion(user, site, 1); err == nil {
		t.Fatal("archived the live version")
	}
	if _, err := os.Stat(filepath.Join(base, user, site, "v1")); err != nil {
		t.Errorf("live version damaged by refused archive: %v", err)
	}
}

// The failed-deploy window: HideCurrent renames `current` to an opaque token and
// releases the site lock, so for a while no `current` exists even though a
// version is about to be restored as live. Archiving it here would leave the
// rollback pointing at a directory that no longer exists — the site goes down.
func TestArchiveRefusesAVersionHeldOnlyByAHiddenCurrentToken(t *testing.T) {
	store, base := newArchiveTestStorage(t)
	const user, site = "alice", "portfolio"

	writeVersion(t, store, user, site, 1, map[string][]byte{"index.html": []byte("live")})
	if err := store.SetCurrentVersion(user, site, 1); err != nil {
		t.Fatalf("set current: %v", err)
	}

	token, err := store.HideCurrent(user, site)
	if err != nil {
		t.Fatalf("hide current: %v", err)
	}
	if token == "" {
		t.Fatal("expected a rollback token")
	}
	if _, err := os.Lstat(filepath.Join(base, user, site, "current")); !os.IsNotExist(err) {
		t.Fatalf("precondition: current should be hidden, got %v", err)
	}

	if err := store.ArchiveVersion(user, site, 1); err == nil {
		t.Error("archived a version still referenced by a hidden rollback token")
	}

	// The rollback must still work, which is the whole point of refusing.
	if err := store.RestoreCurrent(user, site, token); err != nil {
		t.Fatalf("restore current: %v", err)
	}
	root, err := store.OpenCurrent(user, site)
	if err != nil {
		t.Fatalf("site not serving after restore: %v", err)
	}
	_ = root.Close()
}

// The user-facing promise: a version that has been compressed can still be
// rolled back to, and the site then serves exactly that version's content.
func TestRollbackToAnArchivedVersionServesItsContent(t *testing.T) {
	store, base := newArchiveTestStorage(t)
	const user, site = "alice", "portfolio"

	writeVersion(t, store, user, site, 1, map[string][]byte{
		"index.html":     []byte("<h1>version one</h1>"),
		"assets/one.css": []byte("body{color:red}"),
	})
	writeVersion(t, store, user, site, 2, map[string][]byte{"index.html": []byte("<h1>version two</h1>")})
	if err := store.SetCurrentVersion(user, site, 2); err != nil {
		t.Fatalf("set current to v2: %v", err)
	}

	if err := store.ArchiveVersion(user, site, 1); err != nil {
		t.Fatalf("archive v1: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, user, site, "v1.tar.gz")); err != nil {
		t.Fatalf("v1 should be archived: %v", err)
	}

	// Roll back to the archived version, exactly as the rollback handler does.
	if err := store.SetCurrentVersion(user, site, 1); err != nil {
		t.Fatalf("rollback to archived v1: %v", err)
	}

	root, err := store.OpenCurrent(user, site)
	if err != nil {
		t.Fatalf("site not serving after rollback: %v", err)
	}
	defer root.Close()

	data, err := os.ReadFile(filepath.Join(base, user, site, "v1", "index.html"))
	if err != nil {
		t.Fatalf("read served index: %v", err)
	}
	if string(data) != "<h1>version one</h1>" {
		t.Errorf("served the wrong content after rollback: %q", data)
	}
	if _, err := os.Stat(filepath.Join(base, user, site, "v1", "assets", "one.css")); err != nil {
		t.Errorf("nested asset missing after rollback: %v", err)
	}
	// Rolling back materializes it; it must be a real directory again, since
	// serving follows `current` to a directory.
	if _, err := os.Stat(filepath.Join(base, user, site, "v1.tar.gz")); !os.IsNotExist(err) {
		t.Errorf("archive should be gone after rollback: %v", err)
	}
}

// Deleting must work regardless of which form the version is stored in,
// otherwise the 5-version retention prune leaks archives forever.
func TestDeleteRemovesAnArchivedVersion(t *testing.T) {
	store, base := newArchiveTestStorage(t)
	const user, site = "alice", "portfolio"

	writeVersion(t, store, user, site, 1, map[string][]byte{"index.html": []byte("old")})
	writeVersion(t, store, user, site, 2, map[string][]byte{"index.html": []byte("new")})
	if err := store.SetCurrentVersion(user, site, 2); err != nil {
		t.Fatalf("set current: %v", err)
	}
	if err := store.ArchiveVersion(user, site, 1); err != nil {
		t.Fatalf("archive: %v", err)
	}

	if err := store.DeleteVersion(user, site, 1); err != nil {
		t.Fatalf("delete archived version: %v", err)
	}
	if store.VersionExists(user, site, 1) {
		t.Error("version still exists after delete")
	}
	if _, err := os.Stat(filepath.Join(base, user, site, "v1.tar.gz")); !os.IsNotExist(err) {
		t.Errorf("archive left behind after delete: %v", err)
	}
}

// The whole point, end to end: deploying a new version leaves the previous one
// compressed on disk, and the new one serving normally.
func TestDeployingCompressesThePreviousVersion(t *testing.T) {
	store, base := newArchiveTestStorage(t)
	const user, site = "alice", "portfolio"
	sitePath := filepath.Join(base, user, site)

	writeVersion(t, store, user, site, 1, map[string][]byte{"index.html": []byte("one")})
	if err := store.SetCurrentVersion(user, site, 1); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}
	// v1 is live, so nothing is compressed yet.
	if _, err := os.Stat(filepath.Join(sitePath, "v1")); err != nil {
		t.Fatalf("v1 should be a raw directory while live: %v", err)
	}

	writeVersion(t, store, user, site, 2, map[string][]byte{"index.html": []byte("two")})
	if err := store.SetCurrentVersion(user, site, 2); err != nil {
		t.Fatalf("deploy v2: %v", err)
	}

	// v1 stopped being served, so it should now be a single compressed file.
	if _, err := os.Stat(filepath.Join(sitePath, "v1")); !os.IsNotExist(err) {
		t.Errorf("v1 directory should be gone after being superseded: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sitePath, "v1.tar.gz")); err != nil {
		t.Errorf("v1 should be compressed after being superseded: %v", err)
	}
	if !store.VersionExists(user, site, 1) {
		t.Error("superseded version should still exist for rollback")
	}

	// v2 is live and must be untouched and serving.
	if _, err := os.Stat(filepath.Join(sitePath, "v2")); err != nil {
		t.Errorf("live version must stay raw: %v", err)
	}
	root, err := store.OpenCurrent(user, site)
	if err != nil {
		t.Fatalf("site not serving after deploy: %v", err)
	}
	_ = root.Close()

	// And rolling back still works, which is why the old version was kept.
	if err := store.SetCurrentVersion(user, site, 1); err != nil {
		t.Fatalf("rollback to compressed v1: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(sitePath, "v1", "index.html"))
	if err != nil || string(data) != "one" {
		t.Errorf("rollback produced wrong content: %q, err=%v", data, err)
	}
	// v2 has now been superseded in turn, so it should be compressed.
	if _, err := os.Stat(filepath.Join(sitePath, "v2.tar.gz")); err != nil {
		t.Errorf("v2 should be compressed after rollback superseded it: %v", err)
	}
}

// A version being downloaded right now must not be yanked out from under the
// reader; compression is best effort and skips it.
func TestDeployDoesNotCompressAVersionBeingDownloaded(t *testing.T) {
	store, base := newArchiveTestStorage(t)
	const user, site = "alice", "portfolio"

	writeVersion(t, store, user, site, 1, map[string][]byte{"index.html": []byte("one")})
	if err := store.SetCurrentVersion(user, site, 1); err != nil {
		t.Fatalf("deploy v1: %v", err)
	}

	lease, err := store.LeaseVersion(user, site, 1)
	if err != nil {
		t.Fatalf("lease v1: %v", err)
	}

	writeVersion(t, store, user, site, 2, map[string][]byte{"index.html": []byte("two")})
	if err := store.SetCurrentVersion(user, site, 2); err != nil {
		t.Fatalf("deploy v2 while v1 is leased: %v", err)
	}

	// The download is still in progress, so v1 must still be readable as a
	// directory rather than having been compressed away.
	if _, err := os.Stat(filepath.Join(base, user, site, "v1")); err != nil {
		t.Errorf("leased version was compressed while being read: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("close lease: %v", err)
	}
}

// A version nobody is serving is the normal case and must archive cleanly.
func TestArchiveAcceptsANonLiveVersion(t *testing.T) {
	store, _ := newArchiveTestStorage(t)
	const user, site = "alice", "portfolio"

	writeVersion(t, store, user, site, 1, map[string][]byte{"index.html": []byte("old")})
	writeVersion(t, store, user, site, 2, map[string][]byte{"index.html": []byte("new")})
	if err := store.SetCurrentVersion(user, site, 2); err != nil {
		t.Fatalf("set current: %v", err)
	}

	if err := store.ArchiveVersion(user, site, 1); err != nil {
		t.Fatalf("archive non-live version: %v", err)
	}
	if !store.VersionExists(user, site, 1) {
		t.Error("archived version should still exist")
	}
	// Serving is unaffected.
	root, err := store.OpenCurrent(user, site)
	if err != nil {
		t.Fatalf("serving broken after archiving a different version: %v", err)
	}
	_ = root.Close()
}
