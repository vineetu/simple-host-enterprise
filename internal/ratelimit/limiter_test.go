package ratelimit

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func TestLimiterBurstRefillAndRetryAfter(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	limiter := New(10, time.Hour, clock.Now)
	policy := Policy{Name: "test", Burst: 2, RefillPerSecond: 0.5}

	if !limiter.Allow(policy, "client").Allowed || !limiter.Allow(policy, "client").Allowed {
		t.Fatal("initial burst was not available")
	}
	denied := limiter.Allow(policy, "client")
	if denied.Allowed || denied.RetryAfter != 2*time.Second {
		t.Fatalf("denied decision = %+v, want two-second retry", denied)
	}

	clock.Advance(time.Second)
	denied = limiter.Allow(policy, "client")
	if denied.Allowed || denied.RetryAfter != time.Second {
		t.Fatalf("half-refilled decision = %+v", denied)
	}
	clock.Advance(time.Second)
	if decision := limiter.Allow(policy, "client"); !decision.Allowed {
		t.Fatalf("refilled decision = %+v", decision)
	}
}

func TestLimiterNamespacesAndKeysAreIndependent(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	limiter := New(10, time.Hour, clock.Now)
	one := Policy{Name: "one", Burst: 1, RefillPerSecond: 1}
	two := Policy{Name: "two", Burst: 1, RefillPerSecond: 1}

	if !limiter.Allow(one, "same").Allowed {
		t.Fatal("first policy/key was denied")
	}
	if limiter.Allow(one, "same").Allowed {
		t.Fatal("exhausted policy/key was allowed")
	}
	if !limiter.Allow(two, "same").Allowed {
		t.Fatal("independent policy was coupled")
	}
	if !limiter.Allow(one, "different").Allowed {
		t.Fatal("independent key was coupled")
	}
}

func TestLimiterInvalidInputDoesNotAllocate(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	limiter := New(2, time.Hour, clock.Now)
	valid := Policy{Name: "valid", Burst: 1, RefillPerSecond: 1}

	for _, test := range []struct {
		policy Policy
		key    string
	}{
		{policy: valid},
		{policy: Policy{Burst: 1, RefillPerSecond: 1}, key: "key"},
		{policy: Policy{Name: "zero-burst", RefillPerSecond: 1}, key: "key"},
		{policy: Policy{Name: "zero-refill", Burst: 1}, key: "key"},
	} {
		if limiter.Allow(test.policy, test.key).Allowed {
			t.Fatal("invalid input was allowed")
		}
	}
	if got := len(limiter.buckets); got != 0 {
		t.Fatalf("invalid inputs allocated %d buckets", got)
	}
}

func TestLimiterRejectsPolicyDefinitionMutation(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	limiter := New(10, time.Hour, clock.Now)
	original := Policy{Name: "fixed", Burst: 1, RefillPerSecond: 1}
	mutated := Policy{Name: "fixed", Burst: 100, RefillPerSecond: 100}

	if !limiter.Allow(original, "existing").Allowed {
		t.Fatal("original policy was denied")
	}
	if decision := limiter.Allow(mutated, "new"); decision.Allowed {
		t.Fatal("mutated policy definition was accepted")
	}
	if _, ok := limiter.buckets[bucketKey{policy: "fixed", value: "new"}]; ok {
		t.Fatal("rejected policy mutation allocated a bucket")
	}
	if decision := limiter.Allow(original, "existing"); decision.Allowed {
		t.Fatal("policy mutation replenished or expanded the existing bucket")
	}
}

func TestLimiterHardCapCleansIdleBuckets(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	limiter := New(2, time.Minute, clock.Now)
	policy := Policy{Name: "test", Burst: 1, RefillPerSecond: 1}

	limiter.Allow(policy, "old-a")
	limiter.Allow(policy, "old-b")
	clock.Advance(time.Minute)
	limiter.Allow(policy, "new")

	if got := len(limiter.buckets); got != 1 {
		t.Fatalf("bucket count = %d, want 1 after idle cleanup", got)
	}
	if _, ok := limiter.buckets[bucketKey{policy: "test", value: "new"}]; !ok {
		t.Fatal("new bucket missing after cleanup")
	}
}

func TestLimiterHardCapDeterministicallyEvictsOldest(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	limiter := New(2, time.Hour, clock.Now)
	policy := Policy{Name: "test", Burst: 1, RefillPerSecond: 1}

	// The equal timestamps force the lexical tie-breaker: "a" is oldest.
	limiter.Allow(policy, "b")
	limiter.Allow(policy, "a")
	limiter.Allow(policy, "c")

	if got := len(limiter.buckets); got != 2 {
		t.Fatalf("bucket count = %d, want hard cap 2", got)
	}
	if _, ok := limiter.buckets[bucketKey{policy: "test", value: "a"}]; ok {
		t.Fatal("lexically oldest tied key was not evicted")
	}
	for _, key := range []string{"b", "c"} {
		if _, ok := limiter.buckets[bucketKey{policy: "test", value: key}]; !ok {
			t.Fatalf("expected bucket %q missing", key)
		}
	}
}

func TestLimiterConcurrentAllowHonorsHardCap(t *testing.T) {
	limiter := New(7, time.Hour, time.Now)
	policy := Policy{Name: "concurrent", Burst: 1, RefillPerSecond: 1}

	var wait sync.WaitGroup
	for i := 0; i < 200; i++ {
		wait.Add(1)
		go func(key string) {
			defer wait.Done()
			limiter.Allow(policy, key)
		}(fmt.Sprintf("client-%03d", i))
	}
	wait.Wait()

	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if got := len(limiter.buckets); got > limiter.maxKeys {
		t.Fatalf("concurrent bucket count = %d, hard cap = %d", got, limiter.maxKeys)
	}
}
