package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestListSiteFileDownloadSummariesHasAuthorizedScope(t *testing.T) {
	if !strings.Contains(listSiteFileDownloadSummariesQuery, "WHERE site_id = ANY($2::uuid[])") {
		t.Fatalf("download summary query is not site-scoped: %s", listSiteFileDownloadSummariesQuery)
	}
	got, err := ListSiteFileDownloadSummaries(context.Background(), nil, 7, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("empty authorized scope returned %#v", got)
	}
}

func TestListSiteAnalyticsQueriesBoundUTCWindow(t *testing.T) {
	queries := map[string]string{
		"global summaries": listSiteAnalyticsSummariesQuery,
		"scoped summaries": listSiteAnalyticsSummariesForSitesQuery,
		"global series":    listSiteAnalyticsDailySeriesQuery,
		"scoped series":    listSiteAnalyticsDailySeriesForSitesQuery,
	}
	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(query, "day >= timezone('UTC', now())::date - ($1::int - 1)") {
				t.Fatalf("analytics query does not use the inclusive UTC-day lower bound: %s", query)
			}
			if !strings.Contains(query, "day <= timezone('UTC', now())::date") {
				t.Fatalf("analytics query does not exclude future UTC days: %s", query)
			}
		})
	}
}

func TestListSiteAnalyticsForSitesHasAuthorizedScope(t *testing.T) {
	tests := []struct {
		name  string
		query string
		call  func() (int, error)
	}{
		{
			name:  "summaries",
			query: listSiteAnalyticsSummariesForSitesQuery,
			call: func() (int, error) {
				got, err := ListSiteAnalyticsSummariesForSites(context.Background(), nil, 180, nil)
				return len(got), err
			},
		},
		{
			name:  "daily series",
			query: listSiteAnalyticsDailySeriesForSitesQuery,
			call: func() (int, error) {
				got, err := ListSiteAnalyticsDailySeriesForSites(context.Background(), nil, 180, nil)
				return len(got), err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !strings.Contains(test.query, "day >= timezone('UTC', now())::date - ($1::int - 1)") {
				t.Fatalf("analytics query does not use the inclusive UTC-day window: %s", test.query)
			}
			if !strings.Contains(test.query, "site_id = ANY($2::uuid[])") {
				t.Fatalf("analytics query is not site-scoped: %s", test.query)
			}
			gotLen, err := test.call()
			if err != nil {
				t.Fatal(err)
			}
			if gotLen != 0 {
				t.Fatalf("empty authorized scope returned %d entries", gotLen)
			}
		})
	}
}

func TestDeleteSiteRequiresExactlyOneRow(t *testing.T) {
	tests := []struct {
		name      string
		result    sql.Result
		execErr   error
		wantErr   bool
		wantNoRow bool
	}{
		{name: "one", result: fixedResult{rows: 1}},
		{name: "none", result: fixedResult{rows: 0}, wantErr: true, wantNoRow: true},
		{name: "multiple", result: fixedResult{rows: 2}, wantErr: true},
		{name: "exec error", execErr: errors.New("exec failed"), wantErr: true},
		{name: "rows error", result: fixedResult{err: errors.New("rows failed")}, wantErr: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			querier := &deleteResultQuerier{result: test.result, err: test.execErr}
			err := DeleteSite(context.Background(), querier, "user-id", "site")
			if (err != nil) != test.wantErr {
				t.Fatalf("DeleteSite error = %v, wantErr %t", err, test.wantErr)
			}
			if errors.Is(err, sql.ErrNoRows) != test.wantNoRow {
				t.Fatalf("DeleteSite error = %v, ErrNoRows=%t", err, test.wantNoRow)
			}
			if len(querier.args) != 2 || querier.args[0] != "user-id" || querier.args[1] != "site" {
				t.Fatalf("DeleteSite args = %#v", querier.args)
			}
		})
	}
}

func TestMarkSiteUsesStateRequiresExactlyOneRow(t *testing.T) {
	tests := []struct {
		name      string
		result    sql.Result
		execErr   error
		wantErr   bool
		wantNoRow bool
	}{
		{name: "one", result: fixedResult{rows: 1}},
		{name: "none", result: fixedResult{rows: 0}, wantErr: true, wantNoRow: true},
		{name: "multiple", result: fixedResult{rows: 2}, wantErr: true},
		{name: "exec error", execErr: errors.New("exec failed"), wantErr: true},
		{name: "rows error", result: fixedResult{err: errors.New("rows failed")}, wantErr: true},
	}
	for _, versioned := range []bool{false, true} {
		variant := "simple"
		wantColumn := "uses_state"
		if versioned {
			variant = "versioned"
			wantColumn = "uses_versioned_state"
		}
		for _, test := range tests {
			test := test
			t.Run(variant+"/"+test.name, func(t *testing.T) {
				querier := &markResultQuerier{result: test.result, err: test.execErr}
				err := MarkSiteUsesState(context.Background(), querier, "immutable-site-id", versioned)
				if (err != nil) != test.wantErr {
					t.Fatalf("MarkSiteUsesState error = %v, wantErr %t", err, test.wantErr)
				}
				if errors.Is(err, sql.ErrNoRows) != test.wantNoRow {
					t.Fatalf("MarkSiteUsesState error = %v, ErrNoRows=%t", err, test.wantNoRow)
				}
				if test.execErr == nil {
					if !strings.Contains(querier.query, "SET "+wantColumn+" = true") {
						t.Fatalf("MarkSiteUsesState query does not update %s: %s", wantColumn, querier.query)
					}
					if !strings.Contains(querier.query, "WHERE id = $1") {
						t.Fatalf("MarkSiteUsesState query is not immutable-ID scoped: %s", querier.query)
					}
					if len(querier.args) != 1 || querier.args[0] != "immutable-site-id" {
						t.Fatalf("MarkSiteUsesState args = %#v", querier.args)
					}
				}
			})
		}
	}
}

type fixedResult struct {
	rows int64
	err  error
}

func (r fixedResult) LastInsertId() (int64, error) { return 0, errors.New("unsupported") }
func (r fixedResult) RowsAffected() (int64, error) { return r.rows, r.err }

type deleteResultQuerier struct {
	result sql.Result
	err    error
	args   []any
}

type markResultQuerier struct {
	result sql.Result
	err    error
	query  string
	args   []any
}

func (q *markResultQuerier) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	q.query = query
	q.args = args
	return q.result, q.err
}

func (q *markResultQuerier) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	panic("unexpected QueryContext")
}

func (q *markResultQuerier) QueryRowContext(context.Context, string, ...any) *sql.Row {
	panic("unexpected QueryRowContext")
}

func (q *deleteResultQuerier) ExecContext(_ context.Context, _ string, args ...any) (sql.Result, error) {
	q.args = args
	return q.result, q.err
}

func (q *deleteResultQuerier) QueryContext(context.Context, string, ...any) (*sql.Rows, error) {
	panic("unexpected QueryContext")
}

func (q *deleteResultQuerier) QueryRowContext(context.Context, string, ...any) *sql.Row {
	panic("unexpected QueryRowContext")
}

func TestListAllUsersLoadsKindAndMemberCount(t *testing.T) {
	for _, required := range []string{
		"u.kind",
		"LEFT JOIN team_members m ON m.team_id = u.id",
		"count(m.user_id)",
	} {
		if !strings.Contains(listAllUsersQuery, required) {
			t.Errorf("the user listing query is missing %q: %s", required, listAllUsersQuery)
		}
	}
}
