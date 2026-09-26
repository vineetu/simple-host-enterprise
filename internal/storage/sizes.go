package storage

import (
	"context"
	"database/sql"

	db "github.com/vsriram/simple-host/internal/db"
)

const versionSizeBatch = 200

// FillVersionSizes records the stored size of up to one batch of versions
// that have none (migration 0037: versions from before the column, or from
// the restore and migrate-storage subcommands), read from the bucket, and
// returns how many it recorded. Owner quotas count those versions as zero
// until this has run. A version whose object is missing is recorded as zero
// so it is not looked up again. Idempotent, so every replica may run it.
func (s *Store) FillVersionSizes(ctx context.Context, database *sql.DB) (int, error) {
	pending, err := db.ListVersionsWithoutSize(ctx, database, versionSizeBatch)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, version := range pending {
		var size int64
		if key, err := VersionKey(version.SiteID, version.VersionNumber); err == nil {
			// The prefix is the whole key, and ".tar.gz" follows the number,
			// so v1 never matches v10.
			objects, err := s.objects.List(ctx, key)
			if err != nil {
				return done, err
			}
			for _, object := range objects {
				if object.Key == key {
					size = object.Size
				}
			}
		}
		if err := db.SetVersionSize(ctx, database, version.ID, size); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}
