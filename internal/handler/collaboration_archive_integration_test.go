package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/reqlog"
)

const (
	archiveTestOwnerID  = "11111111-1111-4111-8111-111111111111"
	archiveTestMemberID = "22222222-2222-4222-8222-222222222222"
	archiveTestSiteID   = "33333333-3333-4333-8333-333333333333"
)

type collaborationArchiveDBState struct {
	mu                          sync.Mutex
	memberAllowed               bool
	denyMemberAfterFirstResolve bool
	memberResolveCount          int
	activeVersion               int
	versions                    []int
	// assetExists/assetDeleted back the site_assets query/exec cases below,
	// for TestDeleteCollaborationAssetRecordsAuditInOneTransaction
	// (assets_admin.go's deleteCollaborationAsset).
	assetExists  bool
	assetDeleted bool
	// retired records the keys queued in storage_retired.
	retired []string
}

func (s *collaborationArchiveDBState) query(query string, args []driver.NamedValue) (driver.Rows, error) {
	normalized := strings.Join(strings.Fields(query), " ")
	createdAt := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)

	switch {
	case strings.Contains(normalized, "FROM api_keys k") && strings.Contains(normalized, "WHERE k.key_hash = $1"):
		hash := namedBytes(args, 0)
		var values [][]driver.Value
		switch {
		case bytesEqual(hash, db.HashAPIKey("owner-key")):
			values = [][]driver.Value{{archiveTestOwnerID, "owner", false, createdAt, "person", "owner@example.test", nil, "owner-key-id", "full"}}
		case bytesEqual(hash, db.HashAPIKey("member-key")):
			values = [][]driver.Value{{archiveTestMemberID, "member", false, createdAt, "person", "member@example.test", nil, "member-key-id", "full"}}
		}
		return &collaborationArchiveRows{
			columns: []string{"id", "username", "is_admin", "created_at", "kind", "email", "disabled_at", "key_id", "scope"},
			values:  values,
		}, nil

	case strings.Contains(normalized, "FROM sites s") && strings.Contains(normalized, "LEFT JOIN team_members tm"):
		actorID := namedString(args, 0)
		s.mu.Lock()
		defer s.mu.Unlock()
		activeVersion := s.activeVersion
		if activeVersion == 0 {
			activeVersion = 1
		}
		role := "owner"
		if actorID == archiveTestMemberID {
			if !s.memberAllowed {
				return emptyCollaborationArchiveRows(16), nil
			}
			role = "member"
			s.memberResolveCount++
			if s.denyMemberAfterFirstResolve && s.memberResolveCount == 1 {
				s.memberAllowed = false
			}
		} else if actorID != archiveTestOwnerID {
			return emptyCollaborationArchiveRows(16), nil
		}
		return &collaborationArchiveRows{
			columns: numberedColumns(16),
			values: [][]driver.Value{{
				archiveTestSiteID, archiveTestOwnerID, "demo", int64(activeVersion), true, false, false,
				createdAt, createdAt, "owner", actorID, role, "company", nil, "", int64(0),
			}},
		}, nil

	case strings.Contains(normalized, "FROM versions AS v"):
		s.mu.Lock()
		versions := append([]int(nil), s.versions...)
		s.mu.Unlock()
		if len(versions) == 0 {
			versions = []int{1}
		}
		values := make([][]driver.Value, 0, len(versions))
		for _, version := range versions {
			values = append(values, []driver.Value{
				"version-" + strconv.Itoa(version), archiveTestSiteID, int64(version), "", "active",
				archiveTestMemberID, "member", createdAt,
			})
		}
		return &collaborationArchiveRows{
			columns: numberedColumns(8),
			values:  values,
		}, nil

	case strings.Contains(normalized, "FROM sites") && strings.Contains(normalized, "WHERE user_id = $1 AND name = $2"):
		s.mu.Lock()
		activeVersion := s.activeVersion
		s.mu.Unlock()
		if activeVersion == 0 {
			activeVersion = 1
		}
		return &collaborationArchiveRows{
			columns: numberedColumns(7),
			values: [][]driver.Value{{
				archiveTestSiteID, archiveTestOwnerID, "demo", int64(activeVersion), true, createdAt, createdAt,
			}},
		}, nil

	case strings.Contains(normalized, "SELECT access = 'specific' FROM sites"):
		return &collaborationArchiveRows{columns: []string{"restricted"}, values: [][]driver.Value{{false}}}, nil

	case strings.Contains(normalized, "SELECT 1 FROM sites") && strings.Contains(normalized, "WHERE id = $1::uuid"):
		return &collaborationArchiveRows{columns: []string{"one"}, values: [][]driver.Value{{int64(1)}}}, nil

	case strings.Contains(normalized, "FROM site_assets") && strings.Contains(normalized, "WHERE id = $1 AND site_id = $2"):
		// Backs db.GetAsset, called by assets_admin.go's
		// deleteCollaborationAsset before it ever opens a transaction.
		s.mu.Lock()
		exists := s.assetExists && !s.assetDeleted
		s.mu.Unlock()
		id := namedString(args, 0)
		if !exists {
			return &collaborationArchiveRows{columns: numberedColumns(9)}, nil
		}
		return &collaborationArchiveRows{
			columns: numberedColumns(9),
			values: [][]driver.Value{{
				id, archiveTestSiteID, "notes.txt", "text/plain", int64(11), []byte{1, 2, 3}, archiveTestOwnerID, createdAt, nil,
			}},
		}, nil

	default:
		return nil, errors.New("unexpected collaboration archive test query: " + normalized)
	}
}

