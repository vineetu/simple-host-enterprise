package search

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	searchdb "github.com/vsriram/simple-host/internal/db"
)

func TestWorkerReconcilesExactSnapshotAndPublishesDocuments(t *testing.T) {
	work := testSearchWork(searchdb.SiteSearchReconcile, 1)
	snapshot := testSearchSnapshot()
	opener := newWorkerTestVersion(t, map[string]string{
		"index.html": `<html><head><title>Demo</title><meta name="description" content="A demo"></head><body><h1>Welcome</h1><p>Useful body.</p></body></html>`,
	})

	var publishedWork searchdb.SiteSearchWork
	var publishedSnapshot searchdb.SiteSearchSnapshot
	var publishedVersion int
	var publishedDocuments []searchdb.SearchDocument
	var publishedPartial bool
	repository := &fakeSearchRepository{
		loadSnapshot: func(_ context.Context, siteID string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error) {
			if siteID != work.SiteID {
				t.Fatalf("snapshot site ID = %q", siteID)
			}
			return snapshot, searchdb.SiteSearchSnapshotFound, nil
		},
		publishReconcile: func(
			_ context.Context,
			gotWork searchdb.SiteSearchWork,
			gotSnapshot searchdb.SiteSearchSnapshot,
			extractorVersion int,
			documents []searchdb.SearchDocument,
			partial bool,
		) (searchdb.SiteSearchFenceOutcome, error) {
			publishedWork = gotWork
			publishedSnapshot = gotSnapshot
			publishedVersion = extractorVersion
			publishedDocuments = append([]searchdb.SearchDocument(nil), documents...)
			publishedPartial = partial
			return searchdb.SiteSearchFenceApplied, nil
		},
	}

	runner := newWorkerTestRunner(repository, opener)
	runner.process(context.Background(), work)

	if publishedWork != work || publishedSnapshot != snapshot {
		t.Fatalf("published fence = (%+v, %+v)", publishedWork, publishedSnapshot)
	}
	if publishedVersion != ExtractorVersion {
		t.Fatalf("extractor version = %d, want %d", publishedVersion, ExtractorVersion)
	}
	if publishedPartial {
		t.Fatal("ordinary extraction was published as partial")
	}
	if len(publishedDocuments) != 1 {
		t.Fatalf("published documents = %d, want 1", len(publishedDocuments))
	}
	want := searchdb.SearchDocument{
		PagePath:    "index.html",
		URLPath:     "/sites/alice/demo/",
		Title:       "Demo",
		Description: "A demo",
		Headings:    "Welcome",
		BodyText:    "Welcome Useful body.",
	}
	if publishedDocuments[0] != want {
		t.Fatalf("published document = %+v, want %+v", publishedDocuments[0], want)
	}
	if calls := opener.Calls(); !reflect.DeepEqual(calls, []versionOpenCall{{owner: "alice", site: "demo", version: 7}}) {
		t.Fatalf("OpenVersion calls = %#v", calls)
	}
	if repository.acknowledgeCalls() != 0 || repository.retryCalls() != 0 {
		t.Fatalf("acknowledge/retry calls = %d/%d", repository.acknowledgeCalls(), repository.retryCalls())
	}
}

func TestWorkerPublishesZeroDocumentSuccess(t *testing.T) {
	work := testSearchWork(searchdb.SiteSearchReconcile, 1)
	opener := newWorkerTestVersion(t, map[string]string{"notes.txt": "not HTML"})
	published := false
	repository := &fakeSearchRepository{
		loadSnapshot: func(context.Context, string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error) {
			return testSearchSnapshot(), searchdb.SiteSearchSnapshotFound, nil
		},
		publishReconcile: func(_ context.Context, _ searchdb.SiteSearchWork, _ searchdb.SiteSearchSnapshot, _ int, documents []searchdb.SearchDocument, partial bool) (searchdb.SiteSearchFenceOutcome, error) {
			published = true
			if len(documents) != 0 {
				t.Fatalf("documents = %d, want zero", len(documents))
			}
			if partial {
				t.Fatal("zero-document extraction was unexpectedly partial")
			}
			return searchdb.SiteSearchFenceApplied, nil
		},
	}

	newWorkerTestRunner(repository, opener).process(context.Background(), work)
	if !published {
		t.Fatal("zero-document result was not published")
	}
}

