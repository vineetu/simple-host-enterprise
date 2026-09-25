package handler

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

const (
	siteDeleteTestOwnerID = "44444444-4444-4444-8444-444444444444"
	siteDeleteTestSiteID  = "55555555-5555-4555-8555-555555555555"
)

// siteDeleteAuditState is a minimal fake-driver backend for
// TestDeleteSiteCommitsAuditAfterSiteRowIsGone: just enough of
// api_keys/sites/site_search_queue/audit_events to drive
// deleteSiteForTarget (site.go) end to end through the real
// auth.Middleware and the real audit.DBRecorder. It follows the same
// "assert on the query shape, answer canned rows" pattern as
// collaboration_archive_integration_test.go's collaborationArchiveDBState,
// reusing that file's namedString/namedBytes/bytesEqual/numberedColumns
// helpers rather than redeclaring them.
type siteDeleteAuditState struct {
	mu           sync.Mutex
	siteDeleted  bool
	auditActions []string
	auditSiteIDs []string
	retired      []string
}

func (s *siteDeleteAuditState) query(query string, args []driver.NamedValue) (driver.Rows, error) {
	normalized := strings.Join(strings.Fields(query), " ")
	createdAt := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	switch {
	case strings.Contains(normalized, "FROM api_keys k") && strings.Contains(normalized, "WHERE k.key_hash = $1"):
		hash := namedBytes(args, 0)
		if !bytesEqual(hash, db.HashAPIKey("owner-key")) {
			return &siteDeleteAuditRows{columns: numberedColumns(8)}, nil
		}
		return &siteDeleteAuditRows{
			columns: numberedColumns(8),
			values:  [][]driver.Value{{siteDeleteTestOwnerID, "owner", false, createdAt, "person", "owner@example.test", nil, "owner-key-id"}},
		}, nil

	case strings.Contains(normalized, "FROM sites") && strings.Contains(normalized, "WHERE user_id = $1 AND name = $2"):
		// db.GetSite, called by deleteSiteForTarget after
		// LockSiteCollaboration and before db.DeleteSite.
		s.mu.Lock()
		deleted := s.siteDeleted
		s.mu.Unlock()
		if deleted {
			return &siteDeleteAuditRows{columns: numberedColumns(7)}, nil
		}
		return &siteDeleteAuditRows{
			columns: numberedColumns(7),
			values: [][]driver.Value{{
				siteDeleteTestSiteID, siteDeleteTestOwnerID, "demo", int64(1), true, createdAt, createdAt,
			}},
		}, nil

	default:
		return nil, errors.New("unexpected site-delete-audit test query: " + normalized)
	}
}

func (s *siteDeleteAuditState) exec(query string, args []driver.NamedValue) (driver.Result, error) {
	normalized := strings.Join(strings.Fields(query), " ")
	switch {
	case strings.Contains(normalized, "pg_advisory_xact_lock"):
		// db.LockSiteCollaboration.
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "INSERT INTO storage_retired"):
		s.mu.Lock()
		s.retired = append(s.retired, namedString(args, 0))
		s.mu.Unlock()
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "INSERT INTO site_search_queue"):
		// db.EnqueueSiteSearch.
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "DELETE FROM sites"):
		// db.DeleteSite. This runs BEFORE the audit_events insert below,
		// which is exactly the ordering that violates
		// audit_events_site_id_fkey against a real, pre-migration-0028
		// Postgres — a fake driver enforces no referential integrity, so it
		// stands in for the post-migration-0028 schema.
		s.mu.Lock()
		s.siteDeleted = true
		s.mu.Unlock()
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "INSERT INTO audit_events"):
		// db.InsertAuditEvent, via the real audit.DBRecorder (not a fake
		// audit.Recorder) so the actual SQL and argument binding run.
		// Positional args mirror internal/db/audit.go's InsertAuditEvent
		// query: $6 is action, $8 is site_id.
		s.mu.Lock()
		s.auditActions = append(s.auditActions, namedString(args, 5))
		s.auditSiteIDs = append(s.auditSiteIDs, namedString(args, 7))
		s.mu.Unlock()
		return driver.RowsAffected(1), nil
	default:
		return nil, errors.New("unexpected site-delete-audit test exec: " + normalized)
	}
}

type siteDeleteAuditConnector struct{ state *siteDeleteAuditState }

func (c *siteDeleteAuditConnector) Connect(context.Context) (driver.Conn, error) {
	return &siteDeleteAuditConn{state: c.state}, nil
}

