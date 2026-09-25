package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	searchdb "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/search"
)

func TestPublicSearchSuccessNormalizesAndReturnsTelemetry(t *testing.T) {
	service := &fakePublicSearchService{results: []search.PublicResult{
		{
			SiteID:        "11111111-1111-4111-8111-111111111111",
			VersionNumber: 7,
			Owner:         "alice",
			Site:          "planning",
			PagePath:      "roadmap/index.html",
			URL:           "https://alice.foo.example/planning/roadmap/",
			Title:         "Release roadmap",
			Snippet:       "Release calendar and owners.",
			Position:      5,
		},
		{
			SiteID:        "22222222-2222-4222-8222-222222222222",
			VersionNumber: 3,
			Owner:         "bob",
			Site:          "dates",
			PagePath:      "index.html",
			URL:           "https://bob.foo.example/dates/",
			Title:         "Dates",
			Snippet:       "The team calendar.",
			Position:      6,
		},
	}}
	handler := newTestSearchHandler(service)
	handler.cookies = CookiePolicy{Secure: true}

	var telemetryQuery, telemetryDigest string
	var telemetryImpressions []searchdb.SiteSearchTelemetryImpression
	handler.recordTelemetry = func(
		ctx context.Context,
		query string,
		sessionDigest string,
		impressions []searchdb.SiteSearchTelemetryImpression,
	) (searchdb.SiteSearchTelemetryRecord, error) {
		assertSearchTelemetryDeadline(t, ctx)
		telemetryQuery = query
		telemetryDigest = sessionDigest
		telemetryImpressions = append([]searchdb.SiteSearchTelemetryImpression(nil), impressions...)
		return testTelemetryRecord(len(impressions)), nil
	}
	rawQuery := " \trelease\u2003calendar\n "
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/search?q="+url.QueryEscape(rawQuery)+"&limit=2&offset=4&group=site",
		nil,
	)
	request.RemoteAddr = "192.0.2.10:4321"
	request.Header.Set("X-Forwarded-For", "203.0.113.99")
	request.Header.Set("X-Real-IP", "203.0.113.98")
	response := httptest.NewRecorder()
	handler.handleSearch(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	assertSearchNoStoreJSON(t, response)
	if service.calls != 1 || service.request != (search.PublicSearchRequest{Query: "release calendar", Limit: 2, Offset: 4}) {
		t.Fatalf("service calls/request = %d / %+v", service.calls, service.request)
	}
	if telemetryQuery != "release calendar" {
		t.Fatalf("telemetry query = %q", telemetryQuery)
	}
	wantImpressions := []searchdb.SiteSearchTelemetryImpression{
		{ResultPosition: 5, SiteID: service.results[0].SiteID, VersionNumber: 7, PagePath: "roadmap/index.html"},
		{ResultPosition: 6, SiteID: service.results[1].SiteID, VersionNumber: 3, PagePath: "index.html"},
	}
	if !reflect.DeepEqual(telemetryImpressions, wantImpressions) {
		t.Fatalf("telemetry impressions = %#v, want %#v", telemetryImpressions, wantImpressions)
	}
	cookie := requireCookie(t, response, searchSessionCookieName)
	assertSearchCookiePolicy(t, cookie, true)
	decodedCookie, err := base64.RawURLEncoding.Strict().DecodeString(cookie.Value)
	if err != nil || len(decodedCookie) != searchSessionBytes {
		t.Fatalf("search cookie is not 32-byte base64url: len=%d err=%v", len(decodedCookie), err)
	}
	if telemetryDigest != independentSearchDigest(cookie.Value) || strings.Contains(telemetryDigest, cookie.Value) || len(telemetryDigest) != 64 {
		t.Fatalf("telemetry digest = %q for cookie %q", telemetryDigest, cookie.Value)
	}

	body := response.Body.String()
	var got publicSearchResponse
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	wantRecord := testTelemetryRecord(2)
	if got.QueryID != wantRecord.QueryID || got.Query != "release calendar" || got.ResultCount != 2 || len(got.Results) != 2 {
		t.Fatalf("response metadata = %+v", got)
	}
	if got.Results[0] != (publicSearchResultResponse{
		ImpressionID: wantRecord.ImpressionIDs[0],
		Owner:        "alice",
		Site:         "planning",
		PagePath:     "roadmap/index.html",
		URL:          "https://alice.foo.example/planning/roadmap/",
		Title:        "Release roadmap",
		Snippet:      "Release calendar and owners.",
		Position:     5,
	}) {
		t.Fatalf("first result = %+v", got.Results[0])
	}
	for _, privateValue := range []string{"site_id", "version_number", telemetryDigest, cookie.Value} {
		if strings.Contains(body, privateValue) {
			t.Fatalf("response exposed private value %q: %s", privateValue, body)
		}
	}
}

