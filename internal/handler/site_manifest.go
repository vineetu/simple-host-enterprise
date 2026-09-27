package handler

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/storage"
)

// siteManifestTimeout bounds the manifest write a change waits for.
const siteManifestTimeout = 5 * time.Second

// refreshSiteManifest rewrites a site's bucket manifest (storage.SiteManifest)
// from the database after a change to what it records: a deploy, a
// rollback, a rename or hand-over, an uploaded file added or deleted. Best
// effort: nothing serving reads it, so a failure is logged and the next
// change writes it again. Called while the site's mutation lock is still
// held, so two changes to one site write in order.
func refreshSiteManifest(ctx context.Context, database *sql.DB, store *storage.Store, siteID string) {
	if store == nil || database == nil || siteID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), siteManifestTimeout)
	defer cancel()
	if err := WriteSiteManifest(ctx, database, store.Objects(), siteID); err != nil {
		log.Printf("site manifest %s: %v", siteID, err)
	}
}

// WriteSiteManifest writes one site's manifest from the database. A site
// that is gone is not an error: there is nothing to write. Operator
// commands that change a site (restore, rebuild-index) call it too.
func WriteSiteManifest(ctx context.Context, database db.Querier, objects storage.Objects, siteID string) error {
	data, err := db.LoadSiteManifestData(ctx, database, siteID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	m := storage.SiteManifest{SiteID: siteID, Owner: data.Owner, Site: data.Site, LiveVersion: data.LiveVersion, WrittenAt: time.Now().UTC()}
	for _, a := range data.Assets {
		m.Assets = append(m.Assets, storage.ManifestAsset{ID: a.ID, Name: a.Name, ContentType: a.ContentType, Size: a.Size, SHA256: a.SHA256})
	}
	return storage.PutSiteManifest(ctx, objects, m)
}
