package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

// migrate-storage may be re-run after cutover, when the objects it would
// write are live. It must verify what is there, never overwrite it (which
// would also pile up noncurrent bucket versions).
func TestMigrateUploadsNeverOverwrite(t *testing.T) {
	ctx := context.Background()
	memory := NewMemoryObjects()
	objects := newRecordingObjects(memory)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "index.html"), []byte("old volume"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if _, err := UploadVersionDir(ctx, objects, testSiteA, 1, root); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	key, _ := VersionKey(testSiteA, 1)
	first, _ := memory.Get(ctx, key, 1<<20)

	// Same content again: verified in place, no second Put.
	delete(objects.types, key)
	if _, err := UploadVersionDir(ctx, objects, testSiteA, 1, root); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if _, put := objects.types[key]; put {
		t.Fatal("re-run re-uploaded an object that was already there")
	}

	// A different object already at the key (a live version since cutover)
	// is reported, not replaced.
	if err := os.WriteFile(filepath.Join(dir, "docs", "index.html"), []byte("changed on the volume"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := UploadVersionDir(ctx, objects, testSiteA, 1, root); err == nil {
		t.Fatal("re-run over a different stored object succeeded")
	}
	if stored, _ := memory.Get(ctx, key, 1<<20); !bytes.Equal(stored, first) {
		t.Fatal("stored version was overwritten")
	}

	// Assets likewise.
	const assetID = "0a0a0a0a-0000-4000-8000-0000000000d1"
	body := []byte("asset bytes")
	sum := sha256.Sum256(body)
	if err := UploadAsset(ctx, objects, testSiteA, assetID, "text/plain", body, sum[:]); err != nil {
		t.Fatalf("asset upload: %v", err)
	}
	assetKeyName, _ := AssetKey(testSiteA, assetID)
	delete(objects.types, assetKeyName)
	if err := UploadAsset(ctx, objects, testSiteA, assetID, "text/plain", body, sum[:]); err != nil {
		t.Fatalf("asset re-run: %v", err)
	}
	if _, put := objects.types[assetKeyName]; put {
		t.Fatal("asset re-run re-uploaded")
	}
	if err := memory.Put(ctx, assetKeyName, []byte("tampered!!!"), ""); err != nil {
		t.Fatal(err)
	}
	if err := UploadAsset(ctx, objects, testSiteA, assetID, "text/plain", body, sum[:]); err == nil {
		t.Fatal("asset re-run over a mismatched object succeeded")
	}
	if stored, _ := memory.Get(ctx, assetKeyName, 1<<20); string(stored) != "tampered!!!" {
		t.Fatal("stored asset was overwritten")
	}
}