func TestPublicSearchServeMuxRejectsHEADBeforeAllHandlerSideEffects(t *testing.T) {
	service := &fakePublicSearchService{results: []search.PublicResult{testPublicSearchResult(1)}}
	handler := newTestSearchHandler(service)
	entropy := bytes.NewReader(bytes.Repeat([]byte{0x7a}, searchSessionBytes))
	handler.random = entropy
	telemetryCalls := 0
	handler.recordTelemetry = func(
		context.Context,
		string,
		string,
		[]searchdb.SiteSearchTelemetryImpression,
	) (searchdb.SiteSearchTelemetryRecord, error) {
		telemetryCalls++
		return testTelemetryRecord(1), nil
	}
	mux := http.NewServeMux()
	// This test is about handleSearch's own method handling, not the
	// session requirement design.md 7.2 added around it, so Register is
	// given a pass-through in place of the real auth middleware.
	handler.Register(mux, func(next http.Handler) http.Handler { return next })
	peer := "192.0.2.11"
	request := httptest.NewRequest(http.MethodHead, "/api/search?q=release", nil)
	request.RemoteAddr = peer + ":4321"
	request.Header.Set("X-Forwarded-For", "203.0.113.11")
	response := httptest.NewRecorder()

	mux.ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("HEAD response = status %d, Allow %q", response.Code, response.Header().Get("Allow"))
	}
	if len(response.Result().Cookies()) != 0 || entropy.Len() != searchSessionBytes {
		t.Fatalf("HEAD created a session: cookies=%v entropy remaining=%d", response.Result().Cookies(), entropy.Len())
	}
	if service.calls != 0 || telemetryCalls != 0 {
		t.Fatalf("HEAD side effects = service %d, telemetry %d", service.calls, telemetryCalls)
	}
	for index := 0; index < searchQueryPeerPolicy.Burst; index++ {
		if decision := handler.limits.allowSearch(searchQueryPeerPolicy, peer); !decision.Allowed {
			t.Fatalf("HEAD consumed peer token %d: %+v", index, decision)
		}
	}
	predictedDigest := independentSearchDigest(testSearchSessionValue(0x7a))
	for index := 0; index < searchQuerySessionPolicy.Burst; index++ {
		if decision := handler.limits.allowSearch(searchQuerySessionPolicy, predictedDigest); !decision.Allowed {
			t.Fatalf("HEAD consumed session token %d: %+v", index, decision)
		}
	}
}

func TestParsePublicSearchRequestDefaultsBoundsAndUnicodeWhitespace(t *testing.T) {
	request, normalized, err := parsePublicSearchRequest("q=" + url.QueryEscape("\u00a0界\u2003roadmap\t") + "&group=site")
	if err != nil {
		t.Fatal(err)
	}
	want := search.PublicSearchRequest{Query: "界 roadmap", Limit: search.DefaultPublicSearchLimit, Offset: 0}
	if request != want || normalized != want.Query {
		t.Fatalf("parsed = %+v / %q, want %+v", request, normalized, want)
	}

	maxQuery := strings.Repeat("界", search.MaxPublicSearchQueryRunes)
	request, normalized, err = parsePublicSearchRequest(
		"q=" + url.QueryEscape(maxQuery) + "&limit=50&offset=1000",
	)
	if err != nil || request.Query != maxQuery || normalized != maxQuery || request.Limit != 50 || request.Offset != 1000 {
		t.Fatalf("maximum request = %+v / %q / %v", request, normalized, err)
	}
}

func TestPublicSearchRejectsInvalidAndDuplicateParametersBeforeService(t *testing.T) {
	tests := []struct {
		name     string
		rawQuery string
	}{
		{name: "missing q"},
		{name: "duplicate q", rawQuery: "q=one&q=two"},
		{name: "invalid UTF-8", rawQuery: "q=%ff"},
		{name: "empty normalized q", rawQuery: "q=%E2%80%83%09"},
		{name: "too many runes", rawQuery: "q=" + url.QueryEscape(strings.Repeat("界", search.MaxPublicSearchQueryRunes+1))},
		{name: "NUL", rawQuery: "q=release%00calendar"},
		{name: "bad query escape", rawQuery: "q=%zz"},
		{name: "unescaped semicolon", rawQuery: "q=release;calendar"},
		{name: "duplicate limit", rawQuery: "q=release&limit=1&limit=2"},
		{name: "empty limit", rawQuery: "q=release&limit="},
		{name: "zero limit", rawQuery: "q=release&limit=0"},
		{name: "large limit", rawQuery: "q=release&limit=51"},
		{name: "overflowing limit digits", rawQuery: "q=release&limit=" + strings.Repeat("9", 10_000)},
		{name: "signed limit", rawQuery: "q=release&limit=-1"},
		{name: "nonnumeric limit", rawQuery: "q=release&limit=twelve"},
		{name: "duplicate offset", rawQuery: "q=release&offset=1&offset=2"},
		{name: "empty offset", rawQuery: "q=release&offset="},
		{name: "negative offset", rawQuery: "q=release&offset=-1"},
		{name: "large offset", rawQuery: "q=release&offset=1001"},
		{name: "overflowing offset digits", rawQuery: "q=release&offset=" + strings.Repeat("9", 10_000)},
		{name: "nonnumeric offset", rawQuery: "q=release&offset=one"},
		{name: "empty group", rawQuery: "q=release&group="},
		{name: "reserved page group", rawQuery: "q=release&group=page"},
		{name: "duplicate group", rawQuery: "q=release&group=site&group=site"},
		{name: "unknown parameter", rawQuery: "q=release&sort=clicks"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakePublicSearchService{}
			handler := newTestSearchHandler(service)
			telemetryCalls := 0
			handler.recordTelemetry = func(
				context.Context,
				string,
				string,
				[]searchdb.SiteSearchTelemetryImpression,
			) (searchdb.SiteSearchTelemetryRecord, error) {
				telemetryCalls++
				return searchdb.SiteSearchTelemetryRecord{}, nil
			}
			request := httptest.NewRequest(http.MethodGet, "/api/search", nil)
			request.URL.RawQuery = test.rawQuery
			request.RemoteAddr = "192.0.2.20:1234"
			request.AddCookie(&http.Cookie{Name: searchSessionCookieName, Value: testSearchSessionValue(1)})
			response := httptest.NewRecorder()

			handler.handleSearch(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body)
			}
			assertSearchNoStoreJSON(t, response)
			if service.calls != 0 || telemetryCalls != 0 {
				t.Fatalf("invalid request reached service/telemetry: %d/%d", service.calls, telemetryCalls)
			}
			if response.Body.Len() > 128 {
				t.Fatalf("invalid-request response was not bounded: %q", response.Body)
			}
		})
	}
}

