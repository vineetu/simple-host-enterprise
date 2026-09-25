package search

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	searchdb "github.com/vsriram/simple-host/internal/db"
)

const (
	// ExtractorVersion advances when extraction semantics change in a way that
	// requires existing active versions to be reconciled again.
	ExtractorVersion = 1

	workerLeaseDuration       = 2 * time.Minute
	workerIdleDelay           = time.Second
	workerClaimFailureDelay   = time.Second
	workerStartupRetryBase    = time.Second
	workerStartupRetryMaximum = 30 * time.Second
	workerRetryBase           = time.Second
	workerRetryMaximum        = 5 * time.Minute
	workerOperationTimeout    = 10 * time.Second
	workerPublishTimeout      = 60 * time.Second
	workerLeaseSafetyReserve  = 20 * time.Second
	maxWorkerLogFieldBytes    = 256
)

// VersionOpener opens one exact immutable site version, by site id, never
// whatever happens to be live by the time it is read.
type VersionOpener interface {
	OpenVersion(ctx context.Context, siteID string, version int) (OpenedVersion, error)
}

// OpenedVersion is an open version; Close releases it.
type OpenedVersion interface {
	Root() *os.Root
	Close() error
}

type searchRepository interface {
	EnqueueMissing(context.Context, int) (int64, error)
	Claim(context.Context, string, time.Duration) (searchdb.SiteSearchWork, searchdb.SiteSearchClaimOutcome, error)
	LoadSnapshot(context.Context, string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error)
	PublishReconcile(context.Context, searchdb.SiteSearchWork, searchdb.SiteSearchSnapshot, int, []searchdb.SearchDocument, bool) (searchdb.SiteSearchFenceOutcome, error)
	AcknowledgeDelete(context.Context, searchdb.SiteSearchWork) (searchdb.SiteSearchFenceOutcome, error)
	Retry(context.Context, searchdb.SiteSearchWork, time.Time, error) (searchdb.SiteSearchFenceOutcome, error)
}

type postgresSearchRepository struct {
	database *sql.DB
}

func (r postgresSearchRepository) EnqueueMissing(ctx context.Context, extractorVersion int) (int64, error) {
	return searchdb.EnqueueMissingSiteSearch(ctx, r.database, extractorVersion)
}

func (r postgresSearchRepository) Claim(ctx context.Context, token string, lease time.Duration) (searchdb.SiteSearchWork, searchdb.SiteSearchClaimOutcome, error) {
	return searchdb.ClaimSiteSearch(ctx, r.database, token, lease)
}

func (r postgresSearchRepository) LoadSnapshot(ctx context.Context, siteID string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error) {
	return searchdb.LoadSiteSearchSnapshot(ctx, r.database, siteID)
}

func (r postgresSearchRepository) PublishReconcile(
	ctx context.Context,
	work searchdb.SiteSearchWork,
	snapshot searchdb.SiteSearchSnapshot,
	extractorVersion int,
	documents []searchdb.SearchDocument,
	partial bool,
) (searchdb.SiteSearchFenceOutcome, error) {
	return searchdb.PublishSiteSearchReconcile(ctx, r.database, work, snapshot, extractorVersion, documents, partial)
}

func (r postgresSearchRepository) AcknowledgeDelete(ctx context.Context, work searchdb.SiteSearchWork) (searchdb.SiteSearchFenceOutcome, error) {
	return searchdb.AcknowledgeSiteSearchDelete(ctx, r.database, work)
}

func (r postgresSearchRepository) Retry(ctx context.Context, work searchdb.SiteSearchWork, availableAt time.Time, failure error) (searchdb.SiteSearchFenceOutcome, error) {
	return searchdb.RetrySiteSearch(ctx, r.database, work, availableAt, failure)
}

type workerLogger interface {
	Printf(string, ...any)
}

type workerConfig struct {
	leaseDuration       time.Duration
	idleDelay           time.Duration
	claimFailureDelay   time.Duration
	startupRetryBase    time.Duration
	startupRetryMaximum time.Duration
	retryBase           time.Duration
	retryMaximum        time.Duration
	operationTimeout    time.Duration
	publishTimeout      time.Duration
	leaseSafetyReserve  time.Duration
}