func TestWorkerUsesIdempotentDeletionPaths(t *testing.T) {
	t.Run("missing snapshot", func(t *testing.T) {
		work := testSearchWork(searchdb.SiteSearchReconcile, 1)
		repository := &fakeSearchRepository{
			loadSnapshot: func(context.Context, string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error) {
				return searchdb.SiteSearchSnapshot{}, searchdb.SiteSearchSnapshotMissing, nil
			},
			acknowledgeDelete: func(_ context.Context, got searchdb.SiteSearchWork) (searchdb.SiteSearchFenceOutcome, error) {
				if got != work {
					t.Fatalf("acknowledged work = %+v", got)
				}
				return searchdb.SiteSearchFenceApplied, nil
			},
		}
		newWorkerTestRunner(repository, unusedVersionOpener{}).process(context.Background(), work)
		if repository.acknowledgeCalls() != 1 || repository.retryCalls() != 0 {
			t.Fatalf("acknowledge/retry calls = %d/%d", repository.acknowledgeCalls(), repository.retryCalls())
		}
	})

	t.Run("delete tombstone stale acknowledgement", func(t *testing.T) {
		work := testSearchWork(searchdb.SiteSearchDelete, 1)
		repository := &fakeSearchRepository{
			acknowledgeDelete: func(context.Context, searchdb.SiteSearchWork) (searchdb.SiteSearchFenceOutcome, error) {
				return searchdb.SiteSearchFenceStale, nil
			},
		}
		newWorkerTestRunner(repository, unusedVersionOpener{}).process(context.Background(), work)
		if repository.loadCalls() != 0 || repository.acknowledgeCalls() != 1 || repository.retryCalls() != 0 {
			t.Fatalf("load/acknowledge/retry calls = %d/%d/%d", repository.loadCalls(), repository.acknowledgeCalls(), repository.retryCalls())
		}
	})

	t.Run("site disappears at publication fence", func(t *testing.T) {
		work := testSearchWork(searchdb.SiteSearchReconcile, 1)
		repository := &fakeSearchRepository{
			loadSnapshot: func(context.Context, string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error) {
				return testSearchSnapshot(), searchdb.SiteSearchSnapshotFound, nil
			},
			publishReconcile: func(context.Context, searchdb.SiteSearchWork, searchdb.SiteSearchSnapshot, int, []searchdb.SearchDocument, bool) (searchdb.SiteSearchFenceOutcome, error) {
				return searchdb.SiteSearchFenceSiteMissing, nil
			},
			acknowledgeDelete: func(context.Context, searchdb.SiteSearchWork) (searchdb.SiteSearchFenceOutcome, error) {
				return searchdb.SiteSearchFenceApplied, nil
			},
		}
		opener := newWorkerTestVersion(t, map[string]string{"index.html": "<body>demo</body>"})
		newWorkerTestRunner(repository, opener).process(context.Background(), work)
		if repository.acknowledgeCalls() != 1 || repository.retryCalls() != 0 {
			t.Fatalf("acknowledge/retry calls = %d/%d", repository.acknowledgeCalls(), repository.retryCalls())
		}
	})
}

func TestWorkerDiscardsStalePublicationWithoutRetry(t *testing.T) {
	work := testSearchWork(searchdb.SiteSearchReconcile, 1)
	repository := &fakeSearchRepository{
		loadSnapshot: func(context.Context, string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error) {
			return testSearchSnapshot(), searchdb.SiteSearchSnapshotFound, nil
		},
		publishReconcile: func(context.Context, searchdb.SiteSearchWork, searchdb.SiteSearchSnapshot, int, []searchdb.SearchDocument, bool) (searchdb.SiteSearchFenceOutcome, error) {
			return searchdb.SiteSearchFenceStale, nil
		},
	}
	opener := newWorkerTestVersion(t, map[string]string{"index.html": "<body>stale</body>"})

	newWorkerTestRunner(repository, opener).process(context.Background(), work)
	if repository.acknowledgeCalls() != 0 || repository.retryCalls() != 0 {
		t.Fatalf("stale publication ack/retry calls = %d/%d", repository.acknowledgeCalls(), repository.retryCalls())
	}
}

func TestWorkerFailureUsesFencedCappedBackoffAndSanitizedDiagnostics(t *testing.T) {
	work := testSearchWork(searchdb.SiteSearchReconcile, 4)
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	work.LockedUntil = now.Add(workerLeaseDuration)
	secret := "password=super-secret extracted-content-marker"
	opener := failingVersionOpener{err: errors.New(secret)}
	var retryAt time.Time
	var retryFailure error
	repository := &fakeSearchRepository{
		loadSnapshot: func(context.Context, string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error) {
			return testSearchSnapshot(), searchdb.SiteSearchSnapshotFound, nil
		},
		retry: func(_ context.Context, got searchdb.SiteSearchWork, availableAt time.Time, failure error) (searchdb.SiteSearchFenceOutcome, error) {
			if got != work {
				t.Fatalf("retried work = %+v", got)
			}
			retryAt = availableAt
			retryFailure = failure
			return searchdb.SiteSearchFenceApplied, nil
		},
	}
	logger := &recordingWorkerLogger{}
	runner := newWorkerTestRunner(repository, opener)
	runner.now = func() time.Time { return now }
	runner.logger = logger
	runner.config.retryBase = time.Second
	runner.config.retryMaximum = 5 * time.Second

	runner.process(context.Background(), work)

	if want := now.Add(5 * time.Second); !retryAt.Equal(want) {
		t.Fatalf("retry available_at = %s, want %s", retryAt, want)
	}
	if retryFailure == nil {
		t.Fatal("retry did not receive a failure")
	}
	if strings.Contains(retryFailure.Error(), "super-secret") || strings.Contains(retryFailure.Error(), "extracted-content-marker") {
		t.Fatalf("retry failure leaked sensitive input: %q", retryFailure)
	}
	if !strings.Contains(retryFailure.Error(), "open_version") {
		t.Fatalf("retry failure = %q, want bounded stage", retryFailure)
	}
	for _, line := range logger.Lines() {
		if strings.Contains(line, "super-secret") || strings.Contains(line, "extracted-content-marker") {
			t.Fatalf("worker log leaked sensitive input: %q", line)
		}
		if len(line) > 1024 {
			t.Fatalf("worker log is unbounded (%d bytes): %q", len(line), line)
		}
	}
	if repository.retryCalls() != 1 {
		t.Fatalf("retry calls = %d, want 1", repository.retryCalls())
	}
}

