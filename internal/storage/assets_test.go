package storage

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// generousLimits is large enough that none of it is the thing under test,
// for cases exercising something other than a specific limit.
var generousLimits = AssetLimits{MaxFileBytes: 10 << 20, MaxSiteBytes: 100 << 20, MaxSiteCount: 100}

func newSiteForAssets(t *testing.T, store *DiskStorage, user, site string) {
	t.Helper()
	if err := store.WriteFiles(user, site, 1, map[string][]byte{"index.html": []byte("home")}); err != nil {
		t.Fatalf("WriteFiles: %v", err)
	}
}

var pngMagic = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0, 'I', 'H', 'D', 'R'}

func TestCreateAssetWritesFileAndRoundTrips(t *testing.T) {
	store, base := newTestDiskStorage(t)
	newSiteForAssets(t, store, "alice", "demo")

	content := append(append([]byte(nil), pngMagic...), []byte("...more png bytes...")...)
	stored, err := store.CreateAsset("alice", "demo", "image/png", bytes.NewReader(content), int64(len(content)), generousLimits)
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	if stored.ContentType != "image/png" {
		t.Fatalf("ContentType = %q, want image/png", stored.ContentType)
	}
	if !stored.Inline {
		t.Fatal("image/png should be Inline")
	}
	if stored.Size != int64(len(content)) {
		t.Fatalf("Size = %d, want %d", stored.Size, len(content))
	}
	wantSum := sha256.Sum256(content)
	if stored.SHA256 != wantSum {
		t.Fatalf("SHA256 = %x, want %x", stored.SHA256, wantSum)
	}
	if stored.ID == "" {
		t.Fatal("empty asset id")
	}

	// The file must exist directly on disk at <site>/assets/<id>, per
	// design.md 7.3 and the layout Phase 5's backup/restore already expects.
	onDisk, err := os.ReadFile(filepath.Join(base, "alice", "demo", "assets", stored.ID))
	if err != nil {
		t.Fatalf("read asset from disk: %v", err)
	}
	if !bytes.Equal(onDisk, content) {
		t.Fatalf("on-disk content mismatch")
	}

	reader, info, err := store.OpenAsset("alice", "demo", stored.ID)
	if err != nil {
		t.Fatalf("OpenAsset: %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read asset: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("OpenAsset content mismatch")
	}
	if info.Size() != int64(len(content)) {
		t.Fatalf("OpenAsset info.Size() = %d, want %d", info.Size(), len(content))
	}

	// No staging leftovers.
	entries, err := os.ReadDir(filepath.Join(base, "alice", "demo", "assets"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("staging file was not cleaned up: %s", entry.Name())
		}
	}
}

func TestCreateAssetRequiresExistingSite(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	_, err := store.CreateAsset("alice", "never-created", "text/plain", strings.NewReader("hi"), 2, generousLimits)
	if err == nil {
		t.Fatal("expected an error for a site that was never created")
	}
}

func TestCreateAssetClassifiesContentByType(t *testing.T) {
	cases := []struct {
		name        string
		content     []byte
		declared    string
		wantType    string
		wantInline  bool
		wantAllowed bool
	}{
		{name: "png sniffed regardless of declared type", content: pngMagic, declared: "text/html", wantType: "image/png", wantInline: true, wantAllowed: true},
		{name: "pdf", content: []byte("%PDF-1.4\nrest of pdf"), declared: "", wantType: "application/pdf", wantInline: true, wantAllowed: true},
		{name: "plain text with no hint", content: []byte("hello world plain text"), declared: "", wantType: "text/plain", wantInline: false, wantAllowed: true},
		{name: "plain text declared json", content: []byte(`{"a":1,"b":[1,2,3]}`), declared: "application/json", wantType: "application/json", wantInline: false, wantAllowed: true},
		{name: "plain text declared csv", content: []byte("a,b,c\n1,2,3\n"), declared: "text/csv", wantType: "text/csv", wantInline: false, wantAllowed: true},
		{name: "plain text declared json with charset param", content: []byte(`{"a":1}`), declared: "application/json; charset=utf-8", wantType: "application/json", wantInline: false, wantAllowed: true},
		{name: "opaque binary with no signature", content: []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01, 0x02, 0x03, 0xFF, 0xFE}, declared: "application/octet-stream", wantType: "application/octet-stream", wantInline: false, wantAllowed: true},
		{name: "html is refused even declared as plain text", content: []byte("<html><body>hi</body></html>"), declared: "text/plain", wantAllowed: false},
		{name: "zip is allowed as an attachment", content: []byte{0x50, 0x4B, 0x03, 0x04, 0, 0, 0, 0}, declared: "", wantType: "application/zip", wantInline: false, wantAllowed: true},
		{name: "gzip is allowed as an attachment", content: []byte{0x1F, 0x8B, 0x08, 0, 0, 0, 0, 0}, declared: "", wantType: "application/x-gzip", wantInline: false, wantAllowed: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			store, _ := newTestDiskStorage(t)
			newSiteForAssets(t, store, "alice", "demo")
			stored, err := store.CreateAsset("alice", "demo", tc.declared, bytes.NewReader(tc.content), int64(len(tc.content)), generousLimits)
			if !tc.wantAllowed {
				if !errors.Is(err, ErrAssetTypeNotAllowed) {
					t.Fatalf("CreateAsset error = %v, want ErrAssetTypeNotAllowed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateAsset: %v", err)
			}
			if stored.ContentType != tc.wantType {
				t.Fatalf("ContentType = %q, want %q", stored.ContentType, tc.wantType)
			}
			if stored.Inline != tc.wantInline {
				t.Fatalf("Inline = %v, want %v", stored.Inline, tc.wantInline)
			}
		})
	}
}