func TestPublicSearchFailureIsBoundedAndLogsOnlyErrorType(t *testing.T) {
	query := "private roadmap query"
	secret := "database detail that must not be logged"
	service := &fakePublicSearchService{err: &searchTestSecretError{secret: secret}}
	handler := newTestSearchHandler(service)
	logs := captureSearchLogs(handler)
	telemetryCalls := 0
	handler.recordTelemetry = func(
		context.Context,
		string,
		string,
		[]searchdb.SiteSearchTelemetryImpression,
	) (searchdb.SiteSearchTelemetryRecord, error) {
		telemetryCalls++
		return searchdb.SiteSearchTelemetryRecord{}, nil
	}

	request := newSearchGETRequest(query, testSearchSessionValue(2), "192.0.2.21:1234")
	response := httptest.NewRecorder()
	handler.handleSearch(response, request)

	if response.Code != http.StatusServiceUnavailable || telemetryCalls != 0 {
		t.Fatalf("status/telemetry calls = %d/%d, body=%s", response.Code, telemetryCalls, response.Body)
	}
	assertSearchNoStoreJSON(t, response)
	var body errorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error != "search unavailable" {
		t.Fatalf("body = %+v, err=%v", body, err)
	}
	if response.Body.Len() > 128 {
		t.Fatalf("unavailable response was not bounded: %q", response.Body)
	}
	if !strings.Contains(logs.String(), "*handler.searchTestSecretError") ||
		strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), query) {
		t.Fatalf("unsafe or incomplete log = %q", logs.String())
	}
}

func TestPublicSearchTelemetryFailureReturnsFreshV4FallbackIDs(t *testing.T) {
	query := "release calendar"
	secret := "telemetry SQL and private query details"
	service := &fakePublicSearchService{results: []search.PublicResult{
		testPublicSearchResult(1),
		testPublicSearchResult(2),
	}}
	handler := newTestSearchHandler(service)
	logs := captureSearchLogs(handler)
	handler.recordTelemetry = func(
		ctx context.Context,
		_ string,
		_ string,
		_ []searchdb.SiteSearchTelemetryImpression,
	) (searchdb.SiteSearchTelemetryRecord, error) {
		assertSearchTelemetryDeadline(t, ctx)
		return searchdb.SiteSearchTelemetryRecord{}, &searchTestSecretError{secret: secret}
	}
	cookieValue := testSearchSessionValue(3)

	request := newSearchGETRequest(query, cookieValue, "192.0.2.22:1234")
	response := httptest.NewRecorder()
	handler.handleSearch(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body)
	}
	var body publicSearchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !validRFC4122V4(body.QueryID) || len(body.Results) != 2 {
		t.Fatalf("fallback response = %+v", body)
	}
	seen := map[string]struct{}{body.QueryID: {}}
	for _, result := range body.Results {
		if !validRFC4122V4(result.ImpressionID) {
			t.Fatalf("fallback impression ID = %q", result.ImpressionID)
		}
		if _, duplicate := seen[result.ImpressionID]; duplicate {
			t.Fatalf("duplicate fallback ID %q", result.ImpressionID)
		}
		seen[result.ImpressionID] = struct{}{}
	}
	if !strings.Contains(logs.String(), "*handler.searchTestSecretError") {
		t.Fatalf("log omitted error type: %q", logs.String())
	}
	for _, privateValue := range []string{secret, query, cookieValue, independentSearchDigest(cookieValue)} {
		if strings.Contains(logs.String(), privateValue) {
			t.Fatalf("log exposed %q: %q", privateValue, logs.String())
		}
	}
}

func TestPublicSearchMalformedTelemetryRecordUsesFallback(t *testing.T) {
	service := &fakePublicSearchService{results: []search.PublicResult{testPublicSearchResult(1)}}
	handler := newTestSearchHandler(service)
	handler.recordTelemetry = func(
		context.Context,
		string,
		string,
		[]searchdb.SiteSearchTelemetryImpression,
	) (searchdb.SiteSearchTelemetryRecord, error) {
		return searchdb.SiteSearchTelemetryRecord{
			QueryID:       "not-a-uuid",
			ImpressionIDs: []string{"also-invalid"},
		}, nil
	}

	response := httptest.NewRecorder()
	handler.handleSearch(response, newSearchGETRequest("release", testSearchSessionValue(4), "192.0.2.23:1234"))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body)
	}
	var body publicSearchResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !validRFC4122V4(body.QueryID) || len(body.Results) != 1 || !validRFC4122V4(body.Results[0].ImpressionID) {
		t.Fatalf("fallback response = %+v", body)
	}
}

func TestPublicSearchEmptyResultsEncodeAsEmptyArray(t *testing.T) {
	service := &fakePublicSearchService{results: []search.PublicResult{}}
	handler := newTestSearchHandler(service)
	response := httptest.NewRecorder()
	handler.handleSearch(response, newSearchGETRequest("nothing", testSearchSessionValue(5), "192.0.2.24:1234"))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body)
	}
	if !strings.Contains(response.Body.String(), `"result_count":0`) || !strings.Contains(response.Body.String(), `"results":[]`) {
		t.Fatalf("empty response = %s", response.Body)
	}
}

