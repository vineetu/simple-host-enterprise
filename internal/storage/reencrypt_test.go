package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// committedVersions answers ReencryptOptions.VersionCommitted from a set.
func committedVersions(committed ...string) func(context.Context, string, int) (bool, error) {
	set := map[string]bool{}
	for _, key := range committed {
		set[key] = true
	}
	return func(_ context.Context, siteID string, version int) (bool, error) {
		key, err := VersionKey(siteID, version)
		if err != nil {
			return false, err
		}
		return set[key], nil
	}
}

func TestReencrypt(t *testing.T) {
	ctx := context.Background()
	fake := newFakeS3()
	oldKey, newKey := testKey("old", 0x31), testKey("new", 0x32)
	oldClient := newS3Objects(fake, fake.bucket, "p", types.ServerSideEncryptionAes256, "", []EnvelopeKey{oldKey})
	plainClient := newS3Objects(fake, fake.bucket, "p", types.ServerSideEncryptionAes256, "", nil)
	rotated := newS3Objects(fake, fake.bucket, "p", types.ServerSideEncryptionAes256, "", []EnvelopeKey{newKey, oldKey})
	rotated.plaintextAllowed = true

	v1Legacy, _ := VersionKey(testSiteA, 1)
	v2Old, _ := VersionKey(testSiteA, 2)
	v3Plain, _ := VersionKey(testSiteA, 3)
	v4Corrupt, _ := VersionKey(testSiteA, 4)
	v5Uncommitted, _ := VersionKey(testSiteA, 5)
	assetOld, _ := AssetKey(testSiteB, testSiteC)
	assetCurrent, _ := AssetKey(testSiteB, testSiteA)
	stray := "sites/" + testSiteA + "/notes.txt"
	plaintexts := map[string][]byte{}
	for _, key := range []string{v1Legacy, v2Old, v3Plain, v4Corrupt, v5Uncommitted, assetOld, assetCurrent, stray} {
		plaintexts[key] = []byte("plaintext of " + key)
	}

	// v1: the v1.1.0-v1.1.2 form (no associated data, no format field).
	ciphertext, metadata, err := wrapObject("", plaintexts[v1Legacy], oldKey)
	if err != nil {
		t.Fatal(err)
	}
	delete(metadata, metaEnvelopeFormat)
	fake.objects["p/"+v1Legacy] = fakeObject{body: ciphertext, metadata: metadata, contentType: "application/octet-stream"}
	for key, client := range map[string]*S3Objects{
		v2Old: oldClient, v4Corrupt: oldClient, v5Uncommitted: oldClient, assetOld: oldClient,
		v3Plain: plainClient, stray: plainClient, assetCurrent: rotated,
	} {
		if err := client.Put(ctx, key, plaintexts[key], "application/gzip"); err != nil {
			t.Fatal(err)
		}
	}
	corrupt := fake.raw(t, "p/"+v4Corrupt)
	corrupt.body[0] ^= 0xff
	fake.objects["p/"+v4Corrupt] = corrupt
	before := map[string]fakeObject{}
	for key, object := range fake.objects {
		before[key] = object
	}
	current := fake.raw(t, "p/"+assetCurrent)

	var lines []string
	opts := ReencryptOptions{
		Concurrency:      3,
		VersionCommitted: committedVersions(v1Legacy, v2Old, v3Plain, v4Corrupt),
		Logf:             func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) },
		ProgressEvery:    2,
	}

	// A dry run reports and writes nothing.
	opts.DryRun = true
	stats, err := rotated.Reencrypt(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ReencryptStats{Scanned: 8, Rewritten: 4, Current: 1, Skipped: 2, Failed: 1}); stats != want {
		t.Fatalf("dry run = %v, want %v", stats, want)
	}
	for key, object := range before {
		if got := fake.raw(t, key); !bytes.Equal(got.body, object.body) {
			t.Fatalf("dry run changed %s", key)
		}
	}

	opts.DryRun = false
	stats, err = rotated.Reencrypt(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ReencryptStats{Scanned: 8, Rewritten: 4, Current: 1, Skipped: 2, Failed: 1}); stats != want {
		t.Fatalf("run = %v, want %v\n%v", stats, want, lines)
	}

	// Everything the store serves now reads with the new key alone and no
	// plaintext allowance.
	newOnly := newS3Objects(fake, fake.bucket, "p", types.ServerSideEncryptionAes256, "", []EnvelopeKey{newKey})
	for _, key := range []string{v1Legacy, v2Old, v3Plain, assetOld, assetCurrent} {
		object := fake.raw(t, "p/"+key)
		if object.metadata[metaEnvelopeKeyID] != "new" || object.metadata[metaEnvelopeFormat] != envelopeFormatKeyBound {
			t.Errorf("%s: metadata %v, want the new key in format 2", key, object.metadata)
		}
		got, err := newOnly.Get(ctx, key, 1<<20)
		if err != nil || !bytes.Equal(got, plaintexts[key]) {
			t.Errorf("%s: Get with the new key alone = %q, %v", key, got, err)
		}
	}
	// The already-current object was not rewritten.
	if got := fake.raw(t, "p/"+assetCurrent); !bytes.Equal(got.body, current.body) {
		t.Error("an object already current was rewritten")
	}
	// The corrupted, uncommitted and stray objects are untouched.
	for _, key := range []string{v4Corrupt, v5Uncommitted, stray} {
		if got := fake.raw(t, "p/"+key); !bytes.Equal(got.body, before["p/"+key].body) {
			t.Errorf("%s was rewritten", key)
		}
	}
	var sawFailure, sawProgress bool
	for _, line := range lines {
		sawFailure = sawFailure || bytes.Contains([]byte(line), []byte("FAILED "+v4Corrupt))
		sawProgress = sawProgress || bytes.HasPrefix([]byte(line), []byte("reencrypt: 2 scanned"))
	}
	if !sawFailure || !sawProgress {
		t.Errorf("log lines lack the failure or a progress line: %v", lines)
	}

	// A second run rewrites nothing.
	stats, err = rotated.Reencrypt(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ReencryptStats{Scanned: 8, Current: 5, Skipped: 2, Failed: 1}); stats != want {
		t.Fatalf("second run = %v, want %v", stats, want)
	}
}

