package handler

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	searchdb "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/search"
)

const (
	searchSessionBytes     = 32
	maxPublicSearchOffset  = 1_000
	searchTelemetryTimeout = 500 * time.Millisecond
)

var (
	errInvalidPublicSearchRequest = errors.New("invalid public search request")
	errInvalidSearchSession       = errors.New("invalid search session")
	errInvalidTelemetryRecord     = errors.New("invalid search telemetry record")
)

// PublicSearchService is the HTTP handler's narrow dependency on the ranked
// search service. The production implementation is *search.PublicService.
type PublicSearchService interface {
	Search(context.Context, search.PublicSearchRequest) ([]search.PublicResult, error)
}

type recordSearchTelemetryFunc func(
	context.Context,
	string,
	string,
	[]searchdb.SiteSearchTelemetryImpression,
) (searchdb.SiteSearchTelemetryRecord, error)

type recordSearchClickFunc func(context.Context, string, string) (bool, error)

// SearchHandler owns the public search/click HTTP contracts and their
// pseudonymous session, abuse-limit, and best-effort telemetry boundaries.
type SearchHandler struct {
	service PublicSearchService
	cookies CookiePolicy
	limits  *AbuseLimits

	random          io.Reader
	logf            func(string, ...any)
	recordTelemetry recordSearchTelemetryFunc
	recordClick     recordSearchClickFunc
}

func NewSearchHandler(
	database *sql.DB,
	service PublicSearchService,
	cookies CookiePolicy,
	limits ...*AbuseLimits,
) *SearchHandler {
	return &SearchHandler{
		service: service,
		cookies: cookies,
		limits:  chooseAbuseLimits(limits),
		random:  cryptorand.Reader,
		logf:    log.Printf,
		recordTelemetry: func(
			ctx context.Context,
			query string,
			sessionDigest string,
			impressions []searchdb.SiteSearchTelemetryImpression,
		) (searchdb.SiteSearchTelemetryRecord, error) {
			return searchdb.RecordSiteSearchTelemetry(ctx, database, query, sessionDigest, impressions)
		},
		recordClick: func(ctx context.Context, impressionID, sessionDigest string) (bool, error) {
			return searchdb.RecordSiteSearchClick(ctx, database, impressionID, sessionDigest)
		},
	}
}

// Register wires the search and click routes behind authMiddleware:
// design.md 7.2 removes anonymous viewing everywhere, search included, so
// both routes now require a valid session or key the same way any other
// authenticated JSON API does (a 401 in authMiddleware's own shape, not the
// pseudonymous-cookie treatment these handlers used to be the only routes
// answering unauthenticated).
func (h *SearchHandler) Register(mux *http.ServeMux, authMiddleware func(http.Handler) http.Handler) {
	mux.Handle("GET /api/search", authMiddleware(http.HandlerFunc(h.handleSearch)))
	mux.Handle("POST /api/search/click", authMiddleware(http.HandlerFunc(h.handleSearchClick)))
}

type publicSearchResponse struct {
	QueryID     string                       `json:"query_id"`
	Query       string                       `json:"query"`
	ResultCount int                          `json:"result_count"`
	Results     []publicSearchResultResponse `json:"results"`
}

type publicSearchResultResponse struct {
	ImpressionID string `json:"impression_id"`
	Owner        string `json:"owner"`
	Site         string `json:"site"`
	PagePath     string `json:"page_path"`
	URL          string `json:"url"`
	Title        string `json:"title"`
	Snippet      string `json:"snippet"`
	Position     int    `json:"position"`
}

func (h *SearchHandler) handleSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if h.limits == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "search unavailable"})
		return
	}
	if decision := h.limits.allowSearch(searchQueryPeerPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}

	request, normalizedQuery, err := parsePublicSearchRequest(r.URL.RawQuery)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid search request"})
		return
	}
	sessionDigest, err := h.searchSessionDigest(w, r)
	if err != nil {
		h.logErrorType("create search session", err)
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "search unavailable"})
		return
	}
	if decision := h.limits.allowSearch(searchQuerySessionPolicy, sessionDigest); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	if h.service == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "search unavailable"})
		return
	}
	releaseQuery, acquired := h.limits.acquireSearchQuery()
	if !acquired {
		writeConcurrencyRateLimit(w)
		return
	}
	queryReleased := false
	defer func() {
		if !queryReleased {
			releaseQuery()
		}
	}()

	results, err := h.service.Search(r.Context(), request)
	if err != nil {
		h.logErrorType("public search", err)
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "search unavailable"})
		return
	}

	record, err := h.recordResultTelemetry(r.Context(), normalizedQuery, sessionDigest, results)
	if err != nil {
		h.logErrorType("record search telemetry", err)
		record, err = h.fallbackTelemetryRecord(len(results))
		if err != nil {
			h.logErrorType("create fallback search telemetry IDs", err)
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "search unavailable"})
			return
		}
	}

	releaseQuery()
	queryReleased = true
	responseResults := make([]publicSearchResultResponse, len(results))
	for index, result := range results {
		responseResults[index] = publicSearchResultResponse{
			ImpressionID: record.ImpressionIDs[index],
			Owner:        result.Owner,
			Site:         result.Site,
			PagePath:     result.PagePath,
			URL:          result.URL,
			Title:        result.Title,
			Snippet:      result.Snippet,
			Position:     result.Position,
		}
	}
	writeJSON(w, http.StatusOK, publicSearchResponse{
		QueryID:     record.QueryID,
		Query:       normalizedQuery,
		ResultCount: len(responseResults),
		Results:     responseResults,
	})
}

