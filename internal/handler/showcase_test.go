package handler

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/db"
)

// newTestShowcaseHandler builds a ShowcaseHandler whose sessionUser always
// reports a fake signed-in caller (the showcase now lives
// behind the base session), so these tests can go on driving the same
// database/sql/driver fake they always have without also faking the
// sessions/users join a real session check would run.
func newTestShowcaseHandler(database *sql.DB, hosts HostModel) *ShowcaseHandler {
	h := NewShowcaseHandler(database, hosts, nil, 0)
	h.sessionUser = func(*http.Request) *db.User { return &db.User{ID: "test-viewer", Username: "test-viewer"} }
	return h
}

func TestShowcaseWithoutQueryPreservesPublicGallery(t *testing.T) {
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	script := &showcaseTestDBScript{
		query: func(query string, _ []driver.NamedValue) (driver.Rows, error) {
			switch {
			case strings.Contains(query, "FROM users"):
				return &showcaseTestRows{
					columns: []string{"id", "username", "is_admin", "created_at", "kind", "member_count", "disabled_at", "active_member_count"},
					values: [][]driver.Value{
						{"user-public", "alice", false, now, "person", int64(0), nil, int64(0)},
						{"user-private", "bob", false, now, "person", int64(0), nil, int64(0)},
					},
				}, nil
			case strings.Contains(query, "access = 'specific'"):
				return &showcaseTestRows{columns: []string{"id"}}, nil
			case strings.Contains(query, "FROM sites"):
				return &showcaseTestRows{
					columns: []string{"id", "user_id", "name", "active_version", "public", "uses_state", "uses_versioned_state", "created_at", "updated_at", "access"},
					values: [][]driver.Value{
						{"site-public", "user-public", "portfolio", int64(3), true, false, false, now, now, "listed"},
						{"site-private", "user-private", "secret", int64(4), false, false, false, now, now, "company"},
					},
				}, nil
			case strings.Contains(query, "FROM site_daily_analytics"):
				return &showcaseTestRows{
					columns: []string{"site_id", "today_pageviews", "today_visits", "last7_pageviews", "last7_visits", "last7_bot_pageviews", "last7_bot_visits"},
					values: [][]driver.Value{
						{"site-public", int64(12), int64(5), int64(30), int64(11), int64(7), int64(3)},
					},
				}, nil
			default:
				return nil, errors.New("unexpected showcase query")
			}
		},
	}
	handler := newTestShowcaseHandler(openShowcaseTestDB(t, script), testHostModel(t))
	response := httptest.NewRecorder()

	handler.page(response, httptest.NewRequest(http.MethodGet, "/showcase", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
	body := response.Body.String()
	for _, fragment := range []string{
		`<header class="site-header">`,
		`<form class="filter-form" action="/showcase" method="get">`,
		`<h1 id="showcase-filter-heading">Filter sites</h1>`,
		`name="q" maxlength="200" value=""`,
		`<div class="sites" id="showcase-sites">`,
		`<select id="showcase-sort" name="sort">`,
		// The owner's name credits them and links to their own page.
		`<div class="site-owner"><a href="https://alice.foo.example/" target="_blank" rel="noopener">alice</a></div>`,
		`data-rank-views="0"`,
		`data-filter-text="alice portfolio"`,
		`href="https://alice.foo.example/portfolio/"`,
		`<span class="chip">v3</span>`,
		`<b>12</b> views / <b>5</b> visits today <span>30 views 7d</span>`,
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("response does not contain %q", fragment)
		}
	}
	for _, privateValue := range []string{"bob", "secret", "v4", `name="type"`} {
		if strings.Contains(body, privateValue) {
			t.Errorf("private gallery value %q was rendered", privateValue)
		}
	}
	if got := script.queryCount(); got != 4 {
		t.Fatalf("database queries = %d, want 4", got)
	}
	if got := response.Header().Get("Cache-Control"); got == "no-store" {
		t.Fatalf("no-query Cache-Control = %q, want ordinary gallery caching behavior", got)
	}
}

func TestShowcaseQueryIsEscapedAndPreservesGallery(t *testing.T) {
	maliciousQuery := `"></script><script>window.showcasePwned = true</script>&"'`
	tests := []struct {
		name     string
		query    string
		rawQuery string
	}{
		{name: "non-empty malicious query", query: maliciousQuery},
		{name: "present empty query", query: ""},
		{name: "malformed query value", rawQuery: "q=%ZZ"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := showcaseGalleryTestScript()
			handler := newTestShowcaseHandler(openShowcaseTestDB(t, script), testHostModel(t))
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/showcase?q="+url.QueryEscape(test.query), nil)
			if test.rawQuery != "" {
				request = httptest.NewRequest(http.MethodGet, "/showcase", nil)
				request.URL.RawQuery = test.rawQuery
			}

			handler.page(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if got := response.Header().Get("Referrer-Policy"); got != "no-referrer" {
				t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
			}
			// Cache-Control: no-store is applied once, for every
			// authenticated base-host response, by SecurityHeaders
			// (security.go) — calling .page directly here bypasses that
			// middleware, so it is covered separately in security_test.go
			// instead.
			if got := script.queryCount(); got != 4 {
				t.Fatalf("database queries = %d, want 4", got)
			}
			body := response.Body.String()
			escapedValue := `value="` + html.EscapeString(test.query) + `"`
			if !strings.Contains(body, escapedValue) {
				t.Errorf("query input does not contain escaped value %q", escapedValue)
			}
			if strings.Contains(body, maliciousQuery) {
				t.Fatal("response contains the raw script-breaking query")
			}
			if !strings.Contains(body, `data-filter-text="alice portfolio"`) {
				t.Fatal("filter page does not contain the ordinary public gallery")
			}
			if strings.Contains(body, `id="search-results"`) {
				t.Fatal("filter page unexpectedly contains the old full-text result region")
			}
		})
	}
}

func TestShowcaseErrorsSetReferrerPolicy(t *testing.T) {
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		query func(string, []driver.NamedValue) (driver.Rows, error)
	}{
		{
			name: "user list error",
			query: func(string, []driver.NamedValue) (driver.Rows, error) {
				return nil, errors.New("user list failed")
			},
		},
		{
			name: "site list error",
			query: func(query string, _ []driver.NamedValue) (driver.Rows, error) {
				if strings.Contains(query, "FROM users") {
					return &showcaseTestRows{
						columns: []string{"id", "username", "is_admin", "created_at", "kind", "member_count", "disabled_at", "active_member_count"},
						values:  [][]driver.Value{{"user-public", "alice", false, now, "person", int64(0), nil, int64(0)}},
					}, nil
				}
				return nil, errors.New("site list failed")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newTestShowcaseHandler(openShowcaseTestDB(t, &showcaseTestDBScript{query: test.query}), testHostModel(t))
			response := httptest.NewRecorder()

			handler.page(response, httptest.NewRequest(http.MethodGet, "/showcase", nil))

			if response.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusInternalServerError)
			}
			if got := response.Header().Get("Referrer-Policy"); got != "no-referrer" {
				t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
			}
		})
	}
}

