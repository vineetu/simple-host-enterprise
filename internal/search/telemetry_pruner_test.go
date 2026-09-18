package search

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTelemetryPrunerStartsImmediatelyAndSchedulesSixHoursAfterShortBatch(t *testing.T) {
	started := make(chan struct{})
	delays := make(chan time.Duration, 1)
	var calls atomic.Int32
	pruner, err := startTelemetryPruner(context.Background(), telemetryPrunerDependencies{
		repository: telemetryPruneRepositoryFunc(func(ctx context.Context, batchSize int) (int64, error) {
			calls.Add(1)
			if batchSize != telemetryPruneBatchSize {
				t.Fatalf("batch size = %d, want %d", batchSize, telemetryPruneBatchSize)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("initial prune omitted its attempt deadline")
			}
			close(started)
			return 0, nil
		}),
		wait: func(ctx context.Context, delay time.Duration) bool {
			delays <- delay
			<-ctx.Done()
			return false
		},
		logger: &recordingTelemetryPrunerLogger{},
		config: defaultTelemetryPrunerConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pruner.Stop)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("autonomous pruner did not begin its initial attempt promptly")
	}
	select {
	case delay := <-delays:
		if delay != telemetryPruneInterval {
			t.Fatalf("short-batch delay = %v, want %v", delay, telemetryPruneInterval)
		}
	case <-time.After(time.Second):
		t.Fatal("autonomous pruner did not schedule after its initial attempt")
	}
	if calls.Load() != 1 {
		t.Fatalf("initial attempt calls = %d, want 1", calls.Load())
	}

	pruner.Stop()
	pruner.Stop()
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := pruner.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
	if err := pruner.Wait(waitCtx); err != nil {
		t.Fatalf("second Wait = %v", err)
	}
}

func TestTelemetryPrunerAttemptUsesOneDeadlineUntilShortBatch(t *testing.T) {
	results := []int64{telemetryPruneBatchSize, telemetryPruneBatchSize, telemetryPruneBatchSize - 1}
	var firstDeadline time.Time
	calls := 0
	runner := newTelemetryPrunerTestRunner(telemetryPruneRepositoryFunc(func(ctx context.Context, batchSize int) (int64, error) {
		if batchSize != telemetryPruneBatchSize {
			t.Fatalf("batch size = %d, want %d", batchSize, telemetryPruneBatchSize)
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("prune batch omitted its attempt deadline")
		}
		if calls == 0 {
			firstDeadline = deadline
		} else if !deadline.Equal(firstDeadline) {
			t.Fatalf("batch %d deadline = %v, want shared deadline %v", calls+1, deadline, firstDeadline)
		}
		result := results[calls]
		calls++
		return result, nil
	}))

	delay, keepRunning := runner.pruneAttempt(context.Background())
	if !keepRunning || delay != telemetryPruneInterval || calls != len(results) {
		t.Fatalf("attempt = delay %v, keep %t, calls %d", delay, keepRunning, calls)
	}
}

func TestTelemetryPrunerRetriesAfterDeleteErrorWithoutLoggingDetails(t *testing.T) {
	secret := "retention SQL password=super-secret"
	logger := &recordingTelemetryPrunerLogger{}
	runner := newTelemetryPrunerTestRunner(telemetryPruneRepositoryFunc(func(context.Context, int) (int64, error) {
		return 0, &telemetryPrunerSecretError{secret: secret}
	}))
	runner.logger = logger

	delay, keepRunning := runner.pruneAttempt(context.Background())
	if !keepRunning || delay != telemetryPruneRetryDelay {
		t.Fatalf("error attempt = delay %v, keep %t", delay, keepRunning)
	}
	logs := logger.String()
	if strings.Contains(logs, secret) || strings.Contains(logs, "super-secret") {
		t.Fatalf("pruner log exposed error details: %q", logs)
	}
	if !strings.Contains(logs, "*search.telemetryPrunerSecretError") {
		t.Fatalf("pruner log omitted concrete error type: %q", logs)
	}
}

func TestTelemetryPrunerRunUsesRetryThenRegularSchedule(t *testing.T) {
	secret := "first attempt SQL details"
	calls := 0
	delays := make([]time.Duration, 0, 2)
	runner := newTelemetryPrunerTestRunner(telemetryPruneRepositoryFunc(func(context.Context, int) (int64, error) {
		calls++
		if calls == 1 {
			return 0, &telemetryPrunerSecretError{secret: secret}
		}
		return 0, nil
	}))
	logger := &recordingTelemetryPrunerLogger{}
	runner.logger = logger
	runner.wait = func(_ context.Context, delay time.Duration) bool {
		delays = append(delays, delay)
		return len(delays) == 1
	}

	runner.run(context.Background())
	if calls != 2 {
		t.Fatalf("scheduled attempts = %d, want 2", calls)
	}
	want := []time.Duration{telemetryPruneRetryDelay, telemetryPruneInterval}
	if len(delays) != len(want) || delays[0] != want[0] || delays[1] != want[1] {
		t.Fatalf("scheduled delays = %v, want %v", delays, want)
	}
	if logs := logger.String(); strings.Contains(logs, secret) {
		t.Fatalf("scheduled retry log exposed error details: %q", logs)
	}
}

