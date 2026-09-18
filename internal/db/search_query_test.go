package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestPublicSiteSearchQueryShape(t *testing.T) {
	for _, required := range []string{
		"WITH parsed_input AS MATERIALIZED",
		"websearch_to_tsquery('english'::regconfig, $1::text)",
		"querytree(candidate.parsed_query) NOT IN ('', 'T')",
		"JOIN sites AS live_site",
		"live_site.id = document.site_id",
		"live_site.public = true",
		"live_site.active_version = document.version_number",
		"document.search_vector @@ parsed_input.parsed_query",
		"ts_rank_cd(",
		"ARRAY[0.05, 0.10, 0.40, 1.00]::real[]",
		"32",
		"lower(document.site_name) = parsed_input.exact_query",
		"THEN 0.05::real",
		"lower(document.title) = parsed_input.exact_query",
		"THEN 0.025::real",
		"document.id AS document_id",
		"row_number() OVER",
		"PARTITION BY site_id",
		"paged_sites AS MATERIALIZED",
		"WHERE site_page_number = 1",
		"lower(owner_name) COLLATE \"C\", owner_name COLLATE \"C\"",
		"lower(site_name) COLLATE \"C\", site_name COLLATE \"C\"",
		"lower(page_path) COLLATE \"C\", page_path COLLATE \"C\"",
		"LIMIT $2",
		"OFFSET $3",
		"JOIN site_search_documents AS document",
		"ON document.id = paged_sites.document_id",
		"ts_headline(",
		"NULLIF(document.description, '')",
		"NULLIF(document.headings, '')",
		"NULLIF(document.body_text, '')",
		`'StartSel="", StopSel="", MaxFragments=1, MinWords=12, MaxWords=48, ShortWord=0'`,
		"1024",
		") AS snippet_source",
		"paged_sites.rank DESC",
		"paged_sites.site_id",
	} {
		if !strings.Contains(publicSiteSearchQuery, required) {
			t.Errorf("public search query does not contain %q", required)
		}
	}

	guardAt := strings.Index(publicSiteSearchQuery, "querytree(candidate.parsed_query) NOT IN ('', 'T')")
	rankingScanAt := strings.Index(publicSiteSearchQuery, "FROM site_search_documents AS document")
	if guardAt < 0 || rankingScanAt < 0 || guardAt > rankingScanAt {
		t.Fatalf("non-indexable input is not guarded before the document scan:\n%s", publicSiteSearchQuery)
	}

	groupedAt := strings.Index(publicSiteSearchQuery, "WHERE site_page_number = 1")
	limitAt := strings.Index(publicSiteSearchQuery, "LIMIT $2")
	offsetAt := strings.Index(publicSiteSearchQuery, "OFFSET $3")
	finalSelectAt := strings.Index(publicSiteSearchQuery, "SELECT\n\t\tdocument.site_id::text")
	rejoinAt := strings.LastIndex(publicSiteSearchQuery, "JOIN site_search_documents AS document")
	if groupedAt < 0 || limitAt < groupedAt || offsetAt < limitAt || finalSelectAt < offsetAt || rejoinAt < finalSelectAt {
		t.Fatalf("grouping and pagination do not precede the full-document rejoin:\n%s", publicSiteSearchQuery)
	}
	for _, fullTextField := range []string{
		"NULLIF(document.description, '')",
		"NULLIF(document.headings, '')",
		"NULLIF(document.body_text, '')",
	} {
		if strings.Index(publicSiteSearchQuery, fullTextField) < limitAt {
			t.Errorf("full text field %q is carried before pagination", fullTextField)
		}
	}
	if strings.Contains(publicSiteSearchQuery, "ranked_pages.*") {
		t.Fatal("window CTE carries the entire ranked row")
	}
	if strings.Count(publicSiteSearchQuery, "lower(owner_name) COLLATE \"C\", owner_name COLLATE \"C\"") != 2 ||
		strings.Count(publicSiteSearchQuery, "lower(site_name) COLLATE \"C\", site_name COLLATE \"C\"") != 2 ||
		strings.Count(publicSiteSearchQuery, "lower(page_path) COLLATE \"C\", page_path COLLATE \"C\"") != 2 {
		t.Fatal("page selection and final pagination do not share deterministic text ties")
	}

	lowerQuery := strings.ToLower(publicSiteSearchQuery)
	for _, forbidden := range []string{
		"site_search_clicks",
		"pageview",
		"analytics",
		"indexed_at",
		"created_at",
		"<b>",
		"<mark>",
	} {
		if strings.Contains(lowerQuery, forbidden) {
			t.Errorf("public ranking query contains forbidden input %q", forbidden)
		}
	}
}

