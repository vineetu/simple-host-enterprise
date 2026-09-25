package storage

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
)

// RetireGrace is how long a retired version or site stays in the bucket
// before Sweep deletes it. It only has to outlast a request that resolved the
// version just before it was retired; an hour is generous.
const RetireGrace = time.Hour

const (
	sweepInterval = 5 * time.Minute
	sweepBatch    = 50
	sweepTimeout  = 2 * time.Minute
	// sweepRetryAfter is how long an entry whose deletion failed waits
	// before it is tried again.
	sweepRetryAfter = 30 * time.Minute
)

// Sweep deletes the objects of every retirement-queue entry that is due, in
// batches, and returns how many entries it completed. Every replica may run it
// at once: entries are claimed with SKIP LOCKED, so each is worked by one.
// An entry whose objects could not all be deleted stays queued and is tried
// again later, behind the entries that were waiting.
func (s *Store) Sweep(ctx context.Context, database *sql.DB) (int, error) {
	done := 0
	for {
		n, claimed, err := s.sweepBatch(ctx, database)
		done += n
		// A failed entry is pushed back rather than retried, so a full
		// batch always means there may be more due, never a loop on the
		// same entries.
		if err != nil || claimed < sweepBatch {
			return done, err
		}
	}
}

func (s *Store) sweepBatch(ctx context.Context, database *sql.DB) (int, int, error) {
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	due, err := db.ClaimDueRetiredObjects(ctx, tx, sweepBatch)
	if err != nil {
		return 0, 0, err
	}
	done := 0
	for _, entry := range due {
		if err := s.deleteRetired(ctx, entry.Key); err != nil {
			log.Printf("storage sweep: %s: %v", entry.Key, err)
			if err := db.DeferRetiredObject(ctx, tx, entry.ID, sweepRetryAfter); err != nil {
				return 0, 0, err
			}
			continue
		}
		if err := db.DeleteRetiredObject(ctx, tx, entry.ID); err != nil {
			return 0, 0, err
		}
		done++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return done, len(due), nil
}

// deleteRetired deletes one key, or every object under a prefix ending in "/".
// Only keys under sites/ are ever deleted, whatever the queue says.
func (s *Store) deleteRetired(ctx context.Context, key string) error {
	if !strings.HasPrefix(key, "sites/") || strings.Contains(key, "..") {
		log.Printf("storage sweep: refusing to delete %q outside sites/", key)
		return nil
	}
	if !strings.HasSuffix(key, "/") {
		return s.objects.Delete(ctx, key)
	}
	objects, err := s.objects.List(ctx, key)
	if err != nil {
		return err
	}
	for _, object := range objects {
		if err := s.objects.Delete(ctx, object.Key); err != nil {
			return err
		}
	}
	return nil
}

// RunSweeper runs Sweep every few minutes until ctx ends.
func (s *Store) RunSweeper(ctx context.Context, database *sql.DB) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		if n, err := s.Sweep(ctx, database); err != nil && ctx.Err() == nil {
			log.Printf("storage sweep: %v", err)
		} else if n > 0 {
			log.Printf("storage sweep: retired %d entr(ies)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
