package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"reflect"
	"sync"
	"time"

	searchdb "github.com/vsriram/simple-host/internal/db"
)

const (
	telemetryPruneBatchSize      = 500
	telemetryPruneMaxBatches     = 20
	telemetryPruneInterval       = 6 * time.Hour
	telemetryPruneRetryDelay     = 5 * time.Minute
	telemetryPruneAttemptTimeout = 30 * time.Second
)

type telemetryPruneRepository interface {
	DeleteExpired(context.Context, int) (int64, error)
}

type postgresTelemetryPruneRepository struct {
	database *sql.DB
}

func (r postgresTelemetryPruneRepository) DeleteExpired(ctx context.Context, batchSize int) (int64, error) {
	return searchdb.DeleteExpiredSiteSearchTelemetry(ctx, r.database, batchSize)
}

type telemetryPrunerLogger interface {
	Printf(string, ...any)
}

type telemetryPrunerConfig struct {
	batchSize      int
	maxBatches     int
	interval       time.Duration
	retryDelay     time.Duration
	attemptTimeout time.Duration
}

func defaultTelemetryPrunerConfig() telemetryPrunerConfig {
	return telemetryPrunerConfig{
		batchSize:      telemetryPruneBatchSize,
		maxBatches:     telemetryPruneMaxBatches,
		interval:       telemetryPruneInterval,
		retryDelay:     telemetryPruneRetryDelay,
		attemptTimeout: telemetryPruneAttemptTimeout,
	}
}

func (c telemetryPrunerConfig) validate() error {
	if c.batchSize <= 0 || c.batchSize > searchdb.MaxSiteSearchTelemetryDeleteBatch {
		return fmt.Errorf("site search telemetry pruner batch size must be within 1..%d", searchdb.MaxSiteSearchTelemetryDeleteBatch)
	}
	if c.maxBatches <= 0 {
		return errors.New("site search telemetry pruner maximum batches must be positive")
	}
	for _, setting := range []struct {
		name     string
		duration time.Duration
	}{
		{name: "interval", duration: c.interval},
		{name: "retry delay", duration: c.retryDelay},
		{name: "attempt timeout", duration: c.attemptTimeout},
	} {
		if setting.duration <= 0 {
			return fmt.Errorf("site search telemetry pruner %s must be positive", setting.name)
		}
	}
	return nil
}

type telemetryPrunerDependencies struct {
	repository telemetryPruneRepository
	wait       func(context.Context, time.Duration) bool
	logger     telemetryPrunerLogger
	config     telemetryPrunerConfig
}

// TelemetryPruner runs bounded telemetry-retention work independently of public
// search traffic. Stop is idempotent, and Wait lets application shutdown join
// the loop before Postgres is closed.
type TelemetryPruner struct {
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
}

// StartTelemetryPruner starts the autonomous Postgres retention loop. Database
// failures remain inside the best-effort loop and never affect search requests.
func StartTelemetryPruner(parent context.Context, database *sql.DB) (*TelemetryPruner, error) {
	if database == nil {
		return nil, errors.New("start site search telemetry pruner: nil database")
	}
	return startTelemetryPruner(parent, telemetryPrunerDependencies{
		repository: postgresTelemetryPruneRepository{database: database},
		wait:       waitForTelemetryPruneDelay,
		logger:     log.Default(),
		config:     defaultTelemetryPrunerConfig(),
	})
}

func startTelemetryPruner(parent context.Context, dependencies telemetryPrunerDependencies) (*TelemetryPruner, error) {
	if parent == nil {
		return nil, errors.New("start site search telemetry pruner: nil context")
	}
	if dependencies.repository == nil {
		return nil, errors.New("start site search telemetry pruner: nil repository")
	}
	if dependencies.wait == nil {
		return nil, errors.New("start site search telemetry pruner: nil wait function")
	}
	if dependencies.logger == nil {
		dependencies.logger = log.Default()
	}
	if err := dependencies.config.validate(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(parent)
	pruner := &TelemetryPruner{cancel: cancel, done: make(chan struct{})}
	runner := telemetryPrunerRunner{telemetryPrunerDependencies: dependencies}
	go func() {
		defer close(pruner.done)
		runner.run(ctx)
	}()
	return pruner, nil
}

// Stop requests prompt cancellation of an active delete or scheduled wait.
func (p *TelemetryPruner) Stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(p.cancel)
}

// Wait blocks until the retention loop has stopped or the join context expires.
func (p *TelemetryPruner) Wait(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("wait for site search telemetry pruner: nil context")
	}
	select {
	case <-p.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for site search telemetry pruner: %w", ctx.Err())
	}
}

type telemetryPrunerRunner struct {
	telemetryPrunerDependencies
}

func (r telemetryPrunerRunner) run(ctx context.Context) {
	for ctx.Err() == nil {
		delay, keepRunning := r.pruneAttempt(ctx)
		if !keepRunning || !r.wait(ctx, delay) {
			return
		}
	}
}

func (r telemetryPrunerRunner) pruneAttempt(lifecycleCtx context.Context) (time.Duration, bool) {
	attemptCtx, cancel := context.WithTimeout(lifecycleCtx, r.config.attemptTimeout)
	defer cancel()

	for range r.config.maxBatches {
		deleted, err := r.repository.DeleteExpired(attemptCtx, r.config.batchSize)
		if err != nil {
			if lifecycleCtx.Err() != nil {
				return 0, false
			}
			r.logFailure(err)
			return r.config.retryDelay, true
		}
		if err := attemptCtx.Err(); err != nil {
			if lifecycleCtx.Err() != nil {
				return 0, false
			}
			r.logFailure(err)
			return r.config.retryDelay, true
		}
		if deleted < int64(r.config.batchSize) {
			return r.config.interval, true
		}
	}
	return r.config.retryDelay, true
}

func (r telemetryPrunerRunner) logFailure(err error) {
	if err != nil && r.logger != nil {
		// Resolve the type before handing anything to the logger. This avoids
		// invoking Error or a custom formatter that could expose query/SQL details.
		r.logger.Printf("site search telemetry prune failed: %s", reflect.TypeOf(err).String())
	}
}

func waitForTelemetryPruneDelay(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
