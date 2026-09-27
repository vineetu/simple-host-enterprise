package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"

	db "github.com/vsriram/simple-host/internal/db"
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
	if m := manifest(); m.Site != "renamed" || m.Owner != "alice" || m.OwnerKind != "person" || m.OwnerIdentity != db.ErasedSubjectHash("", "sub-alice") {
		t.Fatalf("after rename: %+v", m)
	}
	seq := manifest().Seq

	// An admin's restriction, a delete and a restore are recorded too, so a
	// rebuild brings the site back as it was.
	if rec := w.adminForm("/api/admin/sites/alice/renamed/restrict", url.Values{"reason": {"exposes data"}}); rec.Code != http.StatusOK {
		t.Fatalf("restrict = %d %s", rec.Code, rec.Body)
	}
	if m := manifest(); !m.Restricted || m.RestrictedReason != "exposes data" || m.Seq <= seq {
		t.Fatalf("after restrict: %+v", m)
	}
	if rec := w.api("alice", http.MethodDelete, "/api/sites/renamed", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	if m := manifest(); m.DeletedAt == nil {
		t.Fatalf("after delete: %+v", m)
	}
	if rec := w.api("alice", http.MethodPost, "/api/sites/renamed/restore", nil); rec.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", rec.Code, rec.Body)
	}
	if m := manifest(); m.DeletedAt != nil || !m.Restricted {
		t.Fatalf("after restore: %+v", m)
	}
	if rec := w.adminForm("/api/admin/sites/alice/renamed/unrestrict", nil); rec.Code != http.StatusOK {
		t.Fatalf("unrestrict = %d %s", rec.Code, rec.Body)
	}
	if m := manifest(); m.Restricted {
		t.Fatalf("after unrestrict: %+v", m)
	}

	// An admin renaming the person: the manifest follows.
	if rec := w.adminForm("/api/admin/users/alice/rename", url.Values{"name": {"alicia"}}); rec.Code != http.StatusOK {
		t.Fatalf("admin rename = %d %s", rec.Code, rec.Body)
	}
	if m := manifest(); m.Owner != "alicia" {
		t.Fatalf("after the person's rename: %+v", m)
	}
}

// Erasing a person deletes their sites' manifests at once, so a rebuild
// from the bucket never brings them back.
func TestEraseDropsSiteManifests(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	key, _ := storage.ManifestKey(w.siteID("alice", "demo"))
	if _, err := w.store.Objects().Get(context.Background(), key, 1<<20); err != nil {
		t.Fatalf("manifest before: %v", err)
	}
	if rec := w.adminForm("/api/admin/users/alice/disable", nil); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body)
	}
	if rec := w.adminForm("/api/admin/users/alice/erase", url.Values{"confirm": {"alice"}}); rec.Code != http.StatusOK {
		t.Fatalf("erase = %d %s", rec.Code, rec.Body)
	}
	if _, err := w.store.Objects().Get(context.Background(), key, 1<<20); !errors.Is(err, storage.ErrObjectNotFound) {
		t.Fatalf("manifest after erasure: %v", err)
	}
}