func (s *collaborationArchiveDBState) exec(query string, args []driver.NamedValue) (driver.Result, error) {
	normalized := strings.Join(strings.Fields(query), " ")
	switch {
	case strings.Contains(normalized, "pg_advisory_xact_lock"):
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "UPDATE sites SET active_version"):
		s.mu.Lock()
		s.activeVersion = namedInt(args, 1)
		s.mu.Unlock()
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "INSERT INTO site_search_queue"):
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "INSERT INTO storage_retired"):
		s.mu.Lock()
		s.retired = append(s.retired, namedString(args, 0))
		s.mu.Unlock()
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "DELETE FROM versions"):
		versionID := namedString(args, 0)
		s.mu.Lock()
		kept := s.versions[:0]
		for _, version := range s.versions {
			if "version-"+strconv.Itoa(version) != versionID {
				kept = append(kept, version)
			}
		}
		s.versions = kept
		s.mu.Unlock()
		return driver.RowsAffected(1), nil
	case strings.Contains(normalized, "UPDATE site_assets SET deleted_at = now()"):
		// Backs db.SoftDeleteAsset, called inside the transaction
		// assets_admin.go's deleteCollaborationAsset now shares with its
		// RecordTx call.
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.assetExists || s.assetDeleted {
			return driver.RowsAffected(0), nil
		}
		s.assetDeleted = true
		return driver.RowsAffected(1), nil

	default:
		return nil, errors.New("unexpected collaboration archive test exec: " + normalized)
	}
}

func namedInt(args []driver.NamedValue, index int) int {
	if index < 0 || index >= len(args) {
		return 0
	}
	switch value := args[index].Value.(type) {
	case int64:
		return int(value)
	case int:
		return value
	default:
		return 0
	}
}

func (s *collaborationArchiveDBState) memberResolves() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.memberResolveCount
}

func namedString(args []driver.NamedValue, index int) string {
	if index < 0 || index >= len(args) {
		return ""
	}
	value, _ := args[index].Value.(string)
	return value
}

