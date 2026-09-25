package handler

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/storage"
)

// siteAPITestRecorder is a fake audit.Recorder that captures every Event
// passed to Record, for TestSiteAPIAuditEventResolvesOwnerUsernameToID.
type siteAPITestRecorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *siteAPITestRecorder) Record(_ context.Context, event audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *siteAPITestRecorder) RecordTx(ctx context.Context, _ *sql.Tx, event audit.Event) error {
	r.Record(ctx, event)
	return nil
}

func (r *siteAPITestRecorder) last() audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return audit.Event{}
	}
	return r.events[len(r.events)-1]
}

// siteAPITestRow is one canned row a siteAPITestState answers with, keyed by
// a substring of the normalized query text — the same "assert on the query
// shape, answer canned rows" pattern collaboration_archive_integration_test.go
// and showcase_test.go already use for this package's other DB-backed tests
// (no live Postgres in internal/handler; internal/db's own tests, and this
// its site_viewers_db_test.go, are what integration-test the SQL itself).
type siteAPITestState struct {
	mu sync.Mutex
	// retired records keys queued in storage_retired.
	retired []string
	// assets is the live-row store CreateAsset/GetAsset/ListAssets/
	// SoftDeleteAsset read and write, keyed by asset id.
	assets map[string]siteAPITestAssetRow
	// siteExists backs the fake UPDATE sites ... SET state = query
	// PutState/PutStateVersioned run: false answers "0 rows affected"
	// (db.UpdateSiteState's own sql.ErrNoRows path), for
	// TestSiteAPIPutStateFailureRecordsNoAudit.
	siteExists bool
}

type siteAPITestAssetRow struct {
	id, siteID, name, contentType string
	size                          int64
	sha256                        []byte
	createdBy                     *string
	createdAt                     time.Time
	deleted                       bool
}

func newSiteAPITestDB(t *testing.T, state *siteAPITestState) *sql.DB {
	t.Helper()
	database := sql.OpenDB(&siteAPITestConnector{state: state})
	database.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = database.Close() })
	return database
}

type siteAPITestConnector struct{ state *siteAPITestState }

func (c *siteAPITestConnector) Connect(context.Context) (driver.Conn, error) {
	return &siteAPITestConn{state: c.state}, nil
}
func (c *siteAPITestConnector) Driver() driver.Driver { return siteAPITestDriver{state: c.state} }

type siteAPITestDriver struct{ state *siteAPITestState }

func (d siteAPITestDriver) Open(string) (driver.Conn, error) {
	return &siteAPITestConn{state: d.state}, nil
}

type siteAPITestConn struct{ state *siteAPITestState }

func (c *siteAPITestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}
func (c *siteAPITestConn) Close() error { return nil }

// Begin backs the transactions site_api.go now opens around each write and
// its audit_events row (a mutation without its audit row
// must not commit). database/sql routes a *sql.Tx's own ExecContext/
// QueryContext straight through to this same driver.Conn (a driver-level
// "transaction" is a marker on the connection, not a separate object with
// its own query methods), so nothing else about the fake driver's
// QueryContext/ExecContext switch above needs to change — Commit/Rollback
// here are no-ops because every write already lands directly in s.assets.
func (c *siteAPITestConn) Begin() (driver.Tx, error) {
	return siteAPITestTx{}, nil
}

type siteAPITestTx struct{}

func (siteAPITestTx) Commit() error   { return nil }
func (siteAPITestTx) Rollback() error { return nil }

