package ratelimit_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/vsriram/simple-host/internal/migrate"
	"github.com/vsriram/simple-host/internal/ratelimit"
)

// sharedTestDB applies the full migration chain to a fresh database on the
// server MIGRATE_TEST_DSN names (one database per test, as
// internal/audit/prune_test.go does), and skips without it.
func sharedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("MIGRATE_TEST_DSN")
	if dsn == "" {
		t.Skip("MIGRATE_TEST_DSN not set")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse MIGRATE_TEST_DSN: %v", err)
	}
	adminDSN := *parsed
	adminDSN.Path = "/postgres"
	admin, err := sql.Open("postgres", adminDSN.String())
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	dbName := "ratelimit_test_" + hex.EncodeToString(suffix)
	if _, err := admin.Exec(`CREATE DATABASE ` + pq.QuoteIdentifier(dbName)); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("postgres", adminDSN.String())
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(`DROP DATABASE IF EXISTS ` + pq.QuoteIdentifier(dbName) + ` WITH (FORCE)`)
	})
	testDSN := *parsed
	testDSN.Path = "/" + dbName
	database, err := sql.Open("postgres", testDSN.String())
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := migrate.Apply(context.Background(), database, 0, nil); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return database
}

func memoryLimiter() *ratelimit.Limiter {
	return ratelimit.New(100, time.Hour, time.Now)
}

// awayFromBoundary waits out the current window when fewer than margin
// seconds of it remain (by the database's clock), so a test's requests all
// land in one window.
func awayFromBoundary(t *testing.T, database *sql.DB, window time.Duration, margin float64) {
	t.Helper()
	var remaining float64
	err := database.QueryRow(`SELECT EXTRACT(EPOCH FROM (date_bin(make_interval(secs => $1::double precision), now(), TIMESTAMPTZ '1970-01-01 00:00:00+00') + make_interval(secs => $1::double precision) - now()))::double precision`, window.Seconds()).Scan(&remaining)
	if err != nil {
		t.Fatal(err)
	}
	if remaining < margin {
		time.Sleep(time.Duration((remaining + 0.2) * float64(time.Second)))
	}
}

func TestFixedWindowMapping(t *testing.T) {
	cases := []struct {
		burst  int
		refill float64
		limit  int
		window time.Duration
	}{
		{20, 0.2, 20, 100 * time.Second},
		{30, 0.1, 30, 300 * time.Second},
		{120, 2, 120, 60 * time.Second},
	}
	for _, c := range cases {
		limit, window := ratelimit.FixedWindow(ratelimit.Policy{Name: "p", Burst: c.burst, RefillPerSecond: c.refill})
		if limit != c.limit || window != c.window {
			t.Errorf("FixedWindow(%d, %v) = %d/%v, want %d/%v", c.burst, c.refill, limit, window, c.limit, c.window)
		}
	}
	if limit, _ := ratelimit.FixedWindow(ratelimit.Policy{Name: "p", Burst: 1}); limit != 0 {
		t.Error("a zero refill must not map to a usable window")
	}
}

func TestSharedKeyIsHashedAndPolicyScoped(t *testing.T) {
	ip := "203.0.113.7"
	a := ratelimit.SharedKey("auth-client", ip)
	if a != ratelimit.SharedKey("auth-client", ip) || len(a) != 64 || strings.Contains(a, ip) {
		t.Fatalf("shared key %q is unstable, the wrong size, or keeps the plaintext", a)
	}
	if a == ratelimit.SharedKey("oauth-token", ip) {
		t.Fatal("two policies share a counter key")
	}
}

