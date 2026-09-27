package storage

import (
	"context"
	"testing"
)

// The bucket alone says whose each site is and what is there to recover.
func TestListRecoverableSites(t *testing.T) {
	ctx := context.Background()
	objects := NewMemoryObjects()
	put := func(key string) {
		if err := objects.Put(ctx, key, []byte("x"), "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
	}
	put("sites/" + testSiteA + "/v1.tar.gz")
	put("sites/" + testSiteA + "/v2.tar.gz")
	put("sites/" + testSiteA + "/v3.tar.gz")
	put("sites/" + testSiteA + "/assets/" + testSiteB)
	put("sites/" + testSiteB + "/v1.tar.gz") // no manifest: deployed before manifests
	put("sites/not-a-uuid/v1.tar.gz")
	put("readyz-probe")
	if err := PutSiteManifest(ctx, objects, SiteManifest{SiteID: testSiteA, Owner: "alice", Site: "notes", LiveVersion: 2,
		Assets: []ManifestAsset{{ID: testSiteB, Name: "a.pdf", ContentType: "application/pdf", Size: 1, SHA256: "00"}}}); err != nil {
		t.Fatal(err)
	}

	sites, err := ListRecoverableSites(ctx, objects)
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 2 {
		t.Fatalf("sites = %+v, want two", sites)
	}
	a := sites[0]
	if a.SiteID != testSiteA || a.Manifest == nil || a.Manifest.Owner != "alice" || a.Manifest.Site != "notes" || len(a.Versions) != 3 || !a.AssetIDs[testSiteB] {
		t.Fatalf("first = %+v, want alice/notes with three versions and the asset", a)
	}
	if a.NewestVersion() != 2 {
		t.Fatalf("NewestVersion = %d, want the manifest's live version 2", a.NewestVersion())
	}
	b := sites[1]
	if b.SiteID != testSiteB || b.Manifest != nil || b.ManifestErr != nil || b.NewestVersion() != 1 {
		t.Fatalf("second = %+v, want a site with no manifest and v1", b)
	}
}