func TestWorkerDatabaseOperationsUseDeadlines(t *testing.T) {
	const operationTimeout = 5 * time.Millisecond

	t.Run("startup enqueue", func(t *testing.T) {
		calls := 0
		repository := &fakeSearchRepository{
			enqueueMissing: func(ctx context.Context, _ int) (int64, error) {
				calls++
				if calls == 1 {
					waitForWorkerDeadline(t, ctx, operationTimeout)
					return 0, ctx.Err()
				}
				return 1, nil
			},
		}
		runner := newWorkerTestRunner(repository, unusedVersionOpener{})
		runner.config.operationTimeout = operationTimeout

		if !runner.enqueueMissingUntilSuccessful(context.Background()) {
			t.Fatal("startup enqueue stopped after an operation deadline")
		}
		if calls != 2 {
			t.Fatalf("startup enqueue calls = %d, want 2", calls)
		}
	})

	t.Run("claim", func(t *testing.T) {
		repository := &fakeSearchRepository{
			claim: func(ctx context.Context, _ string, _ time.Duration) (searchdb.SiteSearchWork, searchdb.SiteSearchClaimOutcome, error) {
				waitForWorkerDeadline(t, ctx, operationTimeout)
				return searchdb.SiteSearchWork{}, searchdb.SiteSearchClaimUnknown, ctx.Err()
			},
		}
		runner := newWorkerTestRunner(repository, unusedVersionOpener{})
		runner.config.operationTimeout = operationTimeout

		if !runner.claimAndProcessOne(context.Background()) {
			t.Fatal("claim deadline stopped the live worker lifecycle")
		}
	})

	t.Run("snapshot load", func(t *testing.T) {
		work := testSearchWork(searchdb.SiteSearchReconcile, 1)
		repository := &fakeSearchRepository{
			loadSnapshot: func(ctx context.Context, _ string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error) {
				waitForWorkerDeadline(t, ctx, operationTimeout)
				return searchdb.SiteSearchSnapshot{}, searchdb.SiteSearchSnapshotUnknown, ctx.Err()
			},
			retry: func(ctx context.Context, got searchdb.SiteSearchWork, _ time.Time, _ error) (searchdb.SiteSearchFenceOutcome, error) {
				if got != work {
					t.Fatalf("retried work = %+v, want %+v", got, work)
				}
				requireWorkerDeadline(t, ctx, operationTimeout)
				return searchdb.SiteSearchFenceApplied, nil
			},
		}
		runner := newWorkerTestRunner(repository, unusedVersionOpener{})
		runner.config.operationTimeout = operationTimeout

		runner.process(context.Background(), work)
		if repository.retryCalls() != 1 {
			t.Fatalf("snapshot deadline retry calls = %d, want 1", repository.retryCalls())
		}
	})

	t.Run("delete acknowledge", func(t *testing.T) {
		work := testSearchWork(searchdb.SiteSearchDelete, 1)
		repository := &fakeSearchRepository{
			acknowledgeDelete: func(ctx context.Context, _ searchdb.SiteSearchWork) (searchdb.SiteSearchFenceOutcome, error) {
				waitForWorkerDeadline(t, ctx, operationTimeout)
				return searchdb.SiteSearchFenceUnknown, ctx.Err()
			},
			retry: func(ctx context.Context, got searchdb.SiteSearchWork, _ time.Time, _ error) (searchdb.SiteSearchFenceOutcome, error) {
				if got != work {
					t.Fatalf("retried work = %+v, want %+v", got, work)
				}
				requireWorkerDeadline(t, ctx, operationTimeout)
				return searchdb.SiteSearchFenceApplied, nil
			},
		}
		runner := newWorkerTestRunner(repository, unusedVersionOpener{})
		runner.config.operationTimeout = operationTimeout

		runner.process(context.Background(), work)
		if repository.acknowledgeCalls() != 1 || repository.retryCalls() != 1 {
			t.Fatalf("acknowledge/retry calls = %d/%d, want 1/1", repository.acknowledgeCalls(), repository.retryCalls())
		}
	})

	t.Run("fenced retry", func(t *testing.T) {
		work := testSearchWork(searchdb.SiteSearchReconcile, 1)
		repository := &fakeSearchRepository{
			retry: func(ctx context.Context, _ searchdb.SiteSearchWork, _ time.Time, _ error) (searchdb.SiteSearchFenceOutcome, error) {
				waitForWorkerDeadline(t, ctx, operationTimeout)
				return searchdb.SiteSearchFenceUnknown, ctx.Err()
			},
		}
		runner := newWorkerTestRunner(repository, unusedVersionOpener{})
		runner.config.operationTimeout = operationTimeout

		runner.retryFailure(context.Background(), work, "test", errors.New("test failure"))
		if repository.retryCalls() != 1 {
			t.Fatalf("fenced retry calls = %d, want 1", repository.retryCalls())
		}
	})
}

