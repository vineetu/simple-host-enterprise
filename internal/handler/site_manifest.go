package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/safepath"
	"github.com/vsriram/simple-host/internal/storage"
)

// siteManifestTimeout bounds the manifest write a change waits for.
const siteManifestTimeout = 5 * time.Second

// refreshSiteManifest rewrites a site's bucket manifest (storage.SiteManifest)
// from the database after a committed change to what it records: a deploy,
// a rollback, a rename or hand-over, a delete or restore, an admin's
// restriction, an uploaded file added or deleted. Best effort: nothing
// serving reads it, so a failure is logged and the next change writes it
// again. WriteSiteManifest orders concurrent writes, so the last one to run
// reads the newest committed state.
func refreshSiteManifest(ctx context.Context, database *sql.DB, store *storage.Store, siteID string) {
	if store == nil || database == nil || siteID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), siteManifestTimeout)
	defer cancel()
	if err := WriteSiteManifest(ctx, database, store.Objects(), store.OwnerIssuer(), siteID); err != nil {
		log.Printf("site manifest %s: %v", siteID, err)
	}
}

// WriteSiteManifest writes one site's manifest from the database's
// committed state. Writes for one site take turns (db.LockSiteManifest):
// each reads the state after the previous one finished and numbers its
// manifest one past the one it replaces, so a slow write never overwrites
// a newer one. A site that is gone is not an error: there is nothing to
// write. issuer is OIDC_ISSUER, for the owner's identity hash. Operator
// commands that change a site (restore, rebuild-index) call it too.
func WriteSiteManifest(ctx context.Context, database *sql.DB, objects storage.Objects, issuer, siteID string) error {
	return withSiteManifestLock(ctx, database, siteID, func(tx *sql.Tx) error {
		_, err := writeSiteManifestLocked(ctx, tx, objects, issuer, siteID)
		return err
	})
}

// ReencryptSiteManifest is `simple-host reencrypt`'s rewrite of a manifest
// under the current envelope key: in turn with every other write of it,
// from the database when the site is there (so it cannot put back an older
// manifest), else its current bytes as they are.
func ReencryptSiteManifest(ctx context.Context, database *sql.DB, objects storage.Objects, issuer, siteID string) error {
	return withSiteManifestLock(ctx, database, siteID, func(tx *sql.Tx) error {
		written, err := writeSiteManifestLocked(ctx, tx, objects, issuer, siteID)
		if err != nil || written {
			return err
		}
		key, err := storage.ManifestKey(siteID)
		if err != nil {
			return err
		}
		body, err := objects.Get(ctx, key, 16<<20)
		if errors.Is(err, storage.ErrObjectNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return objects.Put(ctx, key, body, "application/json")
	})
}

func withSiteManifestLock(ctx context.Context, database *sql.DB, siteID string, fn func(tx *sql.Tx) error) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer audit.Rollback(tx)
	if err := db.LockSiteManifest(ctx, tx, siteID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// writeSiteManifestLocked writes the manifest under the caller's lock.
// written is false when the site is not in the database.
func writeSiteManifestLocked(ctx context.Context, tx *sql.Tx, objects storage.Objects, issuer, siteID string) (written bool, err error) {
	data, err := db.LoadSiteManifestData(ctx, tx, siteID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var seq int64
	if prev, err := storage.GetSiteManifest(ctx, objects, siteID); err == nil {
		seq = prev.Seq
	} else if !errors.Is(err, storage.ErrObjectNotFound) {
		// Unreadable (an old key, damage): start again past any number a
		// manifest could have, rather than fail the write.
		seq = time.Now().UnixMilli()
	}
	m := storage.SiteManifest{
		SiteID: siteID, Seq: seq + 1, Owner: data.Owner, OwnerKind: data.OwnerKind,
		Site: data.Site, LiveVersion: data.LiveVersion, DeletedAt: data.DeletedAt,
		Restricted: data.Restricted, RestrictedReason: data.RestrictedReason, WrittenAt: time.Now().UTC(),
	}
	switch data.OwnerKind {
	case storage.ManifestOwnerTeam:
		m.TeamID = data.OwnerID
	default:
		if data.OwnerSubject != "" {
			m.OwnerIdentity = db.ErasedSubjectHash(issuer, data.OwnerSubject)
		}
	}
	for _, a := range data.Assets {
		m.Assets = append(m.Assets, storage.ManifestAsset{ID: a.ID, Name: a.Name, ContentType: a.ContentType, Size: a.Size, SHA256: a.SHA256})
	}
	return true, storage.PutSiteManifest(ctx, objects, m)
}

// ValidateRebuiltNames applies the rules a create applies to a manifest's
// owner and site names before rebuild-index writes them: a person's name
// as sign-in would register it, a team's as team create would, and a site
// name as a new site's.
func ValidateRebuiltNames(owner, kind, site string) error {
	switch kind {
	case storage.ManifestOwnerTeam:
		if !strings.HasPrefix(owner, db.TeamPrefix) || validateTypedName(owner) != nil {
			return fmt.Errorf("team name %q is not one a team could be created with", owner)
		}
	default:
		if strings.HasPrefix(owner, db.TeamPrefix) || validateOwnerName(owner) != nil {
			return fmt.Errorf("owner name %q is not one sign-in could register", owner)
		}
	}
	if safepath.ValidateSegment(site) != nil || reservedSiteNames[site] || !isValidLabel(site) || strings.HasPrefix(site, "xn--") {
		return fmt.Errorf("site name %q is not one a new site could have", site)
	}
	return nil
}

// dropSiteManifests deletes the manifests of sites that are gone for good,
// once their deletion has committed. Best effort, like every manifest
// write.
func dropSiteManifests(ctx context.Context, store *storage.Store, siteIDs []string) {
	if store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), siteManifestTimeout)
	defer cancel()
	for _, id := range siteIDs {
		if err := storage.DeleteSiteManifest(ctx, store.Objects(), id); err != nil {
			log.Printf("site manifest %s: delete: %v", id, err)
		}
	}
}
