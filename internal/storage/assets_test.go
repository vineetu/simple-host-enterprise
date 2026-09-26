package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// generousLimits is large enough that none of it is the thing under test.
var generousLimits = AssetLimits{MaxFileBytes: 10 << 20, MaxSiteBytes: 100 << 20, MaxSiteCount: 100}

var pngMagic = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0, 'I', 'H', 'D', 'R'}

func newAssetStore(t *testing.T) (*Store, *recordingObjects, *MemoryObjects) {
	t.Helper()
	memory := NewMemoryObjects()
	recording := newRecordingObjects(memory)
	store, _ := newTestStore(t, recording, nil, 1<<30)
	return store, recording, memory
}

func TestCreateAssetStoresObjectAndRoundTrips(t *testing.T) {
	store, recording, memory := newAssetStore(t)
	ctx := context.Background()
	content := append(append([]byte(nil), pngMagic...), []byte("...more png bytes...")...)

	stored, err := store.CreateAsset(ctx, testSiteA, "text/html", bytes.NewReader(content), generousLimits)
	if err != nil {
		t.Fatalf("CreateAsset: %v", err)
	}
	if stored.ContentType != "image/png" || !stored.Inline {
		t.Fatalf("stored = %+v, want inline image/png", stored)
	}
	if stored.Size != int64(len(content)) || stored.SHA256 != sha256.Sum256(content) {
		t.Fatalf("size/hash mismatch: %+v", stored)
	}
	if !isUUID(stored.ID) {
		t.Fatalf("asset id %q is not a canonical uuid", stored.ID)
	}
	key := "sites/" + testSiteA + "/assets/" + stored.ID
	if keys := memory.Keys(); len(keys) != 1 || keys[0] != key {
		t.Fatalf("bucket keys = %v, want [%s]", keys, key)
	}
	if recording.types[key] != "image/png" {
		t.Fatalf("object content type = %q, want image/png", recording.types[key])
	}

	lease, err := store.OpenAsset(ctx, testSiteA, stored.ID, generousLimits.MaxFileBytes, stored.SHA256[:])
	if err != nil {
		t.Fatalf("OpenAsset: %v", err)
	}
	defer lease.Close()
	got, err := io.ReadAll(lease.File)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("OpenAsset content mismatch (%v)", err)
	}

	// Range request through the seekable file.
	request := httptest.NewRequest(http.MethodGet, "/asset", nil)
	request.Header.Set("Range", "bytes=2-5")
	recorder := httptest.NewRecorder()
	http.ServeContent(recorder, request, "", time.Time{}, lease.File)
	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", recorder.Code)
	}
	if !bytes.Equal(recorder.Body.Bytes(), content[2:6]) {
		t.Fatalf("range body = %x, want %x", recorder.Body.Bytes(), content[2:6])
	}

	// A second open is served from the cache.
	second, err := store.OpenAsset(ctx, testSiteA, stored.ID, generousLimits.MaxFileBytes, stored.SHA256[:])
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
	if n := recording.getCount(key); n != 1 {
		t.Fatalf("asset fetched %d times, want 1", n)
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
		{name: "pdf", content: []byte("%PDF-1.4\nrest of pdf"), wantType: "application/pdf", wantInline: true, wantAllowed: true},
		{name: "plain text with no hint", content: []byte("hello world plain text"), wantType: "text/plain", wantAllowed: true},
		{name: "plain text declared json", content: []byte(`{"a":1,"b":[1,2,3]}`), declared: "application/json", wantType: "application/json", wantAllowed: true},
		{name: "plain text declared csv", content: []byte("a,b,c\n1,2,3\n"), declared: "text/csv", wantType: "text/csv", wantAllowed: true},
		{name: "json with charset param", content: []byte(`{"a":1}`), declared: "application/json; charset=utf-8", wantType: "application/json", wantAllowed: true},
		{name: "plain text declared html stays plain", content: []byte("just words"), declared: "text/html", wantType: "text/plain", wantAllowed: true},
		{name: "opaque binary", content: []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01, 0x02, 0x03, 0xFF, 0xFE}, declared: "application/octet-stream", wantType: "application/octet-stream", wantAllowed: true},
		{name: "executable sniffs as opaque binary", content: []byte("MZ\x90\x00\x03\x00\x00\x00\x04\x00\x00\x00\xFF\xFF\x00\x00"), wantType: "application/octet-stream", wantAllowed: true},
		{name: "zip as attachment", content: []byte{0x50, 0x4B, 0x03, 0x04, 0, 0, 0, 0}, wantType: "application/zip", wantAllowed: true},
		{name: "gzip as attachment", content: []byte{0x1F, 0x8B, 0x08, 0, 0, 0, 0, 0}, wantType: "application/x-gzip", wantAllowed: true},
		{name: "html refused even declared plain text", content: []byte("<html><body>hi</body></html>"), declared: "text/plain", wantAllowed: false},
		{name: "html refused declared image", content: []byte("<!DOCTYPE html><script>x</script>"), declared: "image/png", wantAllowed: false},
		{name: "xml refused", content: []byte("<?xml version=\"1.0\"?><svg/>"), declared: "image/svg+xml", wantAllowed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, _, memory := newAssetStore(t)
			stored, err := store.CreateAsset(context.Background(), testSiteA, tc.declared, bytes.NewReader(tc.content), generousLimits)
			if !tc.wantAllowed {
				if !errors.Is(err, ErrAssetTypeNotAllowed) {
					t.Fatalf("error = %v, want ErrAssetTypeNotAllowed", err)
				}
				if keys := memory.Keys(); len(keys) != 0 {
					t.Fatalf("refused upload wrote %v", keys)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateAsset: %v", err)
			}
			if stored.ContentType != tc.wantType || stored.Inline != tc.wantInline {
				t.Fatalf("stored %q inline=%v, want %q inline=%v", stored.ContentType, stored.Inline, tc.wantType, tc.wantInline)
			}
		})
	}
}

