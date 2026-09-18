package storage

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupVersionSkipsArchiveWithoutMaterializing(t *testing.T) {
	store, base := newTestDiskStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{
		"index.html": []byte("archived backup source"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveVersion("alice", "demo", 1); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(base, "alice", "demo", "v1.tar.gz")
	before, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	// A nil client would panic if BackupVersion continued into PutObject.
	backup := &Backup{}
	if err := backup.BackupVersion(context.Background(), base, "alice", "demo", 1); err != nil {
		t.Fatalf("BackupVersion archived target: %v", err)
	}
	after, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("BackupVersion changed archived bytes")
	}
	if _, err := os.Lstat(filepath.Join(base, "alice", "demo", "v1")); !os.IsNotExist(err) {
		t.Fatalf("BackupVersion materialized raw directory: %v", err)
	}
}