func TestSearchPublicSitesBindsAndScansPlainSnippetSource(t *testing.T) {
	script := &searchQueryDBScript{}
	script.query = func(query string, args []driver.NamedValue) (driver.Rows, error) {
		if query != publicSiteSearchQuery {
			t.Fatalf("query text changed unexpectedly:\n%s", query)
		}
		wantArgs := []any{"release calendar", int64(12), int64(4)}
		if got := namedValues(args); !reflect.DeepEqual(got, wantArgs) {
			t.Fatalf("query args = %#v, want %#v", got, wantArgs)
		}
		return &searchQueryRows{
			columns: []string{
				"site_id", "version_number", "owner_name", "site_name", "page_path",
				"url_path", "title", "snippet_source",
			},
			values: [][]driver.Value{{
				"11111111-1111-4111-8111-111111111111",
				int64(7),
				"alice",
				"planning",
				"roadmap/index.html",
				"/sites/alice/planning/roadmap/",
				"Release roadmap",
				"The complete release calendar.",
			}},
		}, nil
	}
	database := openSearchQueryTestDB(t, script)

	documents, err := SearchPublicSites(context.Background(), database, "\t release\u00a0calendar\u2003 ", 12, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := []PublicSiteSearchDocument{{
		SiteID:        "11111111-1111-4111-8111-111111111111",
		VersionNumber: 7,
		OwnerName:     "alice",
		SiteName:      "planning",
		PagePath:      "roadmap/index.html",
		URLPath:       "/sites/alice/planning/roadmap/",
		Title:         "Release roadmap",
		SnippetSource: "The complete release calendar.",
	}}
	if !reflect.DeepEqual(documents, want) {
		t.Fatalf("documents = %#v, want %#v", documents, want)
	}
}

func TestSearchPublicSitesRejectsUnboundedInputsBeforeQuery(t *testing.T) {
	querier := &siteSearchExecQuerier{}
	for _, test := range []struct {
		name   string
		query  string
		limit  int
		offset int
	}{
		{name: "empty", query: " \t\n", limit: 1},
		{name: "too long", query: strings.Repeat("界", MaxPublicSiteSearchQueryRunes+1), limit: 1},
		{name: "zero limit", query: "release", limit: 0},
		{name: "large limit", query: "release", limit: MaxPublicSiteSearchLimit + 1},
		{name: "negative offset", query: "release", limit: 1, offset: -1},
		{name: "offset max plus one", query: "release", limit: 1, offset: MaxPublicSiteSearchOffset + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := SearchPublicSites(context.Background(), querier, test.query, test.limit, test.offset); err == nil {
				t.Fatal("SearchPublicSites unexpectedly succeeded")
			}
		})
	}
}