func (c *siteDeleteAuditConnector) Driver() driver.Driver {
	return siteDeleteAuditDriver{state: c.state}
}

type siteDeleteAuditDriver struct{ state *siteDeleteAuditState }

func (d siteDeleteAuditDriver) Open(string) (driver.Conn, error) {
	return &siteDeleteAuditConn{state: d.state}, nil
}

type siteDeleteAuditConn struct{ state *siteDeleteAuditState }

func (c *siteDeleteAuditConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (c *siteDeleteAuditConn) Close() error { return nil }

func (c *siteDeleteAuditConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *siteDeleteAuditConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return siteDeleteAuditTx{}, nil
}

func (c *siteDeleteAuditConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.state.query(query, args)
}

func (c *siteDeleteAuditConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.state.exec(query, args)
}

type siteDeleteAuditTx struct{}

func (siteDeleteAuditTx) Commit() error   { return nil }
func (siteDeleteAuditTx) Rollback() error { return nil }

type siteDeleteAuditRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *siteDeleteAuditRows) Columns() []string { return r.columns }
func (r *siteDeleteAuditRows) Close() error      { return nil }

func (r *siteDeleteAuditRows) Next(destination []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(destination, r.values[r.index])
	r.index++
	return nil
}

// TestDeleteSiteCommitsAuditAfterSiteRowIsGone is the fake-driver
// regression test for the Phase 7 verification bug: deleteSiteForTarget
// (site.go) runs db.DeleteSite and then h.audit.RecordTx for site_delete
// in the same transaction, so by the time the audit INSERT runs, the site
// row its site_id names is already gone. Migration 0028 is the actual fix
// (it drops audit_events_site_id_fkey and its siblings — audit_events must
// outlive what it describes); a fake driver has no referential integrity
// to violate, so this test cannot reproduce the FK error itself. What it
// locks in is the code path around that fix: the delete-then-audit order,
// exercised against the real audit.DBRecorder so the actual INSERT INTO
// audit_events SQL and argument binding run, with the handler still
// reporting success end to end. internal/migrate's own live-Postgres
// coverage (MIGRATE_TEST_DSN-gated) is what proves the constraint is
// actually gone in a real database.
func TestDeleteSiteCommitsAuditAfterSiteRowIsGone(t *testing.T) {
	state := &siteDeleteAuditState{}
	database := sql.OpenDB(&siteDeleteAuditConnector{state: state})
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })

	store := newTestStore(t)
	store.publish(t, "owner", "demo", siteDeleteTestSiteID, 1, map[string]string{"index.html": "<h1>bye</h1>"})

	limits := testAbuseLimits(time.Now)
	siteHandler := NewSiteHandler(database, store.Store, "https://simple-host.example", newTestHostModel(t, "https://simple-host.example"), limits)
	siteHandler.WithAudit(audit.NewDBRecorder(database))
	mux := http.NewServeMux()
	identity := func(next http.Handler) http.Handler { return next }
	siteHandler.Register(mux, auth.Middleware(database, nil, 0), identity)

	request := httptest.NewRequest(http.MethodDelete, "/api/sites/demo", nil)
	request.Header.Set("X-API-Key", "owner-key")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code < 200 || response.Code >= 300 {
		t.Fatalf("DELETE /api/sites/demo = %d, want 2xx (body %q)", response.Code, response.Body.String())
	}

	state.mu.Lock()
	deleted := state.siteDeleted
	actions := append([]string(nil), state.auditActions...)
	siteIDs := append([]string(nil), state.auditSiteIDs...)
	retired := append([]string(nil), state.retired...)
	state.mu.Unlock()

	// The site's objects are queued for the sweep in the same transaction.
	if len(retired) != 1 || retired[0] != "sites/"+siteDeleteTestSiteID+"/" {
		t.Fatalf("retired = %v, want the site's prefix", retired)
	}

	if !deleted {
		t.Fatal("DELETE FROM sites was never executed")
	}
	if len(actions) != 1 || actions[0] != "site_delete" {
		t.Fatalf("audit actions = %v, want exactly one site_delete", actions)
	}
	if len(siteIDs) != 1 || siteIDs[0] != siteDeleteTestSiteID {
		t.Fatalf("audit site_id = %v, want exactly one %q (the already-deleted site's id)", siteIDs, siteDeleteTestSiteID)
	}
}
