package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDiskStorageWritePublishAndOpenCurrent(t *testing.T) {
	store, base := newTestDiskStorage(t)
	files := map[string][]byte{
		"index.html":        []byte("home"),
		"assets/app.js":     []byte("app"),
		"downloads/app.dmg": {0, 1, 2, 3},
	}
	if err := store.WriteFiles("alice", "demo", 1, files); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}
	if err := store.SetCurrentVersion("alice", "demo", 1); err != nil {
		t.Fatalf("SetCurrentVersion: %v", err)
	}

	root, err := store.OpenCurrent("alice", "demo")
	if err != nil {
		t.Fatalf("OpenCurrent: %v", err)
	}
	defer root.Close()
	for name, want := range files {
		got, err := fs.ReadFile(root.FS(), name)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("ReadFile(%q) = %q, want %q", name, got, want)
		}
	}

	target, err := os.Readlink(filepath.Join(base, "alice", "demo", "current"))
	if err != nil {
		t.Fatalf("Readlink(current): %v", err)
	}
	if target != "v1" {
		t.Fatalf("current -> %q, want v1", target)
	}
	entries, err := os.ReadDir(filepath.Join(base, "alice", "demo"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".stage-") {
			t.Fatalf("staging directory was not cleaned: %s", entry.Name())
		}
	}
}

func TestDiskStorageRejectsInvalidInputs(t *testing.T) {
	store, base := newTestDiskStorage(t)
	invalidSegments := []string{"", ".", "..", "../escape", "a/b", `a\b`, "a\x00b"}
	for _, segment := range invalidSegments {
		if err := store.WriteFiles(segment, "demo", 1, map[string][]byte{"index.html": []byte("x")}); err == nil {
			t.Errorf("WriteFiles accepted user %q", segment)
		}
		if err := store.WriteFiles("alice", segment, 1, map[string][]byte{"index.html": []byte("x")}); err == nil {
			t.Errorf("WriteFiles accepted site %q", segment)
		}
		if _, err := store.OpenCurrent(segment, "demo"); err == nil {
			t.Errorf("OpenCurrent accepted user %q", segment)
		}
		if _, err := store.OpenVersion(segment, "demo", 1); err == nil {
			t.Errorf("OpenVersion accepted user %q", segment)
		}
		if _, err := store.OpenVersion("alice", segment, 1); err == nil {
			t.Errorf("OpenVersion accepted site %q", segment)
		}
		if err := store.DeleteSite("alice", segment); err == nil {
			t.Errorf("DeleteSite accepted site %q", segment)
		}
		if _, err := store.UserDirExists(segment); err == nil {
			t.Errorf("UserDirExists accepted user %q", segment)
		}
	}
	for _, name := range []string{"../escape", "/absolute", `a\b`, "./index.html", "a/../b", "a\x00b"} {
		if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{name: []byte("x")}); err == nil {
			t.Errorf("WriteFiles accepted relative path %q", name)
		}
	}
	if err := store.WriteFiles("alice", "demo", 0, nil); err == nil {
		t.Error("WriteFiles accepted version 0")
	}
	if store.VersionDirExists("alice", "demo", 0) {
		t.Error("VersionDirExists accepted version 0")
	}
	if _, err := store.OpenVersion("alice", "demo", 0); err == nil {
		t.Error("OpenVersion accepted version 0")
	}
	if _, err := store.OpenVersion("alice", "missing", 1); err == nil {
		t.Error("OpenVersion accepted a missing site/version")
	}
	if _, err := os.Stat(filepath.Join(base, "escape")); !os.IsNotExist(err) {
		t.Fatalf("invalid paths created escape target: %v", err)
	}
}