func TestWorkerPublishDeadlineUsesFreshFencedRetryContext(t *testing.T) {
	const (
		operationTimeout = 20 * time.Millisecond
		publishTimeout   = 5 * time.Millisecond
	)
	work := testSearchWork(searchdb.SiteSearchReconcile, 1)
	var publishDeadline time.Time
	retryCalled := false
	repository := &fakeSearchRepository{
		loadSnapshot: func(context.Context, string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error) {
			return testSearchSnapshot(), searchdb.SiteSearchSnapshotFound, nil
		},
		publishReconcile: func(ctx context.Context, _ searchdb.SiteSearchWork, _ searchdb.SiteSearchSnapshot, _ int, _ []searchdb.SearchDocument, _ bool) (searchdb.SiteSearchFenceOutcome, error) {
			publishDeadline = requireWorkerDeadline(t, ctx, publishTimeout)
			<-ctx.Done()
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Fatalf("publish context error = %v, want deadline exceeded", ctx.Err())
			}
			return searchdb.SiteSearchFenceUnknown, ctx.Err()
		},
		retry: func(ctx context.Context, got searchdb.SiteSearchWork, _ time.Time, _ error) (searchdb.SiteSearchFenceOutcome, error) {
			retryCalled = true
			if got != work {
				t.Fatalf("retried work = %+v, want %+v", got, work)
			}
			if ctx.Err() != nil {
				t.Fatalf("fresh retry context is already canceled: %v", ctx.Err())
			}
			retryDeadline := requireWorkerDeadline(t, ctx, operationTimeout)
			if !retryDeadline.After(publishDeadline) {
				t.Fatalf("retry deadline %s is not after expired publish deadline %s", retryDeadline, publishDeadline)
			}
			return searchdb.SiteSearchFenceApplied, nil
		},
	}
	runner := newWorkerTestRunner(repository, newWorkerTestVersion(t, map[string]string{"index.html": "<body>demo</body>"}))
	runner.config.operationTimeout = operationTimeout
	runner.config.publishTimeout = publishTimeout

	runner.process(context.Background(), work)
	if !retryCalled || repository.retryCalls() != 1 {
		t.Fatalf("publish deadline retry called/count = %t/%d, want true/1", retryCalled, repository.retryCalls())
	}
}

func TestWorkerLifecycleCancellationDoesNotRetry(t *testing.T) {
	const operationTimeout = 5 * time.Millisecond
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	defer cancelLifecycle()
	work := testSearchWork(searchdb.SiteSearchReconcile, 1)
	repository := &fakeSearchRepository{
		loadSnapshot: func(ctx context.Context, _ string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error) {
			requireWorkerDeadline(t, ctx, operationTimeout)
			cancelLifecycle()
			<-ctx.Done()
			return searchdb.SiteSearchSnapshot{}, searchdb.SiteSearchSnapshotUnknown, ctx.Err()
		},
		retry: func(context.Context, searchdb.SiteSearchWork, time.Time, error) (searchdb.SiteSearchFenceOutcome, error) {
			t.Fatal("shutdown cancellation invoked fenced retry")
			return searchdb.SiteSearchFenceUnknown, nil
		},
	}
	runner := newWorkerTestRunner(repository, unusedVersionOpener{})
	runner.config.operationTimeout = operationTimeout

	runner.process(lifecycleCtx, work)
	if repository.retryCalls() != 0 {
		t.Fatalf("shutdown cancellation retry calls = %d, want 0", repository.retryCalls())
	}
}

