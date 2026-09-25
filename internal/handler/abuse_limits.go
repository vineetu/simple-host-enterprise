package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/vsriram/simple-host/internal/ratelimit"
)

const (
	abuseLimitMaxKeys          = 10_000
	searchAbuseLimitMaxKeys    = 4_096
	abuseLimitIdleAfter        = time.Hour
	searchQueryConcurrency     = 8
	searchClickConcurrency     = 16
	uploadConcurrency          = 2
	archiveDownloadConcurrency = 2
	concurrencyRetryAfter      = time.Second

	smallJSONBodyBytes = 16 << 10
	smallFormBodyBytes = 8 << 10
)

var (
	authClientPolicy = ratelimit.Policy{
		Name: "auth-client", Burst: 20, RefillPerSecond: 0.2,
	}
	authEmailPolicy = ratelimit.Policy{
		Name: "auth-email", Burst: 5, RefillPerSecond: 0.02,
	}
	managementClientPolicy = ratelimit.Policy{
		Name: "management-client", Burst: 60, RefillPerSecond: 1,
	}
	managementUserPolicy = ratelimit.Policy{
		Name: "management-user", Burst: 30, RefillPerSecond: 0.1,
	}
	stateClientPolicy = ratelimit.Policy{
		Name: "state-client", Burst: 60, RefillPerSecond: 1,
	}
	stateSitePolicy = ratelimit.Policy{
		Name: "state-site", Burst: 60, RefillPerSecond: 1,
	}
	stateReadClientPolicy = ratelimit.Policy{
		Name: "state-read-client", Burst: 120, RefillPerSecond: 2,
	}
	stateReadSitePolicy = ratelimit.Policy{
		Name: "state-read-site", Burst: 300, RefillPerSecond: 5,
	}
	adminClientPolicy = ratelimit.Policy{
		Name: "admin-client", Burst: 10, RefillPerSecond: 0.1,
	}
	adminIdentityPolicy = ratelimit.Policy{
		Name: "admin-identity", Burst: 10, RefillPerSecond: 0.1,
	}
	// A layer-4 load balancer leaves its own peers in RemoteAddr. These peer
	// buckets therefore provide coarse aggregate load-shed, not end-user quotas;
	// forwarded headers remain untrusted and are never used as limiter keys.
	searchQueryPeerPolicy = ratelimit.Policy{
		Name: "search-query-peer", Burst: 200, RefillPerSecond: 20,
	}
	searchQuerySessionPolicy = ratelimit.Policy{
		Name: "search-query-session", Burst: 60, RefillPerSecond: 1,
	}
	searchClickPeerPolicy = ratelimit.Policy{
		Name: "search-click-peer", Burst: 500, RefillPerSecond: 50,
	}
	searchClickSessionPolicy = ratelimit.Policy{
		Name: "search-click-session", Burst: 60, RefillPerSecond: 1,
	}
)

// AbuseLimits owns the process-wide limiter state and the memory-sensitive
// concurrency slots. Handler constructors accept a shared instance so every
// route observes one set of process-wide gates. Anonymous search traffic has a
// separate hard-capped limiter so rotated session cookies cannot evict the
// authenticated, administrative, or shared-state buckets.
type AbuseLimits struct {
	buckets              *ratelimit.Limiter
	searchBuckets        *ratelimit.Limiter
	searchQuerySlots     chan struct{}
	searchClickSlots     chan struct{}
	uploadSlots          chan struct{}
	archiveDownloadSlots chan struct{}
}

func NewAbuseLimits() *AbuseLimits {
	now := time.Now
	return newAbuseLimits(
		ratelimit.New(abuseLimitMaxKeys, abuseLimitIdleAfter, now),
		ratelimit.New(searchAbuseLimitMaxKeys, abuseLimitIdleAfter, now),
	)
}