func TestReencryptWithoutKey(t *testing.T) {
	fake := newFakeS3()
	objects := newS3Objects(fake, fake.bucket, "", types.ServerSideEncryptionAes256, "", nil)
	_, err := objects.Reencrypt(context.Background(), ReencryptOptions{VersionCommitted: committedVersions()})
	if !errors.Is(err, ErrNoEnvelopeKey) {
		t.Fatalf("Reencrypt with no key = %v, want ErrNoEnvelopeKey", err)
	}
}

func TestReencryptPlaintextRefusedWithoutAllowance(t *testing.T) {
	ctx := context.Background()
	fake := newFakeS3()
	key, _ := VersionKey(testSiteA, 1)
	plain := newS3Objects(fake, fake.bucket, "", types.ServerSideEncryptionAes256, "", nil)
	if err := plain.Put(ctx, key, []byte("plain"), "application/gzip"); err != nil {
		t.Fatal(err)
	}
	objects := newS3Objects(fake, fake.bucket, "", types.ServerSideEncryptionAes256, "", []EnvelopeKey{testKey("k", 0x33)})
	stats, err := objects.Reencrypt(ctx, ReencryptOptions{VersionCommitted: committedVersions(key)})
	if err != nil || stats.Failed != 1 || stats.Rewritten != 0 {
		t.Fatalf("Reencrypt = %v, %v; want the plaintext object failed, not rewritten", stats, err)
	}
	if got := fake.raw(t, key); string(got.body) != "plain" {
		t.Fatal("plaintext object was overwritten")
	}
}

func TestParseStoreKey(t *testing.T) {
	for _, tc := range []struct {
		key     string
		version int
		asset   bool
		ok      bool
	}{
		{"sites/" + testSiteA + "/v12.tar.gz", 12, false, true},
		{"sites/" + testSiteA + "/assets/" + testSiteB, 0, true, true},
		{"sites/" + testSiteA + "/v012.tar.gz", 0, false, false},
		{"sites/" + testSiteA + "/v0.tar.gz", 0, false, false},
		{"sites/" + testSiteA + "/assets/x", 0, true, false},
		{"sites/not-a-uuid/v1.tar.gz", 0, false, false},
		{"readyz-probe", 0, false, false},
	} {
		_, version, asset, ok := parseStoreKey(tc.key)
		if ok != tc.ok || (ok && (version != tc.version || asset != tc.asset)) {
			t.Errorf("parseStoreKey(%q) = %d %v %v", tc.key, version, asset, ok)
		}
	}
}