func TestCreateAssetEnforcesPerFileSizeCap(t *testing.T) {
	store, _, memory := newAssetStore(t)
	ctx := context.Background()
	limits := AssetLimits{MaxFileBytes: 10, MaxSiteBytes: 1 << 20, MaxSiteCount: 100}

	if _, err := store.CreateAsset(ctx, testSiteA, "text/plain", strings.NewReader("this is way more than ten bytes"), limits); !errors.Is(err, ErrAssetTooLarge) {
		t.Fatalf("error = %v, want ErrAssetTooLarge", err)
	}
	// Past the sniffing window too.
	big := AssetLimits{MaxFileBytes: 1000, MaxSiteBytes: 1 << 20, MaxSiteCount: 100}
	if _, err := store.CreateAsset(ctx, testSiteA, "", bytes.NewReader(bytes.Repeat([]byte("a"), 1001)), big); !errors.Is(err, ErrAssetTooLarge) {
		t.Fatalf("error = %v, want ErrAssetTooLarge", err)
	}
	if keys := memory.Keys(); len(keys) != 0 {
		t.Fatalf("oversize upload wrote %v", keys)
	}
	stored, err := store.CreateAsset(ctx, testSiteA, "text/plain", strings.NewReader("0123456789"), limits)
	if err != nil || stored.Size != 10 {
		t.Fatalf("upload at exactly the cap: %+v, %v", stored, err)
	}
}

func TestWriteAssetContentNeverWritesPastCap(t *testing.T) {
	var destination bytes.Buffer
	_, _, _, err := writeAssetContent(&destination, bytes.NewReader(bytes.Repeat([]byte("a"), 5000)), "", 1000)
	if !errors.Is(err, ErrAssetTooLarge) {
		t.Fatalf("error = %v, want ErrAssetTooLarge", err)
	}
	if destination.Len() > 1001 {
		t.Fatalf("wrote %d bytes past a 1000-byte cap", destination.Len())
	}
}