func TestSearchSessionCookieGenerationReuseAndReplacement(t *testing.T) {
	t.Run("generate", func(t *testing.T) {
		handler := newTestSearchHandler(&fakePublicSearchService{})
		handler.cookies = CookiePolicy{Secure: true}
		handler.random = bytes.NewReader(bytes.Repeat([]byte{0xab}, searchSessionBytes))
		request := httptest.NewRequest(http.MethodGet, "/api/search", nil)
		response := httptest.NewRecorder()

		digest, err := handler.searchSessionDigest(response, request)
		if err != nil {
			t.Fatal(err)
		}
		cookie := requireCookie(t, response, searchSessionCookieName)
		assertSearchCookiePolicy(t, cookie, true)
		if digest != independentSearchDigest(cookie.Value) || !validSearchSessionValue(cookie.Value) {
			t.Fatalf("cookie/digest = %q / %q", cookie.Value, digest)
		}
	})

	t.Run("reuse valid", func(t *testing.T) {
		handler := newTestSearchHandler(&fakePublicSearchService{})
		value := testSearchSessionValue(7)
		request := httptest.NewRequest(http.MethodGet, "/api/search", nil)
		request.AddCookie(&http.Cookie{Name: searchSessionCookieName, Value: value})
		response := httptest.NewRecorder()

		digest, err := handler.searchSessionDigest(response, request)
		if err != nil || digest != independentSearchDigest(value) {
			t.Fatalf("digest/error = %q / %v", digest, err)
		}
		if len(response.Result().Cookies()) != 0 {
			t.Fatalf("valid session was unnecessarily replaced: %v", response.Result().Cookies())
		}
	})

	invalidCases := []struct {
		name    string
		cookies []string
	}{
		{name: "bad base64", cookies: []string{"not+base64"}},
		{name: "wrong byte length", cookies: []string{base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, searchSessionBytes-1))}},
		{name: "padded noncanonical", cookies: []string{testSearchSessionValue(8) + "="}},
		{name: "duplicate", cookies: []string{testSearchSessionValue(9), testSearchSessionValue(9)}},
	}
	for _, test := range invalidCases {
		t.Run("replace "+test.name, func(t *testing.T) {
			handler := newTestSearchHandler(&fakePublicSearchService{})
			handler.random = bytes.NewReader(bytes.Repeat([]byte{0xcd}, searchSessionBytes))
			request := httptest.NewRequest(http.MethodGet, "/api/search", nil)
			for _, value := range test.cookies {
				request.AddCookie(&http.Cookie{Name: searchSessionCookieName, Value: value})
			}
			response := httptest.NewRecorder()

			digest, err := handler.searchSessionDigest(response, request)
			if err != nil {
				t.Fatal(err)
			}
			replacement := requireCookie(t, response, searchSessionCookieName)
			if !validSearchSessionValue(replacement.Value) || digest != independentSearchDigest(replacement.Value) {
				t.Fatalf("replacement/digest = %q / %q", replacement.Value, digest)
			}
		})
	}
}

func TestSearchSessionEntropyFailureIsBoundedAndSkipsService(t *testing.T) {
	secret := "entropy device details"
	service := &fakePublicSearchService{}
	handler := newTestSearchHandler(service)
	handler.random = searchFailingReader{err: &searchTestSecretError{secret: secret}}
	logs := captureSearchLogs(handler)
	request := httptest.NewRequest(http.MethodGet, "/api/search?q=release", nil)
	request.RemoteAddr = "192.0.2.25:1234"
	response := httptest.NewRecorder()
	handler.handleSearch(response, request)

	if response.Code != http.StatusServiceUnavailable || service.calls != 0 {
		t.Fatalf("status/service calls = %d/%d", response.Code, service.calls)
	}
	assertSearchNoStoreJSON(t, response)
	if strings.Contains(logs.String(), secret) || !strings.Contains(logs.String(), "*handler.searchTestSecretError") {
		t.Fatalf("unsafe entropy log = %q", logs.String())
	}
}

func TestPublicSearchRateLimitsByPeerAndSessionAndIgnoresForwardedHeaders(t *testing.T) {
	t.Run("peer", func(t *testing.T) {
		service := &fakePublicSearchService{}
		handler := newTestSearchHandler(service)
		peer := "192.0.2.30"
		for range searchQueryPeerPolicy.Burst {
			if decision := handler.limits.allowSearch(searchQueryPeerPolicy, peer); !decision.Allowed {
				t.Fatalf("preload peer token denied: %+v", decision)
			}
		}
		request := newSearchGETRequest("release", testSearchSessionValue(10), peer+":4321")
		request.Header.Set("X-Forwarded-For", "203.0.113.10")
		request.Header.Set("X-Real-IP", "203.0.113.11")
		response := httptest.NewRecorder()

		handler.handleSearch(response, request)
		if response.Code != http.StatusTooManyRequests || service.calls != 0 || response.Header().Get("Retry-After") == "" {
			t.Fatalf("limited response = status %d, calls %d, retry %q", response.Code, service.calls, response.Header().Get("Retry-After"))
		}
		assertSearchNoStoreJSON(t, response)
	})

	t.Run("session", func(t *testing.T) {
		service := &fakePublicSearchService{}
		handler := newTestSearchHandler(service)
		cookieValue := testSearchSessionValue(11)
		digest := independentSearchDigest(cookieValue)
		for range searchQuerySessionPolicy.Burst {
			if decision := handler.limits.allowSearch(searchQuerySessionPolicy, digest); !decision.Allowed {
				t.Fatalf("preload session token denied: %+v", decision)
			}
		}
		response := httptest.NewRecorder()
		handler.handleSearch(response, newSearchGETRequest("release", cookieValue, "192.0.2.31:4321"))
		if response.Code != http.StatusTooManyRequests || service.calls != 0 {
			t.Fatalf("limited response = status %d, calls %d", response.Code, service.calls)
		}
	})
}

