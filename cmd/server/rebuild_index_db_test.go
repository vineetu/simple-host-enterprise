package main

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/handler"
	"github.com/vsriram/simple-host/internal/storage"
)

const rebuildIssuer = "https://issuer.example.test"

// The database is lost and the bucket is not: rebuild-index names every
// site from its manifest and gives it back only to the account with the
// same sign-in identity (not whoever took the username), under the same
// id, with its versions and uploaded files, and it serves.
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
	if err := handler.WriteSiteManifest(ctx, before, objects, rebuildIssuer, site.ID); err != nil {
		t.Fatal(err)
	}
	if err := handler.WriteSiteManifest(ctx, before, objects, rebuildIssuer, site.ID); err != nil {
		t.Fatal(err)
	}
	m, err := storage.GetSiteManifest(ctx, objects, site.ID)
	if err != nil || m.Seq != 2 || m.OwnerKind != "person" || m.OwnerIdentity != db.ErasedSubjectHash(rebuildIssuer, "sub-rena") {
		t.Fatalf("manifest = %+v, %v; want seq 2 and rena's identity hash", m, err)
	}
	if raw, _ := objects.Get(ctx, "sites/"+site.ID+"/manifest.json", 1<<20); bytes.Contains(raw, []byte("rena@example.com")) {
		t.Fatal("the manifest holds the owner's email")
	}
	store.Close()

	// After: a new, empty database.
	after := operatorTestDB(t)
	run := func(opts rebuildOptions) (rebuildResult, string) {
		t.Helper()
		opts.Issuer = rebuildIssuer
		var out bytes.Buffer
		res, err := rebuildIndex(ctx, after, objects, &out, opts)
		if err != nil {
			t.Fatalf("rebuild: %v\n%s", err, out.String())
		}
		return res, out.String()
	}
	if res, out := run(rebuildOptions{Apply: true}); res.Waiting != 1 || res.Recreated != 0 || !strings.Contains(out, "rena signs in once") {
		t.Fatalf("before the owner is back: %+v\n%s", res, out)
	}
	// Somebody else signs in first and is handed the username: the sites
	// are not theirs.
	if _, err := db.CreateOIDCUser(ctx, after, "rena", "sub-impostor", "rena@other.example.com", false); err != nil {
		t.Fatal(err)
	}
	if res, out := run(rebuildOptions{Apply: true}); res.Waiting != 1 || res.Recreated != 0 {
		t.Fatalf("with someone else holding the name: %+v\n%s", res, out)
	}
	// Nor does a mapping to them override the identity once it is back.
	back, err := db.CreateOIDCUser(ctx, after, "rena-2", "sub-rena", "rena@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	if res, out := run(rebuildOptions{Map: map[string]string{"rena": "rena"}}); res.Refused != 1 || !strings.Contains(out, "belongs to rena-2") {
		t.Fatalf("a mapping against the identity: %+v\n%s", res, out)
	}
	res, out := run(rebuildOptions{})
	if res.Recoverable != 1 || !strings.Contains(out, "to rena-2 (by sign-in identity)") || !strings.Contains(out, "Nothing was changed") {
		t.Fatalf("list only: %+v\n%s", res, out)
	}
	if n := countRows(t, after, `SELECT count(*) FROM sites`); n != 0 {
		t.Fatalf("a list-only run created %d sites", n)
	}
	if res, out := run(rebuildOptions{Apply: true}); res.Recreated != 1 {
		t.Fatalf("apply: %+v\n%s", res, out)
	}

	var id, ownerID string
	var live, versions, assets int
	if err := after.QueryRow(`SELECT s.id::text, s.user_id::text, s.active_version, (SELECT count(*) FROM versions v WHERE v.site_id = s.id),
		(SELECT count(*) FROM site_assets a WHERE a.site_id = s.id AND a.name = 'hello.txt') FROM sites s WHERE s.name = 'notes'`).Scan(&id, &ownerID, &live, &versions, &assets); err != nil {
		t.Fatal(err)
	}
	if id != site.ID || ownerID != back.ID || live != 1 || versions != 2 || assets != 1 {
		t.Fatalf("recreated site = id %s owner %s live %d versions %d assets %d; want the same id, rena-2, v1 live, two versions, the file", id, ownerID, live, versions, assets)
	}
	restored, err := storage.New(storage.Options{Objects: objects, Index: storage.NewDBIndex(after), CacheDir: t.TempDir(), CacheMaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if got := drillServes(t, restored, "rena-2", "notes"); got != "<h1>first</h1>" {
		t.Fatalf("recreated site serves %q", got)
	}

	// Now the database has sites: -apply refuses without -force-live-db.
	var buf bytes.Buffer
	if _, err := rebuildIndex(ctx, after, objects, &buf, rebuildOptions{Apply: true, Issuer: rebuildIssuer}); err == nil || !strings.Contains(err.Error(), "-force-live-db") {
		t.Fatalf("apply against a database with sites: %v", err)
	}
	if res, out := run(rebuildOptions{Apply: true, ForceLiveDB: true}); res.Present != 1 || res.Recreated != 0 {
		t.Fatalf("second run: %+v\n%s", res, out)
	}
}

// What the bucket says is checked before anything is written: names and
// files as a create would check them, the live version's own archive (never
// a newer one nobody made live), and a site deleted or restricted comes
// back that way.
func TestRebuildIndexRefusesAndKeepsState(t *testing.T) {
	ctx := context.Background()
	objects := storage.NewMemoryObjects()
	database := operatorTestDB(t)
	person, err := db.CreateOIDCUser(ctx, database, "pat", "sub-pat", "pat@example.com", false)
	if err != nil {
		t.Fatal(err)
	}
	identity := db.ErasedSubjectHash(rebuildIssuer, "sub-pat")
	ids := map[string]string{}
	add := func(label string, m storage.SiteManifest, versions ...int) {
		t.Helper()
		id := newUUID(t, database)
		ids[label] = id
		for _, v := range versions {
			key, _ := storage.VersionKey(id, v)
			if err := objects.Put(ctx, key, []byte("x"), "application/octet-stream"); err != nil {
				t.Fatal(err)
			}
		}
		m.SiteID = id
		if m.OwnerKind == "" {
			m.Owner, m.OwnerKind, m.OwnerIdentity = "pat", "person", identity
		}
		if err := storage.PutSiteManifest(ctx, objects, m); err != nil {
			t.Fatal(err)
		}
	}
	deletedAt := time.Now().Add(-time.Hour).UTC()
	longAgo := time.Now().Add(-400 * 24 * time.Hour).UTC()
	add("good", storage.SiteManifest{Site: "good", LiveVersion: 1}, 1)
	add("deleted", storage.SiteManifest{Site: "gone", LiveVersion: 1, DeletedAt: &deletedAt}, 1)
	add("expired", storage.SiteManifest{Site: "old", LiveVersion: 1, DeletedAt: &longAgo}, 1)
	add("restricted", storage.SiteManifest{Site: "taken-down", LiveVersion: 1, Restricted: true, RestrictedReason: "phishing"}, 1)
	add("held", storage.SiteManifest{Site: "held", LiveVersion: 1}, 2) // only a newer, never-live archive
	add("badsite", storage.SiteManifest{Site: "../payroll", LiveVersion: 1}, 1)
	add("badowner", storage.SiteManifest{Owner: "CEO", OwnerKind: "person", OwnerIdentity: identity, Site: "x", LiveVersion: 1}, 1)
	add("badtype", storage.SiteManifest{Site: "files", LiveVersion: 1, Assets: []storage.ManifestAsset{
		{ID: newUUID(t, database), Name: "a.html", ContentType: "text/html", Size: 1, SHA256: strings.Repeat("0", 64)}}}, 1)
	add("team", storage.SiteManifest{Owner: "team-ops", OwnerKind: "team", TeamID: newUUID(t, database), Site: "board", LiveVersion: 1}, 1)

	var out bytes.Buffer
	res, err := rebuildIndex(ctx, database, objects, &out, rebuildOptions{Apply: true, Issuer: rebuildIssuer})
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if res.Recreated != 3 || res.Refused != 5 || res.Waiting != 1 {
		t.Fatalf("result = %+v, want good, gone and taken-down recreated; five refused; the team waiting\n%s", res, text)
	}
	for _, want := range []string{"archive is not in the bucket", "recovery window has ended", `site name "../payroll"`, `owner name "CEO"`, `content type "text/html"`, "create the team again"} {
		if !strings.Contains(text, want) {
			t.Errorf("listing does not say %q:\n%s", want, text)
		}
	}
	for _, label := range []string{"held", "badsite", "badowner", "badtype", "expired", "team"} {
		if n := countRows(t, database, `SELECT count(*) FROM sites WHERE id = $1::uuid`, ids[label]); n != 0 {
			t.Errorf("%s was recreated", label)
		}
	}
	var deleted sql.NullTime
	if err := database.QueryRow(`SELECT deleted_at FROM sites WHERE id = $1::uuid`, ids["deleted"]).Scan(&deleted); err != nil || !deleted.Valid || deleted.Time.Sub(deletedAt).Abs() > time.Millisecond {
		t.Fatalf("deleted site came back with deleted_at %v (%v), want %v", deleted, err, deletedAt)
	}
	var decision, reason, access string
	if err := database.QueryRow(`SELECT COALESCE(access_decision, ''), COALESCE(access_decision_reason, ''), access FROM sites WHERE id = $1::uuid`, ids["restricted"]).Scan(&decision, &reason, &access); err != nil ||
		decision != "restricted" || reason != "phishing" || access != "only_me" {
		t.Fatalf("restricted site came back as %q %q %q (%v)", decision, reason, access, err)
	}
	var owner string
	_ = database.QueryRow(`SELECT user_id::text FROM sites WHERE id = $1::uuid`, ids["good"]).Scan(&owner)
	if owner != person.ID {
		t.Fatalf("good went to %s", owner)
	}

	// A team created again gets a new id: only an explicit mapping gives it
	// the team's sites.
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTeam(ctx, tx, "team-ops", person.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if res, err := rebuildIndex(ctx, database, objects, &out, rebuildOptions{Apply: true, ForceLiveDB: true, Issuer: rebuildIssuer}); err != nil || res.Waiting != 1 {
		t.Fatalf("team back under a new id, no mapping: %+v %v\n%s", res, err, out.String())
	}
	out.Reset()
	if res, err := rebuildIndex(ctx, database, objects, &out, rebuildOptions{Apply: true, ForceLiveDB: true, Issuer: rebuildIssuer, Map: map[string]string{"team-ops": "team-ops"}}); err != nil || res.Recreated != 1 {
		t.Fatalf("team mapped: %+v %v\n%s", res, err, out.String())
	}
}

// Two manifests naming one owner with different identities are refused,
// not settled by whoever has the name.
func TestRebuildIndexAmbiguousOwner(t *testing.T) {
	ctx := context.Background()
	objects := storage.NewMemoryObjects()
	database := operatorTestDB(t)
	if _, err := db.CreateOIDCUser(ctx, database, "sam", "sub-sam", "sam@example.com", false); err != nil {
		t.Fatal(err)
	}
	for i, sub := range []string{"sub-sam", "sub-other"} {
		id := newUUID(t, database)
		key, _ := storage.VersionKey(id, 1)
		_ = objects.Put(ctx, key, []byte("x"), "application/octet-stream")
		if err := storage.PutSiteManifest(ctx, objects, storage.SiteManifest{SiteID: id, Owner: "sam", OwnerKind: "person",
			OwnerIdentity: db.ErasedSubjectHash(rebuildIssuer, sub), Site: []string{"one", "two"}[i], LiveVersion: 1}); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	res, err := rebuildIndex(ctx, database, objects, &out, rebuildOptions{Apply: true, Issuer: rebuildIssuer})
	if err != nil || res.Refused != 2 || res.Recreated != 0 || !strings.Contains(out.String(), "different sign-in identities") {
		t.Fatalf("ambiguous owner: %+v %v\n%s", res, err, out.String())
	}
}

func newUUID(t *testing.T, database *sql.DB) string {
	t.Helper()
	var id string
	if err := database.QueryRow(`SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func countRows(t *testing.T, database *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := database.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
