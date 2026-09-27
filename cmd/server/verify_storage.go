package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/vsriram/simple-host/internal/config"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/storage"
)

// runVerifyStorage checks that every object the database depends on is in
// the bucket: each site's live version and each live uploaded file, for
// sites in use and in Recently deleted. Run it after a database
// point-in-time restore (docs/install.md, "Restore drill"): the restored
// database can point at objects the sweeper retired after that time. Each
// missing object is printed with its key, which is what the bucket's own
// versioning brings back; the command exits non-zero when anything is
// missing. It reads only.
func runVerifyStorage(args []string) error {
	fs := flag.NewFlagSet("verify-storage", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
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
	missing, checked, err := verifyStorage(context.Background(), database, objects, os.Stdout)
	if err != nil {
		return err
	}
	if missing > 0 {
		return fmt.Errorf("%d of %d objects are missing from the bucket; bring each key above back from the bucket's noncurrent versions, then run verify-storage again", missing, checked)
	}
	fmt.Printf("storage OK: all %d objects the database depends on are in the bucket\n", checked)
	return nil
}

// verifyStorage lists each site's objects once and reports every expected
// key that is not there, one line each on out. It returns how many were
// missing and how many were checked.
func verifyStorage(ctx context.Context, database *sql.DB, objects storage.Objects, out io.Writer) (missing, checked int, err error) {
	expected, err := db.ListStoredObjects(ctx, database)
	if err != nil {
		return 0, 0, fmt.Errorf("list what the database depends on: %w", err)
	}
	present := map[string]bool{}
	listed := ""
	for _, o := range expected {
		if o.SiteID != listed {
			prefix, err := storage.SitePrefix(o.SiteID)
			if err != nil {
				return missing, checked, err
			}
			found, err := objects.List(ctx, prefix)
			if err != nil {
				return missing, checked, fmt.Errorf("list %s: %w", prefix, err)
			}
			present = make(map[string]bool, len(found))
			for _, f := range found {
				present[f.Key] = true
			}
			listed = o.SiteID
		}
		var key, what string
		if o.AssetID != "" {
			key, err = storage.AssetKey(o.SiteID, o.AssetID)
			what = "uploaded file " + o.AssetID
		} else {
			key, err = storage.VersionKey(o.SiteID, o.Version)
			what = fmt.Sprintf("live version v%d", o.Version)
		}
		if err != nil {
			return missing, checked, err
		}
		checked++
		if present[key] {
			continue
		}
		missing++
		state := ""
		if o.Deleted {
			state = " (in Recently deleted)"
		}
		fmt.Fprintf(out, "missing %s  %s/%s%s %s\n", key, o.Owner, o.Site, state, what)
	}
	return missing, checked, nil
}