func defaultWorkerConfig() workerConfig {
	return workerConfig{
		leaseDuration:       workerLeaseDuration,
		idleDelay:           workerIdleDelay,
		claimFailureDelay:   workerClaimFailureDelay,
		startupRetryBase:    workerStartupRetryBase,
		startupRetryMaximum: workerStartupRetryMaximum,
		retryBase:           workerRetryBase,
		retryMaximum:        workerRetryMaximum,
		operationTimeout:    workerOperationTimeout,
		publishTimeout:      workerPublishTimeout,
		leaseSafetyReserve:  workerLeaseSafetyReserve,
	}
}

func (c workerConfig) validate() error {
	for _, setting := range []struct {
		name     string
		duration time.Duration
	}{
		{name: "lease duration", duration: c.leaseDuration},
		{name: "idle delay", duration: c.idleDelay},
		{name: "claim failure delay", duration: c.claimFailureDelay},
		{name: "startup retry base", duration: c.startupRetryBase},
		{name: "startup retry maximum", duration: c.startupRetryMaximum},
		{name: "retry base", duration: c.retryBase},
		{name: "retry maximum", duration: c.retryMaximum},
		{name: "operation timeout", duration: c.operationTimeout},
		{name: "publish timeout", duration: c.publishTimeout},
		{name: "lease safety reserve", duration: c.leaseSafetyReserve},
	} {
		if setting.duration <= 0 {
			return fmt.Errorf("site search worker %s must be positive", setting.name)
		}
	}
	// A reconcile can spend one general operation timeout loading its snapshot
	// before extraction and publication, while the reserve stays untouched.
	remainingLease := c.leaseDuration
	for _, budget := range []time.Duration{c.operationTimeout, extractionDeadline, c.publishTimeout, c.leaseSafetyReserve} {
		if budget > remainingLease {
			return fmt.Errorf(
				"site search worker lease %s must cover operation timeout %s, extraction deadline %s, publish timeout %s, and safety reserve %s",
				c.leaseDuration,
				c.operationTimeout,
				extractionDeadline,
				c.publishTimeout,
				c.leaseSafetyReserve,
			)
		}
		remainingLease -= budget
	}
	if c.startupRetryMaximum < c.startupRetryBase {
		return errors.New("site search worker startup retry maximum is below its base")
	}
	if c.retryMaximum < c.retryBase {
		return errors.New("site search worker retry maximum is below its base")
	}
	return nil
}

type workerDependencies struct {
	repository searchRepository
	versions   VersionOpener
	extract    func(context.Context, *os.Root, string, string) (Result, error)
	leaseToken func() (string, error)
	now        func() time.Time
	wait       func(context.Context, time.Duration) bool
	logger     workerLogger
	config     workerConfig
}

// Worker is a single sequential reconciliation loop. Stop is idempotent and
// Wait lets application shutdown join the loop before its DB and disk
// dependencies are closed.
type Worker struct {
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
}

// StartWorker constructs the production Postgres adapter and starts the
// startup-backfill/reconciliation loop asynchronously. Startup reconciliation
// failures are retried in the loop and therefore never delay listener startup
// or readiness. siteBase decides the address indexed documents carry; nil
// means the relative long path (see SiteBase).
func StartWorker(parent context.Context, database *sql.DB, versions VersionOpener, siteBase SiteBase) (*Worker, error) {
	if database == nil {
		return nil, errors.New("start site search worker: nil database")
	}
	return startWorker(parent, workerDependencies{
		repository: postgresSearchRepository{database: database},
		versions:   versions,
		extract:    NewExtractor(siteBase),
		leaseToken: randomLeaseToken,
		now:        time.Now,
		wait:       waitForWorkerDelay,
		logger:     log.Default(),
		config:     defaultWorkerConfig(),
	})
}

