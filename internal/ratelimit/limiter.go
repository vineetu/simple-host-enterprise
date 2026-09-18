// Package ratelimit provides a small in-process token-bucket limiter.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Policy describes one independently-accounted token bucket namespace.
// RefillPerSecond is expressed as tokens per second.
type Policy struct {
	Name            string
	Burst           int
	RefillPerSecond float64
}

// Decision is the result of consuming one token.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration
}

type bucketKey struct {
	policy string
	value  string
}

type bucket struct {
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
}

// Limiter is safe for concurrent use. It has a hard key cap; idle buckets are
// cleaned before deterministic oldest-bucket eviction makes room for a new key.
type Limiter struct {
	mu        sync.Mutex
	buckets   map[bucketKey]*bucket
	policies  map[string]Policy
	maxKeys   int
	idleAfter time.Duration
	now       func() time.Time
}

// New constructs a limiter. maxKeys, idleAfter, and now must all be valid.
func New(maxKeys int, idleAfter time.Duration, now func() time.Time) *Limiter {
	if maxKeys <= 0 {
		panic("ratelimit: maxKeys must be positive")
	}
	if idleAfter <= 0 {
		panic("ratelimit: idleAfter must be positive")
	}
	if now == nil {
		panic("ratelimit: clock must not be nil")
	}
	return &Limiter{
		buckets:   make(map[bucketKey]*bucket),
		policies:  make(map[string]Policy),
		maxKeys:   maxKeys,
		idleAfter: idleAfter,
		now:       now,
	}
}

// Allow consumes one token for policy/key. Invalid policies and empty keys are
// rejected without allocating map state.
func (l *Limiter) Allow(policy Policy, key string) Decision {
	if policy.Name == "" || key == "" || policy.Burst <= 0 || policy.RefillPerSecond <= 0 ||
		math.IsNaN(policy.RefillPerSecond) || math.IsInf(policy.RefillPerSecond, 0) {
		return Decision{RetryAfter: time.Second}
	}

	now := l.now()
	k := bucketKey{policy: policy.Name, value: key}

	l.mu.Lock()
	defer l.mu.Unlock()
	if registered, exists := l.policies[policy.Name]; exists {
		if registered != policy {
			return Decision{RetryAfter: time.Second}
		}
	} else {
		// Policy names are configuration, not request input. Keeping this
		// registry bounded as well prevents accidental dynamic policy names from
		// becoming an unbounded secondary map.
		if len(l.policies) >= l.maxKeys {
			return Decision{RetryAfter: time.Second}
		}
		l.policies[policy.Name] = policy
	}

	b, exists := l.buckets[k]
	if !exists {
		l.makeRoom(now)
		b = &bucket{
			tokens:     float64(policy.Burst),
			lastRefill: now,
			lastSeen:   now,
		}
		l.buckets[k] = b
	}

	if now.After(b.lastRefill) {
		elapsed := now.Sub(b.lastRefill).Seconds()
		b.tokens = math.Min(float64(policy.Burst), b.tokens+elapsed*policy.RefillPerSecond)
		b.lastRefill = now
	}
	if now.After(b.lastSeen) {
		b.lastSeen = now
	}

	if b.tokens >= 1 {
		b.tokens--
		return Decision{Allowed: true}
	}

	waitSeconds := (1 - b.tokens) / policy.RefillPerSecond
	retryAfter := time.Duration(math.Ceil(waitSeconds)) * time.Second
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	return Decision{RetryAfter: retryAfter}
}

func (l *Limiter) makeRoom(now time.Time) {
	if len(l.buckets) < l.maxKeys {
		return
	}

	for key, candidate := range l.buckets {
		if !now.Before(candidate.lastSeen) && now.Sub(candidate.lastSeen) >= l.idleAfter {
			delete(l.buckets, key)
		}
	}
	if len(l.buckets) < l.maxKeys {
		return
	}

	var oldestKey bucketKey
	var oldest *bucket
	for key, candidate := range l.buckets {
		if oldest == nil || candidate.lastSeen.Before(oldest.lastSeen) ||
			(candidate.lastSeen.Equal(oldest.lastSeen) && lessKey(key, oldestKey)) {
			oldestKey = key
			oldest = candidate
		}
	}
	if oldest != nil {
		delete(l.buckets, oldestKey)
	}
}

func lessKey(left, right bucketKey) bool {
	if left.policy != right.policy {
		return left.policy < right.policy
	}
	return left.value < right.value
}