func TestTelemetryPrunerRetriesAfterAttemptDeadline(t *testing.T) {
	logger := &recordingTelemetryPrunerLogger{}
	runner := newTelemetryPrunerTestRunner(telemetryPruneRepositoryFunc(func(ctx context.Context, _ int) (int64, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 50*time.Millisecond {
			t.Fatalf("attempt deadline = %v, ok %t", deadline, ok)
		}
		<-ctx.Done()
		return 0, ctx.Err()
	}))
	runner.logger = logger
	runner.config.attemptTimeout = 5 * time.Millisecond

	delay, keepRunning := runner.pruneAttempt(context.Background())
	if !keepRunning || delay != telemetryPruneRetryDelay {
		t.Fatalf("timed-out attempt = delay %v, keep %t", delay, keepRunning)
	}
	if logs := logger.String(); !strings.Contains(logs, "context.deadlineExceededError") {
		t.Fatalf("deadline log omitted error type: %q", logs)
	}
}

func TestTelemetryPrunerRetriesAfterTwentyFullBatches(t *testing.T) {
	calls := 0
	runner := newTelemetryPrunerTestRunner(telemetryPruneRepositoryFunc(func(context.Context, int) (int64, error) {
		calls++
		return telemetryPruneBatchSize, nil
	}))

	delay, keepRunning := runner.pruneAttempt(context.Background())
	if !keepRunning || delay != telemetryPruneRetryDelay || calls != telemetryPruneMaxBatches {
		t.Fatalf("full attempt = delay %v, keep %t, calls %d", delay, keepRunning, calls)
	}
}

func TestTelemetryPrunerStopCancelsActiveDeleteAndWaitJoins(t *testing.T) {
	started := make(chan struct{})
	returned := make(chan struct{})
	logger := &recordingTelemetryPrunerLogger{}
	pruner, err := startTelemetryPruner(context.Background(), telemetryPrunerDependencies{
		repository: telemetryPruneRepositoryFunc(func(ctx context.Context, _ int) (int64, error) {
			close(started)
			<-ctx.Done()
			close(returned)
			return 0, ctx.Err()
		}),
		wait:   waitForTelemetryPruneDelay,
		logger: logger,
		config: defaultTelemetryPrunerConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("pruner delete did not start")
	}

	pruner.Stop()
	pruner.Stop()
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := pruner.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("active delete did not observe lifecycle cancellation")
	}
	if logs := logger.String(); logs != "" {
		t.Fatalf("ordinary lifecycle cancellation was logged as a failure: %q", logs)
	}
	if err := pruner.Wait(nil); err == nil {
		t.Fatal("Wait(nil) unexpectedly succeeded")
	}
}

func TestStartTelemetryPrunerValidatesProductionDependencies(t *testing.T) {
	if _, err := StartTelemetryPruner(nil, new(sql.DB)); err == nil {
		t.Fatal("nil parent context unexpectedly succeeded")
	}
	if _, err := StartTelemetryPruner(context.Background(), nil); err == nil {
		t.Fatal("nil database unexpectedly succeeded")
	}

	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	pruner, err := StartTelemetryPruner(parent, new(sql.DB))
	if err != nil {
		t.Fatal(err)
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	if err := pruner.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}

	dependencies := telemetryPrunerDependencies{
		repository: telemetryPruneRepositoryFunc(func(context.Context, int) (int64, error) { return 0, nil }),
		wait:       waitForTelemetryPruneDelay,
		logger:     &recordingTelemetryPrunerLogger{},
		config:     defaultTelemetryPrunerConfig(),
	}
	dependencies.config.batchSize = 0
	if _, err := startTelemetryPruner(context.Background(), dependencies); err == nil {
		t.Fatal("invalid batch size unexpectedly succeeded")
	}
}

type telemetryPruneRepositoryFunc func(context.Context, int) (int64, error)

func (f telemetryPruneRepositoryFunc) DeleteExpired(ctx context.Context, batchSize int) (int64, error) {
	return f(ctx, batchSize)
}

func newTelemetryPrunerTestRunner(repository telemetryPruneRepository) telemetryPrunerRunner {
	return telemetryPrunerRunner{telemetryPrunerDependencies: telemetryPrunerDependencies{
		repository: repository,
		wait:       waitForTelemetryPruneDelay,
		logger:     &recordingTelemetryPrunerLogger{},
		config:     defaultTelemetryPrunerConfig(),
	}}
}

type recordingTelemetryPrunerLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingTelemetryPrunerLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *recordingTelemetryPrunerLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

type telemetryPrunerSecretError struct {
	secret string
}

func (e *telemetryPrunerSecretError) Error() string {
	return e.secret
}

func (e *telemetryPrunerSecretError) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprint(state, e.secret)
}

var _ error = (*telemetryPrunerSecretError)(nil)
