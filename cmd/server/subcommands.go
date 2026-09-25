package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/config"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/migrate"
	"github.com/vsriram/simple-host/internal/safepath"
	"github.com/vsriram/simple-host/internal/storage"
)

// runSubcommand dispatches the binary's non-server modes. The same image runs
// as the init container (`migrate`), the server, the prune CronJob and the
// one-off storage commands, so an operator only ever ships one artifact.
func runSubcommand(name string, args []string) error {
	switch name {
	case "migrate":
		return runMigrate(args)
	case "restore":
		return runRestore(args)
	case "migrate-storage":
		return runMigrateStorage(args)
	case "prune":
		return runPrune(args)
	case "version":
		fmt.Println(versionString())
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q (expected: migrate, restore, migrate-storage, prune, version)", name)
	}
}

func versionString() string {
	latest, err := migrate.Latest()
	if err != nil {
		return "simple-host (schema: unknown: " + err.Error() + ")"
	}
	return fmt.Sprintf("simple-host (schema %04d)", latest)
}

// runMigrate applies pending migrations, or with --status only reports them.
// It waits for the database rather than failing at once: in a fresh cluster
// the init container regularly starts before Postgres accepts connections.
func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	status := fs.Bool("status", false, "report pending migrations without applying them")
	wait := fs.Duration("wait", 2*time.Minute, "how long to wait for the database to accept connections")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dsn, err := config.LoadDatabase()
	if err != nil {
		return err
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := waitForDatabase(ctx, db, *wait); err != nil {
		return err
	}
	if *status {
		pending, err := migrate.Pending(ctx, db)
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			fmt.Println("schema is current")
			return nil
		}
		for _, m := range pending {
			fmt.Println("pending", m.Name)
		}
		return nil
	}
	applied, err := migrate.Apply(ctx, db, func(msg string) { log.Print(msg) })
	for _, name := range applied {
		log.Printf("applied %s", name)
	}
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		log.Print("schema is current")
	}

	// The application role's grants come from a migration, but its password
	// never does (design 9.3): set it every run, applied migrations or not,
	// so a rotated DB_APP_PASSWORD takes effect the next time migrate runs
	// without needing a schema change to carry it.
	appPassword, err := config.LoadAppRolePassword()
	if err != nil {
		return err
	}
	if err := migrate.SetAppRolePassword(ctx, db, appPassword); err != nil {
		return err
	}
	log.Printf("set %s password", migrate.AppRoleName)
	return nil
}