func startWorker(parent context.Context, dependencies workerDependencies) (*Worker, error) {
	if parent == nil {
		return nil, errors.New("start site search worker: nil context")
	}
	if dependencies.repository == nil {
		return nil, errors.New("start site search worker: nil repository")
	}
	if dependencies.versions == nil {
		return nil, errors.New("start site search worker: nil version opener")
	}
	if dependencies.extract == nil || dependencies.leaseToken == nil || dependencies.now == nil || dependencies.wait == nil {
		return nil, errors.New("start site search worker: incomplete dependencies")
	}
	if dependencies.logger == nil {
		dependencies.logger = log.Default()
	}
	if err := dependencies.config.validate(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(parent)
	worker := &Worker{cancel: cancel, done: make(chan struct{})}
	runner := workerRunner{workerDependencies: dependencies}
	go func() {
		defer close(worker.done)
		runner.run(ctx)
	}()
	return worker, nil
}

// Stop requests prompt cancellation of startup retry, polling, extraction, or
// database work.
func (w *Worker) Stop() {
	if w == nil {
		return
	}
	w.stopOnce.Do(w.cancel)
}

// Wait blocks until the worker loop has stopped or the caller's join context
// expires.
func (w *Worker) Wait(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("wait for site search worker: nil context")
	}
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for site search worker: %w", ctx.Err())
	}
}

type workerRunner struct {
	workerDependencies
}

func (r workerRunner) run(ctx context.Context) {
	if !r.enqueueMissingUntilSuccessful(ctx) {
		return
	}
	for ctx.Err() == nil {
		if !r.claimAndProcessOne(ctx) {
			return
		}
	}
}

func (r workerRunner) enqueueMissingUntilSuccessful(ctx context.Context) bool {
	for attempt := 1; ; attempt++ {
		operationCtx, cancel := context.WithTimeout(ctx, r.config.operationTimeout)
		_, err := r.repository.EnqueueMissing(operationCtx, ExtractorVersion)
		cancel()
		if err == nil {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		r.logFailure("startup_enqueue", nil, err)
		delay := cappedExponentialDelay(r.config.startupRetryBase, r.config.startupRetryMaximum, attempt)
		if !r.wait(ctx, delay) {
			return false
		}
	}
}

func (r workerRunner) claimAndProcessOne(ctx context.Context) bool {
	token, err := r.leaseToken()
	if err != nil || token == "" {
		if err == nil {
			err = errors.New("empty lease token")
		}
		r.logFailure("lease_token", nil, err)
		return r.wait(ctx, r.config.claimFailureDelay)
	}

	operationCtx, cancel := context.WithTimeout(ctx, r.config.operationTimeout)
	work, outcome, err := r.repository.Claim(operationCtx, token, r.config.leaseDuration)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		r.logFailure("claim", nil, err)
		return r.wait(ctx, r.config.claimFailureDelay)
	}

	switch outcome {
	case searchdb.SiteSearchClaimEmpty:
		return r.wait(ctx, r.config.idleDelay)
	case searchdb.SiteSearchClaimed:
		if work.LeaseToken != token {
			r.logFailure("claim_fence", &work, errors.New("claimed lease token did not match generated token"))
			return r.wait(ctx, r.config.claimFailureDelay)
		}
		r.process(ctx, work)
		return ctx.Err() == nil
	case searchdb.SiteSearchClaimUnknown:
		r.logFailure("claim_outcome", nil, errors.New("unknown claim outcome"))
		return r.wait(ctx, r.config.claimFailureDelay)
	default:
		r.logFailure("claim_outcome", nil, errors.New("invalid claim outcome"))
		return r.wait(ctx, r.config.claimFailureDelay)
	}
}

func (r workerRunner) process(lifecycleCtx context.Context, work searchdb.SiteSearchWork) {
	if lifecycleCtx.Err() != nil {
		return
	}
	workBudget := r.claimedWorkBudget(work)
	if workBudget <= 0 {
		r.retryFailure(lifecycleCtx, work, "lease_budget", errors.New("claimed work lease budget exhausted"))
		return
	}
	claimedCtx, cancel := context.WithTimeout(lifecycleCtx, workBudget)
	defer cancel()

	switch work.Operation {
	case searchdb.SiteSearchReconcile:
		r.reconcile(lifecycleCtx, claimedCtx, work)
	case searchdb.SiteSearchDelete:
		r.acknowledgeDelete(lifecycleCtx, claimedCtx, work)
	default:
		r.retryFailure(lifecycleCtx, work, "operation", errors.New("unsupported queue operation"))
	}
}