func (c *siteAPITestConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	normalized := strings.Join(strings.Fields(query), " ")
	s := c.state
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case strings.Contains(normalized, "INSERT INTO site_assets"):
		row := siteAPITestAssetRow{
			id: args[0].Value.(string), siteID: args[1].Value.(string), name: args[2].Value.(string),
			contentType: args[3].Value.(string), size: args[4].Value.(int64), sha256: args[5].Value.([]byte),
			createdAt: time.Now(),
		}
		if s := args[6].Value; s != nil {
			id := s.(string)
			row.createdBy = &id
		}
		if s.assets == nil {
			s.assets = map[string]siteAPITestAssetRow{}
		}
		s.assets[row.id] = row
		return assetRows([]siteAPITestAssetRow{row}), nil

	case strings.Contains(normalized, "FROM site_assets") && strings.Contains(normalized, "WHERE id = $1 AND site_id = $2"):
		id, siteID := args[0].Value.(string), args[1].Value.(string)
		row, ok := s.assets[id]
		if !ok || row.siteID != siteID || row.deleted {
			return nil, sql.ErrNoRows
		}
		return assetRows([]siteAPITestAssetRow{row}), nil

	case strings.Contains(normalized, "FROM site_assets") && strings.Contains(normalized, "ORDER BY created_at DESC"):
		siteID := args[0].Value.(string)
		var rows []siteAPITestAssetRow
		for _, row := range s.assets {
			if row.siteID == siteID && !row.deleted {
				rows = append(rows, row)
			}
		}
		return assetRows(rows), nil

	case strings.Contains(normalized, "SELECT count(*), COALESCE(sum(size), 0) FROM site_assets"):
		// Backs db.SumAssetUsage inside db.CreateAssetWithinQuota.
		siteID := args[0].Value.(string)
		var count, total int64
		for _, row := range s.assets {
			if row.siteID == siteID && !row.deleted {
				count++
				total += row.size
			}
		}
		return &siteAPITestRows{columns: []string{"count", "sum"}, values: [][]driver.Value{{count, total}}}, nil

	case strings.Contains(normalized, "FROM users") && strings.Contains(normalized, "WHERE username = $1"):
		// Backs db.GetUserByUsername, which h.auditEvent (site_api.go) calls
		// to resolve siteAPICall.Owner (a username) to the users.id
		// audit_events.owner_id actually needs — see
		// TestSiteAPIAuditEventResolvesOwnerUsernameToID.
		username := args[0].Value.(string)
		return &siteAPITestRows{
			columns: []string{"id", "username", "is_admin", "created_at", "kind"},
			values:  [][]driver.Value{{"owner-id-" + username, username, false, time.Now(), "person"}},
		}, nil

	default:
		return nil, errors.New("siteAPITestConn: unexpected query " + normalized)
	}
}

func (c *siteAPITestConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	normalized := strings.Join(strings.Fields(query), " ")
	s := c.state
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case strings.Contains(normalized, "pg_advisory_xact_lock"):
		return driver.RowsAffected(1), nil

	case strings.Contains(normalized, "INSERT INTO storage_retired"):
		s.retired = append(s.retired, args[0].Value.(string))
		return driver.RowsAffected(1), nil

	case strings.Contains(normalized, "UPDATE site_assets SET deleted_at = now()"):
		id, siteID := args[0].Value.(string), args[1].Value.(string)
		row, ok := s.assets[id]
		if !ok || row.siteID != siteID || row.deleted {
			return driver.RowsAffected(0), nil
		}
		row.deleted = true
		s.assets[id] = row
		return driver.RowsAffected(1), nil

	case strings.Contains(normalized, "UPDATE sites s") && strings.Contains(normalized, "uses_state = true"):
		// Backs db.UpdateSiteState (site_api.go's PutState). No site row is
		// ever actually modeled here — TestSiteAPIPutStateFailureRecordsNoAudit
		// is the only test that reaches this case, and it wants the
		// "site not found" (0 rows affected) branch specifically.
		if !s.siteExists {
			return driver.RowsAffected(0), nil
		}
		return driver.RowsAffected(1), nil

	case strings.Contains(normalized, "site_state_history"):
		// db.RecordStateHistory's insert and prune, in the same transaction.
		return driver.RowsAffected(1), nil

	default:
		return nil, errors.New("siteAPITestConn: unexpected exec " + normalized)
	}
}

func assetRows(rows []siteAPITestAssetRow) driver.Rows {
	values := make([][]driver.Value, len(rows))
	for i, r := range rows {
		var createdBy any
		if r.createdBy != nil {
			createdBy = *r.createdBy
		}
		values[i] = []driver.Value{r.id, r.siteID, r.name, r.contentType, r.size, r.sha256, createdBy, r.createdAt, nil}
	}
	return &siteAPITestRows{
		columns: []string{"id", "site_id", "name", "content_type", "size", "sha256", "created_by", "created_at", "deleted_at"},
		values:  values,
	}
}

type siteAPITestRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *siteAPITestRows) Columns() []string { return r.columns }
func (r *siteAPITestRows) Close() error      { return nil }
func (r *siteAPITestRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

// multipartUploadRequest builds a POST with a single-file multipart body
// named "file", the shape a plain <input type=file> form (or a minimal
// fetch(FormData) call) produces.
func multipartUploadRequest(t *testing.T, filename string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write multipart content: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/sites/demo/assets", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

// twoFilePartUploadRequest builds a multipart POST carrying two file parts
// under different field names, for the "exactly one file part" review
// finding: ranging over r.MultipartForm.File (a map keyed by field name)
// to pick "the first one found" would make the choice depend on Go's
// randomized map iteration order.
func twoFilePartUploadRequest(t *testing.T, filename1 string, content1 []byte, filename2 string, content2 []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part1, err := writer.CreateFormFile("file", filename1)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part1.Write(content1); err != nil {
		t.Fatalf("write multipart content: %v", err)
	}
	part2, err := writer.CreateFormFile("evil", filename2)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part2.Write(content2); err != nil {
		t.Fatalf("write multipart content: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/sites/demo/assets", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

const siteAPITestSiteID = "0a0a0a0a-0000-4000-8000-0000000000b1"

func testAssetLimits() storage.AssetLimits {
	return storage.AssetLimits{MaxFileBytes: 1 << 20, MaxSiteBytes: 10 << 20, MaxSiteCount: 100}
}

// A minimal valid PNG signature plus filler bytes: enough for
// http.DetectContentType to sniff "image/png" without needing a real image.
var testPNGBytes = append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, bytes.Repeat([]byte{0}, 32)...)

func TestSiteAPICreateAssetAndServeHeaders(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "demo", "index")
	state := &siteAPITestState{}
	database := newSiteAPITestDB(t, state)
	handler := NewSiteAPIHandler(database, store.Store, testAssetLimits(), nil, testHostModel(t))
	call := siteAPICall{Owner: "alice", SiteName: "demo", SiteID: siteAPITestSiteID}

	request := multipartUploadRequest(t, "logo.png", testPNGBytes)
	response := httptest.NewRecorder()
	handler.CreateAsset(response, request, call)
	if response.Code != http.StatusCreated {
		t.Fatalf("CreateAsset status = %d, body %q", response.Code, response.Body.String())
	}
	var created struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ID == "" || !strings.Contains(created.URL, "/demo/_assets/"+created.ID+"/logo.png") {
		t.Fatalf("create response = %+v", created)
	}

	// Inline (image/*): no Content-Disposition.
	serveRequest := httptest.NewRequest(http.MethodGet, "/demo/_assets/"+created.ID+"/logo.png", nil)
	serveResponse := httptest.NewRecorder()
	handler.ServeAsset(serveResponse, serveRequest, call, created.ID)
	if serveResponse.Code != http.StatusOK {
		t.Fatalf("ServeAsset status = %d, body %q", serveResponse.Code, serveResponse.Body.String())
	}
	if got := serveResponse.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if got := serveResponse.Header().Get("Content-Disposition"); got != "" {
		t.Fatalf("inline asset carries Content-Disposition: %q", got)
	}
	if got := serveResponse.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := serveResponse.Header().Get("Cache-Control"); got != "private, max-age=3600" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if serveResponse.Body.Len() != len(testPNGBytes) {
		t.Fatalf("served %d bytes, want %d", serveResponse.Body.Len(), len(testPNGBytes))
	}

	// Non-media (text/plain): attachment, with a sanitized filename.
	textRequest := multipartUploadRequest(t, "notes.txt", []byte("hello world"))
	textResponse := httptest.NewRecorder()
	handler.CreateAsset(textResponse, textRequest, call)
	if textResponse.Code != http.StatusCreated {
		t.Fatalf("CreateAsset(text) status = %d body %q", textResponse.Code, textResponse.Body.String())
	}
	var textCreated struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(textResponse.Body.Bytes(), &textCreated)
	textServe := httptest.NewRecorder()
	handler.ServeAsset(textServe, httptest.NewRequest(http.MethodGet, "/demo/_assets/"+textCreated.ID, nil), call, textCreated.ID)
	if got := textServe.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") || !strings.Contains(got, "notes.txt") {
		t.Fatalf("Content-Disposition = %q, want an attachment naming notes.txt", got)
	}

	// List reflects both.
	listResponse := httptest.NewRecorder()
	handler.ListAssets(listResponse, httptest.NewRequest(http.MethodGet, "/api/sites/demo/assets", nil), call)
	var list struct {
		Assets []struct {
			ID string `json:"id"`
		} `json:"assets"`
	}
	_ = json.Unmarshal(listResponse.Body.Bytes(), &list)
	if len(list.Assets) != 2 {
		t.Fatalf("ListAssets returned %d assets, want 2: %+v", len(list.Assets), list.Assets)
	}
}

func TestSiteAPICreateAssetRejectsDisallowedType(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "demo", "index")
	handler := NewSiteAPIHandler(newSiteAPITestDB(t, &siteAPITestState{}), store.Store, testAssetLimits(), nil, testHostModel(t))
	call := siteAPICall{Owner: "alice", SiteName: "demo", SiteID: siteAPITestSiteID}

	request := multipartUploadRequest(t, "page.html", []byte("<html><body>hi</body></html>"))
	response := httptest.NewRecorder()
	handler.CreateAsset(response, request, call)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415 (body %q)", response.Code, response.Body.String())
	}
}

