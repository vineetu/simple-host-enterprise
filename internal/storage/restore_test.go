package storage

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/vsriram/simple-host/internal/safepath"
)

func TestBackupVersionSendsSSEHeader(t *testing.T) {
	store, base := newTestDiskStorage(t)
	if err := store.WriteFiles("alice", "blog", 1, map[string][]byte{"index.html": []byte("hi")}); err != nil {
		t.Fatal(err)
	}

	t.Run("AES256 default, no envelope", func(t *testing.T) {
		fake := newFakeS3()
		backup := newBackup(fake, "bucket", "backups/", types.ServerSideEncryptionAes256, "", nil)
		if err := backup.BackupVersion(context.Background(), base, "alice", "blog", 1); err != nil {
			t.Fatal(err)
		}
		var obj fakeObject
		for _, o := range fake.objects {
			obj = o
		}
		if obj.sse != types.ServerSideEncryptionAes256 {
			t.Fatalf("ServerSideEncryption = %q, want AES256", obj.sse)
		}
		if obj.sseKMSKeyID != "" {
			t.Fatalf("SSEKMSKeyId = %q, want empty for AES256", obj.sseKMSKeyID)
		}
		if len(obj.metadata) != 0 {
			t.Fatalf("metadata = %v, want none without an envelope key", obj.metadata)
		}
		if obj.contentType != "application/gzip" {
			t.Fatalf("ContentType = %q, want application/gzip for a plain object", obj.contentType)
		}
	})

	t.Run("aws:kms with a key id", func(t *testing.T) {
		fake := newFakeS3()
		backup := newBackup(fake, "bucket", "backups/", types.ServerSideEncryptionAwsKms, "arn:aws:kms:us-east-1:1:key/abc", nil)
		if err := backup.BackupVersion(context.Background(), base, "alice", "blog", 1); err != nil {
			t.Fatal(err)
		}
		var obj fakeObject
		for _, o := range fake.objects {
			obj = o
		}
		if obj.sse != types.ServerSideEncryptionAwsKms {
			t.Fatalf("ServerSideEncryption = %q, want aws:kms", obj.sse)
		}
		if obj.sseKMSKeyID != "arn:aws:kms:us-east-1:1:key/abc" {
			t.Fatalf("SSEKMSKeyId = %q", obj.sseKMSKeyID)
		}
	})
}

func TestBackupVersionEnvelopeMetadataAndOpaqueBody(t *testing.T) {
	store, base := newTestDiskStorage(t)
	plaintextFile := []byte("<html>secret site content</html>")
	if err := store.WriteFiles("alice", "blog", 1, map[string][]byte{"index.html": plaintextFile}); err != nil {
		t.Fatal(err)
	}

	fake := newFakeS3()
	key := testKey("k1", 0x11)
	backup := newBackup(fake, "bucket", "backups/", types.ServerSideEncryptionAes256, "", []EnvelopeKey{key})
	if err := backup.BackupVersion(context.Background(), base, "alice", "blog", 1); err != nil {
		t.Fatal(err)
	}

	var obj fakeObject
	for _, o := range fake.objects {
		obj = o
	}
	if obj.metadata["sh-envelope-key-id"] != "k1" {
		t.Fatalf("metadata key id = %q, want k1", obj.metadata["sh-envelope-key-id"])
	}
	if obj.metadata["sh-envelope-wrap-nonce"] == "" || obj.metadata["sh-envelope-wrapped-key"] == "" || obj.metadata["sh-envelope-data-nonce"] == "" {
		t.Fatalf("incomplete envelope metadata: %v", obj.metadata)
	}
	if obj.contentType != "application/octet-stream" {
		t.Fatalf("ContentType = %q, want application/octet-stream for an enveloped object", obj.contentType)
	}
	if bytes.Contains(obj.body, plaintextFile) {
		t.Fatal("backup object body contains the plaintext file content unencrypted")
	}
}