func TestPublicSearchAdmissionOrderIsPeerValidationSession(t *testing.T) {
	t.Run("peer load-shed precedes validation and session creation", func(t *testing.T) {
		service := &fakePublicSearchService{}
		handler := newTestSearchHandler(service)
		peer := "192.0.2.32"
		for range searchQueryPeerPolicy.Burst {
			if decision := handler.limits.allowSearch(searchQueryPeerPolicy, peer); !decision.Allowed {
				t.Fatalf("preload peer token denied: %+v", decision)
			}
		}
		entropy := bytes.NewReader(bytes.Repeat([]byte{0x31}, searchSessionBytes))
		handler.random = entropy
		request := httptest.NewRequest(http.MethodGet, "/api/search?q=%zz", nil)
		request.RemoteAddr = peer + ":1234"
		response := httptest.NewRecorder()

		handler.handleSearch(response, request)

		if response.Code != http.StatusTooManyRequests || service.calls != 0 {
			t.Fatalf("peer-first response = status %d, service calls %d", response.Code, service.calls)
		}
		if len(response.Result().Cookies()) != 0 || entropy.Len() != searchSessionBytes {
			t.Fatalf("peer rejection created session state: cookies=%v entropy=%d", response.Result().Cookies(), entropy.Len())
		}
	})

	t.Run("validation consumes peer but not existing session token", func(t *testing.T) {
		service := &fakePublicSearchService{}
		handler := newTestSearchHandler(service)
		peer := "192.0.2.33"
		cookieValue := testSearchSessionValue(0x32)
		request := httptest.NewRequest(http.MethodGet, "/api/search?q=%zz", nil)
		request.RemoteAddr = peer + ":1234"
		request.AddCookie(&http.Cookie{Name: searchSessionCookieName, Value: cookieValue})
		response := httptest.NewRecorder()

		handler.handleSearch(response, request)

		if response.Code != http.StatusBadRequest || service.calls != 0 {
			t.Fatalf("validation response = status %d, service calls %d", response.Code, service.calls)
		}
		for index := 1; index < searchQueryPeerPolicy.Burst; index++ {
			if decision := handler.limits.allowSearch(searchQueryPeerPolicy, peer); !decision.Allowed {
				t.Fatalf("peer token %d denied after one invalid request: %+v", index, decision)
			}
		}
		if handler.limits.allowSearch(searchQueryPeerPolicy, peer).Allowed {
			t.Fatal("invalid query did not consume its coarse peer token")
		}
		digest := independentSearchDigest(cookieValue)
		for index := 0; index < searchQuerySessionPolicy.Burst; index++ {
			if decision := handler.limits.allowSearch(searchQuerySessionPolicy, digest); !decision.Allowed {
				t.Fatalf("validation consumed session token %d: %+v", index, decision)
			}
		}
	})
}

func TestSearchClickStrictValidationRejectsBeforeDatabase(t *testing.T) {
	validID := testTelemetryUUID(10)
	tests := []struct {
		name   string
		body   string
		status int
	}{
		{name: "empty object", body: `{}`, status: http.StatusBadRequest},
		{name: "empty ID", body: `{"impression_id":""}`, status: http.StatusBadRequest},
		{name: "wrong shape", body: `{"impression_id":"not-a-uuid"}`, status: http.StatusBadRequest},
		{name: "wrong version", body: `{"impression_id":"aaaaaaaa-aaaa-1aaa-8aaa-aaaaaaaaaaaa"}`, status: http.StatusBadRequest},
		{name: "wrong variant", body: `{"impression_id":"aaaaaaaa-aaaa-4aaa-7aaa-aaaaaaaaaaaa"}`, status: http.StatusBadRequest},
		{name: "nonhex", body: `{"impression_id":"gggggggg-gggg-4ggg-8ggg-gggggggggggg"}`, status: http.StatusBadRequest},
		{name: "number", body: `{"impression_id":123}`, status: http.StatusBadRequest},
		{name: "null", body: `{"impression_id":null}`, status: http.StatusBadRequest},
		{name: "unknown field", body: `{"impression_id":"` + validID + `","extra":true}`, status: http.StatusBadRequest},
		{name: "duplicate field", body: `{"impression_id":"` + validID + `","impression_id":"` + validID + `"}`, status: http.StatusBadRequest},
		{name: "array", body: `[]`, status: http.StatusBadRequest},
		{name: "trailing JSON", body: `{"impression_id":"` + validID + `"} {}`, status: http.StatusBadRequest},
		{name: "oversized", body: `{"impression_id":"` + strings.Repeat("a", smallJSONBodyBytes) + `"}`, status: http.StatusRequestEntityTooLarge},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestSearchHandler(&fakePublicSearchService{})
			clickCalls := 0
			handler.recordClick = func(context.Context, string, string) (bool, error) {
				clickCalls++
				return true, nil
			}
			request := newSearchClickRequest(test.body, testSearchSessionValue(12), "192.0.2.40:1234")
			response := httptest.NewRecorder()
			handler.handleSearchClick(response, request)

			if response.Code != test.status || clickCalls != 0 {
				t.Fatalf("status/click calls = %d/%d, want %d/0; body=%s", response.Code, clickCalls, test.status, response.Body)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestSearchClickAdmissionOrderIsPeerValidationSession(t *testing.T) {
	t.Run("peer load-shed precedes body validation", func(t *testing.T) {
		handler := newTestSearchHandler(&fakePublicSearchService{})
		peer := "192.0.2.42"
		for range searchClickPeerPolicy.Burst {
			if decision := handler.limits.allowSearch(searchClickPeerPolicy, peer); !decision.Allowed {
				t.Fatalf("preload peer token denied: %+v", decision)
			}
		}
		clickCalls := 0
		handler.recordClick = func(context.Context, string, string) (bool, error) {
			clickCalls++
			return true, nil
		}
		response := httptest.NewRecorder()
		handler.handleSearchClick(response, newSearchClickRequest("{", "", peer+":1234"))
		if response.Code != http.StatusTooManyRequests || clickCalls != 0 {
			t.Fatalf("peer-first click = status %d, DB calls %d", response.Code, clickCalls)
		}
	})

	t.Run("validation consumes peer but not session token", func(t *testing.T) {
		handler := newTestSearchHandler(&fakePublicSearchService{})
		peer := "192.0.2.43"
		cookieValue := testSearchSessionValue(0x43)
		response := httptest.NewRecorder()
		handler.handleSearchClick(response, newSearchClickRequest("{", cookieValue, peer+":1234"))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid click status = %d", response.Code)
		}
		for index := 1; index < searchClickPeerPolicy.Burst; index++ {
			if decision := handler.limits.allowSearch(searchClickPeerPolicy, peer); !decision.Allowed {
				t.Fatalf("peer token %d denied after invalid body: %+v", index, decision)
			}
		}
		if handler.limits.allowSearch(searchClickPeerPolicy, peer).Allowed {
			t.Fatal("invalid click did not consume its coarse peer token")
		}
		digest := independentSearchDigest(cookieValue)
		for index := 0; index < searchClickSessionPolicy.Burst; index++ {
			if decision := handler.limits.allowSearch(searchClickSessionPolicy, digest); !decision.Allowed {
				t.Fatalf("validation consumed click session token %d: %+v", index, decision)
			}
		}
	})
}

func TestSearchClickWithoutExactlyOneValidExistingSessionDropsWithoutStateOrDatabase(t *testing.T) {
	validBody := `{"impression_id":"` + testTelemetryUUID(12) + `"}`
	for _, test := range []struct {
		name    string
		cookies []string
	}{
		{name: "missing"},
		{name: "invalid", cookies: []string{"not+base64"}},
		{name: "duplicate", cookies: []string{testSearchSessionValue(1), testSearchSessionValue(1)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestSearchHandler(&fakePublicSearchService{})
			entropyFill := byte(0x50 + len(test.name))
			entropy := bytes.NewReader(bytes.Repeat([]byte{entropyFill}, searchSessionBytes))
			handler.random = entropy
			clickCalls := 0
			handler.recordClick = func(context.Context, string, string) (bool, error) {
				clickCalls++
				return true, nil
			}
			peer := "192.0.2.44"
			request := newSearchClickRequest(validBody, "", peer+":1234")
			for _, value := range test.cookies {
				request.AddCookie(&http.Cookie{Name: searchSessionCookieName, Value: value})
			}
			response := httptest.NewRecorder()

			handler.handleSearchClick(response, request)

			if response.Code != http.StatusNoContent || response.Body.Len() != 0 || clickCalls != 0 {
				t.Fatalf("sessionless click = status %d, body %q, DB calls %d", response.Code, response.Body, clickCalls)
			}
			if len(response.Result().Cookies()) != 0 || entropy.Len() != searchSessionBytes {
				t.Fatalf("sessionless click minted state: cookies=%v entropy=%d", response.Result().Cookies(), entropy.Len())
			}
			predictedDigest := independentSearchDigest(testSearchSessionValue(entropyFill))
			for index := 0; index < searchClickSessionPolicy.Burst; index++ {
				if decision := handler.limits.allowSearch(searchClickSessionPolicy, predictedDigest); !decision.Allowed {
					t.Fatalf("sessionless click allocated session bucket at token %d: %+v", index, decision)
				}
			}
		})
	}
}

func TestSearchClickAlwaysReturnsNoContentForValidShapedTelemetryOutcomes(t *testing.T) {
	secret := "click database details"
	for _, test := range []struct {
		name     string
		recorded bool
		err      error
	}{
		{name: "existing", recorded: true},
		{name: "missing"},
		{name: "duplicate"},
		{name: "wrong session"},
		{name: "database failure", err: &searchTestSecretError{secret: secret}},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestSearchHandler(&fakePublicSearchService{})
			logs := captureSearchLogs(handler)
			cookieValue := testSearchSessionValue(13)
			impressionID := strings.ToUpper(testTelemetryUUID(14))
			clickCalls := 0
			handler.recordClick = func(ctx context.Context, gotID, gotDigest string) (bool, error) {
				clickCalls++
				assertSearchTelemetryDeadline(t, ctx)
				if gotID != impressionID || gotDigest != independentSearchDigest(cookieValue) {
					t.Fatalf("click args = %q / %q", gotID, gotDigest)
				}
				return test.recorded, test.err
			}
			response := httptest.NewRecorder()
			handler.handleSearchClick(response, newSearchClickRequest(
				`{"impression_id":"`+impressionID+`"}`,
				cookieValue,
				"192.0.2.41:1234",
			))

			if response.Code != http.StatusNoContent || response.Body.Len() != 0 || clickCalls != 1 {
				t.Fatalf("response = status %d, body %q, calls %d", response.Code, response.Body, clickCalls)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
			}
			if test.err != nil {
				if !strings.Contains(logs.String(), "*handler.searchTestSecretError") || strings.Contains(logs.String(), secret) {
					t.Fatalf("unsafe click log = %q", logs.String())
				}
			}
		})
	}
}