func TestSearchPublicSitesLeavesNonIndexableQueriesToGuardedFTS(t *testing.T) {
	for _, query := range []string{"-release", "the and"} {
		t.Run(query, func(t *testing.T) {
			script := &searchQueryDBScript{}
			script.query = func(gotQuery string, args []driver.NamedValue) (driver.Rows, error) {
				if gotQuery != publicSiteSearchQuery {
					t.Fatalf("query text changed unexpectedly:\n%s", gotQuery)
				}
				if got := namedValues(args); !reflect.DeepEqual(got, []any{query, int64(1), int64(0)}) {
					t.Fatalf("query args = %#v", got)
				}
				return &searchQueryRows{columns: []string{
					"site_id", "version_number", "owner_name", "site_name", "page_path",
					"url_path", "title", "snippet_source",
				}}, nil
			}
			database := openSearchQueryTestDB(t, script)

			documents, err := SearchPublicSites(context.Background(), database, query, 1, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(documents) != 0 {
				t.Fatalf("documents = %#v, want none", documents)
			}
		})
	}
}

func TestNormalizePublicSiteSearchTextCollapsesUnicodeWhitespace(t *testing.T) {
	invalid := string([]byte{0xff})
	got := normalizePublicSiteSearchText("\t release\u00a0calendar\u2003milestones\n" + invalid + " ")
	if want := "release calendar milestones �"; got != want {
		t.Fatalf("normalized text = %q, want %q", got, want)
	}
}

func TestRecordSiteSearchTelemetryIsAtomicAndReturnsRFC4122V4IDs(t *testing.T) {
	script := &searchQueryDBScript{}
	script.exec = func(index int, query string, args []driver.NamedValue) (driver.Result, error) {
		switch index {
		case 0:
			if query != insertSiteSearchTelemetryQuery {
				t.Fatalf("query telemetry SQL:\n%s", query)
			}
			if got := namedValues(args); len(got) != 4 || got[1] != "release calendar milestones" || got[2] != int64(2) || got[3] != "session-digest" {
				t.Fatalf("query telemetry args = %#v", got)
			}
			assertRFC4122V4(t, namedValues(args)[0].(string))
			return driver.RowsAffected(1), nil
		case 1:
			if !strings.HasPrefix(query, insertSiteSearchTelemetryImpressionsPrefix) {
				t.Fatalf("impression telemetry SQL:\n%s", query)
			}
			got := namedValues(args)
			if len(got) != 12 {
				t.Fatalf("impression telemetry args = %#v", got)
			}
			if got[1] != got[7] || got[2] != int64(5) || got[8] != int64(6) {
				t.Fatalf("impression query IDs/positions = %#v", got)
			}
			assertRFC4122V4(t, got[0].(string))
			assertRFC4122V4(t, got[6].(string))
			return driver.RowsAffected(2), nil
		default:
			t.Fatalf("unexpected exec %d: %s", index, query)
			return nil, errors.New("unreachable")
		}
	}
	database := openSearchQueryTestDB(t, script)
	impressions := []SiteSearchTelemetryImpression{
		{ResultPosition: 5, SiteID: "11111111-1111-4111-8111-111111111111", VersionNumber: 7, PagePath: "roadmap/index.html"},
		{ResultPosition: 6, SiteID: "22222222-2222-4222-8222-222222222222", VersionNumber: 3, PagePath: "index.html"},
	}

	record, err := RecordSiteSearchTelemetry(
		context.Background(),
		database,
		"\t release\u00a0calendar\u2003milestones ",
		"session-digest",
		impressions,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertRFC4122V4(t, record.QueryID)
	if len(record.ImpressionIDs) != 2 || record.ImpressionIDs[0] == record.ImpressionIDs[1] {
		t.Fatalf("impression IDs = %#v", record.ImpressionIDs)
	}
	if got := script.eventSnapshot(); !reflect.DeepEqual(got, []string{"begin", "exec", "exec", "commit"}) {
		t.Fatalf("transaction events = %#v", got)
	}
}

func TestRecordSiteSearchTelemetryRollsBackAllRowsOnImpressionFailure(t *testing.T) {
	script := &searchQueryDBScript{}
	script.exec = func(index int, _ string, _ []driver.NamedValue) (driver.Result, error) {
		if index == 0 {
			return driver.RowsAffected(1), nil
		}
		return nil, errors.New("impression insert failed")
	}
	database := openSearchQueryTestDB(t, script)

	record, err := RecordSiteSearchTelemetry(
		context.Background(),
		database,
		"release",
		"session-digest",
		[]SiteSearchTelemetryImpression{{
			ResultPosition: 1,
			SiteID:         "11111111-1111-4111-8111-111111111111",
			VersionNumber:  1,
			PagePath:       "index.html",
		}},
	)
	if err == nil || record.QueryID != "" || record.ImpressionIDs != nil {
		t.Fatalf("RecordSiteSearchTelemetry = (%+v, %v)", record, err)
	}
	if got := script.eventSnapshot(); !reflect.DeepEqual(got, []string{"begin", "exec", "exec", "rollback"}) {
		t.Fatalf("transaction events = %#v", got)
	}
}

func TestSiteSearchClickIsSessionValidatedAndDeduplicated(t *testing.T) {
	for _, test := range []struct {
		name         string
		rows         int64
		rowsErr      error
		wantRecorded bool
		wantErr      bool
	}{
		{name: "inserted", rows: 1, wantRecorded: true},
		{name: "invalid duplicate or other session", rows: 0},
		{name: "impossible multiple", rows: 2, wantErr: true},
		{name: "rows error", rowsErr: errors.New("rows failed"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			querier := &siteSearchExecQuerier{result: siteSearchResult{rows: test.rows, err: test.rowsErr}}
			recorded, err := RecordSiteSearchClick(context.Background(), querier, "impression-id", "session-digest")
			if recorded != test.wantRecorded || (err != nil) != test.wantErr {
				t.Fatalf("RecordSiteSearchClick = (%t, %v)", recorded, err)
			}
			if querier.query != insertSiteSearchClickQuery || !reflect.DeepEqual(querier.args, []any{"impression-id", "session-digest"}) {
				t.Fatalf("click query/args = %q / %#v", querier.query, querier.args)
			}
		})
	}

	for _, required := range []string{
		"FROM site_search_impressions AS impression",
		"JOIN site_search_queries AS search_query",
		"search_query.id = impression.query_id",
		"impression.id = $1",
		"search_query.session_digest = $2",
		"ON CONFLICT (impression_id, session_digest) DO NOTHING",
	} {
		if !strings.Contains(insertSiteSearchClickQuery, required) {
			t.Errorf("click query does not contain %q", required)
		}
	}
}

func TestDeleteExpiredSiteSearchTelemetryIsAgeAndBatchBounded(t *testing.T) {
	querier := &siteSearchExecQuerier{result: siteSearchResult{rows: 25}}
	deleted, err := DeleteExpiredSiteSearchTelemetry(context.Background(), querier, 100)
	if err != nil || deleted != 25 {
		t.Fatalf("DeleteExpiredSiteSearchTelemetry = (%d, %v)", deleted, err)
	}
	if querier.query != deleteExpiredSiteSearchTelemetryQuery || !reflect.DeepEqual(querier.args, []any{100}) {
		t.Fatalf("retention query/args = %q / %#v", querier.query, querier.args)
	}
	for _, required := range []string{
		"created_at < now() - interval '180 days'",
		"ORDER BY created_at, id",
		"FOR UPDATE SKIP LOCKED",
		"LIMIT $1",
		"DELETE FROM site_search_queries",
	} {
		if !strings.Contains(deleteExpiredSiteSearchTelemetryQuery, required) {
			t.Errorf("retention query does not contain %q", required)
		}
	}
	for _, batchSize := range []int{0, -1, MaxSiteSearchTelemetryDeleteBatch + 1} {
		if _, err := DeleteExpiredSiteSearchTelemetry(context.Background(), querier, batchSize); err == nil {
			t.Fatalf("batch size %d unexpectedly succeeded", batchSize)
		}
	}
	querier.result = siteSearchResult{rows: 101}
	if _, err := DeleteExpiredSiteSearchTelemetry(context.Background(), querier, 100); err == nil {
		t.Fatal("impossible over-batch deletion unexpectedly succeeded")
	}
}

func TestSiteSearchTelemetryImpressionValidation(t *testing.T) {
	valid := SiteSearchTelemetryImpression{ResultPosition: 1, SiteID: "site-id", VersionNumber: 1, PagePath: "index.html"}
	for _, test := range []struct {
		name        string
		impressions []SiteSearchTelemetryImpression
		wantErr     bool
	}{
		{name: "empty"},
		{name: "valid", impressions: []SiteSearchTelemetryImpression{valid}},
		{name: "position", impressions: []SiteSearchTelemetryImpression{{SiteID: "site-id", VersionNumber: 1, PagePath: "index.html"}}, wantErr: true},
		{name: "duplicate position", impressions: []SiteSearchTelemetryImpression{valid, valid}, wantErr: true},
		{name: "site", impressions: []SiteSearchTelemetryImpression{{ResultPosition: 1, VersionNumber: 1, PagePath: "index.html"}}, wantErr: true},
		{name: "version", impressions: []SiteSearchTelemetryImpression{{ResultPosition: 1, SiteID: "site-id", PagePath: "index.html"}}, wantErr: true},
		{name: "path", impressions: []SiteSearchTelemetryImpression{{ResultPosition: 1, SiteID: "site-id", VersionNumber: 1}}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateSiteSearchTelemetryImpressions(test.impressions)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateSiteSearchTelemetryImpressions error = %v", err)
			}
		})
	}
}