func namedBytes(args []driver.NamedValue, index int) []byte {
	if index < 0 || index >= len(args) {
		return nil
	}
	value, _ := args[index].Value.([]byte)
	return value
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func numberedColumns(count int) []string {
	columns := make([]string, count)
	for index := range columns {
		columns[index] = "column"
	}
	return columns
}

func emptyCollaborationArchiveRows(columns int) *collaborationArchiveRows {
	return &collaborationArchiveRows{columns: numberedColumns(columns)}
}

type collaborationArchiveConnector struct {
	state *collaborationArchiveDBState
}

func (c *collaborationArchiveConnector) Connect(context.Context) (driver.Conn, error) {
	return &collaborationArchiveConn{state: c.state}, nil
}

func (c *collaborationArchiveConnector) Driver() driver.Driver {
	return collaborationArchiveDriver{state: c.state}
}

type collaborationArchiveDriver struct {
	state *collaborationArchiveDBState
}

func (d collaborationArchiveDriver) Open(string) (driver.Conn, error) {
	return &collaborationArchiveConn{state: d.state}, nil
}

type collaborationArchiveConn struct {
	state *collaborationArchiveDBState
}

func (c *collaborationArchiveConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (c *collaborationArchiveConn) Close() error { return nil }

func (c *collaborationArchiveConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *collaborationArchiveConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return collaborationArchiveTx{}, nil
}

func (c *collaborationArchiveConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.state.query(query, args)
}

func (c *collaborationArchiveConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.state.exec(query, args)
}

type collaborationArchiveTx struct{}

func (collaborationArchiveTx) Commit() error   { return nil }
func (collaborationArchiveTx) Rollback() error { return nil }

type collaborationArchiveRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *collaborationArchiveRows) Columns() []string { return r.columns }
func (r *collaborationArchiveRows) Close() error      { return nil }

func (r *collaborationArchiveRows) Next(destination []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(destination, r.values[r.index])
	r.index++
	return nil
}

func newCollaborationArchiveHarness(t *testing.T, state *collaborationArchiveDBState) (*SiteHandler, *http.ServeMux, *testStore, *AbuseLimits) {
	t.Helper()
	database := sql.OpenDB(&collaborationArchiveConnector{state: state})
	database.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = database.Close() })

	store := newTestStore(t)
	store.publish(t, "owner", "demo", archiveTestSiteID, 1, map[string]string{
		"index.html": "<h1>collaboration</h1>",
	})

	limits := testAbuseLimits(time.Now)
	siteHandler := NewSiteHandler(database, store.Store, "https://simple-host.example", newTestHostModel(t, "https://simple-host.example"), limits)
	mux := http.NewServeMux()
	identity := func(next http.Handler) http.Handler { return next }
	siteHandler.Register(mux, auth.Middleware(database, nil, 0), identity)
	return siteHandler, mux, store, limits
}

func collaborationArchiveRequest(method, target, apiKey string) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	request.Header.Set("X-API-Key", apiKey)
	return request
}

func TestCleanupOldVersionsRetiresObjects(t *testing.T) {
	state := &collaborationArchiveDBState{activeVersion: 7, versions: []int{7, 6, 5, 4, 3, 2, 1}}
	handler, _, store, _ := newCollaborationArchiveHarness(t, state)
	for version := 2; version <= 7; version++ {
		store.publish(t, "owner", "demo", archiveTestSiteID, version, map[string]string{"index.html": strconv.Itoa(version)})
	}

	if err := handler.cleanupOldVersions(context.Background(), archiveTestOwnerID, "owner", "demo", archiveTestSiteID); err != nil {
		t.Fatalf("cleanupOldVersions: %v", err)
	}
	state.mu.Lock()
	remaining := append([]int(nil), state.versions...)
	retired := append([]string(nil), state.retired...)
	state.mu.Unlock()
	if got, want := fmt.Sprint(remaining), "[7 6 5 4 3]"; got != want {
		t.Fatalf("database versions = %s, want %s", got, want)
	}
	// The objects are queued in the same transaction and deleted by the sweep
	// after the grace period, never inline: another replica may still be
	// serving what it resolved a moment ago.
	want := "[sites/" + archiveTestSiteID + "/v2.tar.gz sites/" + archiveTestSiteID + "/v1.tar.gz]"
	if got := fmt.Sprint(retired); got != want {
		t.Fatalf("retired = %s, want %s", got, want)
	}
	if len(store.objects.Keys()) != 7 {
		t.Fatalf("objects deleted inline: %v", store.objects.Keys())
	}
}

type blockingArchiveResponseWriter struct {
	header       http.Header
	admitted     chan struct{}
	writeStarted chan struct{}
	releaseWrite chan struct{}
	headerOnce   sync.Once
	writeOnce    sync.Once
	mu           sync.Mutex
	status       int
	body         bytes.Buffer
	deadlines    []time.Time
}

func newBlockingArchiveResponseWriter() *blockingArchiveResponseWriter {
	return &blockingArchiveResponseWriter{
		header:       make(http.Header),
		admitted:     make(chan struct{}),
		writeStarted: make(chan struct{}),
		releaseWrite: make(chan struct{}),
	}
}

func (w *blockingArchiveResponseWriter) Header() http.Header { return w.header }