func TestSearchClickRateLimitsByPeerAndSession(t *testing.T) {
	for _, test := range []struct {
		name    string
		policy  string
		preload func(*SearchHandler, string)
		peer    string
		cookie  string
	}{
		{
			name:   "peer",
			policy: "peer",
			peer:   "192.0.2.50",
			cookie: testSearchSessionValue(15),
			preload: func(handler *SearchHandler, key string) {
				for range searchClickPeerPolicy.Burst {
					handler.limits.allowSearch(searchClickPeerPolicy, key)
				}
			},
		},
		{
			name:   "session",
			policy: "session",
			peer:   "192.0.2.51",
			cookie: testSearchSessionValue(16),
			preload: func(handler *SearchHandler, key string) {
				for range searchClickSessionPolicy.Burst {
					handler.limits.allowSearch(searchClickSessionPolicy, key)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestSearchHandler(&fakePublicSearchService{})
			key := test.peer
			if test.policy == "session" {
				key = independentSearchDigest(test.cookie)
			}
			test.preload(handler, key)
			clickCalls := 0
			handler.recordClick = func(context.Context, string, string) (bool, error) {
				clickCalls++
				return true, nil
			}
			request := newSearchClickRequest(
				`{"impression_id":"`+testTelemetryUUID(17)+`"}`,
				test.cookie,
				test.peer+":1234",
			)
			request.Header.Set("X-Forwarded-For", "203.0.113.50")
			response := httptest.NewRecorder()
			handler.handleSearchClick(response, request)
			if response.Code != http.StatusTooManyRequests || clickCalls != 0 || response.Header().Get("Retry-After") == "" {
				t.Fatalf("limited click = status %d, calls %d, retry %q", response.Code, clickCalls, response.Header().Get("Retry-After"))
			}
		})
	}
}

func TestPublicSearchQueryGateIsProcessWideAndHeldThroughTelemetry(t *testing.T) {
	limits := testAbuseLimits(func() time.Time { return time.Unix(100, 0) })
	var blockingServiceCalls atomic.Int32
	blockingService := publicSearchServiceFunc(func(
		context.Context,
		search.PublicSearchRequest,
	) ([]search.PublicResult, error) {
		blockingServiceCalls.Add(1)
		return []search.PublicResult{testPublicSearchResult(1)}, nil
	})
	blockingHandler := newTestSearchHandlerWithLimits(blockingService, limits)
	telemetryStarted := make(chan struct{}, searchQueryConcurrency)
	releaseTelemetry := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(releaseTelemetry) }) }
	t.Cleanup(releaseAll)
	var telemetryCalls atomic.Int32
	blockingHandler.recordTelemetry = func(
		context.Context,
		string,
		string,
		[]searchdb.SiteSearchTelemetryImpression,
	) (searchdb.SiteSearchTelemetryRecord, error) {
		telemetryCalls.Add(1)
		telemetryStarted <- struct{}{}
		<-releaseTelemetry
		return testTelemetryRecord(1), nil
	}

	responses := make(chan *httptest.ResponseRecorder, searchQueryConcurrency)
	for range searchQueryConcurrency {
		go func() {
			response := httptest.NewRecorder()
			blockingHandler.handleSearch(response, newSearchGETRequest(
				"release",
				testSearchSessionValue(20),
				"192.0.2.60:1234",
			))
			responses <- response
		}()
	}
	deadline := time.After(2 * time.Second)
	for index := 0; index < searchQueryConcurrency; index++ {
		select {
		case <-telemetryStarted:
		case <-deadline:
			releaseAll()
			t.Fatalf("only %d query telemetry operations reached the gate", index)
		}
	}

	var overflowServiceCalls atomic.Int32
	overflowService := publicSearchServiceFunc(func(
		context.Context,
		search.PublicSearchRequest,
	) ([]search.PublicResult, error) {
		overflowServiceCalls.Add(1)
		return []search.PublicResult{testPublicSearchResult(2)}, nil
	})
	overflowHandler := newTestSearchHandlerWithLimits(overflowService, limits)
	overflowResponse := httptest.NewRecorder()
	overflowHandler.handleSearch(overflowResponse, newSearchGETRequest(
		"release",
		testSearchSessionValue(20),
		"192.0.2.60:1234",
	))
	if overflowResponse.Code != http.StatusTooManyRequests || overflowServiceCalls.Load() != 0 {
		t.Fatalf("saturated query = status %d, overflow service calls %d", overflowResponse.Code, overflowServiceCalls.Load())
	}
	if blockingServiceCalls.Load() != searchQueryConcurrency || telemetryCalls.Load() != searchQueryConcurrency {
		t.Fatalf("admitted query calls = service %d, telemetry %d", blockingServiceCalls.Load(), telemetryCalls.Load())
	}

	releaseAll()
	completionDeadline := time.After(2 * time.Second)
	for index := 0; index < searchQueryConcurrency; index++ {
		select {
		case response := <-responses:
			if response.Code != http.StatusOK {
				t.Fatalf("admitted query %d status = %d, body=%s", index, response.Code, response.Body)
			}
		case <-completionDeadline:
			t.Fatalf("only %d admitted queries completed", index)
		}
	}
	replacement := httptest.NewRecorder()
	overflowHandler.handleSearch(replacement, newSearchGETRequest(
		"release",
		testSearchSessionValue(20),
		"192.0.2.60:1234",
	))
	if replacement.Code != http.StatusOK || overflowServiceCalls.Load() != 1 {
		t.Fatalf("query after release = status %d, service calls %d", replacement.Code, overflowServiceCalls.Load())
	}
}

