package main

import (
	"context"
	"testing"

	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/storage"
)

func TestSummaryBackfillReadsBucketAndReruns(t *testing.T) {
	database := operatorTestDB(t)
	ctx := context.Background()
	owner, err := db.CreateOIDCUser(ctx, database, "alice", "alice-sub", "alice@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	objects := storage.NewMemoryObjects()
	store, err := storage.New(storage.Options{Objects: objects, Index: storage.NewDBIndex(database), CacheDir: t.TempDir(), CacheMaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, name := range []string{"first", "second"} {
		site, err := db.CreateSite(ctx, database, owner.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		for v := 1; v <= 2; v++ {
			key, _ := storage.VersionKey(site.ID, v)
			if _, err := db.CreateVersion(ctx, database, site.ID, v, key, &owner.ID); err != nil {
				t.Fatal(err)
			}
			title := name
			if v == 2 {
				title = "held"
			}
			if _, err := store.PutVersion(ctx, site.ID, v, map[string][]byte{"index.html": []byte("<title>" + title + "</title><h1>Welcome</h1>")}); err != nil {
				t.Fatal(err)
			}
		}
		if err := db.UpdateSiteActiveVersion(ctx, database, site.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	// A separate empty cache proves the backfill fetches from objects.
	reader, err := storage.New(storage.Options{Objects: objects, Index: storage.NewDBIndex(database), CacheDir: t.TempDir(), CacheMaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, tc := range []struct{ limit, want int }{{1, 1}, {0, 1}, {0, 0}} {
		n, err := backfillSiteSummaries(ctx, database, reader, tc.limit)
		if err != nil || n != tc.want {
			t.Fatalf("backfill %d: %d %v", tc.limit, n, err)
		}
	}
	sites, err := db.ListSitesByUser(ctx, database, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range sites {
		if site.Summary.Title != site.Name || site.Summary.Headings != "Welcome" {
			t.Fatalf("summary: %+v", site.Summary)
		}
		var held bool
		if err := database.QueryRow(`SELECT summary IS NULL FROM versions WHERE site_id=$1 AND version_number=2`, site.ID).Scan(&held); err != nil || !held {
			t.Fatalf("backfilled held version: %v", err)
		}
		// A concurrent writer cannot be overwritten by a rerun or stale candidate.
		if err := db.SaveVersionSummary(ctx, database, site.ID, 1, db.SiteSummary{Title: "stale"}); err != nil {
			t.Fatal(err)
		}
	}
	sites, err = db.ListSitesByUser(ctx, database, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range sites {
		if site.Summary.Title != site.Name {
			t.Fatal("overwrote existing summary")
		}
	}
}