func TestWorkerExhaustedLeaseBudgetSkipsWorkAndRetriesFresh(t *testing.T) {
	const (
		operationTimeout   = 20 * time.Millisecond
		leaseSafetyReserve = 5 * time.Millisecond
	)
	work := testSearchWork(searchdb.SiteSearchReconcile, 1)
	work.LockedUntil = workerTestNow().Add(leaseSafetyReserve)
	opener := newWorkerTestVersion(t, map[string]string{"index.html": "<body>must not open</body>"})
	repository := &fakeSearchRepository{
		loadSnapshot: func(context.Context, string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error) {
			t.Fatal("exhausted lease budget loaded a snapshot")
			return searchdb.SiteSearchSnapshot{}, searchdb.SiteSearchSnapshotUnknown, nil
		},
		publishReconcile: func(context.Context, searchdb.SiteSearchWork, searchdb.SiteSearchSnapshot, int, []searchdb.SearchDocument, bool) (searchdb.SiteSearchFenceOutcome, error) {
			t.Fatal("exhausted lease budget published documents")
			return searchdb.SiteSearchFenceUnknown, nil
		},
		retry: func(ctx context.Context, got searchdb.SiteSearchWork, _ time.Time, failure error) (searchdb.SiteSearchFenceOutcome, error) {
			if got != work {
				t.Fatalf("retried work = %+v, want %+v", got, work)
			}
			if ctx.Err() != nil {
				t.Fatalf("lease-budget retry context is canceled: %v", ctx.Err())
			}
			requireWorkerDeadline(t, ctx, operationTimeout)
			if failure == nil || !strings.Contains(failure.Error(), "lease_budget") {
				t.Fatalf("lease-budget retry failure = %v", failure)
			}
			return searchdb.SiteSearchFenceApplied, nil
		},
	}
	runner := newWorkerTestRunner(repository, opener)
	runner.config.operationTimeout = operationTimeout
	runner.config.leaseSafetyReserve = leaseSafetyReserve
	runner.extract = func(context.Context, *os.Root, string, string) (Result, error) {
		t.Fatal("exhausted lease budget invoked extraction")
		return Result{}, nil
	}

	if budget := runner.claimedWorkBudget(work); budget != 0 {
		t.Fatalf("claimed work budget = %s, want 0", budget)
	}
	runner.process(context.Background(), work)
	if calls := opener.Calls(); len(calls) != 0 {
		t.Fatalf("exhausted lease budget opened versions: %#v", calls)
	}
	if repository.retryCalls() != 1 {
		t.Fatalf("lease-budget retry calls = %d, want 1", repository.retryCalls())
	}
}

func TestWorkerConfigValidatesTimeoutsAndLeaseBudget(t *testing.T) {
	configured := defaultWorkerConfig()
	if configured.operationTimeout != 10*time.Second || configured.publishTimeout != 60*time.Second || configured.leaseSafetyReserve != 20*time.Second {
		t.Fatalf(
			"default operation/publish/reserve = %s/%s/%s, want 10s/60s/20s",
			configured.operationTimeout,
			configured.publishTimeout,
			configured.leaseSafetyReserve,
		)
	}
	if extractionDeadline != 30*time.Second {
		t.Fatalf("extraction deadline = %s, want 30s", extractionDeadline)
	}
	if err := configured.validate(); err != nil {
		t.Fatalf("default worker config: %v", err)
	}

	for _, test := range []struct {
		name   string
		update func(*workerConfig)
	}{
		{name: "operation timeout", update: func(config *workerConfig) { config.operationTimeout = 0 }},
		{name: "publish timeout", update: func(config *workerConfig) { config.publishTimeout = 0 }},
		{name: "lease safety reserve", update: func(config *workerConfig) { config.leaseSafetyReserve = 0 }},
	} {
		t.Run(test.name+" must be positive", func(t *testing.T) {
			invalid := configured
			test.update(&invalid)
			if err := invalid.validate(); err == nil || !strings.Contains(err.Error(), test.name) {
				t.Fatalf("validation error = %v, want %q positivity error", err, test.name)
			}
		})
	}

	requiredLease := configured.operationTimeout + extractionDeadline + configured.publishTimeout + configured.leaseSafetyReserve
	exact := configured
	exact.leaseDuration = requiredLease
	if err := exact.validate(); err != nil {
		t.Fatalf("exact deadline budget should fit lease: %v", err)
	}
	insufficient := exact
	insufficient.leaseDuration--
	if err := insufficient.validate(); err == nil {
		t.Fatal("worker config accepted a lease shorter than its deadline budget")
	}

	runner := workerRunner{workerDependencies: workerDependencies{
		now:    workerTestNow,
		config: configured,
	}}
	work := testSearchWork(searchdb.SiteSearchReconcile, 1)
	work.LockedUntil = workerTestNow().Add(configured.leaseSafetyReserve + 17*time.Second)
	if budget := runner.claimedWorkBudget(work); budget != 17*time.Second {
		t.Fatalf("claimed work budget = %s, want 17s", budget)
	}
}