func TestShowcaseFilterScriptUsesSafeDOMAndNoNetwork(t *testing.T) {
	script := showcaseGalleryTestScript()
	handler := newTestShowcaseHandler(openShowcaseTestDB(t, script), testHostModel(t))
	response := httptest.NewRecorder()

	handler.page(response, httptest.NewRequest(http.MethodGet, "/showcase?q=release+calendar", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q, want no-referrer", got)
	}
	body := response.Body.String()
	for _, fragment := range []string{
		`data-filter-text="alice portfolio"`,
		`site.dataset.filterText.toLowerCase().indexOf(filterText) === -1`,
		`site.hidden = site.dataset.filterText`,
		`sortSel.addEventListener("change", function () { applySort(true); })`,
		`input.addEventListener("input", applyFilter)`,
		`event.preventDefault()`,
		`clear.addEventListener("click"`,
		`No sites match this filter.`,
		`message = "No sites match this filter."`,
		`input.value = ""`,
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("response does not contain %q", fragment)
		}
	}
	for _, forbidden := range []string{"innerHTML", "/api/search", "sendBeacon", "fetch(", `role="search"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("filter page contains forbidden %q", forbidden)
		}
	}
	if got := script.queryCount(); got != 4 {
		t.Fatalf("database queries = %d, want 4", got)
	}
}

func TestShowcaseFilterScriptBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the Showcase DOM behavior test")
	}

	script := strings.TrimSuffix(strings.TrimPrefix(showcaseFilterScript, "<script>"), "</script>")
	command := exec.Command(node, "testdata/showcase_filter_test.js")
	command.Env = append(os.Environ(), "SHOWCASE_FILTER_SCRIPT="+script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run Showcase filter behavior test: %v\n%s", err, output)
	}
}

func showcaseGalleryTestScript() *showcaseTestDBScript {
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	return &showcaseTestDBScript{
		query: func(query string, _ []driver.NamedValue) (driver.Rows, error) {
			switch {
			case strings.Contains(query, "FROM users"):
				return &showcaseTestRows{
					columns: []string{"id", "username", "is_admin", "created_at", "kind", "member_count", "disabled_at", "active_member_count"},
					values: [][]driver.Value{
						{"user-public", "alice", false, now, "person", int64(0), nil, int64(0)},
						{"user-private", "bob", false, now, "person", int64(0), nil, int64(0)},
					},
				}, nil
			case strings.Contains(query, "access = 'specific'"):
				return &showcaseTestRows{columns: []string{"id"}}, nil
			case strings.Contains(query, "FROM sites"):
				return &showcaseTestRows{
					columns: []string{"id", "user_id", "name", "active_version", "public", "uses_state", "uses_versioned_state", "created_at", "updated_at", "access"},
					values: [][]driver.Value{
						{"site-public", "user-public", "portfolio", int64(3), true, false, false, now, now, "listed"},
						{"site-private", "user-private", "secret", int64(4), false, false, false, now, now, "company"},
					},
				}, nil
			case strings.Contains(query, "FROM site_daily_analytics"):
				return &showcaseTestRows{
					columns: []string{"site_id", "today_pageviews", "today_visits", "last7_pageviews", "last7_visits", "last7_bot_pageviews", "last7_bot_visits"},
					values:  [][]driver.Value{{"site-public", int64(12), int64(5), int64(30), int64(11), int64(7), int64(3)}},
				}, nil
			default:
				return nil, errors.New("unexpected showcase query")
			}
		},
	}
}

type showcaseTestDBScript struct {
	mu      sync.Mutex
	queries int
	query   func(string, []driver.NamedValue) (driver.Rows, error)
}

func (s *showcaseTestDBScript) runQuery(query string, args []driver.NamedValue) (driver.Rows, error) {
	s.mu.Lock()
	s.queries++
	s.mu.Unlock()
	return s.query(query, args)
}

func (s *showcaseTestDBScript) queryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queries
}

type showcaseTestConnector struct {
	script *showcaseTestDBScript
}

func (c *showcaseTestConnector) Connect(context.Context) (driver.Conn, error) {
	return &showcaseTestConn{script: c.script}, nil
}

func (c *showcaseTestConnector) Driver() driver.Driver {
	return showcaseTestDriver{script: c.script}
}

type showcaseTestDriver struct {
	script *showcaseTestDBScript
}

func (d showcaseTestDriver) Open(string) (driver.Conn, error) {
	return &showcaseTestConn{script: d.script}, nil
}

type showcaseTestConn struct {
	script *showcaseTestDBScript
}

func (c *showcaseTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (c *showcaseTestConn) Close() error { return nil }

func (c *showcaseTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c *showcaseTestConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.script.runQuery(query, args)
}

type showcaseTestRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *showcaseTestRows) Columns() []string { return r.columns }
func (r *showcaseTestRows) Close() error      { return nil }

func (r *showcaseTestRows) Next(destination []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(destination, r.values[r.index])
	r.index++
	return nil
}

func openShowcaseTestDB(t *testing.T, script *showcaseTestDBScript) *sql.DB {
	t.Helper()
	database := sql.OpenDB(&showcaseTestConnector{script: script})
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	return database
}