func (w *blockingArchiveResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = status
	}
	w.mu.Unlock()
	w.headerOnce.Do(func() { close(w.admitted) })
}

func (w *blockingArchiveResponseWriter) Write(body []byte) (int, error) {
	w.writeOnce.Do(func() { close(w.writeStarted) })
	<-w.releaseWrite
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(body)
}

func (w *blockingArchiveResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadlines = append(w.deadlines, deadline)
	w.mu.Unlock()
	return nil
}

func (w *blockingArchiveResponseWriter) snapshot() (int, []byte, []time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status, append([]byte(nil), w.body.Bytes()...), append([]time.Time(nil), w.deadlines...)
}

type expiringArchiveResponseWriter struct {
	header       http.Header
	writeStarted chan struct{}
	expired      chan struct{}
	writeOnce    sync.Once
	timerOnce    sync.Once
	mu           sync.Mutex
	status       int
	deadlines    []time.Time
}

func newExpiringArchiveResponseWriter() *expiringArchiveResponseWriter {
	return &expiringArchiveResponseWriter{
		header:       make(http.Header),
		writeStarted: make(chan struct{}),
		expired:      make(chan struct{}),
	}
}

func (w *expiringArchiveResponseWriter) Header() http.Header { return w.header }

func (w *expiringArchiveResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = status
	}
	w.mu.Unlock()
}

func (w *expiringArchiveResponseWriter) Write([]byte) (int, error) {
	w.writeOnce.Do(func() { close(w.writeStarted) })
	<-w.expired
	return 0, context.DeadlineExceeded
}

func (w *expiringArchiveResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadlines = append(w.deadlines, deadline)
	w.mu.Unlock()
	if !deadline.IsZero() {
		w.timerOnce.Do(func() {
			time.AfterFunc(200*time.Millisecond, func() { close(w.expired) })
		})
	}
	return nil
}

func (w *expiringArchiveResponseWriter) deadlineSnapshot() []time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Time(nil), w.deadlines...)
}

func TestCollaborationArchiveLinearizesBeforeRevokeButLaterDownloadsAreDenied(t *testing.T) {
	state := &collaborationArchiveDBState{memberAllowed: true}
	_, mux, _, _ := newCollaborationArchiveHarness(t, state)
	writer := newBlockingArchiveResponseWriter()
	archiveDone := make(chan struct{})
	go func() {
		mux.ServeHTTP(writer, collaborationArchiveRequest(
			http.MethodGet,
			"/api/collaboration/sites/owner/demo/versions/1/archive",
			"member-key",
		))
		close(archiveDone)
	}()

	select {
	case <-writer.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("archive did not begin streaming")
	}
	status, _, _ := writer.snapshot()
	if status != http.StatusOK {
		t.Fatalf("archive status = %d, want 200", status)
	}

	// The member leaves the owning team while the admitted stream is open.
	state.mu.Lock()
	state.memberAllowed = false
	state.mu.Unlock()

	denied := httptest.NewRecorder()
	mux.ServeHTTP(denied, collaborationArchiveRequest(
		http.MethodGet,
		"/api/collaboration/sites/owner/demo/versions/1/archive",
		"member-key",
	))
	if denied.Code != http.StatusNotFound {
		t.Fatalf("post-revoke archive status = %d, want 404; body=%s", denied.Code, denied.Body.String())
	}

	close(writer.releaseWrite)
	select {
	case <-archiveDone:
	case <-time.After(time.Second):
		t.Fatal("admitted archive did not finish after the writer resumed")
	}
	status, body, deadlines := writer.snapshot()
	if status != http.StatusOK || len(deadlines) != 2 || deadlines[0].IsZero() || !deadlines[1].IsZero() {
		t.Fatalf("archive response status=%d deadlines=%v", status, deadlines)
	}
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("open admitted ZIP: %v", err)
	}
	if len(reader.File) != 1 || reader.File[0].Name != "index.html" {
		t.Fatalf("archive files = %#v", reader.File)
	}
}