// TestCreateAssetExecutableMagicBytesAreIndistinguishableFromOpaqueBinary
// documents, rather than tests a regression in, an existing limitation:
// net/http.DetectContentType has no signature entry for a Windows PE or an
// ELF executable, so both sniff identically to any other unrecognized
// binary blob (application/octet-stream) — a limitation that predates and
// is unrelated to allowing application/zip and application/gzip. There is
// no sniffed value this package could refuse specifically for "this is an
// executable" without also refusing every other opaque binary upload the
// allowlist intends to accept.
func TestCreateAssetExecutableMagicBytesAreIndistinguishableFromOpaqueBinary(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	newSiteForAssets(t, store, "alice", "demo")
	peHeader := []byte("MZ\x90\x00\x03\x00\x00\x00\x04\x00\x00\x00\xFF\xFF\x00\x00")
	stored, err := store.CreateAsset("alice", "demo", "", bytes.NewReader(peHeader), int64(len(peHeader)), generousLimits)
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	if stored.ContentType != "application/octet-stream" {
		t.Fatalf("ContentType = %q, want application/octet-stream", stored.ContentType)
	}
}

func TestCreateAssetRefusalLeavesNoFileBehind(t *testing.T) {
	store, base := newTestDiskStorage(t)
	newSiteForAssets(t, store, "alice", "demo")
	html := []byte("<html><body>refused</body></html>")
	if _, err := store.CreateAsset("alice", "demo", "text/plain", bytes.NewReader(html), int64(len(html)), generousLimits); !errors.Is(err, ErrAssetTypeNotAllowed) {
		t.Fatalf("expected ErrAssetTypeNotAllowed, got %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(base, "alice", "demo", "assets"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected an empty assets directory after refusal, found %v", entries)
	}
}

func TestCreateAssetEnforcesPerFileSizeCap(t *testing.T) {
	store, base := newTestDiskStorage(t)
	newSiteForAssets(t, store, "alice", "demo")
	limits := AssetLimits{MaxFileBytes: 10, MaxSiteBytes: 1 << 20, MaxSiteCount: 100}

	content := []byte("this is way more than ten bytes of plain text")
	_, err := store.CreateAsset("alice", "demo", "text/plain", bytes.NewReader(content), int64(len(content)), limits)
	if !errors.Is(err, ErrAssetTooLarge) {
		t.Fatalf("expected ErrAssetTooLarge, got %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(base, "alice", "demo", "assets"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected an empty assets directory after size-cap refusal, found %v", entries)
	}

	// Within the cap, declared size unknown (0): must still succeed and be
	// checked against the actual bytes streamed.
	small := []byte("0123456789")
	stored, err := store.CreateAsset("alice", "demo", "text/plain", bytes.NewReader(small), 0, limits)
	if err != nil {
		t.Fatalf("CreateAsset at exactly the cap: %v", err)
	}
	if stored.Size != int64(len(small)) {
		t.Fatalf("Size = %d, want %d", stored.Size, len(small))
	}
}