func TestSearchClickGateIsProcessWideAndDropsWhenFull(t *testing.T) {
	limits := testAbuseLimits(func() time.Time { return time.Unix(100, 0) })
	blockingHandler := newTestSearchHandlerWithLimits(&fakePublicSearchService{}, limits)
	clickStarted := make(chan struct{}, searchClickConcurrency)
	releaseClicks := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(releaseClicks) }) }
	t.Cleanup(releaseAll)
	var blockingClickCalls atomic.Int32
	blockingHandler.recordClick = func(context.Context, string, string) (bool, error) {
		blockingClickCalls.Add(1)
		clickStarted <- struct{}{}
		<-releaseClicks
		return true, nil
	}
	requestBody := `{"impression_id":"` + testTelemetryUUID(21) + `"}`
	responses := make(chan *httptest.ResponseRecorder, searchClickConcurrency)
	for range searchClickConcurrency {
		go func() {
			response := httptest.NewRecorder()
			blockingHandler.handleSearchClick(response, newSearchClickRequest(
				requestBody,
				testSearchSessionValue(22),
				"192.0.2.61:1234",
			))
			responses <- response
		}()
	}
	deadline := time.After(2 * time.Second)
	for index := 0; index < searchClickConcurrency; index++ {
		select {
		case <-clickStarted:
		case <-deadline:
			releaseAll()
			t.Fatalf("only %d click writes reached the gate", index)
		}
	}

	overflowHandler := newTestSearchHandlerWithLimits(&fakePublicSearchService{}, limits)
	var overflowClickCalls atomic.Int32
	overflowHandler.recordClick = func(context.Context, string, string) (bool, error) {
		overflowClickCalls.Add(1)
		return true, nil
	}
	overflowResponse := httptest.NewRecorder()
	overflowHandler.handleSearchClick(overflowResponse, newSearchClickRequest(
		requestBody,
		testSearchSessionValue(22),
		"192.0.2.61:1234",
	))
	if overflowResponse.Code != http.StatusNoContent || overflowResponse.Body.Len() != 0 || overflowClickCalls.Load() != 0 {
		t.Fatalf("saturated click = status %d, body %q, overflow DB calls %d", overflowResponse.Code, overflowResponse.Body, overflowClickCalls.Load())
	}
	if blockingClickCalls.Load() != searchClickConcurrency {
		t.Fatalf("admitted click DB calls = %d", blockingClickCalls.Load())
	}

	releaseAll()
	completionDeadline := time.After(2 * time.Second)
	for index := 0; index < searchClickConcurrency; index++ {
		select {
		case response := <-responses:
			if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
				t.Fatalf("admitted click %d = status %d, body %q", index, response.Code, response.Body)
			}
		case <-completionDeadline:
			t.Fatalf("only %d admitted clicks completed", index)
		}
	}
	replacement := httptest.NewRecorder()
	overflowHandler.handleSearchClick(replacement, newSearchClickRequest(
		requestBody,
		testSearchSessionValue(22),
		"192.0.2.61:1234",
	))
	if replacement.Code != http.StatusNoContent || overflowClickCalls.Load() != 1 {
		t.Fatalf("click after release = status %d, DB calls %d", replacement.Code, overflowClickCalls.Load())
	}
}

