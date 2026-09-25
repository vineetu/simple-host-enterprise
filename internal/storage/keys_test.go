package storage

import (
	"errors"
	"strings"
	"testing"
)

func TestKeysRejectNonCanonicalIDs(t *testing.T) {
	bad := []string{
		"",
		strings.ToUpper(testSiteA),
		strings.ReplaceAll(testSiteA, "-", ""),
		"../" + testSiteA[3:],
		testSiteA[:35] + "/",
		testSiteA[:8] + "/" + testSiteA[9:],
		testSiteA + "/",
		"/" + testSiteA[1:],
		testSiteA[:35] + "g",
		testSiteA[:35],
		testSiteA + "0",
		"{" + testSiteA[1:35] + "}",
		"..",
		"sites",
	}
	for _, id := range bad {
		if key, err := SitePrefix(id); err == nil {
			t.Errorf("SitePrefix(%q) = %q, want error", id, key)
		}
		if key, err := VersionKey(id, 1); err == nil {
			t.Errorf("VersionKey(%q, 1) = %q, want error", id, key)
		}
		if key, err := AssetKey(id, testSiteB); err == nil {
			t.Errorf("AssetKey(site %q) = %q, want error", id, key)
		}
		if key, err := AssetKey(testSiteA, id); !errors.Is(err, ErrAssetNotFound) {
			t.Errorf("AssetKey(asset %q) = %q, %v, want ErrAssetNotFound", id, key, err)
		}
	}
	for _, version := range []int{0, -1, -1 << 31} {
		if key, err := VersionKey(testSiteA, version); err == nil {
			t.Errorf("VersionKey(version %d) = %q, want error", version, key)
		}
	}
}

func TestKeysAcceptCanonical(t *testing.T) {
	prefix, err := SitePrefix(testSiteA)
	if err != nil || prefix != "sites/"+testSiteA+"/" {
		t.Fatalf("SitePrefix = %q, %v", prefix, err)
	}
	key, err := VersionKey(testSiteA, 12)
	if err != nil || key != "sites/"+testSiteA+"/v12.tar.gz" {
		t.Fatalf("VersionKey = %q, %v", key, err)
	}
	key, err = AssetKey(testSiteA, testSiteB)
	if err != nil || key != "sites/"+testSiteA+"/assets/"+testSiteB {
		t.Fatalf("assetKey = %q, %v", key, err)
	}
	for i := 0; i < 20; i++ {
		id, err := newAssetID()
		if err != nil {
			t.Fatal(err)
		}
		if !isUUID(id) {
			t.Fatalf("newAssetID produced non-canonical id %q", id)
		}
	}
}
