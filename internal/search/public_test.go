package search

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	searchdb "github.com/vsriram/simple-host/internal/db"
)

func TestPublicServiceNormalizesDelegatesAndBuildsPlainResults(t *testing.T) {
	backend := &fakePublicBackend{documents: []PublicDocument{{
		SiteID:        "11111111-1111-4111-8111-111111111111",
		VersionNumber: 7,
		Owner:         "alice",
		Site:          "planning",
		PagePath:      "roadmap/index.html",
		URL:           "https://alice.foo.example/planning/roadmap/",
		Title:         "Release roadmap",
		SnippetSource: " \tRelease\u00a0calendar\u2003and dates. ",
	}}}
	service, err := NewPublicService(backend)
	if err != nil {
		t.Fatal(err)
	}

	results, err := service.Search(context.Background(), PublicSearchRequest{
		Query:  "\t release\u00a0calendar\u2003 ",
		Offset: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantRequest := PublicSearchRequest{Query: "release calendar", Limit: DefaultPublicSearchLimit, Offset: 4}
	if backend.calls != 1 || backend.request != wantRequest {
		t.Fatalf("backend calls/request = %d / %+v, want 1 / %+v", backend.calls, backend.request, wantRequest)
	}
	deadline, hasDeadline := backend.ctx.Deadline()
	if !hasDeadline || time.Until(deadline) <= 0 || time.Until(deadline) > PublicSearchTimeout {
		t.Fatalf("backend deadline = %s, present=%t", deadline, hasDeadline)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	result := results[0]
	if result.SiteID != backend.documents[0].SiteID ||
		result.VersionNumber != 7 ||
		result.Owner != "alice" ||
		result.Site != "planning" ||
		result.PagePath != "roadmap/index.html" ||
		result.URL != "https://alice.foo.example/planning/roadmap/" ||
		result.Title != "Release roadmap" ||
		result.Position != 5 {
		t.Fatalf("result metadata = %+v", result)
	}
	if result.Snippet != "Release calendar and dates." {
		t.Fatalf("snippet = %q", result.Snippet)
	}
	if utf8.RuneCountInString(result.Snippet) > PublicSnippetRuneLimit {
		t.Fatalf("snippet has %d runes", utf8.RuneCountInString(result.Snippet))
	}
}

func TestPublicServiceValidatesBeforeCallingBackend(t *testing.T) {
	if _, err := NewPublicService(nil); err == nil {
		t.Fatal("nil backend unexpectedly succeeded")
	}
	var nilService *PublicService
	if _, err := nilService.Search(context.Background(), PublicSearchRequest{Query: "release", Limit: 1}); err == nil {
		t.Fatal("nil service unexpectedly succeeded")
	}

	for _, test := range []struct {
		name    string
		ctx     context.Context
		request PublicSearchRequest
	}{
		{name: "nil context", request: PublicSearchRequest{Query: "release", Limit: 1}},
		{name: "empty", ctx: context.Background(), request: PublicSearchRequest{Query: " \n\t", Limit: 1}},
		{name: "too long", ctx: context.Background(), request: PublicSearchRequest{Query: strings.Repeat("界", MaxPublicSearchQueryRunes+1), Limit: 1}},
		{name: "negative limit", ctx: context.Background(), request: PublicSearchRequest{Query: "release", Limit: -1}},
		{name: "large limit", ctx: context.Background(), request: PublicSearchRequest{Query: "release", Limit: MaxPublicSearchLimit + 1}},
		{name: "negative offset", ctx: context.Background(), request: PublicSearchRequest{Query: "release", Limit: 1, Offset: -1}},
		{name: "offset max plus one", ctx: context.Background(), request: PublicSearchRequest{Query: "release", Limit: 1, Offset: MaxPublicSearchOffset + 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakePublicBackend{}
			service, err := NewPublicService(backend)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Search(test.ctx, test.request); err == nil {
				t.Fatal("Search unexpectedly succeeded")
			}
			if backend.calls != 0 {
				t.Fatalf("backend calls = %d, want zero", backend.calls)
			}
		})
	}
}

func TestPublicServicePreservesBackendErrorsAndBoundsResults(t *testing.T) {
	backendFailure := errors.New("backend failed")
	service, err := NewPublicService(&fakePublicBackend{err: backendFailure})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Search(context.Background(), PublicSearchRequest{Query: "release", Limit: 1}); !errors.Is(err, backendFailure) {
		t.Fatalf("backend error = %v", err)
	}

	service, err = NewPublicService(&fakePublicBackend{documents: []PublicDocument{{}, {}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Search(context.Background(), PublicSearchRequest{Query: "release", Limit: 1}); err == nil {
		t.Fatal("over-limit backend result unexpectedly succeeded")
	}
}

func TestPlainTextSnippetIsNormalizedUTF8SafeAndPlain(t *testing.T) {
	invalid := string([]byte{0xff, 0xfe})
	snippet := PlainTextSnippet(invalid + "\tOwners\u00a0and\u2003milestones")
	if !utf8.ValidString(snippet) {
		t.Fatalf("snippet is invalid UTF-8: %q", snippet)
	}
	if got := utf8.RuneCountInString(snippet); got > PublicSnippetRuneLimit {
		t.Fatalf("snippet runes = %d, max %d", got, PublicSnippetRuneLimit)
	}
	if !strings.HasSuffix(snippet, "Owners and milestones") {
		t.Fatalf("snippet whitespace was not normalized: %q", snippet)
	}
	for _, markup := range []string{"<b>", "</b>", "<mark>", "</mark>"} {
		if strings.Contains(snippet, markup) {
			t.Fatalf("snippet contains generated markup %q: %q", markup, snippet)
		}
	}
}

func TestPlainTextSnippetTreatsFragmentAsUntrustedPlainText(t *testing.T) {
	for _, test := range []struct {
		name     string
		fragment string
		want     string
	}{
		{
			name:     "Unicode whitespace",
			fragment: "  Release\n\tplan\u00a0for\u2003teams  ",
			want:     "Release plan for teams",
		},
		{
			name:     "markup-like source remains ordinary text",
			fragment: "<b>Release</b> calendar",
			want:     "<b>Release</b> calendar",
		},
		{
			name: "empty",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := PlainTextSnippet(test.fragment); got != test.want {
				t.Fatalf("PlainTextSnippet = %q, want %q", got, test.want)
			}
		})
	}
}

func TestPublicSnippetHardCapUsesRunesNotBytes(t *testing.T) {
	snippet := PlainTextSnippet(strings.Repeat("界", 300))
	if got := utf8.RuneCountInString(snippet); got != PublicSnippetRuneLimit {
		t.Fatalf("snippet runes = %d, want %d", got, PublicSnippetRuneLimit)
	}
	if !utf8.ValidString(snippet) || !strings.HasSuffix(snippet, "…") {
		t.Fatalf("snippet is not a valid bounded prefix: %q", snippet)
	}
}

func TestPostgreSQLPublicDocumentAdapterIsPlainAndLossless(t *testing.T) {
	if _, err := NewPostgreSQLPublicBackend(nil); err == nil {
		t.Fatal("nil PostgreSQL database unexpectedly succeeded")
	}
	source := []searchdb.PublicSiteSearchDocument{{
		SiteID:        "site-id",
		VersionNumber: 4,
		OwnerName:     "alice",
		SiteName:      "planning",
		PagePath:      "index.html",
		URLPath:       "https://alice.foo.example/planning/",
		Title:         "Planning",
		SnippetSource: "Selected fragment",
	}}
	want := []PublicDocument{{
		SiteID:        "site-id",
		VersionNumber: 4,
		Owner:         "alice",
		Site:          "planning",
		PagePath:      "index.html",
		URL:           "https://alice.foo.example/planning/",
		Title:         "Planning",
		SnippetSource: "Selected fragment",
	}}
	if got := publicDocumentsFromPostgreSQL(source); !reflect.DeepEqual(got, want) {
		t.Fatalf("converted documents = %#v, want %#v", got, want)
	}
}

type fakePublicBackend struct {
	documents []PublicDocument
	err       error
	request   PublicSearchRequest
	ctx       context.Context
	calls     int
}

func (b *fakePublicBackend) Search(ctx context.Context, request PublicSearchRequest) ([]PublicDocument, error) {
	b.calls++
	b.ctx = ctx
	b.request = request
	return b.documents, b.err
}