func TestSiteAPICreateAssetRejectsMultipleFileParts(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "demo", "index")
	handler := NewSiteAPIHandler(newSiteAPITestDB(t, &siteAPITestState{}), store.Store, testAssetLimits(), nil, testHostModel(t))
	call := siteAPICall{Owner: "alice", SiteName: "demo", SiteID: siteAPITestSiteID}

	request := twoFilePartUploadRequest(t, "logo.png", testPNGBytes, "sneaky.png", testPNGBytes)
	response := httptest.NewRecorder()
	handler.CreateAsset(response, request, call)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", response.Code, response.Body.String())
	}
}

func TestSiteAPIDeleteAssetRetiresObjectWithRow(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "demo", "index")
	state := &siteAPITestState{}
	database := newSiteAPITestDB(t, state)
	handler := NewSiteAPIHandler(database, store.Store, testAssetLimits(), nil, testHostModel(t))
	call := siteAPICall{Owner: "alice", SiteName: "demo", SiteID: siteAPITestSiteID}

	createResponse := httptest.NewRecorder()
	handler.CreateAsset(createResponse, multipartUploadRequest(t, "logo.png", testPNGBytes), call)
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(createResponse.Body.Bytes(), &created)

	deleteResponse := httptest.NewRecorder()
	handler.DeleteAsset(deleteResponse, httptest.NewRequest(http.MethodDelete, "/api/sites/demo/assets/"+created.ID, nil), call, created.ID)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("DeleteAsset status = %d, body %q", deleteResponse.Code, deleteResponse.Body.String())
	}

	// The object is queued in the delete's own transaction, not removed
	// before it: a rolled-back delete leaves a working asset.
	key, _ := storage.AssetKey(siteAPITestSiteID, created.ID)
	state.mu.Lock()
	retired := append([]string(nil), state.retired...)
	state.mu.Unlock()
	if len(retired) != 1 || retired[0] != key {
		t.Fatalf("retired = %v, want [%s]", retired, key)
	}
	if keys := store.objects.Keys(); !slices.Contains(keys, key) {
		t.Fatalf("object deleted before the sweep: %v", keys)
	}

	serveResponse := httptest.NewRecorder()
	handler.ServeAsset(serveResponse, httptest.NewRequest(http.MethodGet, "/demo/_assets/"+created.ID, nil), call, created.ID)
	if serveResponse.Code != http.StatusNotFound {
		t.Fatalf("deleted asset still serves: status = %d", serveResponse.Code)
	}

	// Second delete of the same id is a clean not-found, not an error.
	secondDelete := httptest.NewRecorder()
	handler.DeleteAsset(secondDelete, httptest.NewRequest(http.MethodDelete, "/api/sites/demo/assets/"+created.ID, nil), call, created.ID)
	if secondDelete.Code != http.StatusNotFound {
		t.Fatalf("second DeleteAsset status = %d, want 404", secondDelete.Code)
	}
}

// TestSiteAPIAuditEventResolvesOwnerUsernameToID guards against a real bug
// found while wiring a real DBRecorder in: siteAPICall.Owner is the
// owner's *username* (every disk path and URL in this file correctly keys
// on it), but audit_events.owner_id is a uuid column, so passing the
// username straight through failed every state_write/asset_create/
// asset_delete audit row's INSERT outright — silently, since Record logs
// and swallows the error rather than surfacing it to the request. h.auditEvent
// now resolves Owner to its users.id first (site_api.go); this test fails
// if that resolution is ever removed or bypassed again.
func TestSiteAPIAuditEventResolvesOwnerUsernameToID(t *testing.T) {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "demo", "index")
	state := &siteAPITestState{}
	database := newSiteAPITestDB(t, state)
	recorder := &siteAPITestRecorder{}
	handler := NewSiteAPIHandler(database, store.Store, testAssetLimits(), recorder, testHostModel(t))
	call := siteAPICall{Owner: "alice", SiteName: "demo", SiteID: siteAPITestSiteID, ActorUserID: "user-1", ActorKind: "person"}

	response := httptest.NewRecorder()
	handler.CreateAsset(response, multipartUploadRequest(t, "logo.png", testPNGBytes), call)
	if response.Code != http.StatusCreated {
		t.Fatalf("CreateAsset status = %d, body %q", response.Code, response.Body.String())
	}

	event := recorder.last()
	if event.Action != "asset_create" {
		t.Fatalf("recorded action = %q, want asset_create", event.Action)
	}
	if event.OwnerID != "owner-id-alice" {
		t.Fatalf("OwnerID = %q, want the resolved owner id (\"owner-id-alice\"), not the raw username", event.OwnerID)
	}
	if event.OwnerID == call.Owner {
		t.Fatalf("OwnerID = %q equals the raw username %q — the bug this test guards against", event.OwnerID, call.Owner)
	}
}

