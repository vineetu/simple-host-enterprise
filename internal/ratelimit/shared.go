package ratelimit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"log"
	"math"
	"sync"
	"time"
)

const (
	// sharedCheckTimeout bounds the one statement per check. A slower
	// database falls back to the in-memory limiter rather than stalling
	// sign-in behind it.
	sharedCheckTimeout = time.Second
	// MaxSharedWindow is the longest window a shared policy may use. Rows
	// are pruned once their window started more than sharedPruneAge ago, so
	// every window must be well inside that.
	MaxSharedWindow  = 30 * time.Minute
	sharedPruneAge   = time.Hour
	sharedPruneEvery = time.Minute
	sharedPruneBatch = 10_000
	sharedLogEvery   = time.Minute
)

// FixedWindow maps a token-bucket policy onto the fixed window the shared
// limiter counts in: Limit = Burst requests per Window = Burst/Refill (the
// time an empty bucket takes to refill). That keeps both the long-run rate
// (Burst/Window = Refill) and the burst. The worst case also matches: a
// bucket admits at most 2*Burst in any Window (a full bucket plus one
// refill), and a fixed window admits at most 2*Limit across one boundary.
// Retry-After is up to one Window rather than one token's refill time.
func FixedWindow(p Policy) (limit int, window time.Duration) {
	if p.Burst <= 0 || p.RefillPerSecond <= 0 || math.IsNaN(p.RefillPerSecond) || math.IsInf(p.RefillPerSecond, 0) {
		return 0, 0
	}
	seconds := math.Ceil(float64(p.Burst) / p.RefillPerSecond)
	return p.Burst, time.Duration(seconds) * time.Second
}

// Shared is a Postgres-backed fixed-window limiter shared by every replica
// (table rate_limit_counters, migration 0039). A database error falls back
// to the per-pod in-memory limiter for that request: failing closed would
// lock every user out of sign-in during a database blip, failing open would
// drop brute-force protection entirely; per-pod limits are exactly the
// protection every release before this one had.
type Shared struct {
	db       *sql.DB
	fallback *Limiter
	now      func() time.Time

	mu        sync.Mutex
	lastPrune time.Time
	lastLog   time.Time
}

// NewShared returns a shared limiter over database, falling back to
// fallback on any database error.
func NewShared(database *sql.DB, fallback *Limiter) *Shared {
	if database == nil || fallback == nil {
		panic("ratelimit: shared limiter needs a database and a fallback")
	}
	return &Shared{db: database, fallback: fallback, now: time.Now}
}

// SharedKey is the stored key: a sha256 digest of the policy name and the
// caller key, so no raw address, user id or email reaches the table and two
// policies never share a counter.
func SharedKey(policy, key string) string {
	sum := sha256.Sum256([]byte(policy + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

const sharedCheckSQL = `
INSERT INTO rate_limit_counters (key, window_start, count)
VALUES ($1, date_bin(make_interval(secs => $2::double precision), now(), TIMESTAMPTZ '1970-01-01 00:00:00+00'), 1)
ON CONFLICT (key, window_start) DO UPDATE SET count = rate_limit_counters.count + 1
RETURNING count, EXTRACT(EPOCH FROM (window_start + make_interval(secs => $2::double precision) - now()))::double precision`

// Allow counts one request for policy/key in the current window, shared
// across every pod on the database.
func (s *Shared) Allow(policy Policy, key string) Decision {
	limit, window := FixedWindow(policy)
	if policy.Name == "" || key == "" || limit <= 0 || window > MaxSharedWindow {
		return Decision{RetryAfter: time.Second}
	}
	s.maybePrune()

	ctx, cancel := context.WithTimeout(context.Background(), sharedCheckTimeout)
	defer cancel()
	var count int64
	var remaining float64
	err := s.db.QueryRowContext(ctx, sharedCheckSQL, SharedKey(policy.Name, key), int64(window/time.Second)).Scan(&count, &remaining)
	if err != nil {
		s.logThrottled("ratelimit: shared limiter unavailable, using per-pod limits: %v", err)
		return s.fallback.Allow(policy, key)
	}
	if count <= int64(limit) {
		return Decision{Allowed: true}
	}
	retry := time.Duration(math.Ceil(remaining)) * time.Second
	if retry < time.Second {
		retry = time.Second
	}
	return Decision{RetryAfter: retry}
}

// maybePrune deletes counters whose window is long over, at most once a
// minute per pod and at most sharedPruneBatch rows per run, in the
// background so no request waits on it. No separate job is needed: any pod
// that is checking limits is also cleaning up after them.
func (s *Shared) maybePrune() {
	now := s.now()
	s.mu.Lock()
	if !s.lastPrune.IsZero() && now.Sub(s.lastPrune) < sharedPruneEvery {
		s.mu.Unlock()
		return
	}
	s.lastPrune = now
	s.mu.Unlock()
	go func() {
		if _, err := s.Prune(context.Background()); err != nil {
			s.logThrottled("ratelimit: prune shared counters: %v", err)
		}
	}()
}

// Prune deletes up to sharedPruneBatch counters whose window started more
// than an hour ago (every shared window is at most MaxSharedWindow, so each
// of those windows has ended). The ctid array keeps the delete bounded.
func (s *Shared) Prune(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := s.db.ExecContext(ctx, `
DELETE FROM rate_limit_counters WHERE ctid = ANY(ARRAY(
    SELECT ctid FROM rate_limit_counters
    WHERE window_start < now() - make_interval(secs => $1::double precision)
    LIMIT $2))`, sharedPruneAge.Seconds(), sharedPruneBatch)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Shared) logThrottled(format string, args ...any) {
	now := s.now()
	s.mu.Lock()
	if !s.lastLog.IsZero() && now.Sub(s.lastLog) < sharedLogEvery {
		s.mu.Unlock()
		return
	}
	s.lastLog = now
	s.mu.Unlock()
	log.Printf(format, args...)
}