func parsePublicSearchRequest(rawQuery string) (search.PublicSearchRequest, string, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return search.PublicSearchRequest{}, "", errInvalidPublicSearchRequest
	}
	for key := range values {
		switch key {
		case "q", "limit", "offset", "group":
		default:
			return search.PublicSearchRequest{}, "", errInvalidPublicSearchRequest
		}
	}

	queries := values["q"]
	if len(queries) != 1 || !utf8.ValidString(queries[0]) {
		return search.PublicSearchRequest{}, "", errInvalidPublicSearchRequest
	}
	normalizedQuery := strings.Join(strings.Fields(queries[0]), " ")
	if normalizedQuery == "" || strings.ContainsRune(normalizedQuery, '\x00') ||
		utf8.RuneCountInString(normalizedQuery) > search.MaxPublicSearchQueryRunes {
		return search.PublicSearchRequest{}, "", errInvalidPublicSearchRequest
	}

	limit, err := parseSingleBoundedDecimal(
		values,
		"limit",
		search.DefaultPublicSearchLimit,
		1,
		search.MaxPublicSearchLimit,
	)
	if err != nil {
		return search.PublicSearchRequest{}, "", err
	}
	offset, err := parseSingleBoundedDecimal(values, "offset", 0, 0, maxPublicSearchOffset)
	if err != nil {
		return search.PublicSearchRequest{}, "", err
	}
	groups := values["group"]
	if len(groups) > 1 || (len(groups) == 1 && groups[0] != "site") {
		return search.PublicSearchRequest{}, "", errInvalidPublicSearchRequest
	}

	return search.PublicSearchRequest{
		Query:  normalizedQuery,
		Limit:  limit,
		Offset: offset,
	}, normalizedQuery, nil
}

func parseSingleBoundedDecimal(values url.Values, name string, defaultValue, minimum, maximum int) (int, error) {
	provided, exists := values[name]
	if !exists {
		return defaultValue, nil
	}
	if len(provided) != 1 || provided[0] == "" {
		return 0, errInvalidPublicSearchRequest
	}

	value := 0
	for _, digit := range provided[0] {
		if digit < '0' || digit > '9' {
			return 0, errInvalidPublicSearchRequest
		}
		numericDigit := int(digit - '0')
		if value > maximum/10 || (value == maximum/10 && numericDigit > maximum%10) {
			return 0, errInvalidPublicSearchRequest
		}
		value = value*10 + numericDigit
	}
	if value < minimum {
		return 0, errInvalidPublicSearchRequest
	}
	return value, nil
}

func (h *SearchHandler) recordResultTelemetry(
	parent context.Context,
	normalizedQuery string,
	sessionDigest string,
	results []search.PublicResult,
) (searchdb.SiteSearchTelemetryRecord, error) {
	if h.recordTelemetry == nil {
		return searchdb.SiteSearchTelemetryRecord{}, errInvalidTelemetryRecord
	}
	impressions := make([]searchdb.SiteSearchTelemetryImpression, len(results))
	for index, result := range results {
		impressions[index] = searchdb.SiteSearchTelemetryImpression{
			ResultPosition: result.Position,
			SiteID:         result.SiteID,
			VersionNumber:  result.VersionNumber,
			PagePath:       result.PagePath,
		}
	}

	ctx, cancel := context.WithTimeout(parent, searchTelemetryTimeout)
	defer cancel()
	record, err := h.recordTelemetry(ctx, normalizedQuery, sessionDigest, impressions)
	if err != nil {
		return searchdb.SiteSearchTelemetryRecord{}, err
	}
	if !validTelemetryRecord(record, len(results)) {
		return searchdb.SiteSearchTelemetryRecord{}, errInvalidTelemetryRecord
	}
	return record, nil
}

func validTelemetryRecord(record searchdb.SiteSearchTelemetryRecord, impressionCount int) bool {
	if !validRFC4122V4(record.QueryID) || len(record.ImpressionIDs) != impressionCount {
		return false
	}
	seen := map[string]struct{}{record.QueryID: {}}
	for _, impressionID := range record.ImpressionIDs {
		if !validRFC4122V4(impressionID) {
			return false
		}
		if _, duplicate := seen[impressionID]; duplicate {
			return false
		}
		seen[impressionID] = struct{}{}
	}
	return true
}

