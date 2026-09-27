package handler

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/vsriram/simple-host/internal/oplimits"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/auth"
	"github.com/vsriram/simple-host/internal/ratelimit"
	"github.com/vsriram/simple-host/internal/reqlog"
)

const (
	abuseLimitMaxKeys       = 10_000
	searchAbuseLimitMaxKeys = 4_096
	abuseLimitIdleAfter     = time.Hour
	searchQueryConcurrency  = 8
	searchClickConcurrency  = 16
	concurrencyRetryAfter   = time.Second

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
	// API key mint has its own bucket (the management-user numbers it used
	// to share) so it can be counted across replicas; see sharedPolicies.
	apiKeyMintPolicy = ratelimit.Policy{
		Name: "api-key-mint", Burst: 30, RefillPerSecond: 0.1,
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
	// Keyed by clientLimitKey like every other per-caller limit; these are
	// the coarse load-shed buckets, the session ones below are the quotas.
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

// sharedPolicies are the security-relevant limits: a caller who can spread
// requests over N replicas must not get N times the budget for sign-in
// (/auth/login, /auth/callback, the connector's /oauth/authorize, all on
// auth-client), the session hand-off (/auth/handoff and the host gate's
// /auth/session, also auth-client), API key mint, or the connector's token
// and client registration endpoints. When a shared store is set (the server
// always sets one) they are counted in Postgres in fixed windows derived by
// ratelimit.FixedWindow from the same Policy:
//
//	auth-client     20 per 100s  (burst 20, 0.2/s)
//	api-key-mint    30 per 300s  (burst 30, 0.1/s)
//	oauth-register  30 per 300s  (burst 30, 0.1/s)
//	oauth-token    120 per 60s   (burst 120, 2/s)
//
// Everything else stays per pod in memory: those are abuse and cost guards
// (state reads and writes, management, admin, search) where N times the
// budget with N pods is acceptable and a database round trip per request
// is not worth it.
var sharedPolicies = map[string]bool{
	authClientPolicy.Name:    true,
	apiKeyMintPolicy.Name:    true,
	oauthRegisterPolicy.Name: true,
	oauthTokenPolicy.Name:    true,
}

// configurablePolicies are the limits an installation may change with
// RATE_LIMIT_<NAME> (the policy name upper-cased, "-" as "_"). The search
// click buckets stay fixed: they only shed load behind the query ones.
var configurablePolicies = []*ratelimit.Policy{
	&authClientPolicy, &authEmailPolicy,
	&managementClientPolicy, &managementUserPolicy, &apiKeyMintPolicy,
	&stateClientPolicy, &stateSitePolicy, &stateReadClientPolicy, &stateReadSitePolicy,
	&adminClientPolicy, &adminIdentityPolicy,
	&searchQueryPeerPolicy, &searchQuerySessionPolicy,
	&oauthRegisterPolicy, &oauthTokenPolicy,
}

// sensitivePolicies guard sign-in, the email code, API key mint, the admin
// API and the connector's OAuth. An installation may make them stricter
// freely but at most sensitiveLoosen times looser than the built-in value:
// a burst at most that many times it, an interval at least that fraction of
// it. Any other limit may go further, with a startup warning past
// warnLoosen times.
var sensitivePolicies = map[string]bool{
	authClientPolicy.Name: true, authEmailPolicy.Name: true, apiKeyMintPolicy.Name: true,
	adminClientPolicy.Name: true, adminIdentityPolicy.Name: true,
	oauthRegisterPolicy.Name: true, oauthTokenPolicy.Name: true,
}

const (
	sensitiveLoosen = 4
	warnLoosen      = 10
)

// builtinRateLimits are the limits as shipped, before any RATE_LIMIT_*
// override: the reference the ceilings and warnings measure from.
var builtinRateLimits = RateLimitDefaults()

// SensitiveRateLimit reports whether the named limit is security-sensitive,
// and its loosest allowed setting.
func SensitiveRateLimit(name string) (RateLimit, bool) {
	if !sensitivePolicies[name] {
		return RateLimit{}, false
	}
	d := builtinRateLimits[name]
	return RateLimit{Burst: d.Burst * sensitiveLoosen, Every: d.Every / sensitiveLoosen}, true
}

func rateLimitEnv(name string) string {
	return "RATE_LIMIT_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// RateLimit is one limit override: Burst requests at once, then one more
// every Every.
type RateLimit struct {
	Burst int
	Every time.Duration
}

// RateLimitDefaults returns every configurable limit as it stands, by name.
func RateLimitDefaults() map[string]RateLimit {
	out := make(map[string]RateLimit, len(configurablePolicies))
	for _, p := range configurablePolicies {
		out[p.Name] = RateLimit{Burst: p.Burst, Every: refillInterval(*p)}
	}
	return out
}

// refillInterval is how long a policy takes to refill one request, to the
// nearest microsecond.
func refillInterval(p ratelimit.Policy) time.Duration {
	return time.Duration(math.Round(float64(time.Second)/p.RefillPerSecond/1e3)) * time.Microsecond
}

// ConfigureRateLimits applies RATE_LIMIT_* overrides. It is called once at
// startup, before any handler serves. It refuses a security-sensitive limit
// (sensitivePolicies) looser than its ceiling, and a limit counted across
// replicas (sharedPolicies) whose window, burst times interval, is longer
// than the shared counter keeps (ratelimit.MaxSharedWindow). It returns
// warnings for the log: an unknown name (a typo, or another program's
// variable), which is ignored, and any other limit set more than warnLoosen
// times looser than built in.
func ConfigureRateLimits(overrides map[string]RateLimit) ([]string, error) {
	byName := make(map[string]*ratelimit.Policy, len(configurablePolicies))
	for _, p := range configurablePolicies {
		byName[p.Name] = p
	}
	var warnings []string
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		limit := overrides[name]
		p, ok := byName[name]
		if !ok {
			warnings = append(warnings, rateLimitEnv(name)+" is not a limit this server has, so it changes nothing (docs/configuration.md lists them)")
			continue
		}
		if limit.Burst < 1 || limit.Every <= 0 {
			return nil, fmt.Errorf("%s: burst and interval must be positive", rateLimitEnv(name))
		}
		def := builtinRateLimits[name]
		if loosest, ok := SensitiveRateLimit(name); ok {
			if limit.Burst > loosest.Burst || limit.Every < loosest.Every {
				return nil, fmt.Errorf("%s: too loose for a security-sensitive limit: at most %d/%s (%d times the built-in %d/%s); it can be made stricter freely",
					rateLimitEnv(name), loosest.Burst, loosest.Every, sensitiveLoosen, def.Burst, def.Every)
			}
		} else if limit.Burst > def.Burst*warnLoosen || limit.Every < def.Every/warnLoosen {
			warnings = append(warnings, fmt.Sprintf("%s=%d/%s is more than %d times looser than the built-in %d/%s", rateLimitEnv(name), limit.Burst, limit.Every, warnLoosen, def.Burst, def.Every))
		}
		next := ratelimit.Policy{Name: p.Name, Burst: limit.Burst, RefillPerSecond: float64(time.Second) / float64(limit.Every)}
		if sharedPolicies[p.Name] {
			if _, window := ratelimit.FixedWindow(next); window <= 0 || window > ratelimit.MaxSharedWindow {
				return nil, fmt.Errorf("%s: burst times interval must be at most %s for this limit, which is counted across replicas", rateLimitEnv(name), ratelimit.MaxSharedWindow)
			}
		}
	}
	for name, limit := range overrides {
		if p := byName[name]; p != nil {
			p.Burst = limit.Burst
			p.RefillPerSecond = float64(time.Second) / float64(limit.Every)
		}
	}
	return warnings, nil
}

// rateLimitPlaceholders are "{{RATE_LIMIT_<NAME>}}" for every configurable
// limit, each with its value in words ("200 at once, then one more every
// 50ms"), for the served documents that state one.
func rateLimitPlaceholders() []string {
	out := make([]string, 0, 2*len(configurablePolicies))
	for _, p := range configurablePolicies {
		every := refillInterval(*p)
		out = append(out, "{{RATE_LIMIT_"+strings.ToUpper(strings.ReplaceAll(p.Name, "-", "_"))+"}}",
			fmt.Sprintf("%d at once, then one more every %s", p.Burst, oplimits.Duration(every)))
	}
	return out
}

// expandServedText fills every operational placeholder a served document
// (a skill file, the plugin bundle, openapi.yaml) may carry: the oplimits
// values and the rate limits.
func expandServedText(text string) string {
	return strings.NewReplacer(append(oplimits.Get().Placeholders(), rateLimitPlaceholders()...)...).Replace(text)
}

// AbuseLimits owns the process-wide limiter state and the memory-sensitive
// concurrency slots. Handler constructors accept a shared instance so every
// route observes one set of process-wide gates. Anonymous search traffic has a
// separate hard-capped limiter so rotated session cookies cannot evict the
// authenticated, administrative, or shared-state buckets.
type AbuseLimits struct {
	buckets              *ratelimit.Limiter
	shared               *ratelimit.Shared
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
		uploadSlots:          make(chan struct{}, oplimits.Get().UploadConcurrency),
		archiveDownloadSlots: make(chan struct{}, oplimits.Get().UploadConcurrency),
	}
}

// WithSharedStore counts sharedPolicies in database, across every replica,
// falling back to this pod's in-memory buckets when the database errors.
// Without it (unit tests) every policy is per pod.
func (l *AbuseLimits) WithSharedStore(database *sql.DB) *AbuseLimits {
	l.shared = ratelimit.NewShared(database, l.buckets)
	return l
}

func chooseAbuseLimits(given []*AbuseLimits) *AbuseLimits {
	if len(given) > 0 && given[0] != nil {
		return given[0]
	}
	return NewAbuseLimits()
}

func (l *AbuseLimits) allow(policy ratelimit.Policy, key string) ratelimit.Decision {
	if l.shared != nil && sharedPolicies[policy.Name] {
		return l.shared.Allow(policy, key)
	}
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

// clientLimitKey is the per-caller key for the client rate limits: the
// signed-in user when the route has already authenticated one (so a
// company behind one egress address does not share a bucket), otherwise
// the client address (reqlog.ClientIP, which honours TRUSTED_PROXY_CIDRS).
func clientLimitKey(r *http.Request) string {
	if user := auth.GetUser(r.Context()); user != nil {
		return "user:" + user.ID
	}
	if ip := reqlog.ClientIP(r); ip != "" {
		return ip
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
