package auth

import (
	"context"
	"database/sql"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vsriram/simple-host/internal/db"
)

// NegativeSessionCacheInterval is the refresh period design.md 5.1 and 6.1
// specify: "the cache window is 60 seconds."
const NegativeSessionCacheInterval = 60 * time.Second

// NegativeSessionCache is the hosted-content path's substitute for a
// per-request database read (design.md 5.1): a background goroutine loads
// the set of session ids that must be treated as invalid (revoked, expired,
// idle, or belonging to a disabled user) every NegativeSessionCacheInterval,
// and VerifyHostedSession consults the in-memory snapshot instead of the
// sessions table. A database blip leaves the last good snapshot in place —
// hosted content keeps serving on stale-but-safe data, the same survival
// property the host gate's disk-based label resolution already has.
type NegativeSessionCache struct {
	database *sql.DB
	idle     time.Duration
	blocked  atomic.Pointer[map[string]struct{}]
	stop     chan struct{}
	done     chan struct{}
}

// NewNegativeSessionCache builds the cache and performs one synchronous,
// best-effort initial load (bounded to a few seconds) before returning, so
// the very first requests after startup are not served against an empty
// snapshot. Call Start to begin the background refresh loop.
func NewNegativeSessionCache(database *sql.DB, idle time.Duration) *NegativeSessionCache {
	c := &NegativeSessionCache{database: database, idle: idle, stop: make(chan struct{}), done: make(chan struct{})}
	empty := map[string]struct{}{}
	c.blocked.Store(&empty)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.refresh(ctx)
	return c
}

// Start runs the refresh loop until Stop is called. Call it once, after
// construction; it returns immediately and refreshes in the background.
func (c *NegativeSessionCache) Start() {
	go func() {
		defer close(c.done)
		ticker := time.NewTicker(NegativeSessionCacheInterval)
		defer ticker.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				c.refresh(ctx)
				cancel()
			}
		}
	}()
}

// Stop ends the background refresh loop and waits for it to exit. Safe to
// call even if Start was never called.
func (c *NegativeSessionCache) Stop() {
	select {
	case <-c.stop:
		// already stopped
	default:
		close(c.stop)
	}
	<-c.done
}

func (c *NegativeSessionCache) refresh(ctx context.Context) {
	ids, err := db.ListBlockedSessionIDs(ctx, c.database, c.idle)
	if err != nil {
		log.Printf("auth: refresh negative session cache: %v", err)
		return
	}
	next := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		next[id] = struct{}{}
	}
	c.blocked.Store(&next)
}

// Blocked reports whether sessionID is in the current snapshot.
func (c *NegativeSessionCache) Blocked(sessionID string) bool {
	snapshot := c.blocked.Load()
	if snapshot == nil {
		return false
	}
	_, blocked := (*snapshot)[sessionID]
	return blocked
}

// VerifyHostedSession authenticates a hosted-content request from its
// __Host-sh_session cookie alone: it checks the cookie's signature and
// embedded expiry (no database call), that the cookie's own Host claim
// (set by SignHostSession at hand-off redemption) equals expectedHost —
// the host the request actually arrived on — and then consults the
// negative cache for revocation, idleness, or a disabled owner. A cookie
// with no Host claim at all (a base-host cookie, or anything not minted by
// SignHostSession) is refused outright: every owner and restricted-site
// host cookie is host-bound now, with no legacy unbound shape to accept.
// It returns no *db.User — hosted content's authorization rule
// (viewerAllowed) needs only the user id, and fetching the full row on
// every asset request would reintroduce the per-request read this path
// exists to avoid.
func VerifyHostedSession(keys []SigningKey, cache *NegativeSessionCache, cookieValue, expectedHost string) (userID, sessionID string, ok bool) {
	verified, err := VerifySessionCookie(keys, cookieValue)
	if err != nil {
		return "", "", false
	}
	if verified.Host == "" || !strings.EqualFold(verified.Host, expectedHost) {
		return "", "", false
	}
	if cache != nil && cache.Blocked(verified.SessionID) {
		return "", "", false
	}
	return verified.UserID, verified.SessionID, true
}