func TestDiskStorageNeverReplacesPublishedVersion(t *testing.T) {
	store, base := newTestDiskStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("original")}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("replacement")}); err == nil {
		t.Fatal("second WriteFiles unexpectedly replaced v1")
	}
	content, err := os.ReadFile(filepath.Join(base, "alice", "demo", "v1", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "original" {
		t.Fatalf("published content changed to %q", content)
	}
}

func TestDiskStorageRejectsFileDirectoryConflicts(t *testing.T) {
	store, base := newTestDiskStorage(t)
	err := store.WriteFiles("alice", "demo", 1, map[string][]byte{
		"assets":        []byte("file"),
		"assets/app.js": []byte("child"),
	})
	if err == nil {
		t.Fatal("WriteFiles accepted a file/directory prefix conflict")
	}
	if _, statErr := os.Stat(filepath.Join(base, "alice", "demo", "v1")); !os.IsNotExist(statErr) {
		t.Fatalf("failed write published v1: %v", statErr)
	}
	entries, readErr := os.ReadDir(filepath.Join(base, "alice", "demo"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".stage-") {
			t.Fatalf("failed write left staging directory %q", entry.Name())
		}
	}
}

func TestDiskStorageRejectsStructuralSymlinks(t *testing.T) {
	t.Run("user", func(t *testing.T) {
		store, base := newTestDiskStorage(t)
		outside := t.TempDir()
		writeSentinel(t, outside)
		if err := os.Symlink(outside, filepath.Join(base, "alice")); err != nil {
			t.Fatal(err)
		}
		if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("x")}); err == nil {
			t.Fatal("WriteFiles followed user symlink")
		}
		if err := store.DeleteSite("alice", "demo"); err == nil {
			t.Fatal("DeleteSite accepted user symlink")
		}
		if root, err := store.OpenVersion("alice", "demo", 1); err == nil {
			root.Close()
			t.Fatal("OpenVersion followed user symlink")
		}
		assertSentinel(t, outside)
	})

	t.Run("site", func(t *testing.T) {
		store, base := newTestDiskStorage(t)
		outside := t.TempDir()
		writeSentinel(t, outside)
		if err := os.Mkdir(filepath.Join(base, "alice"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(base, "alice", "demo")); err != nil {
			t.Fatal(err)
		}
		if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("x")}); err == nil {
			t.Fatal("WriteFiles followed site symlink")
		}
		if err := store.DeleteSite("alice", "demo"); err == nil {
			t.Fatal("DeleteSite accepted site symlink")
		}
		if root, err := store.OpenVersion("alice", "demo", 1); err == nil {
			root.Close()
			t.Fatal("OpenVersion followed site symlink")
		}
		assertSentinel(t, outside)
	})

	t.Run("version", func(t *testing.T) {
		store, base := newTestDiskStorage(t)
		outside := t.TempDir()
		writeSentinel(t, outside)
		sitePath := filepath.Join(base, "alice", "demo")
		if err := os.MkdirAll(sitePath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(sitePath, "v1")); err != nil {
			t.Fatal(err)
		}
		if store.VersionDirExists("alice", "demo", 1) {
			t.Fatal("VersionDirExists accepted version symlink")
		}
		if err := store.SetCurrentVersion("alice", "demo", 1); err == nil {
			t.Fatal("SetCurrentVersion accepted version symlink")
		}
		if err := store.DeleteVersion("alice", "demo", 1); err == nil {
			t.Fatal("DeleteVersion accepted version symlink")
		}
		if root, err := store.OpenVersion("alice", "demo", 1); err == nil {
			root.Close()
			t.Fatal("OpenVersion followed version symlink")
		}
		assertSentinel(t, outside)
	})
}

func TestDiskStorageAuthorizesExactCurrentLink(t *testing.T) {
	unsafeTargets := []string{"../other/v1", "/tmp/v1", "v01", "v0", "v2/../v1"}
	for _, target := range unsafeTargets {
		t.Run(strings.ReplaceAll(target, "/", "_"), func(t *testing.T) {
			store, base := newTestDiskStorage(t)
			if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("demo")}); err != nil {
				t.Fatal(err)
			}
			current := filepath.Join(base, "alice", "demo", "current")
			if err := os.Symlink(target, current); err != nil {
				t.Fatal(err)
			}
			if root, err := store.OpenCurrent("alice", "demo"); err == nil {
				root.Close()
				t.Fatalf("OpenCurrent accepted target %q", target)
			}
			if err := store.SetCurrentVersion("alice", "demo", 1); err == nil {
				t.Fatalf("SetCurrentVersion replaced unsafe target %q", target)
			}
		})
	}
}