func TestCreateAssetEnforcesPerFileSizeCapWithoutDeclaredSize(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	newSiteForAssets(t, store, "alice", "demo")
	limits := AssetLimits{MaxFileBytes: 10, MaxSiteBytes: 1 << 20, MaxSiteCount: 100}

	content := []byte("this is way more than ten bytes of plain text")
	// declaredSize of 0 means "unknown": the cap must still be enforced
	// against the real stream, not skipped because there was nothing to
	// precheck.
	if _, err := store.CreateAsset("alice", "demo", "text/plain", bytes.NewReader(content), 0, limits); !errors.Is(err, ErrAssetTooLarge) {
		t.Fatalf("expected ErrAssetTooLarge, got %v", err)
	}
}

func TestCreateAssetEnforcesSiteCountQuota(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	newSiteForAssets(t, store, "alice", "demo")
	limits := AssetLimits{MaxFileBytes: 1 << 20, MaxSiteBytes: 1 << 20, MaxSiteCount: 1}

	if _, err := store.CreateAsset("alice", "demo", "text/plain", strings.NewReader("one"), 3, limits); err != nil {
		t.Fatalf("first CreateAsset: %v", err)
	}
	if _, err := store.CreateAsset("alice", "demo", "text/plain", strings.NewReader("two"), 3, limits); !errors.Is(err, ErrAssetQuotaExceeded) {
		t.Fatalf("second CreateAsset error = %v, want ErrAssetQuotaExceeded", err)
	}
}

func TestCreateAssetEnforcesSiteByteQuota(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	newSiteForAssets(t, store, "alice", "demo")
	limits := AssetLimits{MaxFileBytes: 1 << 20, MaxSiteBytes: 15, MaxSiteCount: 100}

	if _, err := store.CreateAsset("alice", "demo", "text/plain", strings.NewReader("0123456789"), 10, limits); err != nil {
		t.Fatalf("first CreateAsset: %v", err)
	}
	// 10 + 10 = 20 > 15: the site-wide byte quota, not the per-file cap,
	// must reject this second upload even though it is well under
	// MaxFileBytes on its own.
	if _, err := store.CreateAsset("alice", "demo", "text/plain", strings.NewReader("9876543210"), 10, limits); !errors.Is(err, ErrAssetQuotaExceeded) {
		t.Fatalf("second CreateAsset error = %v, want ErrAssetQuotaExceeded", err)
	}
}

func TestCreateAssetEnforcesSiteByteQuotaWithoutDeclaredSize(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	newSiteForAssets(t, store, "alice", "demo")
	limits := AssetLimits{MaxFileBytes: 1 << 20, MaxSiteBytes: 15, MaxSiteCount: 100}

	if _, err := store.CreateAsset("alice", "demo", "text/plain", strings.NewReader("0123456789"), 10, limits); err != nil {
		t.Fatalf("first CreateAsset: %v", err)
	}
	// declaredSize 0 skips the cheap pre-check; the post-write recount
	// against actual bytes on disk must still catch this.
	if _, err := store.CreateAsset("alice", "demo", "text/plain", strings.NewReader("9876543210"), 0, limits); !errors.Is(err, ErrAssetQuotaExceeded) {
		t.Fatalf("second CreateAsset error = %v, want ErrAssetQuotaExceeded", err)
	}
}

func TestOpenAssetNotFound(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	newSiteForAssets(t, store, "alice", "demo")

	traversal := []string{"missing-id", "../escape", "a/b", "", "."}
	for _, id := range traversal {
		id := id
		t.Run(id, func(t *testing.T) {
			_, _, err := store.OpenAsset("alice", "demo", id)
			if !errors.Is(err, ErrAssetNotFound) {
				t.Fatalf("OpenAsset(%q) error = %v, want ErrAssetNotFound", id, err)
			}
		})
	}
}

func TestOpenAssetRefusesTraversalOutsideAssetsDir(t *testing.T) {
	store, base := newTestDiskStorage(t)
	newSiteForAssets(t, store, "alice", "demo")
	// A secret file that exists on disk but outside the assets directory.
	secret := filepath.Join(base, "alice", "demo", "v1", "index.html")
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("fixture missing: %v", err)
	}
	if _, _, err := store.OpenAsset("alice", "demo", "../v1/index.html"); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("traversal id error = %v, want ErrAssetNotFound", err)
	}
}