func TestWorkerStartupEnqueueRetriesWithoutBlockingStart(t *testing.T) {
	t.Run("retry until success", func(t *testing.T) {
		var mu sync.Mutex
		enqueueCalls := 0
		var versions []int
		claimStarted := make(chan struct{})
		var claimOnce sync.Once
		repository := &fakeSearchRepository{
			enqueueMissing: func(_ context.Context, extractorVersion int) (int64, error) {
				mu.Lock()
				defer mu.Unlock()
				enqueueCalls++
				versions = append(versions, extractorVersion)
				if enqueueCalls < 3 {
					return 0, errors.New("temporary startup failure")
				}
				return 2, nil
			},
			claim: func(ctx context.Context, _ string, _ time.Duration) (searchdb.SiteSearchWork, searchdb.SiteSearchClaimOutcome, error) {
				claimOnce.Do(func() { close(claimStarted) })
				<-ctx.Done()
				return searchdb.SiteSearchWork{}, searchdb.SiteSearchClaimUnknown, ctx.Err()
			},
		}
		var waitMu sync.Mutex
		var delays []time.Duration
		dependencies := newWorkerTestDependencies(repository, unusedVersionOpener{})
		dependencies.wait = func(ctx context.Context, delay time.Duration) bool {
			waitMu.Lock()
			delays = append(delays, delay)
			waitMu.Unlock()
			return ctx.Err() == nil
		}

		worker, err := startWorker(context.Background(), dependencies)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-claimStarted:
		case <-time.After(time.Second):
			worker.Stop()
			t.Fatal("worker did not reach claim after startup retries")
		}
		worker.Stop()
		waitForWorkerTest(t, worker)

		mu.Lock()
		gotCalls := enqueueCalls
		gotVersions := append([]int(nil), versions...)
		mu.Unlock()
		if gotCalls != 3 || !reflect.DeepEqual(gotVersions, []int{ExtractorVersion, ExtractorVersion, ExtractorVersion}) {
			t.Fatalf("startup enqueue calls/versions = %d/%v", gotCalls, gotVersions)
		}
		waitMu.Lock()
		gotDelays := append([]time.Duration(nil), delays...)
		waitMu.Unlock()
		if !reflect.DeepEqual(gotDelays, []time.Duration{time.Second, 2 * time.Second}) {
			t.Fatalf("startup retry delays = %v", gotDelays)
		}
	})

	t.Run("blocked startup work is asynchronous", func(t *testing.T) {
		enqueueStarted := make(chan struct{})
		var once sync.Once
		repository := &fakeSearchRepository{
			enqueueMissing: func(ctx context.Context, _ int) (int64, error) {
				once.Do(func() { close(enqueueStarted) })
				<-ctx.Done()
				return 0, ctx.Err()
			},
		}
		worker, err := startWorker(context.Background(), newWorkerTestDependencies(repository, unusedVersionOpener{}))
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-enqueueStarted:
		case <-time.After(time.Second):
			worker.Stop()
			t.Fatal("startup enqueue did not begin")
		}
		worker.Stop()
		waitForWorkerTest(t, worker)
	})
}

func TestWorkerClaimsWithRandomTwoMinuteLeaseAndCancelsPromptly(t *testing.T) {
	claimStarted := make(chan struct{})
	var once sync.Once
	var token string
	var lease time.Duration
	repository := &fakeSearchRepository{
		enqueueMissing: func(context.Context, int) (int64, error) { return 0, nil },
		claim: func(ctx context.Context, gotToken string, gotLease time.Duration) (searchdb.SiteSearchWork, searchdb.SiteSearchClaimOutcome, error) {
			token = gotToken
			lease = gotLease
			once.Do(func() { close(claimStarted) })
			<-ctx.Done()
			return searchdb.SiteSearchWork{}, searchdb.SiteSearchClaimUnknown, ctx.Err()
		},
	}
	dependencies := newWorkerTestDependencies(repository, unusedVersionOpener{})
	dependencies.leaseToken = randomLeaseToken
	worker, err := startWorker(context.Background(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-claimStarted:
	case <-time.After(time.Second):
		worker.Stop()
		t.Fatal("claim did not start")
	}
	if token == "" {
		worker.Stop()
		t.Fatal("claim used an empty lease token")
	}
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 24 {
		worker.Stop()
		t.Fatalf("lease token is not 24 random bytes encoded as hex: %q, %v", token, err)
	}
	if lease != 2*time.Minute || lease <= extractionDeadline {
		worker.Stop()
		t.Fatalf("lease = %s; want 2m and longer than %s", lease, extractionDeadline)
	}

	started := time.Now()
	worker.Stop()
	waitForWorkerTest(t, worker)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("worker cancellation took %s", elapsed)
	}
}