// TestSiteAPIPutStateFailureRecordsNoAudit is the reviewer-requested
// guard for the other direction of the audit rule: not just "a mutation without
// its audit row cannot commit" (proven by the transactions PutState/
// PutStateVersioned/CreateAsset/DeleteAsset now open around their own
// write and RecordTx call), but also "a write that never happened must
// never gain an audit row either." db.UpdateSiteState returns
// sql.ErrNoRows before PutState ever reaches h.auditEvent/RecordTx, so
// the fake recorder here must see zero events.
func TestSiteAPIPutStateFailureRecordsNoAudit(t *testing.T) {
	store := newTestStore(t)
	state := &siteAPITestState{} // siteExists defaults false
	database := newSiteAPITestDB(t, state)
	recorder := &siteAPITestRecorder{}
	handler := NewSiteAPIHandler(database, store.Store, testAssetLimits(), recorder, testHostModel(t))
	call := siteAPICall{Owner: "alice", SiteName: "demo", SiteID: siteAPITestSiteID, ActorUserID: "user-1", ActorKind: "person"}

	request := httptest.NewRequest(http.MethodPut, "/api/sites/demo/state", strings.NewReader(`{"hello":"world"}`))
	response := httptest.NewRecorder()
	handler.PutState(response, request, call)

	if response.Code != http.StatusNotFound {
		t.Fatalf("PutState status = %d, want 404 (body %q)", response.Code, response.Body.String())
	}
	if got := len(recorder.events); got != 0 {
		t.Fatalf("recorder recorded %d event(s), want 0 for a state write that never happened: %+v", got, recorder.events)
	}
}

// TestSiteAPIPutStateReportsFailureWhenAuditRecordingFails covers the
// other half of the audit rule ("a mutation without its audit row cannot
// commit"): when the DB write itself succeeds but RecordTx fails, PutState
// must report the failure to the caller (500) rather than committing the
// write and silently discarding the audit error — the earlier-return
// structure this asserts is what makes `tx.Commit()` unreachable on this
// path, so the deferred `tx.Rollback()` is what actually undoes the write
// at the real Postgres level; this fake driver applies a write the
// instant ExecContext runs and has no rollback semantics of its own to
// re-verify, so it can only prove the handler-level contract (fail
// closed, don't commit), not the transaction's own atomicity — that half
// is `*sql.Tx`'s and Postgres's own guarantee, already relied on
// everywhere else this handler opens one.
func TestSiteAPIPutStateReportsFailureWhenAuditRecordingFails(t *testing.T) {
	store := newTestStore(t)
	testState := &siteAPITestState{siteExists: true}
	database := newSiteAPITestDB(t, testState)
	recorder := &failingRecorder{}
	handler := NewSiteAPIHandler(database, store.Store, testAssetLimits(), recorder, testHostModel(t))
	call := siteAPICall{Owner: "alice", SiteName: "demo", SiteID: siteAPITestSiteID, ActorUserID: "user-1", ActorKind: "person"}

	request := httptest.NewRequest(http.MethodPut, "/api/sites/demo/state", strings.NewReader(`{"hello":"world"}`))
	response := httptest.NewRecorder()
	handler.PutState(response, request, call)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("PutState status = %d, want 500 when RecordTx fails (body %q)", response.Code, response.Body.String())
	}
	if !recorder.called {
		t.Fatal("failingRecorder.RecordTx was never called")
	}
}

// failingRecorder is an audit.Recorder whose RecordTx always errors, for
// TestSiteAPIPutStateReportsFailureWhenAuditRecordingFails.
type failingRecorder struct{ called bool }

func (r *failingRecorder) Record(context.Context, audit.Event) {}
func (r *failingRecorder) RecordTx(context.Context, *sql.Tx, audit.Event) error {
	r.called = true
	return errors.New("failingRecorder: RecordTx always fails")
}