func (r workerRunner) claimedWorkBudget(work searchdb.SiteSearchWork) time.Duration {
	return work.LockedUntil.Sub(r.now()) - r.config.leaseSafetyReserve
}

func (r workerRunner) reconcile(lifecycleCtx, claimedCtx context.Context, work searchdb.SiteSearchWork) {
	operationCtx, cancel := context.WithTimeout(claimedCtx, r.config.operationTimeout)
	snapshot, outcome, err := r.repository.LoadSnapshot(operationCtx, work.SiteID)
	cancel()
	if err != nil {
		r.retryFailure(lifecycleCtx, work, "load_snapshot", err)
		return
	}

	switch outcome {
	case searchdb.SiteSearchSnapshotMissing:
		r.acknowledgeDelete(lifecycleCtx, claimedCtx, work)
		return
	case searchdb.SiteSearchSnapshotFound:
		// Continue below with the exact authoritative snapshot.
	case searchdb.SiteSearchSnapshotUnknown:
		r.retryFailure(lifecycleCtx, work, "snapshot_outcome", errors.New("unknown snapshot outcome"))
		return
	default:
		r.retryFailure(lifecycleCtx, work, "snapshot_outcome", errors.New("invalid snapshot outcome"))
		return
	}
	if snapshot.SiteID != work.SiteID {
		r.retryFailure(lifecycleCtx, work, "snapshot_identity", errors.New("snapshot site did not match claimed site"))
		return
	}
	if err := claimedCtx.Err(); err != nil {
		r.retryFailure(lifecycleCtx, work, "lease_budget", err)
		return
	}

	opened, err := r.versions.OpenVersion(claimedCtx, snapshot.SiteID, snapshot.ActiveVersion)
	if err != nil {
		r.retryFailure(lifecycleCtx, work, "open_version", err)
		return
	}
	if err := claimedCtx.Err(); err != nil {
		r.retryFailure(lifecycleCtx, work, "lease_budget", errors.Join(err, opened.Close()))
		return
	}
	// Production Extract derives its existing 30-second extraction deadline from
	// the claimed-work context, so lease or lifecycle cancellation still wins.
	result, extractErr := r.extract(claimedCtx, opened.Root(), snapshot.OwnerName, snapshot.SiteName)
	closeErr := opened.Close()
	if extractErr == nil {
		extractErr = claimedCtx.Err()
	}
	if extractErr != nil || closeErr != nil {
		r.retryFailure(lifecycleCtx, work, "extract", errors.Join(extractErr, closeErr))
		return
	}

	documents := make([]searchdb.SearchDocument, len(result.Documents))
	for index, document := range result.Documents {
		documents[index] = searchdb.SearchDocument{
			PagePath:    document.PagePath,
			URLPath:     document.URLPath,
			Title:       document.Title,
			Description: document.Description,
			Headings:    document.Headings,
			BodyText:    document.BodyText,
		}
	}

	publishCtx, cancel := context.WithTimeout(claimedCtx, r.config.publishTimeout)
	fence, err := r.repository.PublishReconcile(
		publishCtx,
		work,
		snapshot,
		ExtractorVersion,
		documents,
		result.Metadata.Partial,
	)
	cancel()
	if err != nil {
		r.retryFailure(lifecycleCtx, work, "publish", err)
		return
	}
	switch fence {
	case searchdb.SiteSearchFenceApplied:
		return
	case searchdb.SiteSearchFenceStale:
		return
	case searchdb.SiteSearchFenceMissing:
		return
	case searchdb.SiteSearchFenceSiteMissing:
		r.acknowledgeDelete(lifecycleCtx, claimedCtx, work)
		return
	case searchdb.SiteSearchFenceUnknown:
		r.retryFailure(lifecycleCtx, work, "publish_fence", errors.New("unknown publish fence outcome"))
		return
	default:
		r.retryFailure(lifecycleCtx, work, "publish_fence", errors.New("invalid publish fence outcome"))
	}
}

