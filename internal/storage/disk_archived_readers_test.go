package storage

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDiskStorageArchivedVersionReadersMaterializePermanently(t *testing.T) {
	files := map[string][]byte{
		"index.html":    []byte("archived home"),
		"assets/app.js": []byte("console.log('archived')"),
	}

	t.Run("SetCurrentVersion", func(t *testing.T) {
		store, base := newTestDiskStorage(t)
		writeArchivedReaderVersion(t, store, 1, files)
		if err := store.WriteFiles("alice", "demo", 2, map[string][]byte{"index.html": []byte("current")}); err != nil {
			t.Fatal(err)
		}
		if err := store.SetCurrentVersion("alice", "demo", 2); err != nil {
			t.Fatal(err)
		}
		if err := store.ArchiveVersion("alice", "demo", 1); err != nil {
			t.Fatal(err)
		}
		assertVersionForms(t, base, 1, false, true)

		done := make(chan error, 1)
		go func() { done <- store.SetCurrentVersion("alice", "demo", 1) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("SetCurrentVersion archived target: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("SetCurrentVersion deadlocked while materializing archived target")
		}
		assertVersionForms(t, base, 1, true, false)
		if current, exists, err := store.CurrentVersion("alice", "demo"); err != nil || !exists || current != 1 {
			t.Fatalf("CurrentVersion = %d, %t, %v; want v1", current, exists, err)
		}
		root, err := store.OpenCurrent("alice", "demo")
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		assertRootFiles(t, root, files)
	})

	t.Run("OpenVersion", func(t *testing.T) {
		store, base := newTestDiskStorage(t)
		writeArchivedReaderVersion(t, store, 1, files)
		if err := store.ArchiveVersion("alice", "demo", 1); err != nil {
			t.Fatal(err)
		}

		root, err := store.OpenVersion("alice", "demo", 1)
		if err != nil {
			t.Fatalf("OpenVersion archived target: %v", err)
		}
		defer root.Close()
		assertRootFiles(t, root, files)
		assertVersionForms(t, base, 1, true, false)
	})
}

func TestDiskStorageLeaseArchivedVersionMatchesRawBytes(t *testing.T) {
	store, base := newTestDiskStorage(t)
	files := map[string][]byte{
		"index.html":      []byte("same bytes"),
		"assets/data.bin": {0, 1, 2, 127, 128, 255},
	}
	writeArchivedReaderVersion(t, store, 1, files)

	rawLease, err := store.LeaseVersion("alice", "demo", 1)
	if err != nil {
		t.Fatal(err)
	}
	rawBytes, err := fs.ReadFile(rawLease.FS(), "assets/data.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err := rawLease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveVersion("alice", "demo", 1); err != nil {
		t.Fatal(err)
	}
	assertVersionForms(t, base, 1, false, true)

	type leaseResult struct {
		lease *VersionLease
		err   error
	}
	done := make(chan leaseResult, 1)
	go func() {
		lease, err := store.LeaseVersion("alice", "demo", 1)
		done <- leaseResult{lease: lease, err: err}
	}()
	var archivedLease *VersionLease
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("LeaseVersion archived target: %v", result.err)
		}
		archivedLease = result.lease
	case <-time.After(2 * time.Second):
		t.Fatal("LeaseVersion deadlocked while materializing archived target")
	}
	defer archivedLease.Close()
	archivedBytes, err := fs.ReadFile(archivedLease.FS(), "assets/data.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(archivedBytes, rawBytes) {
		t.Fatalf("archived lease bytes = %v, want raw bytes %v", archivedBytes, rawBytes)
	}
	assertRootFiles(t, archivedLease.root, files)
	assertVersionForms(t, base, 1, true, false)
}

func TestDiskStorageDeleteVersionRemovesEveryDurableForm(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *DiskStorage, string)
	}{
		{
			name: "raw",
			setup: func(t *testing.T, store *DiskStorage, _ string) {
				writeArchivedReaderVersion(t, store, 1, map[string][]byte{"index.html": []byte("raw")})
			},
		},
		{
			name: "archive",
			setup: func(t *testing.T, store *DiskStorage, _ string) {
				writeArchivedReaderVersion(t, store, 1, map[string][]byte{"index.html": []byte("archive")})
				if err := store.ArchiveVersion("alice", "demo", 1); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "both forms",
			setup: func(t *testing.T, store *DiskStorage, base string) {
				writeArchivedReaderVersion(t, store, 1, map[string][]byte{"index.html": []byte("raw-authoritative")})
				writeTestTarGz(t, filepath.Join(base, "alice", "demo", "v1.tar.gz"), map[string][]byte{"index.html": []byte("stale-archive")})
			},
		},
		{
			name: "missing",
			setup: func(t *testing.T, store *DiskStorage, _ string) {
				writeArchivedReaderVersion(t, store, 2, map[string][]byte{"index.html": []byte("other")})
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, base := newTestDiskStorage(t)
			test.setup(t, store, base)
			if err := store.DeleteVersion("alice", "demo", 1); err != nil {
				t.Fatalf("DeleteVersion: %v", err)
			}
			for _, name := range []string{"v1", "v1.tar.gz"} {
				if _, err := os.Lstat(filepath.Join(base, "alice", "demo", name)); !os.IsNotExist(err) {
					t.Fatalf("%s remains after deletion: %v", name, err)
				}
			}
			if store.VersionExists("alice", "demo", 1) {
				t.Fatal("VersionExists returned true after deletion")
			}
		})
	}
}

func writeArchivedReaderVersion(t *testing.T, store *DiskStorage, version int, files map[string][]byte) {
	t.Helper()
	if err := store.WriteFiles("alice", "demo", version, files); err != nil {
		t.Fatalf("WriteFiles(v%d): %v", version, err)
	}
}

func assertRootFiles(t *testing.T, root *os.Root, files map[string][]byte) {
	t.Helper()
	for name, want := range files {
		got, err := fs.ReadFile(root.FS(), name)
		if err != nil {
			t.Fatalf("read %q: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%q = %v, want %v", name, got, want)
		}
	}
}