func TestWorkerProcessesOnlyOneClaimAtATime(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondClaimed := make(chan struct{})
	var claimMu sync.Mutex
	claims := 0
	repository := &fakeSearchRepository{
		enqueueMissing: func(context.Context, int) (int64, error) { return 0, nil },
		claim: func(ctx context.Context, token string, _ time.Duration) (searchdb.SiteSearchWork, searchdb.SiteSearchClaimOutcome, error) {
			claimMu.Lock()
			claims++
			claimNumber := claims
			claimMu.Unlock()
			switch claimNumber {
			case 1:
				work := testSearchWork(searchdb.SiteSearchReconcile, 1)
				work.SiteID = "site-1"
				work.LeaseToken = token
				return work, searchdb.SiteSearchClaimed, nil
			case 2:
				close(secondClaimed)
				work := testSearchWork(searchdb.SiteSearchReconcile, 1)
				work.SiteID = "site-2"
				work.LeaseToken = token
				return work, searchdb.SiteSearchClaimed, nil
			default:
				<-ctx.Done()
				return searchdb.SiteSearchWork{}, searchdb.SiteSearchClaimUnknown, ctx.Err()
			}
		},
		loadSnapshot: func(_ context.Context, siteID string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error) {
			snapshot := testSearchSnapshot()
			snapshot.SiteID = siteID
			return snapshot, searchdb.SiteSearchSnapshotFound, nil
		},
		publishReconcile: func(context.Context, searchdb.SiteSearchWork, searchdb.SiteSearchSnapshot, int, []searchdb.SearchDocument, bool) (searchdb.SiteSearchFenceOutcome, error) {
			return searchdb.SiteSearchFenceApplied, nil
		},
	}
	opener := newWorkerTestVersion(t, map[string]string{"index.html": "<body>demo</body>"})
	dependencies := newWorkerTestDependencies(repository, opener)
	var extractMu sync.Mutex
	active := 0
	maximumActive := 0
	extractCalls := 0
	dependencies.extract = func(ctx context.Context, root *os.Root, owner, site string) (Result, error) {
		extractMu.Lock()
		active++
		extractCalls++
		call := extractCalls
		if active > maximumActive {
			maximumActive = active
		}
		extractMu.Unlock()
		if call == 1 {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return Result{}, ctx.Err()
			}
		}
		result, err := Extract(ctx, root, owner, site)
		extractMu.Lock()
		active--
		extractMu.Unlock()
		return result, err
	}

	worker, err := startWorker(context.Background(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		worker.Stop()
		t.Fatal("first extraction did not start")
	}
	select {
	case <-secondClaimed:
		worker.Stop()
		close(releaseFirst)
		t.Fatal("second item was claimed while the first was still extracting")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	select {
	case <-secondClaimed:
	case <-time.After(time.Second):
		worker.Stop()
		t.Fatal("second item was not claimed after the first completed")
	}
	worker.Stop()
	waitForWorkerTest(t, worker)
	extractMu.Lock()
	gotMaximum := maximumActive
	extractMu.Unlock()
	if gotMaximum != 1 {
		t.Fatalf("maximum concurrent extractions = %d, want 1", gotMaximum)
	}
}

func TestCappedExponentialDelay(t *testing.T) {
	for _, test := range []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: -1, want: time.Second},
		{attempt: 1, want: time.Second},
		{attempt: 2, want: 2 * time.Second},
		{attempt: 3, want: 4 * time.Second},
		{attempt: 4, want: 5 * time.Second},
		{attempt: 100, want: 5 * time.Second},
	} {
		if got := cappedExponentialDelay(time.Second, 5*time.Second, test.attempt); got != test.want {
			t.Errorf("attempt %d delay = %s, want %s", test.attempt, got, test.want)
		}
	}
}

func newWorkerTestRunner(repository searchRepository, versions VersionOpener) workerRunner {
	return workerRunner{workerDependencies: newWorkerTestDependencies(repository, versions)}
}

func newWorkerTestDependencies(repository searchRepository, versions VersionOpener) workerDependencies {
	return workerDependencies{
		repository: repository,
		versions:   versions,
		extract:    Extract,
		leaseToken: func() (string, error) { return "test-token", nil },
		now:        workerTestNow,
		wait: func(ctx context.Context, _ time.Duration) bool {
			return ctx.Err() == nil
		},
		logger: &recordingWorkerLogger{},
		config: defaultWorkerConfig(),
	}
}

func testSearchWork(operation searchdb.SiteSearchOperation, attempts int) searchdb.SiteSearchWork {
	now := workerTestNow()
	return searchdb.SiteSearchWork{
		SiteID:      "site-id",
		Operation:   operation,
		Generation:  3,
		LeaseToken:  "test-token",
		AvailableAt: now,
		LockedUntil: now.Add(workerLeaseDuration),
		Attempts:    attempts,
	}
}

func workerTestNow() time.Time {
	return time.Unix(1_700_000_000, 0).UTC()
}

func testSearchSnapshot() searchdb.SiteSearchSnapshot {
	return searchdb.SiteSearchSnapshot{
		SiteID:        "site-id",
		OwnerName:     "alice",
		SiteName:      "demo",
		ActiveVersion: 7,
	}
}

func requireWorkerDeadline(t *testing.T, ctx context.Context, maximum time.Duration) time.Time {
	t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("worker operation context has no deadline")
	}
	if remaining := time.Until(deadline); remaining > maximum+time.Millisecond {
		t.Fatalf("worker operation deadline has %s remaining, want at most %s", remaining, maximum)
	}
	return deadline
}

func waitForWorkerDeadline(t *testing.T, ctx context.Context, maximum time.Duration) {
	t.Helper()
	requireWorkerDeadline(t, ctx, maximum)
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("worker operation context error = %v, want deadline exceeded", ctx.Err())
	}
}