func TestOpenAssetNotFound(t *testing.T) {
	store, _, _ := newAssetStore(t)
	ctx := context.Background()
	stored, err := store.CreateAsset(ctx, testSiteA, "", strings.NewReader("hello"), generousLimits)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{
		testSiteC, // well formed, never uploaded
		"missing-id", "../escape", "a/b", "", ".",
		strings.ToUpper(stored.ID),
		"../" + testSiteA + "/assets/" + stored.ID,
	}
	for _, id := range ids {
		if lease, err := store.OpenAsset(ctx, testSiteA, id, generousLimits.MaxFileBytes, nil); !errors.Is(err, ErrAssetNotFound) {
			if lease != nil {
				lease.Close()
			}
			t.Errorf("OpenAsset(%q) error = %v, want ErrAssetNotFound", id, err)
		}
	}
	// Another site's id does not reach this site's asset.
	if _, err := store.OpenAsset(ctx, testSiteB, stored.ID, generousLimits.MaxFileBytes, nil); !errors.Is(err, ErrAssetNotFound) {
		t.Errorf("cross-site OpenAsset error = %v, want ErrAssetNotFound", err)
	}
	if _, err := store.OpenAsset(ctx, testSiteA, stored.ID, 2, nil); !errors.Is(err, ErrObjectTooLarge) {
		t.Errorf("OpenAsset over maxBytes error = %v, want ErrObjectTooLarge", err)
	}
}

func TestDeleteAssetRemovesObject(t *testing.T) {
	store, _, memory := newAssetStore(t)
	ctx := context.Background()
	stored, err := store.CreateAsset(ctx, testSiteA, "text/plain", strings.NewReader("bye"), generousLimits)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAsset(ctx, testSiteA, stored.ID); err != nil {
		t.Fatalf("DeleteAsset: %v", err)
	}
	if keys := memory.Keys(); len(keys) != 0 {
		t.Fatalf("object survived delete: %v", keys)
	}
	if err := store.DeleteAsset(ctx, testSiteA, stored.ID); err != nil {
		t.Fatalf("deleting a missing asset: %v", err)
	}
	if _, err := store.OpenAsset(ctx, testSiteA, stored.ID, generousLimits.MaxFileBytes, stored.SHA256[:]); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("OpenAsset after delete = %v, want ErrAssetNotFound", err)
	}
	if err := store.DeleteAsset(ctx, testSiteA, "../escape"); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("malformed id error = %v, want ErrAssetNotFound", err)
	}
}

func TestCreateAssetRejectsBadInput(t *testing.T) {
	store, _, memory := newAssetStore(t)
	ctx := context.Background()
	for _, siteID := range []string{"", "..", "../x", "alice", strings.ToUpper(testSiteA)} {
		if _, err := store.CreateAsset(ctx, siteID, "text/plain", strings.NewReader("x"), generousLimits); err == nil {
			t.Errorf("CreateAsset accepted site id %q", siteID)
		}
	}
	if _, err := store.CreateAsset(ctx, testSiteA, "text/plain", nil, generousLimits); err == nil {
		t.Error("CreateAsset accepted a nil source")
	}
	for _, limits := range []AssetLimits{
		{MaxFileBytes: 0, MaxSiteBytes: 10, MaxSiteCount: 10},
		{MaxFileBytes: 10, MaxSiteBytes: 0, MaxSiteCount: 10},
		{MaxFileBytes: 10, MaxSiteBytes: 10, MaxSiteCount: 0},
		{MaxFileBytes: -1, MaxSiteBytes: 10, MaxSiteCount: 10},
	} {
		if _, err := store.CreateAsset(ctx, testSiteA, "text/plain", strings.NewReader("x"), limits); err == nil {
			t.Errorf("CreateAsset accepted invalid limits %+v", limits)
		}
	}
	if keys := memory.Keys(); len(keys) != 0 {
		t.Fatalf("refused uploads wrote %v", keys)
	}
}

func TestOpenAssetRefusesChecksumMismatch(t *testing.T) {
	store, _, memory := newAssetStore(t)
	ctx := context.Background()
	stored, err := store.CreateAsset(ctx, testSiteA, "text/plain", strings.NewReader("genuine"), generousLimits)
	if err != nil {
		t.Fatal(err)
	}
	key := "sites/" + testSiteA + "/assets/" + stored.ID
	if err := memory.Put(ctx, key, []byte("swapped"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if lease, err := store.OpenAsset(ctx, testSiteA, stored.ID, generousLimits.MaxFileBytes, stored.SHA256[:]); err == nil {
		lease.Close()
		t.Fatal("OpenAsset served an object that does not match its recorded sha256")
	}
}