func TestDiskStorageRootFSRejectsSymlinkEscape(t *testing.T) {
	store, base := newTestDiskStorage(t)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("demo")}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentVersion("alice", "demo", 1); err != nil {
		t.Fatal(err)
	}
	versionDir := filepath.Join(base, "alice", "demo", "v1")
	relativeTarget, err := filepath.Rel(versionDir, outsideFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relativeTarget, filepath.Join(versionDir, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	root, err := store.OpenCurrent("alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if content, err := fs.ReadFile(root.FS(), "escape.txt"); err == nil {
		t.Fatalf("rooted FS read outside content %q", content)
	}
}

func TestDiskStorageOpenVersionIsExactAndConfined(t *testing.T) {
	store, base := newTestDiskStorage(t)
	for version, content := range map[int]string{1: "v1", 2: "v2"} {
		if err := store.WriteFiles("alice", "demo", version, map[string][]byte{"index.html": []byte(content)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetCurrentVersion("alice", "demo", 2); err != nil {
		t.Fatal(err)
	}

	root, err := store.OpenVersion("alice", "demo", 1)
	if err != nil {
		t.Fatalf("OpenVersion(v1): %v", err)
	}
	defer root.Close()
	content, err := fs.ReadFile(root.FS(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "v1" {
		t.Fatalf("OpenVersion(v1) read %q while current was v2", content)
	}

	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.html")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	versionDir := filepath.Join(base, "alice", "demo", "v1")
	relativeTarget, err := filepath.Rel(versionDir, outsideFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relativeTarget, filepath.Join(versionDir, "escape.html")); err != nil {
		t.Fatal(err)
	}
	if escaped, err := fs.ReadFile(root.FS(), "escape.html"); err == nil {
		t.Fatalf("OpenVersion root read outside content %q", escaped)
	}
}

func TestDiskStorageOpenVersionNeverInspectsCurrent(t *testing.T) {
	store, base := newTestDiskStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("v1")}); err != nil {
		t.Fatal(err)
	}
	currentPath := filepath.Join(base, "alice", "demo", "current")
	if err := os.Symlink("../unsafe", currentPath); err != nil {
		t.Fatal(err)
	}

	root, err := store.OpenVersion("alice", "demo", 1)
	if err != nil {
		t.Fatalf("OpenVersion inspected unsafe current: %v", err)
	}
	defer root.Close()
	content, err := fs.ReadFile(root.FS(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "v1" {
		t.Fatalf("OpenVersion read %q, want v1", content)
	}
}

func TestDiskStorageOpenVersionHonorsQuarantine(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("v1")}); err != nil {
		t.Fatal(err)
	}
	if err := store.QuarantineCurrentServing("alice", "demo"); err != nil {
		t.Fatal(err)
	}
	if root, err := store.OpenVersion("alice", "demo", 1); !errors.Is(err, ErrSiteCurrentQuarantined) {
		if root != nil {
			root.Close()
		}
		t.Fatalf("OpenVersion quarantine error = %v, want %v", err, ErrSiteCurrentQuarantined)
	}
}

func TestDiskStorageVersionLeaseBlocksVersionDeletionUntilClose(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("v1")}); err != nil {
		t.Fatal(err)
	}

	lease, err := store.LeaseVersion("alice", "demo", 1)
	if err != nil {
		t.Fatalf("LeaseVersion: %v", err)
	}
	content, err := fs.ReadFile(lease.FS(), "index.html")
	if err != nil || string(content) != "v1" {
		t.Fatalf("leased file = %q, err = %v", content, err)
	}

	deleted := make(chan error, 1)
	go func() {
		deleted <- store.DeleteVersion("alice", "demo", 1)
	}()
	select {
	case err := <-deleted:
		t.Fatalf("DeleteVersion completed while lease was open: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	if err := lease.Close(); err != nil {
		t.Fatalf("close lease: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("second close lease: %v", err)
	}
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("DeleteVersion after close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DeleteVersion remained blocked after lease close")
	}
}

func TestDiskStorageVersionLeaseDoesNotBlockOtherSiteMutations(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("v1")}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.LeaseVersion("alice", "demo", 1)
	if err != nil {
		t.Fatalf("LeaseVersion: %v", err)
	}
	defer lease.Close()

	updated := make(chan error, 1)
	go func() {
		updated <- store.WriteFiles("alice", "demo", 2, map[string][]byte{"index.html": []byte("v2")})
	}()
	select {
	case err := <-updated:
		if err != nil {
			t.Fatalf("WriteFiles while older version leased: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("exact version lease blocked a same-site new version write")
	}
}

func TestDiskStorageVersionLeaseBlocksSiteDeletionUntilClose(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("v1")}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.LeaseVersion("alice", "demo", 1)
	if err != nil {
		t.Fatalf("LeaseVersion: %v", err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- store.DeleteSite("alice", "demo") }()
	select {
	case err := <-deleted:
		t.Fatalf("DeleteSite completed while lease was open: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("close lease: %v", err)
	}
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("DeleteSite after close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("DeleteSite remained blocked after lease close")
	}
}

func TestDiskStorageDeleteNeverFollowsInteriorSymlink(t *testing.T) {
	store, base := newTestDiskStorage(t)
	outside := t.TempDir()
	writeSentinel(t, outside)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("demo")}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentVersion("alice", "demo", 1); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(base, "alice", "demo", "v1", "outside")); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSite("alice", "demo"); err != nil {
		t.Fatalf("DeleteSite: %v", err)
	}
	assertSentinel(t, outside)
	if _, err := os.Stat(filepath.Join(base, "alice", "demo")); !os.IsNotExist(err) {
		t.Fatalf("site directory still exists: %v", err)
	}
}

func TestDiskStorageHideAndRestoreCurrent(t *testing.T) {
	store, base := newTestDiskStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("v1")}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentVersion("alice", "demo", 1); err != nil {
		t.Fatal(err)
	}
	if current, exists, err := store.CurrentVersion("alice", "demo"); err != nil || !exists || current != 1 {
		t.Fatalf("CurrentVersion = %d, %t, %v", current, exists, err)
	}

	token, err := store.HideCurrent("alice", "demo")
	if err != nil {
		t.Fatalf("HideCurrent: %v", err)
	}
	if !strings.HasPrefix(token, hiddenCurrentPrefix) {
		t.Fatalf("HideCurrent token = %q", token)
	}
	if _, err := store.OpenCurrent("alice", "demo"); err == nil {
		t.Fatal("hidden site remained publicly openable")
	}
	if _, err := os.Lstat(filepath.Join(base, "alice", "demo", "current")); !os.IsNotExist(err) {
		t.Fatalf("current still exists after hide: %v", err)
	}
	if err := store.RestoreCurrent("alice", "demo", token); err != nil {
		t.Fatalf("RestoreCurrent: %v", err)
	}
	if current, exists, err := store.CurrentVersion("alice", "demo"); err != nil || !exists || current != 1 {
		t.Fatalf("restored CurrentVersion = %d, %t, %v", current, exists, err)
	}
}

func TestDiskStorageRestoreCurrentFailsClosed(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("v1")}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFiles("alice", "demo", 2, map[string][]byte{"index.html": []byte("v2")}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentVersion("alice", "demo", 1); err != nil {
		t.Fatal(err)
	}
	token, err := store.HideCurrent("alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentVersion("alice", "demo", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.RestoreCurrent("alice", "demo", token); err == nil {
		t.Fatal("RestoreCurrent replaced a newer current link")
	}
	if current, exists, err := store.CurrentVersion("alice", "demo"); err != nil || !exists || current != 2 {
		t.Fatalf("CurrentVersion = %d, %t, %v; want v2", current, exists, err)
	}
	for _, invalid := range []string{"", "../current", ".hidden-current-bad", ".hidden-current-00000000000000000000000z"} {
		if err := store.RestoreCurrent("alice", "demo", invalid); err == nil {
			t.Errorf("RestoreCurrent accepted token %q", invalid)
		}
	}
}

func TestDiskStorageRemoveCurrentVersionIsExact(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	for version := 1; version <= 2; version++ {
		if err := store.WriteFiles("alice", "demo", version, map[string][]byte{"index.html": []byte{byte('0' + version)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetCurrentVersion("alice", "demo", 2); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveCurrentVersion("alice", "demo", 1); err == nil {
		t.Fatal("RemoveCurrentVersion removed a different target")
	}
	if current, exists, err := store.CurrentVersion("alice", "demo"); err != nil || !exists || current != 2 {
		t.Fatalf("CurrentVersion = %d, %t, %v; want v2", current, exists, err)
	}
	if err := store.RemoveCurrentVersion("alice", "demo", 2); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.CurrentVersion("alice", "demo"); err != nil || exists {
		t.Fatalf("CurrentVersion exists=%t err=%v after exact removal", exists, err)
	}
	if !store.VersionDirExists("alice", "demo", 2) {
		t.Fatal("RemoveCurrentVersion removed version content")
	}
}

func TestDiskStorageArchiveMaterializeRoundTrip(t *testing.T) {
	store, base := newTestDiskStorage(t)
	files := map[string][]byte{
		"index.html":             []byte("home"),
		"assets/app.js":          []byte("console.log('ok')"),
		"assets/nested/data.bin": {0, 1, 2, 3, 255},
		"empty.txt":              {},
	}
	if err := store.WriteFiles("alice", "demo", 1, files); err != nil {
		t.Fatal(err)
	}
	assertVersionForms(t, base, 1, true, false)
	if err := store.ArchiveVersion("alice", "demo", 1); err != nil {
		t.Fatalf("ArchiveVersion: %v", err)
	}
	assertVersionForms(t, base, 1, false, true)
	if !store.VersionExists("alice", "demo", 1) {
		t.Fatal("VersionExists returned false for archive")
	}
	if store.VersionDirExists("alice", "demo", 1) {
		t.Fatal("VersionDirExists returned true for archive")
	}
	if err := store.MaterializeVersion("alice", "demo", 1); err != nil {
		t.Fatalf("MaterializeVersion: %v", err)
	}
	assertVersionForms(t, base, 1, true, false)

	root, err := store.OpenVersion("alice", "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	got := make(map[string][]byte)
	if err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." || entry.IsDir() {
			return nil
		}
		content, err := fs.ReadFile(root.FS(), name)
		if err != nil {
			return err
		}
		got[name] = content
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(files) {
		t.Fatalf("materialized file count = %d, want %d", len(got), len(files))
	}
	for name, want := range files {
		if !bytes.Equal(got[name], want) {
			t.Errorf("materialized %q = %v, want %v", name, got[name], want)
		}
	}
}

func TestDiskStorageArchiveRefusesServingLinks(t *testing.T) {
	tests := []struct {
		name string
		hide bool
	}{
		{name: "current"},
		{name: "hidden current", hide: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, base := newTestDiskStorage(t)
			if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("live")}); err != nil {
				t.Fatal(err)
			}
			if err := store.SetCurrentVersion("alice", "demo", 1); err != nil {
				t.Fatal(err)
			}
			if test.hide {
				token, err := store.HideCurrent("alice", "demo")
				if err != nil {
					t.Fatal(err)
				}
				if token == "" {
					t.Fatal("HideCurrent returned no token")
				}
				if _, err := os.Lstat(filepath.Join(base, "alice", "demo", "current")); !os.IsNotExist(err) {
					t.Fatalf("current exists in hidden-current window: %v", err)
				}
			}
			if err := store.ArchiveVersion("alice", "demo", 1); err == nil {
				t.Fatal("ArchiveVersion archived a serving-link target")
			}
			assertVersionForms(t, base, 1, true, false)
			content, err := os.ReadFile(filepath.Join(base, "alice", "demo", "v1", "index.html"))
			if err != nil || string(content) != "live" {
				t.Fatalf("live raw version changed: %q, %v", content, err)
			}
		})
	}
}

func TestDiskStorageArchivedTargetNoOpsAndVersionExists(t *testing.T) {
	store, base := newTestDiskStorage(t)
	if store.VersionExists("alice", "demo", 1) {
		t.Fatal("VersionExists returned true for absent version")
	}
	if store.VersionExists("alice", "demo", 0) {
		t.Fatal("VersionExists accepted invalid version")
	}
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("v1")}); err != nil {
		t.Fatal(err)
	}
	if !store.VersionExists("alice", "demo", 1) {
		t.Fatal("VersionExists returned false for raw version")
	}
	if err := store.MaterializeVersion("alice", "demo", 1); err != nil {
		t.Fatalf("MaterializeVersion raw no-op: %v", err)
	}
	assertVersionForms(t, base, 1, true, false)
	if err := store.ArchiveVersion("alice", "demo", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveVersion("alice", "demo", 1); err != nil {
		t.Fatalf("ArchiveVersion archived no-op: %v", err)
	}
	assertVersionForms(t, base, 1, false, true)
}

func TestDiskStorageMaterializeRejectsCorruptAndTraversalArchives(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		store, base := newTestDiskStorage(t)
		if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("keep")}); err != nil {
			t.Fatal(err)
		}
		if err := store.ArchiveVersion("alice", "demo", 1); err != nil {
			t.Fatal(err)
		}
		archivePath := filepath.Join(base, "alice", "demo", "v1.tar.gz")
		archive, err := os.ReadFile(archivePath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(archivePath, archive[:len(archive)/2], 0o600); err != nil {
			t.Fatal(err)
		}
		if err := store.MaterializeVersion("alice", "demo", 1); err == nil {
			t.Fatal("MaterializeVersion accepted truncated archive")
		}
		assertVersionForms(t, base, 1, false, true)
		if !store.VersionExists("alice", "demo", 1) {
			t.Fatal("failed materialization lost the archived version")
		}
	})

	t.Run("traversal", func(t *testing.T) {
		store, base := newTestDiskStorage(t)
		sitePath := filepath.Join(base, "alice", "demo")
		if err := os.MkdirAll(sitePath, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestTarGz(t, filepath.Join(sitePath, "v1.tar.gz"), map[string][]byte{"../escape": []byte("bad")})
		if err := store.MaterializeVersion("alice", "demo", 1); err == nil {
			t.Fatal("MaterializeVersion accepted traversal path")
		}
		if _, err := os.Stat(filepath.Join(sitePath, "escape")); !os.IsNotExist(err) {
			t.Fatalf("traversal escaped temporary extraction root: %v", err)
		}
		if _, err := os.Stat(filepath.Join(base, "alice", "escape")); !os.IsNotExist(err) {
			t.Fatalf("traversal wrote outside site: %v", err)
		}
		if _, err := os.Stat(filepath.Join(base, "escape")); !os.IsNotExist(err) {
			t.Fatalf("traversal wrote outside user: %v", err)
		}
		assertVersionForms(t, base, 1, false, true)
	})
}