func TestSearchUUIDGenerationAndValidation(t *testing.T) {
	id, err := newRandomSearchUUID(bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if id != "00000000-0000-4000-8000-000000000000" || !validRFC4122V4(id) || !validRFC4122V4(strings.ToUpper(id)) {
		t.Fatalf("generated UUID = %q", id)
	}
	for _, invalid := range []string{
		"00000000-0000-4000-0000-000000000000",
		"00000000-0000-5000-8000-000000000000",
		"00000000000040008000000000000000",
		"00000000-0000-4000-8000-00000000000z",
	} {
		if validRFC4122V4(invalid) {
			t.Fatalf("invalid UUID accepted: %q", invalid)
		}
	}
}

type fakePublicSearchService struct {
	calls   int
	ctx     context.Context
	request search.PublicSearchRequest
	results []search.PublicResult
	err     error
}

type publicSearchServiceFunc func(
	context.Context,
	search.PublicSearchRequest,
) ([]search.PublicResult, error)

func (function publicSearchServiceFunc) Search(
	ctx context.Context,
	request search.PublicSearchRequest,
) ([]search.PublicResult, error) {
	return function(ctx, request)
}

func (service *fakePublicSearchService) Search(
	ctx context.Context,
	request search.PublicSearchRequest,
) ([]search.PublicResult, error) {
	service.calls++
	service.ctx = ctx
	service.request = request
	return service.results, service.err
}

func newTestSearchHandler(service PublicSearchService) *SearchHandler {
	return newTestSearchHandlerWithLimits(
		service,
		testAbuseLimits(func() time.Time { return time.Unix(100, 0) }),
	)
}

func newTestSearchHandlerWithLimits(service PublicSearchService, limits *AbuseLimits) *SearchHandler {
	handler := NewSearchHandler(
		nil,
		service,
		CookiePolicy{},
		limits,
	)
	handler.logf = func(string, ...any) {}
	handler.recordTelemetry = func(
		_ context.Context,
		_ string,
		_ string,
		impressions []searchdb.SiteSearchTelemetryImpression,
	) (searchdb.SiteSearchTelemetryRecord, error) {
		return testTelemetryRecord(len(impressions)), nil
	}
	handler.recordClick = func(context.Context, string, string) (bool, error) { return true, nil }
	return handler
}

func testTelemetryRecord(impressionCount int) searchdb.SiteSearchTelemetryRecord {
	record := searchdb.SiteSearchTelemetryRecord{
		QueryID:       testTelemetryUUID(1),
		ImpressionIDs: make([]string, impressionCount),
	}
	for index := range record.ImpressionIDs {
		record.ImpressionIDs[index] = testTelemetryUUID(index + 2)
	}
	return record
}

func testTelemetryUUID(value int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", value)
}

func testPublicSearchResult(position int) search.PublicResult {
	return search.PublicResult{
		SiteID:        fmt.Sprintf("00000000-0000-4000-8000-%012x", position+100),
		VersionNumber: 1,
		Owner:         fmt.Sprintf("owner-%d", position),
		Site:          fmt.Sprintf("site-%d", position),
		PagePath:      "index.html",
		URL:           fmt.Sprintf("https://owner-%d.foo.example/site-%d/", position, position),
		Title:         fmt.Sprintf("Result %d", position),
		Snippet:       "A result snippet.",
		Position:      position,
	}
}

func testSearchSessionValue(fill byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{fill}, searchSessionBytes))
}

func independentSearchDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func newSearchGETRequest(query, cookieValue, remoteAddr string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/search?q="+url.QueryEscape(query), nil)
	request.RemoteAddr = remoteAddr
	if cookieValue != "" {
		request.AddCookie(&http.Cookie{Name: searchSessionCookieName, Value: cookieValue})
	}
	return request
}

func newSearchClickRequest(body, cookieValue, remoteAddr string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/search/click", strings.NewReader(body))
	request.RemoteAddr = remoteAddr
	if cookieValue != "" {
		request.AddCookie(&http.Cookie{Name: searchSessionCookieName, Value: cookieValue})
	}
	return request
}

func assertSearchNoStoreJSON(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("Content-Type = %q", contentType)
	}
}

func assertSearchCookiePolicy(t *testing.T, cookie *http.Cookie, secure bool) {
	t.Helper()
	if cookie.Name != searchSessionCookieName || cookie.Path != "/" || !cookie.HttpOnly ||
		cookie.Secure != secure || cookie.SameSite != http.SameSiteLaxMode ||
		cookie.MaxAge != searchSessionCookieMaxAge {
		t.Fatalf("search cookie = %+v", cookie)
	}
}

func assertSearchTelemetryDeadline(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline, ok := ctx.Deadline()
	remaining := time.Until(deadline)
	if !ok || remaining <= 0 || remaining > searchTelemetryTimeout {
		t.Fatalf("telemetry deadline = %v, present=%t, remaining=%v", deadline, ok, remaining)
	}
}

func captureSearchLogs(handler *SearchHandler) *strings.Builder {
	var logs strings.Builder
	handler.logf = func(format string, args ...any) {
		_, _ = fmt.Fprintf(&logs, format, args...)
	}
	return &logs
}

type searchTestSecretError struct {
	secret string
}

func (err *searchTestSecretError) Error() string { return err.secret }

type searchFailingReader struct {
	err error
}

func (reader searchFailingReader) Read([]byte) (int, error) { return 0, reader.err }
