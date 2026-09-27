package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/handler"
	"github.com/vsriram/simple-host/internal/storage"
)

// runRebuildIndex lists every site the bucket alone can bring back after the
// database is lost (and its point-in-time recovery with it), from each
// site's manifest (internal/storage/manifest.go): owner, name, the versions
// and uploaded files that are there. With -apply it recreates each one
// whose owner is back in the database (people sign in once; a team is
// created again by one of its members) under the same site id, so every
// object's key still matches: pages, all kept versions and uploaded files.
// Saved data, its history, access levels, viewers and team members were
// only ever in the database; a recreated site opens only for its owner or
// team until its access level is set again. Without -apply it changes
// nothing.
func runRebuildIndex(args []string) error {
	fs := flag.NewFlagSet("rebuild-index", flag.ContinueOnError)
	apply := fs.Bool("apply", false, "recreate the sites listed as recoverable (default: list only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	database, objects, err := openDatabaseAndObjects(cfg)
	if err != nil {
		return err
	}
	defer database.Close()
	_, err = rebuildIndex(context.Background(), database, objects, os.Stdout, *apply)
	return err
}

// rebuildResult counts what a rebuild-index run found and did.
type rebuildResult struct {
	Recoverable, Recreated, Waiting, Present, Unnamed int
}

func rebuildIndex(ctx context.Context, database *sql.DB, objects storage.Objects, out io.Writer, apply bool) (rebuildResult, error) {
	var res rebuildResult
	sites, err := storage.ListRecoverableSites(ctx, objects)
	if err != nil {
		return res, fmt.Errorf("list the bucket: %w", err)
	}
	recorder := audit.NewDBRecorder(database)
	stream := audit.NewStream(slog.New(slog.NewJSONHandler(os.Stdout, nil)), 0)
	defer stream.Close(5 * time.Second)
	recorder.SetStream(stream)

	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "SITE ID\tOWNER\tSITE\tVERSIONS\tLIVE\tFILES\tSTATUS")
	for _, s := range sites {
		owner, name := "?", "?"
		if s.Manifest != nil {
			owner, name = s.Manifest.Owner, s.Manifest.Site
		}
		status := rebuildOne(ctx, database, objects, recorder, s, apply, &res)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%s\n", s.SiteID, owner, name, len(s.Versions), s.NewestVersion(), len(s.AssetIDs), status)
	}
	if err := tw.Flush(); err != nil {
		return res, err
	}
	fmt.Fprintf(out, "\n%d sites in the bucket: %d recoverable now, %d recreated, %d waiting for their owner, %d already in the database, %d without a manifest.\n",
		len(sites), res.Recoverable, res.Recreated, res.Waiting, res.Present, res.Unnamed)
	fmt.Fprintln(out, "Pages and uploaded files come back; saved data, its history, access levels, viewers and team members were only in the database and do not. A recreated site opens only for its owner or team until its access level is set again.")
	if !apply && res.Recoverable > 0 {
		fmt.Fprintln(out, "Nothing was changed. Run again with -apply to recreate the recoverable sites.")
	}
	return res, nil
}

// rebuildOne decides one site's status and, with apply, recreates it.
func rebuildOne(ctx context.Context, database *sql.DB, objects storage.Objects, recorder *audit.DBRecorder, s storage.RecoverableSite, apply bool, res *rebuildResult) string {
	if s.Manifest == nil {
		res.Unnamed++
		why := "no manifest (not deployed since manifests were written)"
		if s.ManifestErr != nil {
			why = s.ManifestErr.Error()
		}
		if v := s.NewestVersion(); v > 0 {
			return why + fmt.Sprintf("; restore it by hand: simple-host restore -from-site-id %s -version %d -owner <owner> -site <name>", s.SiteID, v)
		}
		return why
	}
	live := s.NewestVersion()
	if live == 0 {
		return "no version archives in the bucket"
	}
	var exists bool
	if err := database.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sites WHERE id = $1::uuid)`, s.SiteID).Scan(&exists); err != nil {
		return "error: " + err.Error()
	}
	if exists {
		res.Present++
		return "already in the database"
	}
	user, err := db.GetUserByUsername(ctx, database, s.Manifest.Owner)
	if errors.Is(err, sql.ErrNoRows) {
		res.Waiting++
		if strings.HasPrefix(s.Manifest.Owner, "team-") {
			return "waiting: a member creates the team " + s.Manifest.Owner + " again, then run again"
		}
		return "waiting: " + s.Manifest.Owner + " signs in once, then run again"
	}
	if err != nil {
		return "error: " + err.Error()
	}
	if !apply {
		res.Recoverable++
		return "recoverable"
	}
	if err := recreateSite(ctx, database, objects, recorder, s, user.ID, live); err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return "not recreated: " + s.Manifest.Owner + " already has a site named " + s.Manifest.Site
		}
		return "not recreated: " + err.Error()
	}
	res.Recreated++
	return "recreated"
}

func recreateSite(ctx context.Context, database *sql.DB, objects storage.Objects, recorder *audit.DBRecorder, s storage.RecoverableSite, ownerID string, live int) error {
	var assets []db.SiteManifestAsset
	for _, a := range s.Manifest.Assets {
		if s.AssetIDs[a.ID] {
			assets = append(assets, db.SiteManifestAsset{ID: a.ID, Name: a.Name, ContentType: a.ContentType, Size: a.Size, SHA256: a.SHA256})
		}
	}
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer audit.Rollback(tx)
	if err := db.LockSiteCollaboration(ctx, tx, ownerID, s.Manifest.Site); err != nil {
		return err
	}
	if err := db.InsertRebuiltSite(ctx, tx, db.RebuiltSite{
		SiteID: s.SiteID, OwnerID: ownerID, Name: s.Manifest.Site, Versions: s.Versions, LiveVersion: live, Assets: assets,
		VersionKey: func(v int) string { key, _ := storage.VersionKey(s.SiteID, v); return key },
	}); err != nil {
		return err
	}
	if err := db.EnqueueSiteSearch(ctx, tx, s.SiteID, db.SiteSearchReconcile); err != nil {
		return err
	}
	if err := recorder.RecordTx(ctx, tx, audit.Event{
		ActorKind: "system", Action: "site_restore", OwnerID: ownerID, SiteID: s.SiteID,
		Extra: map[string]any{"from": "bucket_rebuild", "live_version": live, "versions": len(s.Versions), "files": len(assets)},
	}); err != nil {
		return fmt.Errorf("record audit: %w", err)
	}
	if err := audit.Commit(tx); err != nil {
		return err
	}
	if err := handler.WriteSiteManifest(ctx, database, objects, s.SiteID); err != nil {
		fmt.Fprintf(os.Stderr, "rebuild-index: rewrite the manifest of %s: %v\n", s.SiteID, err)
	}
	return nil
}