func TestDiskStorageArchiveVerificationFailurePreservesRaw(t *testing.T) {
	store, base := newTestDiskStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("known-good")}); err != nil {
		t.Fatal(err)
	}
	siteRoot, err := store.openSite("alice", "demo", false)
	if err != nil {
		t.Fatal(err)
	}
	defer siteRoot.Close()
	rawRoot, err := openRealDir(siteRoot, "v1", false)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := directoryRegularStats(rawRoot)
	if err != nil {
		rawRoot.Close()
		t.Fatal(err)
	}
	if err := rawRoot.Close(); err != nil {
		t.Fatal(err)
	}
	temporary := ".tmp-v1-corrupt-test"
	temporaryFile, err := siteRoot.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temporaryFile.Write([]byte("truncated gzip")); err != nil {
		temporaryFile.Close()
		t.Fatal(err)
	}
	if err := publishVerifiedArchive(siteRoot, temporary, "v1.tar.gz", "v1", expected, temporaryFile); err == nil {
		t.Fatal("publishVerifiedArchive accepted corrupt temporary archive")
	}
	assertVersionForms(t, base, 1, true, false)
	content, err := os.ReadFile(filepath.Join(base, "alice", "demo", "v1", "index.html"))
	if err != nil || string(content) != "known-good" {
		t.Fatalf("verification failure changed raw version: %q, %v", content, err)
	}
	if _, err := siteRoot.Lstat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verification failure left temporary archive: %v", err)
	}
}