// runRestore copies one stored version of any site — live, or deleted but
// still in the bucket — into a target site as its next version, and by
// default makes it live. The copy is server side, and it and the database
// rows land under the target site's advisory lock, so a restore serializes
// with deploys exactly as another deploy would. A deleted site's id is in
// its site_delete audit event. An object already swept from the bucket has
// to be brought back from the bucket's own versioning first
// (docs/storage.md).
func runRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fromSiteID := fs.String("from-site-id", "", "id of the site the version was deployed to")
	version := fs.Int("version", 0, "version number to restore")
	owner := fs.String("owner", "", "username (or team name) that owns the target site")
	site := fs.String("site", "", "target site name; created if it does not exist")
	setCurrent := fs.Bool("set-current", true, "make the restored version the live one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := storage.VersionKey(*fromSiteID, *version); err != nil {
		return fmt.Errorf("-from-site-id and -version: %w", err)
	}
	if err := safepath.ValidateSegment(*owner); err != nil {
		return fmt.Errorf("-owner: %w", err)
	}
	if err := safepath.ValidateSegment(*site); err != nil {
		return fmt.Errorf("-site: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	database, objects, err := openDatabaseAndObjects(cfg)
	if err != nil {
		return err
	}
	defer database.Close()

	ctx := context.Background()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	user, err := db.GetUserByUsername(ctx, tx, *owner)
	if err != nil {
		return fmt.Errorf("owner %q: %w", *owner, err)
	}
	if err := db.LockSiteCollaboration(ctx, tx, user.ID, *site); err != nil {
		return err
	}
	target, err := db.GetSite(ctx, tx, user.ID, *site)
	created := false
	if errors.Is(err, sql.ErrNoRows) {
		target, err = db.CreateSite(ctx, tx, user.ID, *site)
		created = true
	}
	if err != nil {
		return fmt.Errorf("target site: %w", err)
	}
	maxVersion, err := db.GetMaxVersionNumber(ctx, tx, target.ID)
	if err != nil {
		return err
	}
	newVersion := maxVersion + 1
	if err := storage.CopyVersion(ctx, objects, *fromSiteID, *version, target.ID, newVersion); err != nil {
		if errors.Is(err, storage.ErrObjectNotFound) {
			return fmt.Errorf("%s v%d is not in the bucket; recover its noncurrent version with the bucket's versioning first: %w", *fromSiteID, *version, err)
		}
		return err
	}
	key, _ := storage.VersionKey(target.ID, newVersion)
	row, err := db.CreateVersion(ctx, tx, target.ID, newVersion, key, nil)
	if err != nil {
		return err
	}
	if err := db.ActivateVersion(ctx, tx, row.ID); err != nil {
		return err
	}
	if *setCurrent || created {
		if err := db.UpdateSiteActiveVersion(ctx, tx, target.ID, newVersion); err != nil {
			return err
		}
		if err := db.EnqueueSiteSearch(ctx, tx, target.ID, db.SiteSearchReconcile); err != nil {
			return err
		}
	}
	// Recorded in the same transaction, like every other change to what a
	// site serves; an operator command has no signed-in actor.
	if err := audit.NewDBRecorder(database).RecordTx(ctx, tx, audit.Event{
		ActorKind: "system", Action: "site_restore", OwnerID: user.ID, SiteID: target.ID,
		Extra: map[string]any{
			"from_site_id": *fromSiteID, "from_version": *version,
			"version": newVersion, "live": *setCurrent || created, "created_site": created,
		},
	}); err != nil {
		return fmt.Errorf("record audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	log.Printf("restored %s v%d into %s/%s as v%d (site %s, live=%t)", *fromSiteID, *version, *owner, *site, newVersion, target.ID, *setCurrent || created)
	return nil
}

// runMigrateStorage is the one-time move of an install that kept its sites
// on a volume (SITE_DIR, <owner>/<site>/vN and assets/<id>) into the bucket.
// It walks the database, not the volume: every retained version row and
// every live asset row is uploaded from the volume under its site id and read
// back to verify. Anything missing or failing is reported and makes the
// command exit non-zero; re-running it re-uploads, so it is safe to repeat.
func runMigrateStorage(args []string) error {
	fs := flag.NewFlagSet("migrate-storage", flag.ContinueOnError)
	from := fs.String("from", "", "the old site directory (the former SITE_DIR, e.g. /mnt/data/sites)")
	dryRun := fs.Bool("dry-run", false, "only report what would be uploaded")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *from == "" {
		return errors.New("-from is required")
	}
	tree, err := os.OpenRoot(*from)
	if err != nil {
		return err
	}
	defer tree.Close()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	database, objects, err := openDatabaseAndObjects(cfg)
	if err != nil {
		return err
	}
	defer database.Close()

	ctx := context.Background()
	users, err := db.ListAllUsers(ctx, database)
	if err != nil {
		return err
	}
	usernames := make(map[string]string, len(users))
	for _, user := range users {
		usernames[user.ID] = user.Username
	}
	sites, err := db.ListAllSites(ctx, database)
	if err != nil {
		return err
	}

	var versions, assets, failed int
	fail := func(format string, args ...any) {
		failed++
		log.Printf("FAILED "+format, args...)
	}
	for _, site := range sites {
		owner := usernames[site.UserID]
		if !safepath.IsSegment(owner) || !safepath.IsSegment(site.Name) {
			fail("site %s: unusable owner/site name %q/%q", site.ID, owner, site.Name)
			continue
		}
		siteDir, err := tree.OpenRoot(owner + "/" + site.Name)
		if err != nil {
			fail("%s/%s: open site directory: %v", owner, site.Name, err)
			continue
		}
		rows, err := db.ListVersions(ctx, database, site.ID)
		if err != nil {
			siteDir.Close()
			return err
		}
		for _, row := range rows {
			name := fmt.Sprintf("v%d", row.VersionNumber)
			bytes, err := migrateVersion(ctx, objects, siteDir, site.ID, row.VersionNumber, *dryRun)
			if err != nil {
				fail("%s/%s %s: %v", owner, site.Name, name, err)
				continue
			}
			versions++
			log.Printf("%s/%s %s -> site %s (%d bytes)%s", owner, site.Name, name, site.ID, bytes, dryRunSuffix(*dryRun))
		}
		assetRows, err := db.ListAssets(ctx, database, site.ID)
		if err != nil {
			siteDir.Close()
			return err
		}
		for _, asset := range assetRows {
			body, err := siteDir.ReadFile("assets/" + asset.ID)
			if err == nil && !*dryRun {
				err = storage.UploadAsset(ctx, objects, site.ID, asset.ID, asset.ContentType, body, asset.SHA256)
			}
			if err != nil {
				fail("%s/%s asset %s: %v", owner, site.Name, asset.ID, err)
				continue
			}
			assets++
		}
		siteDir.Close()
	}
	log.Printf("migrate-storage: %d version(s), %d asset(s) %s, %d failed", versions, assets, map[bool]string{true: "found", false: "uploaded and verified"}[*dryRun], failed)
	if failed > 0 {
		return fmt.Errorf("%d item(s) failed; fix and re-run", failed)
	}
	return nil
}

// migrateVersion uploads one version from the old tree, whichever form it is
// in there: an unpacked vN directory, or the vN.tar.gz an idle version was
// compressed to.
func migrateVersion(ctx context.Context, objects storage.Objects, siteDir *os.Root, siteID string, version int, dryRun bool) (int64, error) {
	name := fmt.Sprintf("v%d", version)
	if info, err := siteDir.Lstat(name); err == nil && info.IsDir() {
		if dryRun {
			return 0, nil
		}
		dir, err := siteDir.OpenRoot(name)
		if err != nil {
			return 0, err
		}
		defer dir.Close()
		return storage.UploadVersionDir(ctx, objects, siteID, version, dir)
	}
	info, err := siteDir.Lstat(name + ".tar.gz")
	if err != nil {
		return 0, fmt.Errorf("neither %s/ nor %s.tar.gz is on the volume", name, name)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%s.tar.gz is not a regular file", name)
	}
	if dryRun {
		return 0, nil
	}
	archive, err := siteDir.ReadFile(name + ".tar.gz")
	if err != nil {
		return 0, err
	}
	return storage.UploadVersionArchive(ctx, objects, siteID, version, archive)
}

func dryRunSuffix(dryRun bool) string {
	if dryRun {
		return " (dry run)"
	}
	return ""
}

// openDatabaseAndObjects connects the storage subcommands to the database
// and the bucket, with the same translation the server uses.
func openDatabaseAndObjects(cfg config.Config) (*sql.DB, *storage.S3Objects, error) {
	database, err := sql.Open("postgres", cfg.DBDSN)
	if err != nil {
		return nil, nil, fmt.Errorf("open postgres: %w", err)
	}
	objects, err := storage.NewS3Objects(context.Background(), s3Config(cfg))
	if err != nil {
		database.Close()
		return nil, nil, fmt.Errorf("create bucket client: %w", err)
	}
	return database, objects, nil
}

// runPrune drops (or, under -dry-run, lists) expired audit_events and
// access_log partitions: design 9.3 gives the application role no DELETE on
// either table, so retention has to run as its own subcommand under the
// owning credential (the same DSN `migrate` connects with — see
// config.LoadDatabase), never from the server process. This is what
// deploy/base/cronjob-prune.yaml runs monthly.
func runPrune(args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "list partitions that would be dropped without dropping them")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dsn, err := config.LoadDatabase()
	if err != nil {
		return err
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer db.Close()

	retention, err := config.LoadAuditRetention()
	if err != nil {
		return err
	}

	result, err := audit.Prune(context.Background(), db, audit.PruneOptions{
		AuditRetentionDays:  int(retention.RetentionDays),
		AccessRetentionDays: int(retention.AccessLogRetentionDays),
		DryRun:              *dryRun,
	})
	if err != nil {
		return fmt.Errorf("prune: %w", err)
	}

	if *dryRun {
		for _, p := range result.WouldDrop {
			log.Printf("would drop %s partition %s (covers up to %s)", p.Table, p.Partition, p.MonthEnd.Format("2006-01-02"))
		}
		log.Printf("prune -dry-run: %d partition(s) would be dropped", len(result.WouldDrop))
		return nil
	}
	for _, p := range result.Dropped {
		log.Printf("dropped %s partition %s (covered up to %s)", p.Table, p.Partition, p.MonthEnd.Format("2006-01-02"))
	}
	log.Printf("prune: %d partition(s) dropped", len(result.Dropped))
	return nil
}

func waitForDatabase(ctx context.Context, db *sql.DB, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	var last error
	for time.Now().Before(deadline) {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		last = db.PingContext(pingCtx)
		cancel()
		if last == nil {
			return nil
		}
		fmt.Fprintf(os.Stderr, "waiting for database: %v\n", last)
		time.Sleep(3 * time.Second)
	}
	if last == nil {
		last = errors.New("timed out")
	}
	return fmt.Errorf("database not reachable after %s: %w", wait, last)
}