func TestRestoreVersionIntoSecondSite(t *testing.T) {
	store, base := newTestDiskStorage(t)
	files := map[string][]byte{
		"index.html":     []byte("<html>hello</html>"),
		"assets/app.css": []byte("body{}"),
	}
	if err := store.WriteFiles("alice", "blog", 3, files); err != nil {
		t.Fatal(err)
	}

	fake := newFakeS3()
	key := testKey("k1", 0x22)
	backup := newBackup(fake, "bucket", "backups/", types.ServerSideEncryptionAes256, "", []EnvelopeKey{key})
	ctx := context.Background()
	if err := backup.BackupVersion(ctx, base, "alice", "blog", 3); err != nil {
		t.Fatal(err)
	}

	restoredKey, err := backup.RestoreVersion(ctx, store, "alice", "blog", 3, "alice-dr", "blog-dr", true)
	if err != nil {
		t.Fatal(err)
	}
	if restoredKey == "" {
		t.Fatal("RestoreVersion returned an empty object key")
	}

	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(base, "alice-dr", "blog-dr", "v3", name))
		if err != nil {
			t.Fatalf("read restored %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("restored %s = %q, want %q", name, got, want)
		}
	}

	version, ok, err := store.CurrentVersion("alice-dr", "blog-dr")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || version != 3 {
		t.Fatalf("CurrentVersion(alice-dr, blog-dr) = %d, %v, want 3, true", version, ok)
	}
}

