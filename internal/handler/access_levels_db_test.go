package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/scan"
	"github.com/vsriram/simple-host/internal/storage"
)

// accessWorld is the real application — management routes, admin routes,
// the audit routes and the host gate — over a real database, for the access
// level tests: who can open a site at each level, and the network request
// flow end to end.
type accessWorld struct {
	t        *testing.T
	database *sql.DB
	app      http.Handler
	keys     []auth.SigningKey
	apiKeys  map[string]string
	users    map[string]string
}

const accessBase = "hosting.corp.test"

func newAccessWorld(t *testing.T) *accessWorld {
	t.Helper()
	return newAccessWorldWith(t, UploadQuota{}, nil)
}

// newAccessWorldWith is newAccessWorld with upload limits (quota, and a
// malware scanner or nil).
func newAccessWorldWith(t *testing.T, quota UploadQuota, scanner scan.Scanner) *accessWorld {
	t.Helper()
	database := connectorTestDB(t)
	base := "https://" + accessBase
	store, err := storage.New(storage.Options{
		Objects: storage.NewMemoryObjects(), Index: storage.NewDBIndex(database),
		CacheDir: t.TempDir(), CacheMaxBytes: 64 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hosts, err := NewHostModel(base)
	if err != nil {
		t.Fatal(err)
	}
	version, err := PluginVersion()
	if err != nil {
		t.Fatal(err)
	}
	skillMW, err := SkillVersionMiddleware(version, MinimumSupportedSkillVersion)
	if err != nil {
		t.Fatal(err)
	}
	keys := []auth.SigningKey{{ID: "k1", Key: []byte(strings.Repeat("s", 32))}}
	authMW := auth.Middleware(database, keys, time.Hour)
	limits := NewAbuseLimits()
	recorder := audit.NewDBRecorder(database)
	mux := http.NewServeMux()
	NewSiteHandler(database, store, base, hosts, limits).WithAudit(recorder).WithUploadLimits(quota, scanner).Register(mux, authMW, skillMW)
	NewUserHandler(database, limits).WithQuota(quota).Register(mux, authMW, skillMW)
	NewAdminHandler(database, base, hosts, CookiePolicy{Secure: true}, keys, time.Hour, recorder, limits).Register(mux, authMW, skillMW)
	NewAuditHandler(database, audit.NewReader(database), "", limits).Register(mux, authMW, skillMW)
	NewTeamHandler(database, limits).WithAudit(recorder).Register(mux, authMW, skillMW, hosts, base)
	files := NewSiteFiles(store, database, CookiePolicy{Secure: true}, keys, time.Hour)
	siteAPI := NewSiteAPIHandler(database, store, storage.AssetLimits{MaxFileBytes: 1 << 20, MaxSiteBytes: 8 << 20, MaxSiteCount: 100}, recorder, hosts, limits).WithUploadLimits(quota, scanner)
	handoff := NewHandoffHandler(database, keys, hosts, recorder, limits)
	gate := NewHostGate(hosts, files, database, keys, auth.NewNegativeSessionCache(database, time.Hour), handoff, siteAPI, authMW, base)

	w := &accessWorld{t: t, database: database, app: gate(mux), keys: keys, apiKeys: map[string]string{}, users: map[string]string{}}
	for _, name := range []string{"alice", "vera", "olly", "mo", "root"} {
		user, err := db.CreateOIDCUser(context.Background(), database, name, "sub-"+name, name+"@example.com", name == "root")
		if err != nil {
			t.Fatal(err)
		}
		key := "key-" + name + "-" + strings.Repeat("0", 40)
		if _, err := db.CreateAPIKey(context.Background(), database, user.ID, "test", db.HashAPIKey(key), key[:8], time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		w.users[name], w.apiKeys[name] = user.ID, key
	}
	return w
}

func (w *accessWorld) do(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	w.app.ServeHTTP(rec, r)
	return rec
}

// api calls a management route on the base host as user, by API key.
func (w *accessWorld) api(user, method, path string, body any) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if raw, ok := body.([]byte); ok {
		reader = bytes.NewReader(raw)
	} else {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	r := httptest.NewRequest(method, "https://"+accessBase+path, reader)
	r.Header.Set("X-API-Key", w.apiKeys[user])
	r.Header.Set("X-Simple-Host-Client", "control-ui")
	r.Header.Set("Content-Type", "application/json")
	return w.do(r)
}

// cookie mints a signed-in session for user, bound to host ("" for the base
// host).
func (w *accessWorld) cookie(user, host string) *http.Cookie {
	session, err := db.CreateSession(context.Background(), w.database, w.users[user], time.Now().Add(time.Hour), "127.0.0.1", "test")
	if err != nil {
		w.t.Fatal(err)
	}
	var value string
	if host == "" {
		value, err = auth.SignSession(w.keys, session.ID, w.users[user], time.Now().Add(time.Hour))
	} else {
		value, err = auth.SignHostSession(w.keys, session.ID, w.users[user], host, time.Now().Add(time.Hour))
	}
	if err != nil {
		w.t.Fatal(err)
	}
	return &http.Cookie{Name: auth.SessionCookieName, Value: value}
}

// admin calls an admin route on the base host with root's browser session.
func (w *accessWorld) admin(method, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://"+accessBase+path, nil)
	r.AddCookie(w.cookie("root", ""))
	r.Header.Set("Origin", "https://"+accessBase)
	r.Header.Set("X-Simple-Host-Client", "control-ui")
	return w.do(r)
}

// view opens a site's page as user ("" for an anonymous visitor), on the
// owner host or, when restricted, the site's own host.
func (w *accessWorld) view(user, owner, site string, restricted bool) int {
	host, path := owner+"."+accessBase, "/"+site+"/"
	if restricted {
		host, path = owner+"--"+site+"."+accessBase, "/"
	}
	r := httptest.NewRequest(http.MethodGet, "https://"+host+path, nil)
	r.Header.Set("Accept", "text/html")
	if user != "" {
		r.AddCookie(w.cookie(user, host))
	}
	return w.do(r).Code
}

// state reads or writes a site's saved data on the owner host, anonymously.
func (w *accessWorld) anonymousState(method, owner, site, body string) int {
	host := owner + "." + accessBase
	r := httptest.NewRequest(method, "https://"+host+"/api/sites/"+site+"/state/versioned", strings.NewReader(body))
	r.Header.Set("Origin", "https://"+host)
	r.Header.Set("Content-Type", "application/json")
	return w.do(r).Code
}

func (w *accessWorld) deploy(user, path string) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("index.html")
	_, _ = f.Write([]byte("<h1>hi</h1>"))
	_ = zw.Close()
	if rec := w.api(user, http.MethodPost, path, buf.Bytes()); rec.Code != http.StatusCreated {
		w.t.Fatalf("deploy %s = %d %s", path, rec.Code, rec.Body)
	}
}

func (w *accessWorld) setAccess(user, path string, body map[string]any, want int) map[string]any {
	w.t.Helper()
	rec := w.api(user, http.MethodPost, path, body)
	if rec.Code != want {
		w.t.Fatalf("POST %s %v = %d %s, want %d", path, body, rec.Code, rec.Body, want)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

func TestAccessLevelsWhoCanOpen(t *testing.T) {
	w := newAccessWorld(t)
	ctx := context.Background()
	tx, _ := w.database.Begin()
	team, err := db.CreateTeam(ctx, tx, "crew", w.users["mo"])
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_ = team
	w.deploy("alice", "/api/sites/demo")
	w.deploy("mo", "/api/collaboration/sites/crew/board")

	// A new site is only-me.
	var access string
	_ = w.database.QueryRow(`SELECT s.access FROM sites s JOIN users u ON u.id = s.user_id WHERE u.username = 'alice' AND s.name = 'demo'`).Scan(&access)
	if access != db.AccessOnlyMe {
		t.Fatalf("new site access = %q, want only_me", access)
	}

	if rec := w.api("alice", http.MethodPost, "/api/collaboration/sites/alice/demo/viewers", map[string]any{"usernames": []string{"vera"}}); rec.Code != http.StatusOK {
		t.Fatalf("grant viewer = %d %s", rec.Code, rec.Body)
	}

	ok, hidden := http.StatusOK, http.StatusNotFound
	signIn := http.StatusFound // anonymous navigation is sent to sign in
	for _, tc := range []struct {
		level                      string
		owner, viewer, other, anon int
	}{
		{db.AccessOnlyMe, ok, hidden, hidden, signIn},
		{db.AccessSpecific, ok, ok, hidden, signIn},
		{db.AccessCompany, ok, ok, ok, signIn},
		{db.AccessListed, ok, ok, ok, signIn},
	} {
		w.setAccess("alice", "/api/sites/demo/access", map[string]any{"level": tc.level}, http.StatusOK)
		restricted := tc.level == db.AccessSpecific
		got := []int{w.view("alice", "alice", "demo", restricted), w.view("vera", "alice", "demo", restricted), w.view("olly", "alice", "demo", restricted), w.view("", "alice", "demo", restricted)}
		want := []int{tc.owner, tc.viewer, tc.other, tc.anon}
		for i, who := range []string{"owner", "viewer", "other", "anonymous"} {
			if got[i] != want[i] {
				t.Errorf("%s: %s got %d, want %d", tc.level, who, got[i], want[i])
			}
		}
		if restricted {
			if code := w.view("alice", "alice", "demo", false); code != http.StatusNotFound {
				t.Errorf("specific: owner host still serves the site (%d)", code)
			}
		}
	}

	// A team site at only_me: the team's member opens it, nobody else.
	for user, want := range map[string]int{"mo": ok, "alice": hidden} {
		if got := w.view(user, "crew", "board", false); got != want {
			t.Errorf("team only_me: %s got %d, want %d", user, got, want)
		}
	}

	// Editor routes are gone.
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/collaboration/sites/alice/demo/editors"},
		{http.MethodPost, "/api/collaboration/sites/alice/demo/editors"},
		{http.MethodDelete, "/api/collaboration/sites/alice/demo/editors/vera"},
		{http.MethodGet, "/api/collaboration/sites/alice/demo/editor-candidates"},
		{http.MethodPost, "/api/sites/demo/visibility"},
	} {
		if rec := w.api("alice", r.method, r.path, map[string]any{"usernames": []string{"vera"}}); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want gone", r.method, r.path, rec.Code)
		}
	}
	// A stranger cannot change another person's level.
	if rec := w.api("olly", http.MethodPost, "/api/collaboration/sites/alice/demo/access", map[string]any{"level": "company"}); rec.Code != http.StatusNotFound {
		t.Errorf("stranger set access = %d, want 404", rec.Code)
	}
}

func TestNetworkAccessRequestApproveDeclineRevert(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	w.setAccess("alice", "/api/sites/demo/access", map[string]any{"level": "company"}, http.StatusOK)
	if code := w.anonymousState(http.MethodPut, "alice", "demo", `{"version":0,"state":{"n":1}}`); code != http.StatusUnauthorized {
		t.Fatalf("anonymous write before any request = %d", code)
	}

	// A request needs a reason and changes nothing until approved.
	w.setAccess("alice", "/api/sites/demo/access", map[string]any{"level": "network"}, http.StatusBadRequest)
	got := w.setAccess("alice", "/api/sites/demo/access", map[string]any{"level": "network", "reason": "public event page"}, http.StatusAccepted)
	if got["access"] != db.AccessCompany || got["network_request"] == nil {
		t.Fatalf("after request: %v", got)
	}
	if code := w.view("", "alice", "demo", false); code != http.StatusFound {
		t.Fatalf("anonymous view while pending = %d, want the sign-in redirect", code)
	}
	pending, err := db.ListNetworkAccess(context.Background(), w.database)
	if err != nil || len(pending) != 1 || pending[0].RequestedAt == nil || pending[0].Reason != "public event page" {
		t.Fatalf("pending requests = %+v, %v", pending, err)
	}
	// Only an admin approves.
	nonAdmin := httptest.NewRequest(http.MethodPost, "https://"+accessBase+"/api/admin/access-requests/alice/demo/approve", nil)
	nonAdmin.AddCookie(w.cookie("alice", ""))
	nonAdmin.Header.Set("Origin", "https://"+accessBase)
	nonAdmin.Header.Set("X-Simple-Host-Client", "control-ui")
	if rec := w.do(nonAdmin); rec.Code != http.StatusForbidden {
		t.Fatalf("owner approving own request = %d, want 403", rec.Code)
	}

	// Decline: nothing changes, the request is gone.
	if rec := w.admin(http.MethodPost, "/api/admin/access-requests/alice/demo/decline"); rec.Code != http.StatusOK {
		t.Fatalf("decline = %d %s", rec.Code, rec.Body)
	}
	if rec := w.admin(http.MethodPost, "/api/admin/access-requests/alice/demo/approve"); rec.Code != http.StatusConflict {
		t.Fatalf("approve after decline = %d, want 409", rec.Code)
	}

	// Request again and approve: anonymous visitors read, never write.
	w.setAccess("alice", "/api/sites/demo/access", map[string]any{"level": "network", "reason": "again"}, http.StatusAccepted)
	if rec := w.admin(http.MethodPost, "/api/admin/access-requests/alice/demo/approve"); rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body)
	}
	if code := w.view("", "alice", "demo", false); code != http.StatusOK {
		t.Fatalf("anonymous view after approval = %d, want 200", code)
	}
	if code := w.anonymousState(http.MethodGet, "alice", "demo", ""); code != http.StatusOK {
		t.Fatalf("anonymous state read = %d, want 200", code)
	}
	if code := w.anonymousState(http.MethodPut, "alice", "demo", `{"version":0,"state":{"n":1}}`); code != http.StatusUnauthorized {
		t.Fatalf("anonymous state write = %d, want 401", code)
	}
	anonAsset := httptest.NewRequest(http.MethodPost, "https://alice."+accessBase+"/api/sites/demo/assets", strings.NewReader("x"))
	if rec := w.do(anonAsset); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous asset upload = %d, want 401", rec.Code)
	}
	// Signed-in people keep their rights on a network site.
	if code := w.view("olly", "alice", "demo", false); code != http.StatusOK {
		t.Fatalf("signed-in view on a network site = %d", code)
	}
	// ?signin takes a visitor with no session through the hand-off.
	signin := httptest.NewRequest(http.MethodGet, "https://alice."+accessBase+"/demo/?signin", nil)
	signin.Header.Set("Accept", "text/html")
	if rec := w.do(signin); rec.Code != http.StatusFound {
		t.Fatalf("?signin = %d, want the hand-off redirect", rec.Code)
	}
	// An anonymous visitor reaches neither the owner's index nor the
	// management API.
	if code := w.do(httptest.NewRequest(http.MethodGet, "https://alice."+accessBase+"/", nil)).Code; code != http.StatusUnauthorized && code != http.StatusFound {
		t.Fatalf("anonymous owner index = %d", code)
	}
	if code := w.do(httptest.NewRequest(http.MethodGet, "https://alice."+accessBase+"/api/collaboration/sites", nil)).Code; code != http.StatusNotFound {
		t.Fatalf("management API on an owner host = %d, want 404", code)
	}

	// The owner lowering the level revokes the approval.
	w.setAccess("alice", "/api/sites/demo/access", map[string]any{"level": "listed"}, http.StatusOK)
	if code := w.view("", "alice", "demo", false); code != http.StatusFound {
		t.Fatalf("anonymous view after lowering = %d, want the sign-in redirect", code)
	}
	var actions []string
	rows, err := w.database.Query(`SELECT action FROM audit_events WHERE action LIKE 'network_access_%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	want := "network_access_requested,network_access_declined,network_access_requested,network_access_approved,network_access_reverted"
	if strings.Join(actions, ",") != want {
		t.Fatalf("audit = %v, want %s", actions, want)
	}
}

func TestAccessLogOwnerSeesCountsAdminSeesWho(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	now := time.Now().UTC()
	if err := db.InsertAccessLogBatch(context.Background(), w.database, []db.AccessLogEvent{
		{At: now, UserID: w.users["vera"], OwnerLabel: "alice", SiteName: "demo", Path: "/demo/", Method: "GET", Status: 200, IP: "10.0.0.1", UserAgent: "ua", ClientKind: "human"},
		{At: now, UserID: w.users["vera"], OwnerLabel: "alice", SiteName: "demo", Path: "/demo/", Method: "GET", Status: 200, ClientKind: "human"},
		{At: now, UserID: w.users["olly"], OwnerLabel: "alice", SiteName: "demo", Path: "/demo/", Method: "GET", Status: 200, ClientKind: "human"},
	}); err != nil {
		t.Fatal(err)
	}

	rec := w.api("alice", http.MethodGet, "/api/access?owner=alice&site=demo", nil)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(body, "entries") || strings.Contains(body, w.users["vera"]) || strings.Contains(body, "10.0.0.1") {
		t.Fatalf("owner access log = %d %s, want counts only", rec.Code, body)
	}
	var counts struct {
		UniqueViewers int64 `json:"unique_viewers"`
		Days          []struct {
			Views int64 `json:"views"`
		} `json:"days"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &counts)
	if counts.UniqueViewers != 2 || len(counts.Days) != 1 || counts.Days[0].Views != 3 {
		t.Fatalf("owner counts = %+v", counts)
	}

	rec = w.admin(http.MethodGet, "/api/access?owner=alice&site=demo")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), w.users["vera"]) || !strings.Contains(rec.Body.String(), "10.0.0.1") {
		t.Fatalf("admin access log = %d %s, want each visit and who", rec.Code, rec.Body)
	}
}