func newAbuseLimits(
	buckets *ratelimit.Limiter,
	searchBuckets *ratelimit.Limiter,
) *AbuseLimits {
	if buckets == nil {
		panic("handler: abuse limiter must not be nil")
	}
	if searchBuckets == nil {
		panic("handler: search abuse limiter must not be nil")
	}
	return &AbuseLimits{
		buckets:          buckets,
		searchBuckets:    searchBuckets,
		searchQuerySlots: make(chan struct{}, searchQueryConcurrency),
		searchClickSlots: make(chan struct{}, searchClickConcurrency),
		// Two worst-case uploads retain about 1.2 GiB of archive and
		// extracted-file data plus the packed archive being uploaded,
		// leaving headroom in the Pod for Go allocation overhead and the
		// service.
		uploadSlots:          make(chan struct{}, uploadConcurrency),
		archiveDownloadSlots: make(chan struct{}, archiveDownloadConcurrency),
	}
}

func chooseAbuseLimits(given []*AbuseLimits) *AbuseLimits {
	if len(given) > 0 && given[0] != nil {
		return given[0]
	}
	return NewAbuseLimits()
}

func (l *AbuseLimits) allow(policy ratelimit.Policy, key string) ratelimit.Decision {
	return l.buckets.Allow(policy, key)
}

func (l *AbuseLimits) allowSearch(policy ratelimit.Policy, key string) ratelimit.Decision {
	return l.searchBuckets.Allow(policy, key)
}

func (l *AbuseLimits) acquireSearchQuery() (func(), bool) {
	select {
	case l.searchQuerySlots <- struct{}{}:
		return func() { <-l.searchQuerySlots }, true
	default:
		return nil, false
	}
}

func (l *AbuseLimits) acquireSearchClick() (func(), bool) {
	select {
	case l.searchClickSlots <- struct{}{}:
		return func() { <-l.searchClickSlots }, true
	default:
		return nil, false
	}
}

func (l *AbuseLimits) acquireUpload() (func(), bool) {
	select {
	case l.uploadSlots <- struct{}{}:
		return func() { <-l.uploadSlots }, true
	default:
		return nil, false
	}
}

func (l *AbuseLimits) acquireArchiveDownload() (func(), bool) {
	select {
	case l.archiveDownloadSlots <- struct{}{}:
		return func() { <-l.archiveDownloadSlots }, true
	default:
		return nil, false
	}
}

// remoteClientKey deliberately trusts only net/http's peer address and rejects
// forwarded headers as identity input. They are client-controlled at the current
// layer-4 NLB boundary, where E1 intentionally observes the NLB peer.
func remoteClientKey(r *http.Request) string {
	if address, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return address.Addr().Unmap().String()
	}
	if address, err := netip.ParseAddr(r.RemoteAddr); err == nil {
		return address.Unmap().String()
	}
	return "unknown"
}

func siteLimitKey(username, siteName string) string {
	// Both values have already passed the segment validator, which rejects '/'.
	return username + "/" + siteName
}

func hashedLimitKey(value string) string {
	// Fine-grained identity buckets do not need the original identifier. A
	// fixed-size digest avoids retaining normalized email addresses in memory.
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func writeRateLimit(w http.ResponseWriter, decision ratelimit.Decision) {
	w.Header().Set("Retry-After", retryAfterSeconds(decision.RetryAfter))
	writeJSON(w, http.StatusTooManyRequests, errorResponse{Error: "rate limit exceeded"})
}

func writeConcurrencyRateLimit(w http.ResponseWriter) {
	writeRateLimit(w, ratelimit.Decision{RetryAfter: concurrencyRetryAfter})
}

func retryAfterSeconds(wait time.Duration) string {
	seconds := int64(math.Ceil(wait.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10)
}

func decodeSmallJSON(w http.ResponseWriter, r *http.Request, destination any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, smallJSONBodyBytes)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(destination); err != nil {
		if isMaxBytesError(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "request body too large"})
		} else {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		}
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if isMaxBytesError(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: "request body too large"})
		} else {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid request body"})
		}
		return false
	}
	return true
}

func isMaxBytesError(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}