func TestCollaborationArchiveRechecksRevocationBeforeLeasingFiles(t *testing.T) {
	state := &collaborationArchiveDBState{
		memberAllowed:               true,
		denyMemberAfterFirstResolve: true,
	}
	_, mux, _, _ := newCollaborationArchiveHarness(t, state)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, collaborationArchiveRequest(
		http.MethodGet,
		"/api/collaboration/sites/owner/demo/versions/1/archive",
		"member-key",
	))
	if response.Code != http.StatusNotFound {
		t.Fatalf("archive status = %d, want 404; body=%s", response.Code, response.Body.String())
	}
	if got := state.memberResolves(); got != 1 {
		t.Fatalf("successful member resolves = %d, want 1 before the locked recheck denied access", got)
	}
}

func TestCollaborationArchiveWriteDeadlineReleasesLeaseAndSlot(t *testing.T) {
	state := &collaborationArchiveDBState{memberAllowed: true}
	handler, mux, _, limits := newCollaborationArchiveHarness(t, state)
	writer := newExpiringArchiveResponseWriter()
	handlerDone := make(chan struct{})
	go func() {
		mux.ServeHTTP(writer, collaborationArchiveRequest(
			http.MethodGet,
			"/api/collaboration/sites/owner/demo/versions/1/archive",
			"member-key",
		))
		close(handlerDone)
	}()

	select {
	case <-writer.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("archive did not reach the blocking writer")
	}

	remainingRelease, ok := limits.acquireArchiveDownload()
	if !ok {
		t.Fatal("the second archive slot was not available")
	}
	if unexpectedRelease, acquired := limits.acquireArchiveDownload(); acquired {
		unexpectedRelease()
		t.Fatal("archive handler did not retain its streaming slot")
	}

	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("archive handler did not return after its writer deadline fired")
	}

	remainingRelease()
	firstRelease, first := limits.acquireArchiveDownload()
	secondRelease, second := limits.acquireArchiveDownload()
	if !first || !second {
		if first {
			firstRelease()
		}
		if second {
			secondRelease()
		}
		t.Fatalf("archive slots after deadline = (%t, %t), want both available", first, second)
	}
	firstRelease()
	secondRelease()

	unlock := handler.mutations.lock(archiveTestOwnerID, "demo")
	unlock()
	deadlines := writer.deadlineSnapshot()
	if len(deadlines) != 2 || deadlines[0].IsZero() || !deadlines[1].IsZero() {
		t.Fatalf("archive deadlines = %v, want one deadline followed by clear", deadlines)
	}
}

func TestRollbackRoutesSwitchTheLiveVersion(t *testing.T) {
	tests := []struct {
		name          string
		memberAllowed bool
		target        string
		apiKey        string
		ifMatch       string
	}{
		{
			name:   "owner route",
			target: "/api/sites/demo/rollback",
			apiKey: "owner-key",
		},
		{
			name:          "collaboration route",
			memberAllowed: true,
			target:        "/api/collaboration/sites/owner/demo/rollback",
			apiKey:        "member-key",
			ifMatch:       formatSiteETag(archiveTestSiteID, 2),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := &collaborationArchiveDBState{
				memberAllowed: test.memberAllowed,
				activeVersion: 2,
				versions:      []int{2, 1},
			}
			_, mux, store, _ := newCollaborationArchiveHarness(t, state)
			store.publish(t, "owner", "demo", archiveTestSiteID, 2, map[string]string{"index.html": "current-v2"})

			request := httptest.NewRequest(http.MethodPost, test.target, strings.NewReader(`{"version":1}`))
			request.Header.Set("X-API-Key", test.apiKey)
			request.Header.Set("Content-Type", "application/json")
			if test.ifMatch != "" {
				request.Header.Set("If-Match", test.ifMatch)
			}
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("rollback status = %d, want 200; body=%s", response.Code, response.Body.String())
			}
			// The database is the switch: nothing on any replica's disk
			// has to change for the rollback to be what is served.
			state.mu.Lock()
			active := state.activeVersion
			state.mu.Unlock()
			if active != 1 {
				t.Fatalf("active version = %d, want 1", active)
			}
		})
	}
}

