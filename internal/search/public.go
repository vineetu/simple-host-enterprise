package search

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	searchdb "github.com/vsriram/simple-host/internal/db"
)

const (
	DefaultPublicSearchLimit  = 12
	MaxPublicSearchLimit      = searchdb.MaxPublicSiteSearchLimit
	MaxPublicSearchQueryRunes = searchdb.MaxPublicSiteSearchQueryRunes
	MaxPublicSearchOffset     = searchdb.MaxPublicSiteSearchOffset
	PublicSnippetRuneLimit    = 240
	PublicSearchTimeout       = 2 * time.Second
)

// PublicSearchRequest is the backend-independent public retrieval contract.
// Limit zero selects DefaultPublicSearchLimit.
type PublicSearchRequest struct {
	Query  string
	Limit  int
	Offset int
}

// PublicDocument is a backend-neutral ranked page. SnippetSource is an
// untrusted plain-text fragment selected by the backend; it is never markup.
// A later OpenSearch backend can produce the same record without changing the
// service or its callers.
type PublicDocument struct {
	SiteID        string
	VersionNumber int
	Owner         string
	Site          string
	PagePath      string
	URL           string
	Title         string
	SnippetSource string
}

// PublicResult is the plain, presentation-neutral result returned by the
// service. Snippet never contains service-generated HTML markup.
type PublicResult struct {
	SiteID        string
	VersionNumber int
	Owner         string
	Site          string
	PagePath      string
	URL           string
	Title         string
	Snippet       string
	Position      int
}

// PublicBackend ranks public search documents. PostgreSQL implements this
// interface today; OpenSearch can replace it without changing PublicService.
type PublicBackend interface {
	Search(context.Context, PublicSearchRequest) ([]PublicDocument, error)
}

// PublicService validates and normalizes requests, delegates ranking, and
// bounds untrusted plain-text snippet fragments.
type PublicService struct {
	backend PublicBackend
}

func NewPublicService(backend PublicBackend) (*PublicService, error) {
	if backend == nil {
		return nil, errors.New("new public search service: nil backend")
	}
	return &PublicService{backend: backend}, nil
}

func (s *PublicService) Search(ctx context.Context, request PublicSearchRequest) ([]PublicResult, error) {
	if s == nil || s.backend == nil {
		return nil, errors.New("public search: nil service backend")
	}
	if ctx == nil {
		return nil, errors.New("public search: nil context")
	}

	request.Query = normalizePublicSearchText(request.Query)
	if request.Query == "" {
		return nil, errors.New("public search: empty query")
	}
	if utf8.RuneCountInString(request.Query) > MaxPublicSearchQueryRunes {
		return nil, fmt.Errorf("public search: query exceeds %d runes", MaxPublicSearchQueryRunes)
	}
	if request.Limit == 0 {
		request.Limit = DefaultPublicSearchLimit
	}
	if request.Limit < 1 || request.Limit > MaxPublicSearchLimit {
		return nil, fmt.Errorf("public search: limit %d is outside 1..%d", request.Limit, MaxPublicSearchLimit)
	}
	if request.Offset < 0 {
		return nil, fmt.Errorf("public search: negative offset %d", request.Offset)
	}
	if request.Offset > MaxPublicSearchOffset {
		return nil, fmt.Errorf("public search: offset %d exceeds %d", request.Offset, MaxPublicSearchOffset)
	}

	searchCtx, cancel := context.WithTimeout(ctx, PublicSearchTimeout)
	defer cancel()
	documents, err := s.backend.Search(searchCtx, request)
	if err != nil {
		return nil, fmt.Errorf("public search backend: %w", err)
	}
	if len(documents) > request.Limit {
		return nil, fmt.Errorf("public search backend returned %d documents for limit %d", len(documents), request.Limit)
	}

	results := make([]PublicResult, len(documents))
	for index, document := range documents {
		results[index] = PublicResult{
			SiteID:        document.SiteID,
			VersionNumber: document.VersionNumber,
			Owner:         document.Owner,
			Site:          document.Site,
			PagePath:      document.PagePath,
			URL:           document.URL,
			Title:         document.Title,
			Snippet:       PlainTextSnippet(document.SnippetSource),
			Position:      request.Offset + index + 1,
		}
	}
	return results, nil
}

// PostgreSQLPublicBackend adapts the current Postgres implementation to the
// replaceable public backend contract.
type PostgreSQLPublicBackend struct {
	database *sql.DB
}

func NewPostgreSQLPublicBackend(database *sql.DB) (*PostgreSQLPublicBackend, error) {
	if database == nil {
		return nil, errors.New("new PostgreSQL public search backend: nil database")
	}
	return &PostgreSQLPublicBackend{database: database}, nil
}

func (b *PostgreSQLPublicBackend) Search(ctx context.Context, request PublicSearchRequest) ([]PublicDocument, error) {
	if b == nil || b.database == nil {
		return nil, errors.New("PostgreSQL public search: nil database")
	}
	documents, err := searchdb.SearchPublicSites(ctx, b.database, request.Query, request.Limit, request.Offset)
	if err != nil {
		return nil, err
	}
	return publicDocumentsFromPostgreSQL(documents), nil
}

func publicDocumentsFromPostgreSQL(documents []searchdb.PublicSiteSearchDocument) []PublicDocument {
	converted := make([]PublicDocument, len(documents))
	for index, document := range documents {
		converted[index] = PublicDocument{
			SiteID:        document.SiteID,
			VersionNumber: document.VersionNumber,
			Owner:         document.OwnerName,
			Site:          document.SiteName,
			PagePath:      document.PagePath,
			URL:           document.URLPath,
			Title:         document.Title,
			SnippetSource: document.SnippetSource,
		}
	}
	return converted
}

var _ PublicBackend = (*PostgreSQLPublicBackend)(nil)

// PlainTextSnippet normalizes an untrusted backend fragment and enforces a
// UTF-8-safe hard cap. It adds only a Unicode ellipsis when truncated; callers
// must continue to treat the returned string as plain text.
func PlainTextSnippet(fragment string) string {
	fragment = normalizePublicSearchText(fragment)
	characters := []rune(fragment)
	if len(characters) <= PublicSnippetRuneLimit {
		return fragment
	}
	return string(characters[:PublicSnippetRuneLimit-1]) + "…"
}

func normalizePublicSearchText(value string) string {
	validUTF8 := strings.ToValidUTF8(value, "\uFFFD")
	return strings.Join(strings.Fields(validUTF8), " ")
}
