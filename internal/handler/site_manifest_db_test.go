package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/vsriram/simple-host/internal/storage"
)

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