func TestDeleteAssetRemovesFileAndIsIdempotentlyNotFound(t *testing.T) {
	store, base := newTestDiskStorage(t)
	newSiteForAssets(t, store, "alice", "demo")
	stored, err := store.CreateAsset("alice", "demo", "text/plain", strings.NewReader("bye"), 3, generousLimits)
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}

	if err := store.DeleteAsset("alice", "demo", stored.ID); err != nil {
		t.Fatalf("DeleteAsset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "alice", "demo", "assets", stored.ID)); !os.IsNotExist(err) {
		t.Fatalf("asset file still present after delete: %v", err)
	}
	if err := store.DeleteAsset("alice", "demo", stored.ID); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("second DeleteAsset error = %v, want ErrAssetNotFound", err)
	}
	if err := store.DeleteAsset("alice", "demo", "../escape"); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("traversal id error = %v, want ErrAssetNotFound", err)
	}
}

func TestListAssetsWalksDirectlyAndSkipsStaging(t *testing.T) {
	store, base := newTestDiskStorage(t)
	newSiteForAssets(t, store, "alice", "demo")

	empty, err := store.ListAssets("alice", "demo")
	if err != nil {
		t.Fatalf("ListAssets on a site with no assets yet: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected no assets, got %v", empty)
	}

	first, err := store.CreateAsset("alice", "demo", "text/plain", strings.NewReader("one"), 3, generousLimits)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateAsset("alice", "demo", "text/plain", strings.NewReader("twotwo"), 6, generousLimits)
	if err != nil {
		t.Fatal(err)
	}

	// A stray leftover staging file must never be reported as an asset.
	if err := os.WriteFile(filepath.Join(base, "alice", "demo", "assets", ".tmp-asset-leftover"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := store.ListAssets("alice", "demo")
	if err != nil {
		t.Fatalf("ListAssets: %v", err)
	}
	byID := map[string]AssetFileInfo{}
	for _, f := range got {
		byID[f.ID] = f
	}
	if len(byID) != 2 {
		t.Fatalf("ListAssets returned %d entries, want 2: %v", len(byID), got)
	}
	if byID[first.ID].Size != 3 {
		t.Fatalf("first asset size = %d, want 3", byID[first.ID].Size)
	}
	if byID[second.ID].Size != 6 {
		t.Fatalf("second asset size = %d, want 6", byID[second.ID].Size)
	}
}

func TestListAssetsOnSiteWithNoAssetsDirectory(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	newSiteForAssets(t, store, "alice", "demo")
	got, err := store.ListAssets("alice", "demo")
	if err != nil {
		t.Fatalf("ListAssets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no assets, got %v", got)
	}
}

func TestListAssetsOnMissingSite(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	got, err := store.ListAssets("alice", "never-created")
	if err != nil {
		t.Fatalf("ListAssets on a missing site: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no assets, got %v", got)
	}
}

func TestCreateAssetRejectsInvalidIdentity(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	invalid := []string{"", ".", "..", "../escape", "a/b", `a\b`}
	for _, segment := range invalid {
		segment := segment
		t.Run(segment, func(t *testing.T) {
			if _, err := store.CreateAsset(segment, "demo", "text/plain", strings.NewReader("x"), 1, generousLimits); err == nil {
				t.Errorf("CreateAsset accepted user %q", segment)
			}
			if _, err := store.CreateAsset("alice", segment, "text/plain", strings.NewReader("x"), 1, generousLimits); err == nil {
				t.Errorf("CreateAsset accepted site %q", segment)
			}
		})
	}
}

func TestAssetLimitsValidation(t *testing.T) {
	store, _ := newTestDiskStorage(t)
	newSiteForAssets(t, store, "alice", "demo")
	bad := []AssetLimits{
		{MaxFileBytes: 0, MaxSiteBytes: 10, MaxSiteCount: 10},
		{MaxFileBytes: 10, MaxSiteBytes: 0, MaxSiteCount: 10},
		{MaxFileBytes: 10, MaxSiteBytes: 10, MaxSiteCount: 0},
		{MaxFileBytes: -1, MaxSiteBytes: 10, MaxSiteCount: 10},
	}
	for _, limits := range bad {
		if _, err := store.CreateAsset("alice", "demo", "text/plain", strings.NewReader("x"), 1, limits); err == nil {
			t.Errorf("CreateAsset accepted invalid limits %+v", limits)
		}
	}
}