func TestNewSiteSearchUUIDIsUTF8RFC4122V4AndUnique(t *testing.T) {
	first, err := newSiteSearchUUID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newSiteSearchUUID()
	if err != nil {
		t.Fatal(err)
	}
	assertRFC4122V4(t, first)
	assertRFC4122V4(t, second)
	if first == second {
		t.Fatalf("duplicate random UUID %q", first)
	}
	if !utf8.ValidString(first) || !utf8.ValidString(second) {
		t.Fatal("UUID output is not valid UTF-8")
	}
}

func assertRFC4122V4(t *testing.T, value string) {
	t.Helper()
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		t.Fatalf("UUID %q has the wrong shape", value)
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	if err != nil || len(decoded) != 16 {
		t.Fatalf("decode UUID %q: %v", value, err)
	}
	if decoded[6]>>4 != 4 {
		t.Fatalf("UUID %q version = %d, want 4", value, decoded[6]>>4)
	}
	if decoded[8]&0xc0 != 0x80 {
		t.Fatalf("UUID %q variant bits = %#x, want RFC 4122", value, decoded[8]&0xc0)
	}
}

type searchQueryDBScript struct {
	mu        sync.Mutex
	events    []string
	execCount int
	exec      func(int, string, []driver.NamedValue) (driver.Result, error)
	query     func(string, []driver.NamedValue) (driver.Rows, error)
}

