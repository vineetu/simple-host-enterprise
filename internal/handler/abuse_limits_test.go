package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/ratelimit"
	"github.com/vsriram/simple-host/internal/storage"
)

func testAbuseLimits(now func() time.Time) *AbuseLimits {
	return newAbuseLimits(
		ratelimit.New(abuseLimitMaxKeys, abuseLimitIdleAfter, now),
		ratelimit.New(searchAbuseLimitMaxKeys, abuseLimitIdleAfter, now),
	)
}

func TestClientLimitKeyPrefersTheSignedInUser(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "192.0.2.10:4321"
	request.Header.Set("X-Forwarded-For", "203.0.113.99")
	if got := clientLimitKey(request); got != "192.0.2.10" {
		t.Fatalf("anonymous clientLimitKey = %q, want the peer (no trusted proxies configured)", got)
	}
	request = request.WithContext(auth.ContextWithTestAuth(request.Context(), &db.User{ID: "u1"}, "s1", ""))
	if got := clientLimitKey(request); got != "user:u1" {
		t.Fatalf("signed-in clientLimitKey = %q, want the user", got)
	}
}

func TestHashedLimitKeyDoesNotRetainEmail(t *testing.T) {
	email := "person@example.com"
	first := hashedLimitKey(email)
	second := hashedLimitKey(email)
	if first != second || first == "" {
		t.Fatalf("digest is not stable: %q, %q", first, second)
	}
	if strings.Contains(first, email) || len(first) != 64 {
		t.Fatalf("digest %q retained plaintext or has unexpected length", first)
	}
}

func TestPolicyConstantsAndIndependentStateBuckets(t *testing.T) {
	if authClientPolicy.Burst != 20 || authClientPolicy.RefillPerSecond != 0.2 ||
		authEmailPolicy.Burst != 5 || authEmailPolicy.RefillPerSecond != 0.02 ||
		managementClientPolicy.Burst != 60 || managementClientPolicy.RefillPerSecond != 1 ||
		managementUserPolicy.Burst != 30 || managementUserPolicy.RefillPerSecond != 0.1 ||
		stateClientPolicy.Burst != 60 || stateClientPolicy.RefillPerSecond != 1 ||
		stateSitePolicy.Burst != 60 || stateSitePolicy.RefillPerSecond != 1 ||
		stateReadClientPolicy.Burst != 120 || stateReadClientPolicy.RefillPerSecond != 2 ||
		stateReadSitePolicy.Burst != 300 || stateReadSitePolicy.RefillPerSecond != 5 ||
		adminClientPolicy.Burst != 10 || adminClientPolicy.RefillPerSecond != 0.1 ||
		adminIdentityPolicy.Burst != 10 || adminIdentityPolicy.RefillPerSecond != 0.1 ||
		searchAbuseLimitMaxKeys != 4_096 ||
		searchQueryPeerPolicy.Name != "search-query-peer" ||
		searchQueryPeerPolicy.Burst != 200 || searchQueryPeerPolicy.RefillPerSecond != 20 ||
		searchQuerySessionPolicy.Burst != 60 || searchQuerySessionPolicy.RefillPerSecond != 1 ||
		searchClickPeerPolicy.Name != "search-click-peer" ||
		searchClickPeerPolicy.Burst != 500 || searchClickPeerPolicy.RefillPerSecond != 50 ||
		searchClickSessionPolicy.Burst != 60 || searchClickSessionPolicy.RefillPerSecond != 1 {
		t.Fatal("one or more documented abuse-limit constants drifted")
	}

	limits := testAbuseLimits(func() time.Time { return time.Unix(100, 0) })
	for i := 0; i < stateClientPolicy.Burst; i++ {
		if decision := limits.allow(stateClientPolicy, "192.0.2.1"); !decision.Allowed {
			t.Fatalf("client burst token %d denied: %+v", i, decision)
		}
	}
	if limits.allow(stateClientPolicy, "192.0.2.1").Allowed {
		t.Fatal("exhausted client bucket was allowed")
	}
	if decision := limits.allow(stateSitePolicy, "alice/demo"); !decision.Allowed {
		t.Fatal("site bucket was coupled to client bucket")
	}
	if decision := limits.allow(stateClientPolicy, "192.0.2.2"); !decision.Allowed {
		t.Fatal("separate client key was coupled")
	}
}

