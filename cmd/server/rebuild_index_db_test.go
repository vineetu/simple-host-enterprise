package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/handler"
	"github.com/vsriram/simple-host/internal/storage"
)

// The database is lost and the bucket is not: rebuild-index names every
// site from its manifest, waits for its owner to exist again, then
// recreates it under the same id with its versions and uploaded files, and
// it serves.
func TestRebuildIndexFromBucket(t *testing.T) {
	ctx := context.Background()
	objects := storage.NewMemoryObjects()

	// Before: a site with two versions (v1 live after a rollback) and one
	// uploaded file, its manifest written the way a deploy writes it.
	before := operatorTestDB(t)
	owner, err := db.CreateOIDCUser(ctx, before, "rena", "sub-rena", "rena@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	site, err := db.CreateSite(ctx, before, owner.ID, "notes")
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.New(storage.Options{Objects: objects, Index: storage.NewDBIndex(before), CacheDir: t.TempDir(), CacheMaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	for v, page := range map[int]string{1: "<h1>first</h1>", 2: "<h1>second</h1>"} {
		if _, err := store.PutVersion(ctx, site.ID, v, map[string][]byte{"index.html": []byte(page)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpdateSiteActiveVersion(ctx, before, site.ID, 1); err != nil {
		t.Fatal(err)
	}
	asset, err := store.CreateAsset(ctx, site.ID, "text/plain", strings.NewReader("hello"), storage.AssetLimits{MaxFileBytes: 1 << 20, MaxSiteBytes: 1 << 20, MaxSiteCount: 10})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := before.Exec(`INSERT INTO site_assets (id, site_id, name, content_type, size, sha256) VALUES ($1, $2, 'hello.txt', $3, $4, $5)`,
		asset.ID, site.ID, asset.ContentType, asset.Size, asset.SHA256[:]); err != nil {
		t.Fatal(err)
	}
	if err := handler.WriteSiteManifest(ctx, before, objects, site.ID); err != nil {
		t.Fatal(err)
	}
	store.Close()

	// After: a new, empty database.
	after := operatorTestDB(t)
	var out bytes.Buffer
	res, err := rebuildIndex(ctx, after, objects, &out, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Waiting != 1 || res.Recreated != 0 || !strings.Contains(out.String(), "rena signs in once") || !strings.Contains(out.String(), "notes") {
		t.Fatalf("before the owner is back: %+v\n%s", res, out.String())
	}
	if _, err := db.CreateOIDCUser(ctx, after, "rena", "sub-rena", "rena@example.com", false); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if res, err = rebuildIndex(ctx, after, objects, &out, false); err != nil || res.Recoverable != 1 || !strings.Contains(out.String(), "Nothing was changed") {
		t.Fatalf("list only: %+v %v\n%s", res, err, out.String())
	}
	var n int
	_ = after.QueryRow(`SELECT count(*) FROM sites`).Scan(&n)
	if n != 0 {
		t.Fatalf("a list-only run created %d sites", n)
	}
	out.Reset()
	if res, err = rebuildIndex(ctx, after, objects, &out, true); err != nil || res.Recreated != 1 {
		t.Fatalf("apply: %+v %v\n%s", res, err, out.String())
	}

	var id string
	var live, versions, assets int
	if err := after.QueryRow(`SELECT s.id::text, s.active_version, (SELECT count(*) FROM versions v WHERE v.site_id = s.id),
		(SELECT count(*) FROM site_assets a WHERE a.site_id = s.id AND a.name = 'hello.txt') FROM sites s WHERE s.name = 'notes'`).Scan(&id, &live, &versions, &assets); err != nil {
		t.Fatal(err)
	}
	if id != site.ID || live != 1 || versions != 2 || assets != 1 {
		t.Fatalf("recreated site = id %s live %d versions %d assets %d; want the same id, v1 live, two versions, the file", id, live, versions, assets)
	}
	restored, err := storage.New(storage.Options{Objects: objects, Index: storage.NewDBIndex(after), CacheDir: t.TempDir(), CacheMaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if got := drillServes(t, restored, "rena", "notes"); got != "<h1>first</h1>" {
		t.Fatalf("recreated site serves %q", got)
	}

	// A second run finds it in place.
	out.Reset()
	if res, err = rebuildIndex(ctx, after, objects, &out, true); err != nil || res.Present != 1 || res.Recreated != 0 {
		t.Fatalf("second run: %+v %v\n%s", res, err, out.String())
	}
}
