package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io/fs"
	"os"

	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/handler"
	"github.com/vsriram/simple-host/internal/storage"
)

// runSiteSummaryBackfill uses its own temporary cache, never a serving pod's
// cache directory. Content comes from the bucket through OpenVersion.
func runSiteSummaryBackfill(args []string) error {
	flags := flag.NewFlagSet("site-summary-backfill", flag.ContinueOnError)
	limit := flags.Int("limit", 0, "maximum live versions to summarise (0 = all)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *limit < 0 || flags.NArg() != 0 {
		return fmt.Errorf("usage: site-summary-backfill [-limit N], N must be non-negative")
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
	cache, err := os.MkdirTemp("", "site-summary-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(cache)
	store, err := storage.New(storage.Options{Objects: objects, Index: storage.NewDBIndex(database), CacheDir: cache, CacheMaxBytes: cfg.CacheMaxBytes})
	if err != nil {
		return err
	}
	defer store.Close()
	n, err := backfillSiteSummaries(context.Background(), database, store, *limit)
	fmt.Printf("site-summary-backfill: summarised %d live versions\n", n)
	return err
}

func backfillSiteSummaries(ctx context.Context, database *sql.DB, store *storage.Store, limit int) (int, error) {
	rows, err := database.QueryContext(ctx, `SELECT s.id, s.active_version FROM sites s
 JOIN versions v ON v.site_id = s.id AND v.version_number = s.active_version
 WHERE s.deleted_at IS NULL AND v.summary IS NULL ORDER BY s.created_at, s.id LIMIT NULLIF($1,0)`, limit)
	if err != nil {
		return 0, err
	}
	// Release the query connection before writes (including single-connection jobs).
	type candidate struct {
		id      string
		version int
	}
	var sites []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.version); err != nil {
			rows.Close()
			return 0, err
		}
		sites = append(sites, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range sites {
		lease, err := store.OpenVersion(ctx, c.id, c.version)
		if err != nil {
			return n, fmt.Errorf("read version %s/%d: %w", c.id, c.version, err)
		}
		data, err := fs.ReadFile(lease.FS(), "index.html")
		lease.Close()
		if err != nil {
			return n, fmt.Errorf("read index %s/%d: %w", c.id, c.version, err)
		}
		summary := handler.ExtractSiteSummary(map[string][]byte{"index.html": data})
		if err := db.SaveVersionSummary(ctx, database, c.id, c.version, summary); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