func TestAnonymousSearchLimiterIsIsolatedAndCapped(t *testing.T) {
	now := func() time.Time { return time.Unix(100, 0) }
	limits := testAbuseLimits(now)
	primaryKey := "192.0.2.1"
	for range authClientPolicy.Burst {
		if decision := limits.allow(authClientPolicy, primaryKey); !decision.Allowed {
			t.Fatalf("primary preload denied: %+v", decision)
		}
	}

	// All keys have the same last-seen timestamp. The limiter's deterministic
	// tie-break therefore evicts the lexicographically smallest key when the
	// 4,097th anonymous key arrives.
	searchKey := "00000000"
	for range searchQuerySessionPolicy.Burst {
		if decision := limits.allowSearch(searchQuerySessionPolicy, searchKey); !decision.Allowed {
			t.Fatalf("search preload denied: %+v", decision)
		}
	}
	for index := 1; index < searchAbuseLimitMaxKeys; index++ {
		key := "session-" + strconv.Itoa(index)
		if decision := limits.allowSearch(searchQuerySessionPolicy, key); !decision.Allowed {
			t.Fatalf("search key %d denied: %+v", index, decision)
		}
	}
	if decision := limits.allowSearch(searchQuerySessionPolicy, "overflow"); !decision.Allowed {
		t.Fatalf("overflow key denied: %+v", decision)
	}
	if decision := limits.allowSearch(searchQuerySessionPolicy, searchKey); !decision.Allowed {
		t.Fatal("4,096-key search limiter did not evict its deterministic oldest key")
	}
	if limits.allow(authClientPolicy, primaryKey).Allowed {
		t.Fatal("anonymous search churn evicted an exhausted primary limiter bucket")
	}
}

// The site-facing API's rate limit runs before any database access
// (design.md 7.3's routes are all reached with a real siteAPICall already
// resolved by the host gate, so there is no path-validation step left in
// SiteAPIHandler itself to test separately — see host_gate_test.go for the
// gate-level "an unresolvable site never even reaches here" coverage).
func TestSiteAPIStateReadLimitRunsBeforeDatabaseRead(t *testing.T) {
	limits := testAbuseLimits(func() time.Time { return time.Unix(100, 0) })
	handler := NewSiteAPIHandler(nil, nil, storage.AssetLimits{MaxFileBytes: 1, MaxSiteBytes: 1, MaxSiteCount: 1}, nil, HostModel{}, limits)
	client := "192.0.2.30"
	for range stateReadClientPolicy.Burst {
		if decision := limits.allow(stateReadClientPolicy, client); !decision.Allowed {
			t.Fatalf("preload read token denied: %+v", decision)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/api/sites/demo/state", nil)
	request.RemoteAddr = client + ":1234"
	response := httptest.NewRecorder()
	handler.GetState(response, request, siteAPICall{Owner: "alice", SiteName: "demo"})
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("state read status = %d, want 429", response.Code)
	}
}

func TestManagementClientLimitRunsBeforeAuthentication(t *testing.T) {
	limits := testAbuseLimits(func() time.Time { return time.Unix(100, 0) })
	handler := NewSiteHandler(nil, nil, "", HostModel{}, limits)
	client := "192.0.2.15"
	for i := 0; i < managementClientPolicy.Burst; i++ {
		if decision := limits.allow(managementClientPolicy, client); !decision.Allowed {
			t.Fatalf("preload token %d denied: %+v", i, decision)
		}
	}

	authCalled := false
	authMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authCalled = true
			next.ServeHTTP(w, r)
		})
	}
	passthrough := func(next http.Handler) http.Handler { return next }
	mux := http.NewServeMux()
	handler.Register(mux, authMiddleware, passthrough)
	request := httptest.NewRequest(http.MethodPost, "/api/sites/demo", nil)
	request.RemoteAddr = client + ":1234"
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests || authCalled {
		t.Fatalf("limited management request = status %d, authCalled %t", response.Code, authCalled)
	}
	if decision := limits.allow(managementUserPolicy, "stable-user-id"); !decision.Allowed {
		t.Fatal("independent authenticated-user bucket was coupled to client admission")
	}
}