func TestRetireRawVersionCleanupFailuresAreBestEffort(t *testing.T) {
	files := map[string][]byte{
		"first.txt":         []byte("first"),
		"nested/second.txt": []byte("second"),
	}
	setup := func(t *testing.T) (*os.Root, string) {
		t.Helper()
		sitePath := t.TempDir()
		for name, content := range files {
			filename := filepath.Join(sitePath, "v1", filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filename, content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		writeTestTarGz(t, filepath.Join(sitePath, "v1.tar.gz"), files)
		root, err := os.OpenRoot(sitePath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = root.Close() })
		return root, sitePath
	}

	t.Run("directory sync failure preserves complete transient tree", func(t *testing.T) {
		root, sitePath := setup(t)
		syncCalls := 0
		removeCalls := 0
		err := retireRawVersionWithCleanup(
			root,
			"v1",
			func(*os.Root) error {
				syncCalls++
				return errors.New("injected directory sync failure")
			},
			func(*os.Root, string) error {
				removeCalls++
				return nil
			},
		)
		if err != nil {
			t.Fatalf("retireRawVersionWithCleanup: %v", err)
		}
		if syncCalls != 1 || removeCalls != 0 {
			t.Fatalf("cleanup calls sync=%d remove=%d, want sync=1 remove=0", syncCalls, removeCalls)
		}
		assertRetiredRawTree(t, root, sitePath, files)
	})

	t.Run("partial removal failure never restores canonical raw", func(t *testing.T) {
		root, sitePath := setup(t)
		syncCalls := 0
		removeCalls := 0
		err := retireRawVersionWithCleanup(
			root,
			"v1",
			func(*os.Root) error {
				syncCalls++
				return nil
			},
			func(root *os.Root, temporary string) error {
				removeCalls++
				if err := root.Remove(temporary + "/first.txt"); err != nil {
					return err
				}
				return errors.New("injected recursive removal failure")
			},
		)
		if err != nil {
			t.Fatalf("retireRawVersionWithCleanup: %v", err)
		}
		if syncCalls != 1 || removeCalls != 1 {
			t.Fatalf("cleanup calls sync=%d remove=%d, want sync=1 remove=1", syncCalls, removeCalls)
		}
		if _, err := root.Lstat("v1"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("canonical raw version was restored after partial cleanup: %v", err)
		}
		temporary := retiredRawTreeName(t, sitePath)
		if _, err := root.Lstat(temporary + "/first.txt"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("injected cleanup did not remove first entry: %v", err)
		}
		got, err := os.ReadFile(filepath.Join(sitePath, temporary, "nested", "second.txt"))
		if err != nil || !bytes.Equal(got, files["nested/second.txt"]) {
			t.Fatalf("remaining transient content = %q, %v", got, err)
		}
		assertValidTestArchive(t, root, files)
	})
}

func assertRetiredRawTree(t *testing.T, root *os.Root, sitePath string, files map[string][]byte) {
	t.Helper()
	if _, err := root.Lstat("v1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical raw version still exists: %v", err)
	}
	temporary := retiredRawTreeName(t, sitePath)
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(sitePath, temporary, filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("transient raw content %q = %q, %v; want %q", name, got, err, want)
		}
	}
	assertValidTestArchive(t, root, files)
}

