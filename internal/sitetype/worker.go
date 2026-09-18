package sitetype

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	dbstore "github.com/vsriram/simple-host/internal/db"
)

const (
	// defaultInterval paces the poll. Classification is a browse-page nicety,
	// not something anybody waits on, and a slow cadence bounds the model spend
	// no matter how fast somebody redeploys.
	defaultInterval = 2 * time.Minute
	// defaultBatch bounds one pass. Small: each item is a model call, and a
	// pass should finish well inside the shutdown drain.
	defaultBatch = 10
	// callTimeout bounds one model call.
	callTimeout = 20 * time.Second
)

// Worker labels public sites in the background.
//
// It is deliberately a poller rather than a hook on the deploy path. The search
// indexer already re-extracts a site's text on create, update, rollback and
// delete; by reading what it wrote, this worker inherits all four triggers
// without the deploy path gaining a single line, and rollback is covered for
// free.
type Worker struct {
	database   *sql.DB
	classifier Classifier
	interval   time.Duration
	batch      int

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// StartWorker begins polling. A nil classifier returns a nil worker: the
// feature is simply off, which is what happens when AWS is unreachable at
// startup, and the rest of the server is unaffected.
func StartWorker(parent context.Context, database *sql.DB, classifier Classifier) *Worker {
	if database == nil || classifier == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	w := &Worker{
		database:   database,
		classifier: classifier,
		interval:   defaultInterval,
		batch:      defaultBatch,
		cancel:     cancel,
		done:       make(chan struct{}),
	}
	go w.run(ctx)
	return w
}

func (w *Worker) run(ctx context.Context) {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := w.RunOnce(ctx, w.batch); err != nil && ctx.Err() == nil {
				log.Printf("site type worker: %v", err)
			}
		}
	}
}

// Result reports what one pass did.
type Result struct {
	Scanned  int
	Labelled int
	Skipped  int
	Failed   int
}

// RunOnce classifies up to limit sites. Errors on individual sites are counted
// and swallowed: one site that cannot be labelled must not stop the pass, and
// nothing downstream depends on the outcome.
func (w *Worker) RunOnce(ctx context.Context, limit int) (Result, error) {
	var result Result
	if w == nil {
		return result, nil
	}
	candidates, err := dbstore.ListSiteTypeCandidates(ctx, w.database, limit)
	if err != nil {
		return result, err
	}
	result.Scanned = len(candidates)

	for _, candidate := range candidates {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		in := Input{
			SiteName:    candidate.SiteName,
			Title:       candidate.Title,
			Description: candidate.Description,
			Headings:    candidate.Headings,
			BodyText:    candidate.BodyText,
		}
		if in.Empty() {
			result.Skipped++
			continue
		}

		callCtx, cancel := context.WithTimeout(ctx, callTimeout)
		label, err := w.classifier.Classify(callCtx, in)
		cancel()
		if err != nil {
			result.Failed++
			log.Printf("site type: classify %s/%s: %v", candidate.Username, candidate.SiteName, err)
			continue
		}
		if label == "" {
			// The model answered outside the closed set, or had nothing to go
			// on. Leave the site unclassified; a later version retries.
			result.Skipped++
			continue
		}

		// The write re-checks public, so a site made private since it was
		// selected is not labelled.
		written, err := dbstore.SetSiteType(ctx, w.database, candidate.SiteID, string(label), candidate.ActiveVersion)
		if err != nil {
			result.Failed++
			log.Printf("site type: store %s/%s: %v", candidate.Username, candidate.SiteName, err)
			continue
		}
		if !written {
			result.Skipped++
			continue
		}
		result.Labelled++
	}
	return result, nil
}

// Stop signals the worker to finish. Safe to call more than once.
func (w *Worker) Stop() {
	if w == nil {
		return
	}
	w.once.Do(w.cancel)
}

// Wait blocks until the worker has stopped or ctx expires.
func (w *Worker) Wait(ctx context.Context) error {
	if w == nil {
		return nil
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