func TestRestoreVersionWithoutSetCurrentLeavesNoCurrentLink(t *testing.T) {
	store, base := newTestDiskStorage(t)
	if err := store.WriteFiles("alice", "blog", 1, map[string][]byte{"index.html": []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	fake := newFakeS3()
	backup := newBackup(fake, "bucket", "backups/", types.ServerSideEncryptionAes256, "", nil)
	ctx := context.Background()
	if err := backup.BackupVersion(ctx, base, "alice", "blog", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.RestoreVersion(ctx, store, "alice", "blog", 1, "alice2", "blog2", false); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.CurrentVersion("alice2", "blog2"); err != nil || ok {
		t.Fatalf("CurrentVersion(alice2, blog2) = ok=%v, err=%v, want no current link", ok, err)
	}
}

func TestRestoreVersionNoBackupFound(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	fake := newFakeS3()
	backup := newBackup(fake, "bucket", "backups/", types.ServerSideEncryptionAes256, "", nil)
	if _, err := backup.RestoreVersion(context.Background(), store, "alice", "blog", 9, "alice", "blog", true); err == nil {
		t.Fatal("RestoreVersion succeeded with no backup object present")
	}
}

func TestRestoreVersionUnwrapsWithoutTheConfiguredKeyFails(t *testing.T) {
	store, base := newTestDiskStorage(t)
	if err := store.WriteFiles("alice", "blog", 1, map[string][]byte{"index.html": []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	fake := newFakeS3()
	wrapKey := testKey("k1", 0x33)
	writer := newBackup(fake, "bucket", "backups/", types.ServerSideEncryptionAes256, "", []EnvelopeKey{wrapKey})
	ctx := context.Background()
	if err := writer.BackupVersion(ctx, base, "alice", "blog", 1); err != nil {
		t.Fatal(err)
	}

	// A restorer configured with a different key (the escrowed secret was
	// lost, or never distributed) must fail closed rather than serve garbage.
	otherKey := testKey("k2", 0x44)
	reader := newBackup(fake, "bucket", "backups/", types.ServerSideEncryptionAes256, "", []EnvelopeKey{otherKey})
	if _, err := reader.RestoreVersion(ctx, store, "alice", "blog", 1, "alice2", "blog2", false); err == nil {
		t.Fatal("RestoreVersion succeeded without the matching envelope key")
	}
}

func TestBackupAndRestoreAssets(t *testing.T) {
	base := t.TempDir()
	assetsDir := filepath.Join(base, "alice", "blog", "assets")
	if err := os.MkdirAll(filepath.Join(assetsDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("binary-ish asset content")
	if err := os.WriteFile(filepath.Join(assetsDir, "sub", "image.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	fake := newFakeS3()
	key := testKey("k1", 0x55)
	backup := newBackup(fake, "bucket", "backups/", types.ServerSideEncryptionAes256, "", []EnvelopeKey{key})
	ctx := context.Background()

	uploaded, err := backup.BackupAssets(ctx, base, "alice", "blog")
	if err != nil {
		t.Fatal(err)
	}
	if uploaded != 1 {
		t.Fatalf("BackupAssets uploaded = %d, want 1", uploaded)
	}

	target := t.TempDir()
	restored, err := backup.RestoreAssets(ctx, "alice", "blog", target)
	if err != nil {
		t.Fatal(err)
	}
	if restored != 1 {
		t.Fatalf("RestoreAssets restored = %d, want 1", restored)
	}
	got, err := os.ReadFile(filepath.Join(target, "assets", "sub", "image.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("restored asset = %q, want %q", got, content)
	}
}

// TestRestoreAssetsRefusesUnsafeObjectKeys proves the safepath.CanonicalRelativePath
// guard in RestoreAssets (backup.go's sibling restore.go) actually rejects a
// hostile or corrupted backup object key before any bytes reach disk. Unlike
// BackupAssets, which only ever derives keys from a real on-disk walk and so
// cannot organically produce an unsafe relative path, RestoreAssets derives
// its destination path from an S3 object key: attacker-controlled if the
// bucket is ever writable by anything other than this process, and otherwise
// a corruption/typo hazard worth failing closed on regardless.
func TestRestoreAssetsRefusesUnsafeObjectKeys(t *testing.T) {
	cases := []struct {
		name   string
		suffix string
	}{
		{"parent directory traversal", "../../etc/passwd"},
		{"absolute path", "/etc/passwd"},
		{"backslash form", "..\\..\\evil.txt"},
		{"empty path component", "a//b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeS3()
			backup := newBackup(fake, "bucket", "backups/", types.ServerSideEncryptionAes256, "", nil)
			prefix := assetsObjectPrefix(backup.prefix, "alice", "blog")
			maliciousKey := prefix + tc.suffix
			fake.objects[maliciousKey] = fakeObject{body: []byte("payload")}

			target := t.TempDir()
			restored, err := backup.RestoreAssets(context.Background(), "alice", "blog", target)
			if err == nil {
				t.Fatalf("RestoreAssets accepted unsafe object key %q", maliciousKey)
			}
			if restored != 0 {
				t.Fatalf("RestoreAssets restored = %d, want 0 for a refused key", restored)
			}

			// Nothing should have been written anywhere, inside the target
			// directory or (via a traversal) outside it.
			entries, statErr := os.ReadDir(target)
			if statErr != nil && !os.IsNotExist(statErr) {
				t.Fatalf("ReadDir(target): %v", statErr)
			}
			for _, e := range entries {
				t.Fatalf("unexpected entry written under target: %s", e.Name())
			}
			if _, statErr := os.Stat(filepath.Join(target, "assets")); !os.IsNotExist(statErr) {
				t.Fatalf("RestoreAssets created an assets directory despite refusing the key: err=%v", statErr)
			}
		})
	}
}

// TestBackupAssetsRefusesUnsafeRelativePaths documents and pins the same
// safepath.CanonicalRelativePath guard on the BackupAssets side (restore.go
// around backup.go's assetsObjectPrefix use). BackupAssets computes its
// relative path with filepath.Rel over a real filepath.Walk, so none of
// RestoreAssets's adversarial suffixes above are reachable through the
// filesystem (the OS itself refuses to create a path segment of ".." or a
// name containing "/" or "\"): this test instead exercises the boundary
// directly, the same way BackupAssets does at internal/storage/restore.go
// line ~177, so a future refactor that changes how `rel` is derived (for
// example, to build it from something other than a real Walk) cannot
// silently drop the check.
func TestBackupAssetsRefusesUnsafeRelativePaths(t *testing.T) {
	cases := []struct {
		name string
		rel  string
	}{
		{"parent directory traversal", "../../etc/passwd"},
		{"absolute path", "/etc/passwd"},
		{"backslash form", "..\\..\\evil.txt"},
		{"empty path component", "a//b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canonical, err := safepath.CanonicalRelativePath(tc.rel, false)
			if err == nil && canonical == tc.rel {
				t.Fatalf("safepath.CanonicalRelativePath(%q) unexpectedly accepted an unsafe relative path", tc.rel)
			}
		})
	}
}

func TestBackupAssetsNoDirectoryIsNotAnError(t *testing.T) {
	base := t.TempDir()
	fake := newFakeS3()
	backup := newBackup(fake, "bucket", "backups/", types.ServerSideEncryptionAes256, "", nil)
	n, err := backup.BackupAssets(context.Background(), base, "alice", "blog")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("BackupAssets = %d, want 0 for a site with no assets", n)
	}
}