func retiredRawTreeName(t *testing.T, sitePath string) string {
	t.Helper()
	entries, err := os.ReadDir(sitePath)
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".tmp-remove-v1-") {
			continue
		}
		if found != "" {
			t.Fatalf("multiple retired raw trees: %q and %q", found, entry.Name())
		}
		found = entry.Name()
	}
	if found == "" {
		t.Fatal("retired raw tree not found")
	}
	return found
}

func assertValidTestArchive(t *testing.T, root *os.Root, files map[string][]byte) {
	t.Helper()
	stats, err := readArchiveStats(root, "v1.tar.gz")
	if err != nil {
		t.Fatalf("read authoritative archive: %v", err)
	}
	var want regularFileStats
	for _, content := range files {
		if err := want.add(int64(len(content))); err != nil {
			t.Fatal(err)
		}
	}
	if stats != want {
		t.Fatalf("archive stats = %+v, want %+v", stats, want)
	}
}

func TestDiskStorageArchiveWaitsForVersionLease(t *testing.T) {
	store, base := newTestDiskStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("leased")}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.LeaseVersion("alice", "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	archived := make(chan error, 1)
	go func() { archived <- store.ArchiveVersion("alice", "demo", 1) }()
	select {
	case err := <-archived:
		t.Fatalf("ArchiveVersion completed while lease was open: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	content, err := fs.ReadFile(lease.FS(), "index.html")
	if err != nil || string(content) != "leased" {
		t.Fatalf("leased content changed: %q, %v", content, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-archived:
		if err != nil {
			t.Fatalf("ArchiveVersion after lease close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ArchiveVersion remained blocked after lease close")
	}
	assertVersionForms(t, base, 1, false, true)
}

func TestDiskStorageArchiveRecoversBothForms(t *testing.T) {
	tests := []struct {
		name       string
		archiveFor map[string][]byte
	}{
		{name: "matching", archiveFor: map[string][]byte{"index.html": []byte("raw-v1"), "empty": {}}},
		{name: "mismatched", archiveFor: map[string][]byte{"other.html": []byte("different-size-and-set")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, base := newTestDiskStorage(t)
			rawFiles := map[string][]byte{"index.html": []byte("raw-v1"), "empty": {}}
			if err := store.WriteFiles("alice", "demo", 1, rawFiles); err != nil {
				t.Fatal(err)
			}
			writeTestTarGz(t, filepath.Join(base, "alice", "demo", "v1.tar.gz"), test.archiveFor)
			assertVersionForms(t, base, 1, true, true)
			if err := store.ArchiveVersion("alice", "demo", 1); err != nil {
				t.Fatalf("ArchiveVersion recovery: %v", err)
			}
			assertVersionForms(t, base, 1, false, true)
			if err := store.MaterializeVersion("alice", "demo", 1); err != nil {
				t.Fatalf("MaterializeVersion recovered archive: %v", err)
			}
			assertVersionForms(t, base, 1, true, false)
			for name, want := range rawFiles {
				got, err := os.ReadFile(filepath.Join(base, "alice", "demo", "v1", filepath.FromSlash(name)))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("recovered %q = %v, %v; want %v", name, got, err, want)
				}
			}
		})
	}
}

func assertVersionForms(t *testing.T, base string, version int, wantRaw, wantArchive bool) {
	t.Helper()
	sitePath := filepath.Join(base, "alice", "demo")
	rawInfo, rawErr := os.Lstat(filepath.Join(sitePath, "v"+strconv.Itoa(version)))
	archiveInfo, archiveErr := os.Lstat(filepath.Join(sitePath, "v"+strconv.Itoa(version)+".tar.gz"))
	gotRaw := rawErr == nil && rawInfo.IsDir() && rawInfo.Mode()&os.ModeSymlink == 0
	gotArchive := archiveErr == nil && archiveInfo.Mode().IsRegular() && archiveInfo.Mode()&os.ModeSymlink == 0
	if gotRaw != wantRaw || gotArchive != wantArchive {
		t.Fatalf("version forms raw=%t archive=%t, want raw=%t archive=%t (errors: %v, %v)", gotRaw, gotArchive, wantRaw, wantArchive, rawErr, archiveErr)
	}
	entries, err := os.ReadDir(sitePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("completed operation left transient entry %q", entry.Name())
		}
	}
}

func writeTestTarGz(t *testing.T, filename string, files map[string][]byte) {
	t.Helper()
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for name, content := range files {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func newTestDiskStorage(t *testing.T) (*DiskStorage, string) {
	t.Helper()
	base := t.TempDir()
	store, err := NewDiskStorage(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store, base
}

func writeSentinel(t *testing.T, directory string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, "sentinel.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertSentinel(t *testing.T, directory string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(directory, "sentinel.txt"))
	if err != nil {
		t.Fatalf("outside sentinel missing: %v", err)
	}
	if string(content) != "keep" {
		t.Fatalf("outside sentinel changed to %q", content)
	}
}

func TestDiskStorageListUsersSkipsLinksAndFiles(t *testing.T) {
	store, base := newTestDiskStorage(t)
	for _, user := range []string{"alice", "alice.b", "alice-b"} {
		if err := store.WriteFiles(user, "demo", 1, map[string][]byte{"index.html": []byte(user)}); err != nil {
			t.Fatalf("WriteFiles(%s): %v", user, err)
		}
	}
	// A symlink to a user directory, a plain file, and a directory that is
	// not a valid segment must all be left out.
	if err := os.Symlink("alice", filepath.Join(base, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, strings.Repeat("a", 300)), 0o755); err != nil && !errors.Is(err, os.ErrInvalid) {
		// Some filesystems refuse a 300-byte name outright; that is fine.
		t.Logf("long directory name not created: %v", err)
	}

	users, err := store.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	sort.Strings(users)
	if got, want := strings.Join(users, ","), "alice,alice-b,alice.b"; got != want {
		t.Fatalf("ListUsers = %q, want %q", got, want)
	}
}
