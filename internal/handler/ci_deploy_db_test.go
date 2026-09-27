package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/storage"
)

// The one call a CI job needs: PUT with ?create=true creates a missing site
// (201) and updates an existing one (200); without the flag a missing site
// is still 404, and a create of an existing site names the call that works.
func TestCIDeployCreatesWhenMissing(t *testing.T) {
	w := newAccessWorld(t)
	zip := func(body string) []byte { return zipOf(t, map[string][]byte{"index.html": []byte(body)}) }

	if rec := w.api("alice", http.MethodPut, "/api/sites/ci-site", zip("one")); rec.Code != http.StatusNotFound {
		t.Fatalf("update of a missing site without create = %d %s, want 404", rec.Code, rec.Body)
	}
	if rec := w.api("alice", http.MethodPut, "/api/sites/ci-site?create=maybe", zip("one")); rec.Code != http.StatusBadRequest {
		t.Fatalf("create=maybe = %d %s, want 400", rec.Code, rec.Body)
	}
	if rec := w.api("alice", http.MethodPut, "/api/sites/ci-site?create=true", zip("one")); rec.Code != http.StatusCreated {
		t.Fatalf("first create=true deploy = %d %s, want 201", rec.Code, rec.Body)
	}
	rec := w.api("alice", http.MethodPut, "/api/sites/ci-site?create=true", zip("two"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"active_version":2`) {
		t.Fatalf("second create=true deploy = %d %s, want 200 at version 2", rec.Code, rec.Body)
	}
	rec = w.api("alice", http.MethodPost, "/api/sites/ci-site", zip("three"))
	if rec.Code != http.StatusConflict || !containsCode(rec.Body.Bytes(), "site_exists") || !strings.Contains(rec.Body.String(), "?create=true") {
		t.Fatalf("create of an existing site = %d %s, want 409 site_exists naming ?create=true", rec.Code, rec.Body)
	}

	// Team sites: the owner-qualified update route takes the same flag.
	ctx := context.Background()
	tx, _ := w.database.Begin()
	team, err := db.CreateTeam(ctx, tx, "builds", w.users["mo"])
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	path := "/api/collaboration/sites/" + team.Username + "/docs"
	if rec := w.api("mo", http.MethodPut, path+"?create=true", zip("team")); rec.Code != http.StatusCreated {
		t.Fatalf("team create=true deploy = %d %s, want 201", rec.Code, rec.Body)
	}
	// Someone outside the team gets the update route's own refusal, not a create.
	if rec := w.api("alice", http.MethodPut, "/api/collaboration/sites/"+team.Username+"/other?create=true", zip("x")); rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
		t.Fatalf("outsider create=true into a team = %d %s, want a refusal", rec.Code, rec.Body)
	}
}

// Every deploy, rollback and rename rewrites the site's bucket manifest,
// which is what rebuild-index reads when the database is lost.
func TestSiteManifestFollowsDeployAndRename(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	siteID := w.siteID("alice", "demo")
	manifest := func() storage.SiteManifest {
		t.Helper()
		key, _ := storage.ManifestKey(siteID)
		body, err := w.store.Objects().Get(context.Background(), key, 1<<20)
		if err != nil {
			t.Fatalf("manifest: %v", err)
		}
		var m storage.SiteManifest
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := manifest(); m.Owner != "alice" || m.Site != "demo" || m.LiveVersion != 1 || m.SiteID != siteID {
		t.Fatalf("after create: %+v", m)
	}
	if rec := w.api("alice", http.MethodPut, "/api/sites/demo", zipOf(t, map[string][]byte{"index.html": []byte("two")})); rec.Code != http.StatusOK {
		t.Fatalf("update = %d %s", rec.Code, rec.Body)
	}
	if m := manifest(); m.LiveVersion != 2 {
		t.Fatalf("after update: %+v", m)
	}
	if rec := w.api("alice", http.MethodPost, "/api/sites/demo/rename", map[string]any{"name": "renamed"}); rec.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body)
	}
	if m := manifest(); m.Site != "renamed" || m.Owner != "alice" {
		t.Fatalf("after rename: %+v", m)
	}
}
