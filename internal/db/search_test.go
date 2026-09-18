package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSiteSearchMigrationShape(t *testing.T) {
	migration := readSiteSearchMigration(t)

	for _, required := range []string{
		"BEGIN;",
		"CREATE TABLE site_search_documents",
		"bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY",
		"tsvector GENERATED ALWAYS AS",
		"'english'::regconfig",
		"coalesce(owner_name, '')",
		"coalesce(site_name, '')",
		"coalesce(title, '')",
		"coalesce(description, '')",
		"coalesce(headings, '')",
		"coalesce(body_text, '')",
		"'A'",
		"'B'",
		"'D'",
		") STORED",
		"UNIQUE (site_id, version_number, page_path)",
		"USING GIN (search_vector)",
		"ON site_search_documents (site_id, version_number)",
		"CREATE TABLE site_search_queue",
		"CHECK (operation IN ('reconcile', 'delete'))",
		"site_search_queue_lease_pair_check",
		"CREATE TABLE site_search_index_status",
		"document_count    integer NOT NULL",
		"partial           boolean NOT NULL",
		"CREATE TABLE site_search_queries",
		"CREATE TABLE site_search_impressions",
		"CREATE TABLE site_search_clicks",
		"PRIMARY KEY (impression_id, session_digest)",
		"site_search_queries_created_at_idx",
		"site_search_impressions_created_at_idx",
		"site_search_clicks_created_at_idx",
		"INSERT INTO site_search_queue",
		"FROM sites",
		"ON CONFLICT (site_id) DO NOTHING",
		"COMMIT;",
	} {
		if !strings.Contains(migration, required) {
			t.Errorf("migration does not contain %q", required)
		}
	}

	queueDefinition := migrationTableDefinition(t, migration, "site_search_queue")
	if strings.Contains(strings.ToUpper(queueDefinition), "REFERENCES") {
		t.Fatalf("site_search_queue must not have a foreign key:\n%s", queueDefinition)
	}
	impressionDefinition := migrationTableDefinition(t, migration, "site_search_impressions")
	if strings.Contains(impressionDefinition, "REFERENCES sites") ||
		strings.Contains(impressionDefinition, "REFERENCES site_search_documents") {
		t.Fatalf("impressions must retain site/version/page snapshots without mutable-row foreign keys:\n%s", impressionDefinition)
	}
	for _, required := range []string{
		"query_id        uuid NOT NULL REFERENCES site_search_queries(id) ON DELETE CASCADE",
		"impression_id  uuid NOT NULL REFERENCES site_search_impressions(id) ON DELETE CASCADE",
	} {
		if !strings.Contains(migration, required) {
			t.Errorf("telemetry migration does not contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"CREATE EXTENSION",
		"gen_random_uuid",
		"uuid_generate",
		"DEFAULT random",
	} {
		if strings.Contains(strings.ToLower(migration), strings.ToLower(forbidden)) {
			t.Errorf("migration contains forbidden server-generated ID/extension shape %q", forbidden)
		}
	}
}

func TestSiteSearchQueueQueriesAreCoalescingAndFenced(t *testing.T) {
	for _, required := range []string{
		"WITH authoritative_site AS MATERIALIZED",
		"WHERE id = $1",
		"FOR UPDATE",
		"FROM authoritative_site",
		"ON CONFLICT (site_id) DO UPDATE",
		"generation = site_search_queue.generation + 1",
		"lease_token = NULL",
		"locked_until = NULL",
		"attempts = 0",
		"last_error = NULL",
	} {
		if !strings.Contains(enqueueSiteSearchQuery, required) {
			t.Errorf("enqueue query does not contain %q", required)
		}
	}

	for _, required := range []string{
		"status.version_number <> s.active_version",
		"status.extractor_version <> $1",
		"queued.site_id IS NULL",
		"ON CONFLICT (site_id) DO NOTHING",
	} {
		if !strings.Contains(enqueueMissingSiteSearchQuery, required) {
			t.Errorf("startup enqueue query does not contain %q", required)
		}
	}

	for _, required := range []string{
		"available_at <= now()",
		"locked_until IS NULL OR locked_until <= now()",
		"FOR UPDATE SKIP LOCKED",
		"LIMIT 1",
		"lease_token = $1",
		"locked_until = now() + ($2::bigint * interval '1 millisecond')",
		"attempts = queued.attempts + 1",
		"RETURNING",
	} {
		if !strings.Contains(claimSiteSearchQuery, required) {
			t.Errorf("claim query does not contain %q", required)
		}
	}

	for name, query := range map[string]string{
		"acknowledge": acknowledgeSiteSearchQuery,
		"retry":       retrySiteSearchQuery,
	} {
		for _, fence := range []string{
			"site_id = $1",
			"operation = $2",
			"generation = $3",
			"lease_token = $4",
		} {
			if !strings.Contains(query, fence) {
				t.Errorf("%s query does not contain fence %q", name, fence)
			}
		}
	}
	if strings.Contains(retrySiteSearchQuery, "attempts =") {
		t.Error("retry query resets or mutates attempts; attempts must increment only on claim")
	}
	for _, required := range []string{
		"available_at = $5",
		"last_error = $6",
		"lease_token = NULL",
		"locked_until = NULL",
	} {
		if !strings.Contains(retrySiteSearchQuery, required) {
			t.Errorf("retry query does not contain %q", required)
		}
	}
}

func TestSiteSearchPublicationQueriesLockAndReplaceAuthoritatively(t *testing.T) {
	if !strings.Contains(lockSiteSearchSiteQuery, "FOR UPDATE OF s") {
		t.Fatalf("site publication query does not lock site: %s", lockSiteSearchSiteQuery)
	}
	if !strings.Contains(lockSiteSearchQueueQuery, "FOR UPDATE") {
		t.Fatalf("site publication query does not lock queue: %s", lockSiteSearchQueueQuery)
	}
	if !strings.Contains(deleteSiteSearchDocumentsQuery, "WHERE site_id = $1") {
		t.Fatalf("document replacement is not site-scoped: %s", deleteSiteSearchDocumentsQuery)
	}
	for _, required := range []string{
		"ON CONFLICT (site_id) DO UPDATE",
		"document_count = EXCLUDED.document_count",
		"partial = EXCLUDED.partial",
		"indexed_at = EXCLUDED.indexed_at",
	} {
		if !strings.Contains(upsertSiteSearchStatusQuery, required) {
			t.Errorf("status query does not contain %q", required)
		}
	}
}

func TestEnqueueSiteSearchRequiresExactlyOneAffectedRow(t *testing.T) {
	for _, test := range []struct {
		name    string
		rows    int64
		rowsErr error
		wantErr bool
	}{
		{name: "one", rows: 1},
		{name: "none", rows: 0, wantErr: true},
		{name: "multiple", rows: 2, wantErr: true},
		{name: "rows error", rowsErr: errors.New("rows failed"), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			querier := &siteSearchExecQuerier{result: siteSearchResult{rows: test.rows, err: test.rowsErr}}
			err := EnqueueSiteSearch(context.Background(), querier, "site-id", SiteSearchReconcile)
			if (err != nil) != test.wantErr {
				t.Fatalf("EnqueueSiteSearch error = %v, wantErr %t", err, test.wantErr)
			}
			if test.rowsErr == nil {
				if len(querier.args) != 2 || querier.args[0] != "site-id" || querier.args[1] != "reconcile" {
					t.Fatalf("enqueue args = %#v", querier.args)
				}
			}
		})
	}
}

func TestSiteSearchFenceRowsDistinguishesStaleAndRejectsImpossibleCounts(t *testing.T) {
	for _, test := range []struct {
		name        string
		result      sql.Result
		wantOutcome SiteSearchFenceOutcome
		wantErr     bool
	}{
		{name: "stale", result: siteSearchResult{rows: 0}, wantOutcome: SiteSearchFenceStale},
		{name: "applied", result: siteSearchResult{rows: 1}, wantOutcome: SiteSearchFenceApplied},
		{name: "multiple", result: siteSearchResult{rows: 2}, wantOutcome: SiteSearchFenceUnknown, wantErr: true},
		{name: "rows error", result: siteSearchResult{err: errors.New("rows failed")}, wantOutcome: SiteSearchFenceUnknown, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome, err := siteSearchFenceRows(test.result, "test fence")
			if outcome != test.wantOutcome || (err != nil) != test.wantErr {
				t.Fatalf("siteSearchFenceRows = (%v, %v), want (%v, err=%t)", outcome, err, test.wantOutcome, test.wantErr)
			}
		})
	}
}