func TestRateLimitResponseFormats(t *testing.T) {
	t.Run("service JSON", func(t *testing.T) {
		response := httptest.NewRecorder()
		writeRateLimit(response, ratelimit.Decision{RetryAfter: 7200 * time.Millisecond})
		if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "8" {
			t.Fatalf("response = status %d, Retry-After %q", response.Code, response.Header().Get("Retry-After"))
		}
		var body errorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error != "rate limit exceeded" {
			t.Fatalf("body = %q, err = %v", response.Body, err)
		}
	})
}

func TestDecodeSmallJSONRejectsTrailingAndOversizedBodies(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		status int
	}{
		{name: "valid", body: `{"name":"person@example.com"}`, status: http.StatusOK},
		{name: "trailing", body: `{"name":"person@example.com"} null`, status: http.StatusBadRequest},
		{name: "oversized", body: `{"name":"` + strings.Repeat("a", smallJSONBodyBytes) + `"}`, status: http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(test.body))
			response := httptest.NewRecorder()
			var destination mintKeyRequest
			ok := decodeSmallJSON(response, request, &destination)
			if test.status == http.StatusOK {
				if !ok {
					t.Fatalf("valid body rejected with status %d", response.Code)
				}
				return
			}
			if ok || response.Code != test.status {
				t.Fatalf("decode = %t, status = %d, want false/%d", ok, response.Code, test.status)
			}
		})
	}
}

func TestUploadConcurrencyRejectsWhileFullAndReleasesAfterHandlersReturn(t *testing.T) {
	limits := testAbuseLimits(time.Now)
	handler := NewSiteHandler(nil, nil, "", HostModel{}, limits)
	started := make(chan struct{}, uploadConcurrency)
	finish := make(chan struct{})
	wrapped := handler.limitUploadConcurrency(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-finish
		w.WriteHeader(http.StatusNoContent)
	}))

	var wait sync.WaitGroup
	wait.Add(uploadConcurrency)
	for i := 0; i < uploadConcurrency; i++ {
		go func() {
			defer wait.Done()
			wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/sites/demo", nil))
		}()
	}
	for i := 0; i < uploadConcurrency; i++ {
		<-started
	}

	response := httptest.NewRecorder()
	wrapped.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sites/demo", nil))
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("saturated upload = status %d, Retry-After %q", response.Code, response.Header().Get("Retry-After"))
	}
	var body errorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error != "rate limit exceeded" {
		t.Fatalf("saturated upload body = %q, err = %v", response.Body, err)
	}

	close(finish)
	wait.Wait()
	response = httptest.NewRecorder()
	wrapped.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/sites/demo", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("upload after handler release status = %d, want 204", response.Code)
	}
}

func TestArchiveDownloadSemaphoreIsBoundedAndReusable(t *testing.T) {
	limits := testAbuseLimits(time.Now)
	releases := make([]func(), 0, archiveDownloadConcurrency)
	for i := 0; i < archiveDownloadConcurrency; i++ {
		release, acquired := limits.acquireArchiveDownload()
		if !acquired {
			t.Fatalf("archive slot %d was unavailable", i)
		}
		releases = append(releases, release)
	}
	if _, acquired := limits.acquireArchiveDownload(); acquired {
		t.Fatal("archive download exceeded its concurrency bound")
	}
	releases[0]()
	replacement, acquired := limits.acquireArchiveDownload()
	if !acquired {
		t.Fatal("released archive slot was not reusable")
	}
	replacement()
	for _, release := range releases[1:] {
		release()
	}
}