func (s *searchQueryDBScript) addEvent(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

func (s *searchQueryDBScript) execute(query string, args []driver.NamedValue) (driver.Result, error) {
	s.mu.Lock()
	index := s.execCount
	s.execCount++
	s.events = append(s.events, "exec")
	s.mu.Unlock()
	if s.exec == nil {
		return nil, errors.New("unexpected exec")
	}
	return s.exec(index, query, args)
}

func (s *searchQueryDBScript) eventSnapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

type searchQueryConnector struct {
	script *searchQueryDBScript
}

func (c *searchQueryConnector) Connect(context.Context) (driver.Conn, error) {
	return &searchQueryConn{script: c.script}, nil
}

func (c *searchQueryConnector) Driver() driver.Driver {
	return searchQueryDriver{script: c.script}
}

type searchQueryDriver struct {
	script *searchQueryDBScript
}

func (d searchQueryDriver) Open(string) (driver.Conn, error) {
	return &searchQueryConn{script: d.script}, nil
}

type searchQueryConn struct {
	script *searchQueryDBScript
}

func (c *searchQueryConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (c *searchQueryConn) Close() error { return nil }

func (c *searchQueryConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *searchQueryConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.script.addEvent("begin")
	return &searchQueryTx{script: c.script}, nil
}

func (c *searchQueryConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.script.execute(query, args)
}

func (c *searchQueryConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.script.addEvent("query")
	if c.script.query == nil {
		return nil, errors.New("unexpected query")
	}
	return c.script.query(query, args)
}

type searchQueryTx struct {
	script *searchQueryDBScript
}

func (tx *searchQueryTx) Commit() error {
	tx.script.addEvent("commit")
	return nil
}

func (tx *searchQueryTx) Rollback() error {
	tx.script.addEvent("rollback")
	return nil
}

type searchQueryRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *searchQueryRows) Columns() []string { return r.columns }
func (r *searchQueryRows) Close() error      { return nil }

func (r *searchQueryRows) Next(destination []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(destination, r.values[r.index])
	r.index++
	return nil
}

func openSearchQueryTestDB(t *testing.T, script *searchQueryDBScript) *sql.DB {
	t.Helper()
	database := sql.OpenDB(&searchQueryConnector{script: script})
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { database.Close() })
	return database
}

func namedValues(values []driver.NamedValue) []any {
	plain := make([]any, len(values))
	for index, value := range values {
		plain[index] = value.Value
	}
	return plain
}
