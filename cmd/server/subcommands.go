package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/config"
	"github.com/vsriram/simple-host/internal/migrate"
	"github.com/vsriram/simple-host/internal/safepath"
	"github.com/vsriram/simple-host/internal/storage"
)

// runSubcommand dispatches the binary's non-server modes. The same image runs
// as the init container (`migrate`), the server, and the backup/restore
// CronJob and one-off recovery commands, so an operator only ever ships one
// artifact.
func runSubcommand(name string, args []string) error {
	switch name {
	case "migrate":
		return runMigrate(args)
	case "restore":
		return runRestore(args)
	case "backup-assets":
		return runBackupAssets(args)
	case "prune":
		return runPrune(args)
	case "version":
		fmt.Println(versionString())
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q (expected: migrate, restore, backup-assets, prune, version)", name)
	}
}

// version and commit are set at build time:
//
//	-ldflags "-X main.version=v1.1.0 -X main.commit=<sha>"
//
// A plain `go build` says dev/unknown, which is what it is.
var (
	version = "dev"
	commit  = "unknown"
)

func versionString() string {
	latest, err := migrate.Latest()
	if err != nil {
		return fmt.Sprintf("simple-host %s (commit %s, schema unknown: %v)", version, commit, err)
	}
	return fmt.Sprintf("simple-host %s (commit %s, schema %04d)", version, commit, latest)
}

// runMigrate applies pending migrations, or with --status only reports them.
// It waits for the database rather than failing at once: in a fresh cluster
// the init container regularly starts before Postgres accepts connections.
func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	status := fs.Bool("status", false, "report pending migrations without applying them")
	wait := fs.Duration("wait", 2*time.Minute, "how long to wait for the database to accept connections")
	lockWait := fs.Duration("lock-wait", migrate.DefaultLockWait, "how long to wait for another migrator to finish")
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
	applied, err := migrate.Apply(ctx, db, *lockWait, func(msg string) { log.Print(msg) })
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