func TestBoundSiteSearchErrorIsUTF8SafeAndByteBounded(t *testing.T) {
	invalidAndLong := string([]byte{0xff, 0xfe}) + strings.Repeat("é", maxSiteSearchQueueErrorBytes)
	bounded := boundSiteSearchError(invalidAndLong)
	if !utf8.ValidString(bounded) {
		t.Fatalf("bounded error is invalid UTF-8: %q", bounded)
	}
	if len(bounded) > maxSiteSearchQueueErrorBytes {
		t.Fatalf("bounded error has %d bytes, max %d", len(bounded), maxSiteSearchQueueErrorBytes)
	}
	if got := boundSiteSearchError("short"); got != "short" {
		t.Fatalf("short error = %q", got)
	}
}

func readSiteSearchMigration(t *testing.T) string {
	t.Helper()
	contents, err := os.ReadFile("../migrate/sql/0011_add_site_search.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

func migrationTableDefinition(t *testing.T, migration, table string) string {
	t.Helper()
	start := strings.Index(migration, "CREATE TABLE "+table)
	if start < 0 {
		t.Fatalf("migration does not create %s", table)
	}
	remainder := migration[start:]
	end := strings.Index(remainder, "\n);")
	if end < 0 {
		t.Fatalf("migration has no end for %s", table)
	}
	return remainder[:end]
}

type siteSearchResult struct {
	rows int64
	err  error
}

func (r siteSearchResult) LastInsertId() (int64, error) { return 0, errors.New("unsupported") }
func (r siteSearchResult) RowsAffected() (int64, error) { return r.rows, r.err }

type siteSearchExecQuerier struct {
	result sql.Result
	err    error
	query  string
	args   []any
}

func (q *siteSearchExecQuerier) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	q.query = query
	q.args = args
	return q.result, q.err
}

func (q *siteSearchExecQuerier) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	panic("unexpected QueryContext")
}

func (q *siteSearchExecQuerier) QueryRowContext(context.Context, string, ...any) *sql.Row {
	panic("unexpected QueryRowContext")
}
