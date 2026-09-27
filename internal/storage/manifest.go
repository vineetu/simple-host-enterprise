package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// A site's manifest, sites/<site-id>/manifest.json, is the one object that
// says whose a site is: its owner and name, the version that was live and
// its uploaded files' names and types, as of the last deploy, rollback,
// rename, hand-over or file change. The database is the source of truth and
// nothing serving reads the manifest; it exists so that a lost database can
// be rebuilt from the bucket alone (`simple-host rebuild-index`). It is
// written through the same Objects as everything else, so it is encrypted
// the same way, and it goes with the site's prefix when the site is purged.
//
// Saved data, its history, access levels and viewers live only in the
// database and are not in it.

// manifestName is the manifest's name within a site's prefix.
const manifestName = "manifest.json"

// maxManifestBytes bounds a manifest read; a site's asset list is capped
// well below this (ASSET_MAX_SITE_COUNT).
const maxManifestBytes = 16 << 20

// SiteManifest is the manifest's JSON.
type SiteManifest struct {
	SiteID      string          `json:"site_id"`
	Owner       string          `json:"owner"`
	Site        string          `json:"site"`
	LiveVersion int             `json:"live_version"`
	WrittenAt   time.Time       `json:"written_at"`
	Assets      []ManifestAsset `json:"assets"`
}

// ManifestAsset is one uploaded file as the manifest records it: enough to
// recreate its row (SHA256 is hex).
type ManifestAsset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// ManifestKey names a site's manifest.
func ManifestKey(siteID string) (string, error) {
	prefix, err := SitePrefix(siteID)
	if err != nil {
		return "", err
	}
	return prefix + manifestName, nil
}

// PutSiteManifest writes m under its site's prefix, replacing the last one.
func PutSiteManifest(ctx context.Context, objects Objects, m SiteManifest) error {
	key, err := ManifestKey(m.SiteID)
	if err != nil {
		return err
	}
	if m.Assets == nil {
		m.Assets = []ManifestAsset{}
	}
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return objects.Put(ctx, key, body, "application/json")
}

// PutSiteManifest is PutSiteManifest on the store's bucket.
func (s *Store) PutSiteManifest(ctx context.Context, m SiteManifest) error {
	return PutSiteManifest(ctx, s.objects, m)
}

// RecoverableSite is one site found in the bucket: what its manifest says
// (nil when it has none, for a site not deployed since manifests were
// written) and which versions and uploaded files are actually there.
type RecoverableSite struct {
	SiteID      string
	Manifest    *SiteManifest
	ManifestErr error
	Versions    []int
	AssetIDs    map[string]bool
}

// NewestVersion is the version to make live on a rebuild: the manifest's
// live version when its archive is there, else the newest archive, else 0.
func (r RecoverableSite) NewestVersion() int {
	newest := 0
	for _, v := range r.Versions {
		if r.Manifest != nil && v == r.Manifest.LiveVersion {
			return v
		}
		newest = max(newest, v)
	}
	return newest
}

// ListRecoverableSites walks everything under sites/ and groups it by site,
// reading each manifest. Sorted by owner, then site, then id; sites
// without a manifest last.
func ListRecoverableSites(ctx context.Context, objects Objects) ([]RecoverableSite, error) {
	listed, err := objects.List(ctx, "sites/")
	if err != nil {
		return nil, err
	}
	byID := map[string]*RecoverableSite{}
	site := func(id string) *RecoverableSite {
		if byID[id] == nil {
			byID[id] = &RecoverableSite{SiteID: id, AssetIDs: map[string]bool{}}
		}
		return byID[id]
	}
	for _, o := range listed {
		rest, found := strings.CutPrefix(o.Key, "sites/")
		if !found || len(rest) < 37 || !isUUID(rest[:36]) || rest[36] != '/' {
			continue
		}
		id, name := rest[:36], rest[37:]
		switch {
		case name == manifestName:
			site(id)
		case strings.HasPrefix(name, "assets/") && isUUID(strings.TrimPrefix(name, "assets/")):
			site(id).AssetIDs[strings.TrimPrefix(name, "assets/")] = true
		default:
			if n, ok := cutVersionArchiveName(name); ok && "v"+fmt.Sprint(n)+".tar.gz" == name {
				s := site(id)
				s.Versions = append(s.Versions, n)
			}
		}
	}
	out := make([]RecoverableSite, 0, len(byID))
	for id, s := range byID {
		sort.Ints(s.Versions)
		key, _ := ManifestKey(id)
		body, err := objects.Get(ctx, key, maxManifestBytes)
		switch {
		case err == nil:
			var m SiteManifest
			if err := json.Unmarshal(body, &m); err != nil || m.SiteID != id {
				s.ManifestErr = fmt.Errorf("manifest unreadable: %v", err)
			} else {
				s.Manifest = &m
			}
		case errors.Is(err, ErrObjectNotFound):
		default:
			s.ManifestErr = err
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Manifest == nil) != (b.Manifest == nil) {
			return a.Manifest != nil
		}
		if a.Manifest != nil && a.Manifest.Owner != b.Manifest.Owner {
			return a.Manifest.Owner < b.Manifest.Owner
		}
		if a.Manifest != nil && a.Manifest.Site != b.Manifest.Site {
			return a.Manifest.Site < b.Manifest.Site
		}
		return a.SiteID < b.SiteID
	})
	return out, nil
}
