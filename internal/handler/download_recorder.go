package handler

import (
	"context"
	"database/sql"
	"errors"
	"log"

	dbstore "github.com/vsriram/simple-host/internal/db"
)

const (
	fileDownloadWorkers   = 2
	fileDownloadQueueSize = 128
)

type fileDownloadEvent struct {
	username string
	siteName string
	path     string
}

// fileDownloadRecorder keeps public file responses independent of Postgres.
// Enqueue never blocks; saturation drops analytics rather than delaying or
// accumulating work behind a visitor response.
type fileDownloadRecorder struct {
	events chan fileDownloadEvent
}

func newFileDownloadRecorder(database *sql.DB) *fileDownloadRecorder {
	if database == nil {
		return nil
	}
	return newFileDownloadRecorderWith(fileDownloadWorkers, fileDownloadQueueSize, func(ctx context.Context, event fileDownloadEvent) error {
		return dbstore.RecordSiteFileDownload(ctx, database, event.username, event.siteName, event.path)
	})
}

func newFileDownloadRecorderWith(workers, queueSize int, record func(context.Context, fileDownloadEvent) error) *fileDownloadRecorder {
	if workers <= 0 || queueSize <= 0 || record == nil {
		panic("handler: file download recorder requires positive workers, queue, and callback")
	}
	recorder := &fileDownloadRecorder{events: make(chan fileDownloadEvent, queueSize)}
	for range workers {
		go func() {
			for event := range recorder.events {
				ctx, cancel := context.WithTimeout(context.Background(), analyticsWriteLimit)
				err := record(ctx, event)
				cancel()
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					log.Printf("record file download for %s/%s (%s): %v", event.username, event.siteName, event.path, err)
				}
			}
		}()
	}
	return recorder
}

func (r *fileDownloadRecorder) Enqueue(event fileDownloadEvent) bool {
	if r == nil {
		return false
	}
	select {
	case r.events <- event:
		return true
	default:
		return false
	}
}