func TestSharedBudgetAcrossPodsAndWindowReset(t *testing.T) {
	database := sharedTestDB(t)
	policy := ratelimit.Policy{Name: "test-shared", Burst: 5, RefillPerSecond: 5.0 / 60} // 5 per 60s
	_, window := ratelimit.FixedWindow(policy)
	awayFromBoundary(t, database, window, 10)

	// Two pods: independent limiters (and independent in-memory fallbacks)
	// on one database.
	podA := ratelimit.NewShared(database, memoryLimiter())
	podB := ratelimit.NewShared(database, memoryLimiter())
	key := "198.51.100.9"
	for i := 0; i < 5; i++ {
		pod := podA
		if i%2 == 1 {
			pod = podB
		}
		if d := pod.Allow(policy, key); !d.Allowed {
			t.Fatalf("request %d denied inside the shared budget", i+1)
		}
	}
	for _, pod := range []*ratelimit.Shared{podA, podB} {
		d := pod.Allow(policy, key)
		if d.Allowed {
			t.Fatal("a second pod doubled the budget")
		}
		if d.RetryAfter < time.Second || d.RetryAfter > window {
			t.Fatalf("Retry-After %v is not within the window", d.RetryAfter)
		}
	}
	if d := podB.Allow(policy, "another-caller"); !d.Allowed {
		t.Fatal("another caller shares the budget")
	}

	// The stored key is the digest, never the address.
	var stored string
	var count int
	if err := database.QueryRow(`SELECT key, count FROM rate_limit_counters WHERE key = $1`, ratelimit.SharedKey(policy.Name, key)).Scan(&stored, &count); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if count != 7 {
		t.Fatalf("count = %d, want 7", count)
	}
	var raw int
	if err := database.QueryRow(`SELECT count(*) FROM rate_limit_counters WHERE key LIKE '%' || $1 || '%'`, key).Scan(&raw); err != nil || raw != 0 {
		t.Fatalf("raw key stored (%d rows, err %v)", raw, err)
	}

	// Move this window's counter into the past: the next request is in a
	// new window and starts over.
	if _, err := database.Exec(`UPDATE rate_limit_counters SET window_start = window_start - make_interval(secs => $1::double precision)`, window.Seconds()); err != nil {
		t.Fatal(err)
	}
	if d := podA.Allow(policy, key); !d.Allowed {
		t.Fatal("the next window did not reset the budget")
	}
}

func TestSharedPrune(t *testing.T) {
	database := sharedTestDB(t)
	_, err := database.Exec(`INSERT INTO rate_limit_counters (key, window_start, count) VALUES
		('old-1', now() - interval '2 hours', 3),
		('old-2', now() - interval '61 minutes', 1),
		('recent', now() - interval '5 minutes', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := ratelimit.NewShared(database, memoryLimiter()).Prune(context.Background())
	if err != nil || deleted != 2 {
		t.Fatalf("Prune = %d, %v; want 2 rows", deleted, err)
	}
	var left string
	if err := database.QueryRow(`SELECT string_agg(key, ',') FROM rate_limit_counters`).Scan(&left); err != nil || left != "recent" {
		t.Fatalf("left %q (%v), want only the recent counter", left, err)
	}
}

func TestSharedAppRoleGrants(t *testing.T) {
	database := sharedTestDB(t)
	for _, privilege := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		var ok bool
		if err := database.QueryRow(`SELECT has_table_privilege('simplehost_app', 'rate_limit_counters', $1)`, privilege).Scan(&ok); err != nil || !ok {
			t.Errorf("simplehost_app lacks %s on rate_limit_counters (%v)", privilege, err)
		}
	}
	var truncate bool
	if err := database.QueryRow(`SELECT has_table_privilege('simplehost_app', 'rate_limit_counters', 'TRUNCATE')`).Scan(&truncate); err != nil || truncate {
		t.Errorf("simplehost_app has more than it needs (TRUNCATE=%v, %v)", truncate, err)
	}
}

// A database that cannot answer falls back to the per-pod bucket for that
// request: neither every request refused nor every request let through.
func TestSharedFallsBackToMemoryOnClosedDB(t *testing.T) {
	database, err := sql.Open("postgres", "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	database.Close()
	shared := ratelimit.NewShared(database, memoryLimiter())
	policy := ratelimit.Policy{Name: "test-fallback", Burst: 3, RefillPerSecond: 0.01}
	for i := 0; i < 3; i++ {
		if d := shared.Allow(policy, "caller"); !d.Allowed {
			t.Fatalf("fallback request %d denied inside the per-pod burst", i+1)
		}
	}
	if d := shared.Allow(policy, "caller"); d.Allowed {
		t.Fatal("fallback let a request past the per-pod burst")
	}
}