// runRestore rebuilds one site version, and optionally its assets, from the
// most recent matching backup object into a target site directory. The
// target may name a different owner or site than the backup was taken from,
// which is how a version is recovered into a fresh location for inspection
// before it is trusted enough to become the real site's current version, and
// how any environment gets a copy of another's content (design 10.5).
func runRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	owner := fs.String("owner", "", "owner label the backup was taken from")
	site := fs.String("site", "", "site name the backup was taken from")
	version := fs.Int("version", 0, "version number to restore")
	targetOwner := fs.String("target-owner", "", "owner directory to restore into (default: -owner)")
	targetSite := fs.String("target-site", "", "site directory to restore into (default: -site)")
	assets := fs.Bool("assets", false, "also restore the site's assets directory")
	setCurrent := fs.Bool("set-current", true, "point the target site's current version at the restored one")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *owner == "" || *site == "" || *version <= 0 {
		return errors.New("restore: -owner, -site and -version are required")
	}
	if *targetOwner == "" {
		*targetOwner = *owner
	}
	if *targetSite == "" {
		*targetSite = *site
	}
	if err := safepath.ValidateSegment(*targetOwner); err != nil {
		return fmt.Errorf("restore: -target-owner: %w", err)
	}
	if err := safepath.ValidateSegment(*targetSite); err != nil {
		return fmt.Errorf("restore: -target-site: %w", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	backup, disk, err := openBackupAndDisk(cfg)
	if err != nil {
		return err
	}
	defer disk.Close()
	if backup == nil {
		return errors.New("restore: no bucket is configured (BACKUP_STORAGE_BUCKET is required)")
	}

	ctx := context.Background()
	key, err := backup.RestoreVersion(ctx, disk, *owner, *site, *version, *targetOwner, *targetSite, *setCurrent)
	if err != nil {
		return fmt.Errorf("restore version: %w", err)
	}
	log.Printf("restored %s/%s v%d from %s into %s/%s (current=%t)", *owner, *site, *version, key, *targetOwner, *targetSite, *setCurrent)

	if *assets {
		targetDir := filepath.Join(cfg.SiteDir, *targetOwner, *targetSite)
		n, err := backup.RestoreAssets(ctx, *owner, *site, targetDir)
		if err != nil {
			return fmt.Errorf("restore assets: %w", err)
		}
		log.Printf("restored %d asset(s) for %s/%s into %s/%s", n, *owner, *site, *targetOwner, *targetSite)
	}
	return nil
}

// runBackupAssets syncs every site's assets directory to the bucket. It is
// what the backup-assets CronJob runs (deploy/base/backup-assets-cronjob.yaml)
// and is safe to run by hand: every file is re-uploaded, so a run that
// overlaps another, or that repeats one already done, changes nothing an
// operator would notice besides bucket traffic.
func runBackupAssets(args []string) error {
	fs := flag.NewFlagSet("backup-assets", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "list sites with an assets directory without uploading")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	backup, err := storage.NewBackup(context.Background(), storage.BackupConfig{
		Endpoint:        cfg.Backup.Endpoint,
		Region:          cfg.Backup.Region,
		Bucket:          cfg.Backup.Bucket,
		Prefix:          cfg.Backup.Prefix,
		AccessKeyID:     cfg.Backup.AccessKeyID,
		SecretAccessKey: cfg.Backup.SecretAccessKey,
		SSE:             cfg.Backup.SSE,
		SSEKMSKeyID:     cfg.Backup.SSEKMSKeyID,
		EnvelopeKeys:    toStorageEnvelopeKeys(cfg.Backup.EnvelopeKeys),
	})
	if err != nil {
		return fmt.Errorf("create backup client: %w", err)
	}
	if backup == nil {
		return errors.New("backup-assets: no bucket is configured (BACKUP_STORAGE_BUCKET is required)")
	}

	sites, err := sitesWithAssets(cfg.SiteDir)
	if err != nil {
		return fmt.Errorf("scan %s: %w", cfg.SiteDir, err)
	}

	ctx := context.Background()
	var failed int
	for _, site := range sites {
		if *dryRun {
			log.Printf("would back up %s/%s assets", site.owner, site.site)
			continue
		}
		n, err := backup.BackupAssets(ctx, cfg.SiteDir, site.owner, site.site)
		if err != nil {
			log.Printf("backup-assets %s/%s: %v", site.owner, site.site, err)
			failed++
			continue
		}
		log.Printf("backed up %d asset(s) for %s/%s", n, site.owner, site.site)
	}
	if failed > 0 {
		return fmt.Errorf("backup-assets: %d of %d site(s) failed", failed, len(sites))
	}
	log.Printf("backup-assets: %d site(s) with assets processed", len(sites))
	return nil
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

type ownerSite struct {
	owner string
	site  string
}

// sitesWithAssets walks the site tree for every <owner>/<site>/assets
// directory, without going through *storage.DiskStorage: DiskStorage's
// os.Root confinement is for the request path, which mutates live sites
// concurrently with this scan; a read-only directory walk under SITE_DIR
// needs none of that, and Phase 3 (which will start populating these
// directories) is expected to add its own accessor for the request path
// without this scan needing to change.
func sitesWithAssets(siteDir string) ([]ownerSite, error) {
	ownerEntries, err := os.ReadDir(siteDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var found []ownerSite
	for _, ownerEntry := range ownerEntries {
		if !ownerEntry.IsDir() || !safepath.IsSegment(ownerEntry.Name()) {
			continue
		}
		ownerDir := filepath.Join(siteDir, ownerEntry.Name())
		siteEntries, err := os.ReadDir(ownerDir)
		if err != nil {
			return nil, err
		}
		for _, siteEntry := range siteEntries {
			if !siteEntry.IsDir() || !safepath.IsSegment(siteEntry.Name()) {
				continue
			}
			assetsInfo, err := os.Stat(filepath.Join(ownerDir, siteEntry.Name(), "assets"))
			if err != nil {
				continue
			}
			if assetsInfo.IsDir() {
				found = append(found, ownerSite{owner: ownerEntry.Name(), site: siteEntry.Name()})
			}
		}
	}
	return found, nil
}

// openBackupAndDisk constructs the backup client and disk storage restore
// needs, sharing the same config-to-storage translation the server uses at
// startup.
func openBackupAndDisk(cfg config.Config) (*storage.Backup, *storage.DiskStorage, error) {
	backup, err := storage.NewBackup(context.Background(), storage.BackupConfig{
		Endpoint:        cfg.Backup.Endpoint,
		Region:          cfg.Backup.Region,
		Bucket:          cfg.Backup.Bucket,
		Prefix:          cfg.Backup.Prefix,
		AccessKeyID:     cfg.Backup.AccessKeyID,
		SecretAccessKey: cfg.Backup.SecretAccessKey,
		SSE:             cfg.Backup.SSE,
		SSEKMSKeyID:     cfg.Backup.SSEKMSKeyID,
		EnvelopeKeys:    toStorageEnvelopeKeys(cfg.Backup.EnvelopeKeys),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create backup client: %w", err)
	}
	disk, err := storage.NewDiskStorage(cfg.SiteDir)
	if err != nil {
		return nil, nil, fmt.Errorf("open disk storage: %w", err)
	}
	return backup, disk, nil
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