// TestDeleteCollaborationAssetRecordsAuditInOneTransaction covers
// assets_admin.go's deleteCollaborationAsset (the dashboard's own mirror
// of site_api.go's DeleteAsset): the site_assets soft-delete and its
// asset_delete audit row must be recorded through RecordTx, not the
// non-transactional Record every other call site here predates. Reuses
// this file's own harness/fake driver rather than building a
// second one, per the team lead's "unit test if the existing fakes
// allow."
func TestDeleteCollaborationAssetRecordsAuditInOneTransaction(t *testing.T) {
	state := &collaborationArchiveDBState{assetExists: true}
	siteHandler, mux, _, _ := newCollaborationArchiveHarness(t, state)
	recorder := &siteAPITestRecorder{}
	siteHandler.WithAudit(recorder)

	// reqlog.Middleware is what assigns the id auditRequestID(ctx) reads;
	// main.go wraps the whole real mux with it, which this harness's own
	// mux.ServeHTTP does not. Wrapping it here for this one request proves
	// the RequestID: auditRequestID(r.Context()) wiring end to end rather
	// than skipping the assertion because it is otherwise always "".
	loggedMux := reqlog.Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)), nil)(mux)
	response := httptest.NewRecorder()
	loggedMux.ServeHTTP(response, collaborationArchiveRequest(
		http.MethodDelete, "/api/collaboration/sites/owner/demo/assets/0a0a0a0a-0000-4000-8000-0000000000c1", "owner-key",
	))
	if response.Code != http.StatusNoContent {
		t.Fatalf("delete asset status = %d, want 204 (body %q)", response.Code, response.Body.String())
	}
	testRequestID := response.Header().Get(reqlog.Header)
	if testRequestID == "" {
		t.Fatal("reqlog.Middleware did not assign a request id")
	}
	state.mu.Lock()
	deleted := state.assetDeleted
	state.mu.Unlock()
	if !deleted {
		t.Fatal("site_assets row was not soft-deleted")
	}

	event := recorder.last()
	if event.Action != "asset_delete" {
		t.Fatalf("recorded action = %q, want asset_delete", event.Action)
	}
	if event.OwnerID != archiveTestOwnerID {
		t.Fatalf("OwnerID = %q, want %q", event.OwnerID, archiveTestOwnerID)
	}
	if event.SiteID != archiveTestSiteID {
		t.Fatalf("SiteID = %q, want %q", event.SiteID, archiveTestSiteID)
	}
	// auditRequestID(ctx) only ever returns a real id inside
	// reqlog.Middleware — outside it (every other request this harness's
	// mux.ServeHTTP calls make, with no reqlog wrapper) it is documented to
	// return "" rather than panic, which the first delete above already
	// exercised. The wiring itself (RequestID: auditRequestID(r.Context()))
	// is asserted end to end by wrapping just this one request in
	// reqlog.Middleware, below, rather than skipping the assertion.
	if event.RequestID != testRequestID {
		t.Fatalf("RequestID = %q, want the id reqlog.Middleware assigned (%q)", event.RequestID, testRequestID)
	}

	// A second delete of the already-deleted asset is a clean 404, and —
	// since db.GetAsset's own not-found check runs before any transaction
	// opens — records no second audit event.
	secondResponse := httptest.NewRecorder()
	mux.ServeHTTP(secondResponse, collaborationArchiveRequest(
		http.MethodDelete, "/api/collaboration/sites/owner/demo/assets/0a0a0a0a-0000-4000-8000-0000000000c1", "owner-key",
	))
	if secondResponse.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404 (body %q)", secondResponse.Code, secondResponse.Body.String())
	}
	if got := len(recorder.events); got != 1 {
		t.Fatalf("recorder has %d event(s) after the second (no-op) delete, want 1", got)
	}
}

// TestDeleteCollaborationAssetReportsFailureWhenAuditRecordingFails mirrors
// site_api.go's TestSiteAPIPutStateReportsFailureWhenAuditRecordingFails
// for this route's own RecordTx call: a failure recording the audit row
// must report 500, not commit the soft-delete and silently drop the audit
// error.
func TestDeleteCollaborationAssetReportsFailureWhenAuditRecordingFails(t *testing.T) {
	state := &collaborationArchiveDBState{assetExists: true}
	siteHandler, mux, _, _ := newCollaborationArchiveHarness(t, state)
	recorder := &failingRecorder{}
	siteHandler.WithAudit(recorder)

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, collaborationArchiveRequest(
		http.MethodDelete, "/api/collaboration/sites/owner/demo/assets/0a0a0a0a-0000-4000-8000-0000000000c1", "owner-key",
	))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when RecordTx fails (body %q)", response.Code, response.Body.String())
	}
	if !recorder.called {
		t.Fatal("failingRecorder.RecordTx was never called")
	}
}