func (r workerRunner) acknowledgeDelete(lifecycleCtx, claimedCtx context.Context, work searchdb.SiteSearchWork) {
	if err := claimedCtx.Err(); err != nil {
		r.retryFailure(lifecycleCtx, work, "acknowledge_delete", err)
		return
	}
	operationCtx, cancel := context.WithTimeout(claimedCtx, r.config.operationTimeout)
	fence, err := r.repository.AcknowledgeDelete(operationCtx, work)
	cancel()
	if err != nil {
		r.retryFailure(lifecycleCtx, work, "acknowledge_delete", err)
		return
	}
	switch fence {
	case searchdb.SiteSearchFenceApplied:
		return
	case searchdb.SiteSearchFenceStale:
		return
	case searchdb.SiteSearchFenceMissing:
		return
	case searchdb.SiteSearchFenceSiteMissing:
		// Deletion is already authoritative and acknowledgement is idempotent.
		return
	case searchdb.SiteSearchFenceUnknown:
		r.retryFailure(lifecycleCtx, work, "acknowledge_fence", errors.New("unknown delete acknowledgement outcome"))
		return
	default:
		r.retryFailure(lifecycleCtx, work, "acknowledge_fence", errors.New("invalid delete acknowledgement outcome"))
	}
}

func (r workerRunner) retryFailure(lifecycleCtx context.Context, work searchdb.SiteSearchWork, stage string, cause error) {
	if lifecycleCtx.Err() != nil {
		return
	}
	failure := sanitizedWorkerFailure(stage, cause)
	availableAt := r.now().Add(cappedExponentialDelay(r.config.retryBase, r.config.retryMaximum, work.Attempts))
	retryCtx, cancel := context.WithTimeout(lifecycleCtx, r.config.operationTimeout)
	defer cancel()
	if lifecycleCtx.Err() != nil {
		return
	}
	fence, err := r.repository.Retry(retryCtx, work, availableAt, failure)
	if err != nil {
		if lifecycleCtx.Err() == nil {
			r.logFailure("retry_"+stage, &work, err)
		}
		return
	}
	switch fence {
	case searchdb.SiteSearchFenceApplied:
		r.logFailure(stage, &work, cause)
	case searchdb.SiteSearchFenceStale:
		return
	case searchdb.SiteSearchFenceMissing:
		return
	case searchdb.SiteSearchFenceSiteMissing:
		return
	case searchdb.SiteSearchFenceUnknown:
		r.logFailure("retry_fence", &work, errors.New("unknown retry fence outcome"))
	default:
		r.logFailure("retry_fence", &work, errors.New("invalid retry fence outcome"))
	}
}

func (r workerRunner) logFailure(stage string, work *searchdb.SiteSearchWork, cause error) {
	siteID := ""
	operation := ""
	var generation int64
	var attempts int
	if work != nil {
		siteID = work.SiteID
		operation = string(work.Operation)
		generation = work.Generation
		attempts = work.Attempts
	}
	r.logger.Printf(
		"site search worker failure stage=%q site_id=%q operation=%q generation=%d attempts=%d error_type=%q",
		boundWorkerField(stage),
		boundWorkerField(siteID),
		boundWorkerField(operation),
		generation,
		attempts,
		boundWorkerField(fmt.Sprintf("%T", cause)),
	)
}

func randomLeaseToken() (string, error) {
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate site search lease token: %w", err)
	}
	return hex.EncodeToString(random), nil
}

func waitForWorkerDelay(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func cappedExponentialDelay(base, maximum time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := base
	for exponent := 1; exponent < attempt && delay < maximum; exponent++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func sanitizedWorkerFailure(stage string, cause error) error {
	errorType := fmt.Sprintf("%T", cause)
	return errors.New(boundWorkerField(stage) + " failed (" + boundWorkerField(errorType) + ")")
}

func boundWorkerField(value string) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	var builder strings.Builder
	for _, character := range value {
		if unicode.IsControl(character) {
			builder.WriteByte(' ')
			continue
		}
		builder.WriteRune(character)
		if builder.Len() >= maxWorkerLogFieldBytes {
			break
		}
	}
	bounded := builder.String()
	if len(bounded) <= maxWorkerLogFieldBytes {
		return bounded
	}
	bounded = bounded[:maxWorkerLogFieldBytes]
	for !utf8.ValidString(bounded) {
		bounded = bounded[:len(bounded)-1]
	}
	return bounded
}