func (h *SearchHandler) fallbackTelemetryRecord(impressionCount int) (searchdb.SiteSearchTelemetryRecord, error) {
	record := searchdb.SiteSearchTelemetryRecord{
		ImpressionIDs: make([]string, impressionCount),
	}
	seen := make(map[string]struct{}, impressionCount+1)
	newUniqueID := func() (string, error) {
		var id string
		for attempt := 0; attempt < 4; attempt++ {
			candidate, err := newRandomSearchUUID(h.random)
			if err != nil {
				return "", err
			}
			if _, duplicate := seen[candidate]; duplicate {
				continue
			}
			id = candidate
			break
		}
		if id == "" {
			return "", errInvalidTelemetryRecord
		}
		seen[id] = struct{}{}
		return id, nil
	}

	var err error
	record.QueryID, err = newUniqueID()
	if err != nil {
		return searchdb.SiteSearchTelemetryRecord{}, err
	}
	for index := range record.ImpressionIDs {
		record.ImpressionIDs[index], err = newUniqueID()
		if err != nil {
			return searchdb.SiteSearchTelemetryRecord{}, err
		}
	}
	return record, nil
}

type searchClickRequest struct {
	ImpressionID string `json:"impression_id"`
}

func (request *searchClickRequest) UnmarshalJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	opening, ok := token.(json.Delim)
	if err != nil || !ok || opening != '{' {
		return errors.New("search click body must be an object")
	}

	seen := false
	var impressionID string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok || name != "impression_id" || seen {
			return errors.New("search click body has unexpected or duplicate fields")
		}
		if err := decoder.Decode(&impressionID); err != nil {
			return err
		}
		seen = true
	}
	token, err = decoder.Token()
	closing, ok := token.(json.Delim)
	if err != nil || !ok || closing != '}' || !seen {
		return errors.New("search click body is incomplete")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("search click body has trailing data")
	}
	request.ImpressionID = impressionID
	return nil
}

func (h *SearchHandler) handleSearchClick(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.limits == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "search unavailable"})
		return
	}
	if decision := h.limits.allowSearch(searchClickPeerPolicy, clientLimitKey(r)); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}

	var request searchClickRequest
	if !decodeSmallJSON(w, r, &request) {
		return
	}
	if !validRFC4122V4(request.ImpressionID) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid impression_id"})
		return
	}

	sessionDigest, validSession := existingSearchSessionDigest(r)
	if !validSession {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if decision := h.limits.allowSearch(searchClickSessionPolicy, sessionDigest); !decision.Allowed {
		writeRateLimit(w, decision)
		return
	}
	releaseClick, acquired := h.limits.acquireSearchClick()
	if !acquired {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer releaseClick()

	if h.recordClick != nil {
		ctx, cancel := context.WithTimeout(r.Context(), searchTelemetryTimeout)
		_, err := h.recordClick(ctx, request.ImpressionID, sessionDigest)
		cancel()
		if err != nil {
			h.logErrorType("record search click", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *SearchHandler) searchSessionDigest(w http.ResponseWriter, r *http.Request) (string, error) {
	if digest, ok := existingSearchSessionDigest(r); ok {
		return digest, nil
	}
	if h.random == nil {
		return "", errInvalidSearchSession
	}

	valueBytes := make([]byte, searchSessionBytes)
	if _, err := io.ReadFull(h.random, valueBytes); err != nil {
		return "", err
	}
	value := base64.RawURLEncoding.EncodeToString(valueBytes)
	http.SetCookie(w, h.cookies.searchSession(value))
	return hashedLimitKey(value), nil
}

func existingSearchSessionDigest(r *http.Request) (string, bool) {
	cookies := r.CookiesNamed(searchSessionCookieName)
	if len(cookies) != 1 || !validSearchSessionValue(cookies[0].Value) {
		return "", false
	}
	return hashedLimitKey(cookies[0].Value), true
}

func validSearchSessionValue(value string) bool {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(decoded) == searchSessionBytes &&
		base64.RawURLEncoding.EncodeToString(decoded) == value
}

func validRFC4122V4(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil && len(decoded) == 16 && decoded[6]>>4 == 4 && decoded[8]&0xc0 == 0x80
}

func newRandomSearchUUID(random io.Reader) (string, error) {
	if random == nil {
		return "", errInvalidTelemetryRecord
	}
	var value [16]byte
	if _, err := io.ReadFull(random, value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" +
		encoded[16:20] + "-" + encoded[20:32], nil
}

func (h *SearchHandler) logErrorType(operation string, err error) {
	if err != nil && h.logf != nil {
		h.logf("%s failed: %T", operation, err)
	}
}