func waitForWorkerTest(t *testing.T, worker *Worker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := worker.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

type versionOpenCall struct {
	owner   string
	site    string
	version int
}

type workerTestVersionOpener struct {
	path  string
	mu    sync.Mutex
	calls []versionOpenCall
}

func newWorkerTestVersion(t *testing.T, files map[string]string) *workerTestVersionOpener {
	t.Helper()
	base := t.TempDir()
	for name, contents := range files {
		fullPath := filepath.Join(base, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &workerTestVersionOpener{path: base}
}

func (o *workerTestVersionOpener) OpenVersion(owner, site string, version int) (*os.Root, error) {
	o.mu.Lock()
	o.calls = append(o.calls, versionOpenCall{owner: owner, site: site, version: version})
	o.mu.Unlock()
	return os.OpenRoot(o.path)
}

func (o *workerTestVersionOpener) Calls() []versionOpenCall {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]versionOpenCall(nil), o.calls...)
}

type unusedVersionOpener struct{}

func (unusedVersionOpener) OpenVersion(string, string, int) (*os.Root, error) {
	return nil, errors.New("unexpected OpenVersion call")
}

type failingVersionOpener struct {
	err error
}

func (o failingVersionOpener) OpenVersion(string, string, int) (*os.Root, error) {
	return nil, o.err
}

type recordingWorkerLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingWorkerLogger) Printf(format string, arguments ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, arguments...))
	l.mu.Unlock()
}

func (l *recordingWorkerLogger) Lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

type fakeSearchRepository struct {
	enqueueMissing    func(context.Context, int) (int64, error)
	claim             func(context.Context, string, time.Duration) (searchdb.SiteSearchWork, searchdb.SiteSearchClaimOutcome, error)
	loadSnapshot      func(context.Context, string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error)
	publishReconcile  func(context.Context, searchdb.SiteSearchWork, searchdb.SiteSearchSnapshot, int, []searchdb.SearchDocument, bool) (searchdb.SiteSearchFenceOutcome, error)
	acknowledgeDelete func(context.Context, searchdb.SiteSearchWork) (searchdb.SiteSearchFenceOutcome, error)
	retry             func(context.Context, searchdb.SiteSearchWork, time.Time, error) (searchdb.SiteSearchFenceOutcome, error)

	mu               sync.Mutex
	loadCount        int
	acknowledgeCount int
	retryCount       int
}

func (r *fakeSearchRepository) EnqueueMissing(ctx context.Context, extractorVersion int) (int64, error) {
	if r.enqueueMissing == nil {
		return 0, errors.New("unexpected EnqueueMissing call")
	}
	return r.enqueueMissing(ctx, extractorVersion)
}

func (r *fakeSearchRepository) Claim(ctx context.Context, token string, lease time.Duration) (searchdb.SiteSearchWork, searchdb.SiteSearchClaimOutcome, error) {
	if r.claim == nil {
		return searchdb.SiteSearchWork{}, searchdb.SiteSearchClaimUnknown, errors.New("unexpected Claim call")
	}
	return r.claim(ctx, token, lease)
}

func (r *fakeSearchRepository) LoadSnapshot(ctx context.Context, siteID string) (searchdb.SiteSearchSnapshot, searchdb.SiteSearchSnapshotOutcome, error) {
	r.mu.Lock()
	r.loadCount++
	r.mu.Unlock()
	if r.loadSnapshot == nil {
		return searchdb.SiteSearchSnapshot{}, searchdb.SiteSearchSnapshotUnknown, errors.New("unexpected LoadSnapshot call")
	}
	return r.loadSnapshot(ctx, siteID)
}

func (r *fakeSearchRepository) PublishReconcile(ctx context.Context, work searchdb.SiteSearchWork, snapshot searchdb.SiteSearchSnapshot, extractorVersion int, documents []searchdb.SearchDocument, partial bool) (searchdb.SiteSearchFenceOutcome, error) {
	if r.publishReconcile == nil {
		return searchdb.SiteSearchFenceUnknown, errors.New("unexpected PublishReconcile call")
	}
	return r.publishReconcile(ctx, work, snapshot, extractorVersion, documents, partial)
}

func (r *fakeSearchRepository) AcknowledgeDelete(ctx context.Context, work searchdb.SiteSearchWork) (searchdb.SiteSearchFenceOutcome, error) {
	r.mu.Lock()
	r.acknowledgeCount++
	r.mu.Unlock()
	if r.acknowledgeDelete == nil {
		return searchdb.SiteSearchFenceUnknown, errors.New("unexpected AcknowledgeDelete call")
	}
	return r.acknowledgeDelete(ctx, work)
}

func (r *fakeSearchRepository) Retry(ctx context.Context, work searchdb.SiteSearchWork, availableAt time.Time, failure error) (searchdb.SiteSearchFenceOutcome, error) {
	r.mu.Lock()
	r.retryCount++
	r.mu.Unlock()
	if r.retry == nil {
		return searchdb.SiteSearchFenceUnknown, errors.New("unexpected Retry call")
	}
	return r.retry(ctx, work, availableAt, failure)
}

func (r *fakeSearchRepository) loadCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loadCount
}

func (r *fakeSearchRepository) acknowledgeCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.acknowledgeCount
}

func (r *fakeSearchRepository) retryCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.retryCount
}
