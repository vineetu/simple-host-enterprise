package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oplimits"
	"github.com/vsriram/simple-host/internal/storage"
	sp "github.com/vsriram/simple-host/internal/storagepolicy"
)

// storageCaller is one of the people a storage test acts as: on the site's
// page (a host session) or through their own tools (an API key).
type storageTestCaller struct {
	name string
	user string // "" for an anonymous network visitor
	tool bool
}

var storageCallers = []storageTestCaller{
	{"anonymous", "", false},
	{"colleague", "olly", false},
	{"author", "vera", false},
	{"owner-on-site", "alice", false},
	{"owner-tool", "alice", true},
	{"colleague-tool", "olly", true},
}

var storageAddr atomic.Int64

// siteReq builds a request to a site's own host as caller. Each gets its
// own address, so the per-client limits never decide a test.
func (w *accessWorld) siteReq(c storageTestCaller, site, method, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	host := site + ".alice." + accessBase
	r := httptest.NewRequest(method, "https://"+host+path, bytes.NewReader(body))
	n := storageAddr.Add(1)
	r.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:1234", (n>>16)&255, (n>>8)&255, n&255)
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	switch {
	case c.user == "":
	case c.tool:
		r.Header.Set("X-API-Key", w.apiKeys[c.user])
	default:
		r.AddCookie(w.cookie(c.user, host))
		r.Header.Set("Origin", "https://"+host)
	}
	return w.do(r)
}

func (w *accessWorld) storageSite(name, access string) string {
	w.t.Helper()
	var id string
	if err := w.database.QueryRow(`INSERT INTO sites (user_id, name, access) VALUES ($1, $2, $3) RETURNING id`, w.users["alice"], name, access).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// refStorage is the design's "who is who" table, stated over callers rather
// than over sp.Caller, independent of sp.Decide.
func refStorage(who sp.Who, a sp.Action, c storageTestCaller) string {
	if c.user == "alice" && c.tool {
		return "all"
	}
	if c.user == "" {
		if a == sp.Read && who == sp.Anyone {
			return "all"
		}
		return "none"
	}
	switch who {
	case sp.Anyone, sp.SignedIn:
		return "all"
	case sp.Own:
		if c.user == "alice" {
			return "all"
		}
		return "own"
	case sp.Owner:
		if c.user == "alice" {
			return "all"
		}
	}
	return "none"
}

// TestStoragePresetsEveryCallerActionAndKind applies each preset to a
// key-value namespace, a table and a file bucket, and checks every action by
// every kind of caller against the reference, with an entry vera added.
func TestStoragePresetsEveryCallerActionAndKind(t *testing.T) {
	w := newAccessWorld(t)
	ctx := context.Background()
	refused := func(c storageTestCaller) int {
		if c.user == "" {
			return http.StatusUnauthorized
		}
		return http.StatusForbidden
	}
	checked := 0
	for _, preset := range sp.PresetOrder {
		m := sp.Presets[preset]
		for _, kind := range []string{"kv", "db", "files"} {
			site := "s-" + preset + "-" + kind
			siteID := w.storageSite(site, db.AccessNetwork)
			res := db.StorageResource{SiteID: siteID, Name: "r", Kind: kind, Preset: preset, Access: m}
			if kind == "db" {
				res.Access = sp.Presets["private"]
				res.Settings.Tables = map[string]db.StorageTable{"t": {Preset: preset, Access: m, Columns: map[string]sp.Column{"v": {Type: "text"}}}}
			}
			tx, _ := w.database.Begin()
			if _, err := db.PutStorageResource(ctx, tx, res); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			// seed resets the resource to one entry, vera's, and returns
			// the path of that entry and of the collection.
			seed := func() (item, list string) {
				for _, table := range []string{"site_storage_kv", "site_storage_rows", "site_storage_files"} {
					if _, err := w.database.Exec(`DELETE FROM `+table+` WHERE site_id = $1`, siteID); err != nil {
						t.Fatal(err)
					}
				}
				base := "/api/site/storage/"
				switch kind {
				case "kv":
					if _, err := w.database.Exec(`INSERT INTO site_storage_kv (site_id, resource, key, value, added_by) VALUES ($1, 'r', 'vera-entry', '"hi"', $2)`, siteID, w.users["vera"]); err != nil {
						t.Fatal(err)
					}
					return base + "kv/r/keys/vera-entry", base + "kv/r/keys"
				case "db":
					var id int64
					if err := w.database.QueryRow(`INSERT INTO site_storage_rows (site_id, resource, table_name, data, added_by) VALUES ($1, 'r', 't', '{"v":"hi"}', $2) RETURNING id`, siteID, w.users["vera"]).Scan(&id); err != nil {
						t.Fatal(err)
					}
					return fmt.Sprintf("%sdb/r/tables/t/rows/%d", base, id), base + "db/r/tables/t/rows"
				default:
					stored, err := w.store.CreateAsset(ctx, siteID, "text/plain", strings.NewReader("hello"), storage.AssetLimits{MaxFileBytes: 1 << 20, MaxSiteBytes: 1 << 30, MaxSiteCount: 1000})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := w.database.Exec(`INSERT INTO site_storage_files (site_id, resource, path, object_id, content_type, size, sha256, added_by) VALUES ($1, 'r', 'vera.txt', $2, $3, $4, $5, $6)`,
						siteID, stored.ID, stored.ContentType, stored.Size, stored.SHA256[:], w.users["vera"]); err != nil {
						t.Fatal(err)
					}
					return base + "files/r/objects/vera.txt", base + "files/r/objects"
				}
			}
			for _, c := range storageCallers {
				label := fmt.Sprintf("%s/%s/%s", preset, kind, c.name)
				isAuthor := c.user == "vera"
				canRead := refStorage(m.Read, sp.Read, c)
				seesEntry := canRead == "all" || (canRead == "own" && isAuthor)

				item, list := seed()
				// Read one.
				want := http.StatusOK
				switch {
				case canRead == "none":
					want = refused(c)
				case !seesEntry:
					want = http.StatusNotFound
				}
				if rec := w.siteReq(c, site, http.MethodGet, item, nil, ""); rec.Code != want {
					t.Errorf("%s read one = %d %s, want %d", label, rec.Code, rec.Body, want)
				}
				// List.
				rec := w.siteReq(c, site, http.MethodGet, list, nil, "")
				if canRead == "none" {
					if rec.Code != refused(c) {
						t.Errorf("%s list = %d, want %d", label, rec.Code, refused(c))
					}
				} else {
					var out map[string][]any
					_ = json.Unmarshal(rec.Body.Bytes(), &out)
					n := len(out["keys"]) + len(out["rows"]) + len(out["files"])
					wantN := 0
					if seesEntry {
						wantN = 1
					}
					if rec.Code != http.StatusOK || n != wantN {
						t.Errorf("%s list = %d with %d entries, want 200 with %d: %s", label, rec.Code, n, wantN, rec.Body)
					}
					// Who added it shows to the owner only; mine to everyone signed in.
					if n == 1 {
						var entries []any
						for _, v := range out {
							entries = append(entries, v...)
						}
						entry := entries[0].(map[string]any)
						if _, shown := entry["added_by"]; shown != (c.user == "alice") {
							t.Errorf("%s list shows added_by = %v", label, shown)
						}
						if c.user != "" && entry["mine"] != isAuthor {
							t.Errorf("%s mine = %v", label, entry["mine"])
						}
					}
				}
				// Add.
				var addPath, addType string
				var addBody []byte
				switch kind {
				case "kv":
					addPath, addBody, addType = list+"/new-"+c.name, []byte(`{"value":1}`), "application/json"
				case "db":
					addPath, addBody, addType = list, []byte(`{"v":"new"}`), "application/json"
				default:
					addPath, addBody, addType = list+"/new-"+c.name+".txt", []byte("new file"), "text/plain"
				}
				method := http.MethodPut
				if kind == "db" {
					method = http.MethodPost
				}
				want = http.StatusCreated
				if c.user == "" {
					want = http.StatusUnauthorized
				} else if refStorage(m.Add, sp.Add, c) == "none" {
					want = http.StatusForbidden
				}
				if rec := w.siteReq(c, site, method, addPath, addBody, addType); rec.Code != want {
					t.Errorf("%s add = %d %s, want %d", label, rec.Code, rec.Body, want)
				}
				// Edit vera's entry.
				canEdit := refStorage(m.Edit, sp.Edit, c)
				switch {
				case c.user == "":
					want = http.StatusUnauthorized
				case kind != "db" && canEdit == "none" && refStorage(m.Add, sp.Add, c) == "none":
					want = http.StatusForbidden // may neither add nor edit: nothing about the key is told
				case kind != "db" && !seesEntry:
					want = http.StatusConflict // a key or path the caller cannot see is taken
				case canEdit == "none":
					want = http.StatusForbidden
				case canEdit == "own" && !isAuthor:
					want = http.StatusNotFound
				default:
					want = http.StatusOK
				}
				editMethod, editBody, editType := http.MethodPut, []byte(`{"value":2}`), "application/json"
				switch kind {
				case "db":
					editMethod, editBody = http.MethodPatch, []byte(`{"v":"changed"}`)
				case "files":
					editBody, editType = []byte("changed"), "text/plain"
				}
				if rec := w.siteReq(c, site, editMethod, item, editBody, editType); rec.Code != want {
					t.Errorf("%s edit = %d %s, want %d", label, rec.Code, rec.Body, want)
				}
				// Delete vera's entry.
				item, _ = seed()
				canDelete := refStorage(m.Delete, sp.Delete, c)
				switch {
				case c.user == "":
					want = http.StatusUnauthorized
				case canDelete == "none":
					want = http.StatusForbidden
				case canDelete == "own" && !isAuthor:
					want = http.StatusNotFound
				default:
					want = http.StatusNoContent
				}
				if rec := w.siteReq(c, site, http.MethodDelete, item, nil, ""); rec.Code != want {
					t.Errorf("%s delete = %d %s, want %d", label, rec.Code, rec.Body, want)
				}
				checked++
			}
		}
	}
	if checked != 7*3*len(storageCallers) {
		t.Fatalf("checked %d combinations", checked)
	}
}

func (w *accessWorld) settings(user, method, site, name string, body any) *httptest.ResponseRecorder {
	return w.api(user, method, "/api/collaboration/sites/alice/"+site+"/storage/resources/"+name, body)
}

func TestStorageSettingsRoutes(t *testing.T) {
	w := newAccessWorld(t)
	w.storageSite("shop", db.AccessCompany)

	// The four rules, by code.
	for body, rule := range map[string]string{
		`{"kind":"kv","preset":"board","edit":"anyone"}`:                                        "anonymous_change",
		`{"kind":"kv","preset":"personal","add":"own"}`:                                         "add_own",
		`{"kind":"kv","preset":"records","add":"anyone"}`:                                       "own_needs_signed_in_add",
		`{"kind":"kv","preset":"private","edit":"signed-in"}`:                                   "blind_change",
		`{"kind":"kv","preset":"everyone"}`:                                                     "unknown_preset",
		`{"kind":"db","tables":{"t":{"preset":"wall","edit":"anyone","columns":{"v":"text"}}}}`: "anonymous_change",
	} {
		rec := w.settings("alice", http.MethodPut, "shop", "x", json.RawMessage(body))
		var out map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != http.StatusBadRequest || out["rule"] != rule {
			t.Errorf("%s = %d %s, want 400 %s", body, rec.Code, rec.Body, rule)
		}
	}
	for body, code := range map[string]string{
		`{"preset":"public"}`:                                    "invalid_kind",
		`{"kind":"db","preset":"public"}`:                        "invalid_tables",
		`{"kind":"kv","tables":{}}`:                              "invalid_tables",
		`{"kind":"db","tables":{"t":{"columns":{"id":"text"}}}}`: "invalid_columns",
		`{"kind":"db","tables":{"t":{"columns":{"p":{"type":"integer","references":"nope"}}}}}`: "invalid_columns",
	} {
		rec := w.settings("alice", http.MethodPut, "shop", "x", json.RawMessage(body))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), code) {
			t.Errorf("%s = %d %s, want 400 %s", body, rec.Code, rec.Body, code)
		}
	}
	// Create, then a kind cannot change.
	if rec := w.settings("alice", http.MethodPut, "shop", "menu", map[string]any{"kind": "kv", "preset": "public"}); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if rec := w.settings("alice", http.MethodPut, "shop", "menu", map[string]any{"kind": "files"}); rec.Code != http.StatusConflict {
		t.Errorf("kind change = %d", rec.Code)
	}
	// A change keeps what it does not state: an override on public.
	rec := w.settings("alice", http.MethodPut, "shop", "menu", map[string]any{"read": "signed-in"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"preset":"custom"`) || !strings.Contains(rec.Body.String(), `"base":"public"`) {
		t.Errorf("override = %d %s", rec.Code, rec.Body)
	}
	// A colleague, and a publish key, cannot change settings.
	if rec := w.settings("olly", http.MethodPut, "shop", "menu", map[string]any{"preset": "board"}); rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
		t.Errorf("colleague settings = %d", rec.Code)
	}
	publish := "key-alice-publish-" + strings.Repeat("2", 32)
	if _, err := db.CreateAPIKey(context.Background(), w.database, w.users["alice"], "ci", db.HashAPIKey(publish), publish[:8], "", time.Now().Add(time.Hour), db.APIKeyScopePublish); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPut, "https://"+accessBase+"/api/collaboration/sites/alice/shop/storage/resources/menu", strings.NewReader(`{"preset":"board"}`))
	r.Header.Set("X-API-Key", publish)
	r.Header.Set("X-Simple-Host-Client", "control-ui")
	if rec := w.do(r); rec.Code != http.StatusForbidden {
		t.Errorf("publish key settings = %d", rec.Code)
	}
	// A publish key reads storage but never changes it: an entry keeps no
	// history, so its change could not be undone.
	site := "shop.alice." + accessBase
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodGet, "/api/site/storage/kv/menu/keys", "", http.StatusOK},
		{http.MethodPut, "/api/site/storage/kv/menu/keys/k", `{"value":1}`, http.StatusForbidden},
		{http.MethodPut, "/api/sites/shop/storage/kv/menu/keys/k", `{"value":1}`, http.StatusForbidden},
		{http.MethodDelete, "/api/site/storage/kv/menu/keys/k", "", http.StatusForbidden},
		{http.MethodPut, "/api/sites/shop/state/versioned", `{"version":0,"state":{}}`, http.StatusOK},
	} {
		r := httptest.NewRequest(tc.method, "https://"+site+tc.path, strings.NewReader(tc.body))
		r.Header.Set("X-API-Key", publish)
		r.Header.Set("Content-Type", "application/json")
		if rec := w.do(r); rec.Code != tc.want {
			t.Errorf("publish key %s %s = %d %s, want %d", tc.method, tc.path, rec.Code, rec.Body, tc.want)
		}
	}

	// A database: a table left out goes with its rows.
	tables := map[string]any{
		"products": map[string]any{"preset": "public", "columns": map[string]any{"name": "text"}},
		"orders":   map[string]any{"preset": "records", "columns": map[string]any{"product_id": map[string]any{"type": "integer", "references": "products"}, "qty": "integer"}},
		"scratch":  map[string]any{"columns": map[string]any{"x": "text"}},
	}
	if rec := w.settings("alice", http.MethodPut, "shop", "shop", map[string]any{"kind": "db", "tables": tables}); rec.Code != http.StatusCreated {
		t.Fatalf("create db = %d %s", rec.Code, rec.Body)
	}
	owner := storageTestCaller{"owner-tool", "alice", true}
	if rec := w.siteReq(owner, "shop", http.MethodPost, "/api/site/storage/db/shop/tables/scratch/rows", []byte(`{"x":"y"}`), "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("scratch row = %d %s", rec.Code, rec.Body)
	}
	delete(tables, "scratch")
	rec = w.settings("alice", http.MethodPut, "shop", "shop", map[string]any{"tables": tables})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"removed_tables":["scratch"]`) {
		t.Errorf("drop table = %d %s", rec.Code, rec.Body)
	}
	var left int
	_ = w.database.QueryRow(`SELECT count(*) FROM site_storage_rows WHERE table_name = 'scratch'`).Scan(&left)
	if left != 0 {
		t.Errorf("%d scratch rows left", left)
	}

	// The audit log has the settings and the row.
	var n int
	_ = w.database.QueryRow(`SELECT count(*) FROM audit_events WHERE action IN ('storage_resource_set', 'storage_add')`).Scan(&n)
	if n < 5 {
		t.Errorf("%d storage audit rows", n)
	}

	// The list answers usage and limits.
	list := w.api("alice", http.MethodGet, "/api/collaboration/sites/alice/shop/storage", nil)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"data_max_bytes":10000000`) {
		t.Errorf("list = %d %s", list.Code, list.Body)
	}
	// Delete.
	if rec := w.settings("alice", http.MethodDelete, "shop", "shop", nil); rec.Code != http.StatusNoContent {
		t.Errorf("delete = %d %s", rec.Code, rec.Body)
	}
	_ = w.database.QueryRow(`SELECT count(*) FROM site_storage_rows`).Scan(&left)
	if left != 0 {
		t.Errorf("%d rows left after delete", left)
	}
}

func TestStorageTablesReferencesColumnsAndLimits(t *testing.T) {
	w := newAccessWorld(t)
	w.storageSite("shop", db.AccessCompany)
	tables := map[string]any{
		"products": map[string]any{"preset": "public", "columns": map[string]any{"name": map[string]any{"type": "text", "required": true}, "price": "integer"}},
		"orders": map[string]any{"preset": "records", "columns": map[string]any{
			"product_id": map[string]any{"type": "integer", "references": "products"}, "qty": "integer", "status": "text"}},
		"notes": map[string]any{"preset": "records", "columns": map[string]any{
			"order_id": map[string]any{"type": "integer", "references": "orders"}, "text": "text"}},
	}
	if rec := w.settings("alice", http.MethodPut, "shop", "shop", map[string]any{"kind": "db", "tables": tables}); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	owner := storageTestCaller{"owner-tool", "alice", true}
	vera := storageTestCaller{"author", "vera", false}
	olly := storageTestCaller{"colleague", "olly", false}
	rows := func(t string) string { return "/api/site/storage/db/shop/tables/" + t + "/rows" }
	post := func(c storageTestCaller, table, body string) (int, map[string]any) {
		rec := w.siteReq(c, "shop", http.MethodPost, rows(table), []byte(body), "application/json")
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	code, product := post(owner, "products", `{"name":"bread","price":400}`)
	if code != http.StatusCreated {
		t.Fatalf("product = %d %v", code, product)
	}
	pid := int64(product["id"].(float64))
	// Values are checked against their columns; the server's are refused.
	for body, want := range map[string]int{
		`{"price":400}`:                     http.StatusBadRequest, // name is required
		`{"name":"x","price":"4"}`:          http.StatusBadRequest,
		`{"name":"x","colour":"red"}`:       http.StatusBadRequest,
		`{"name":"x","added_by":"someone"}`: http.StatusBadRequest,
		`{"name":"x","id":7}`:               http.StatusBadRequest,
	} {
		if code, out := post(owner, "products", body); code != want {
			t.Errorf("products %s = %d %v, want %d", body, code, out, want)
		}
	}
	// A visitor orders an existing product; a missing one is invalid.
	code, order := post(vera, "orders", fmt.Sprintf(`{"product_id":%d,"qty":2,"status":"new"}`, pid))
	if code != http.StatusCreated || order["mine"] != true {
		t.Fatalf("order = %d %v", code, order)
	}
	oid := int64(order["id"].(float64))
	if code, _ := post(vera, "orders", `{"product_id":99999,"qty":1}`); code != http.StatusNotFound {
		t.Errorf("missing product = %d", code)
	}
	// olly cannot point at vera's order (own read): the same answer as a
	// missing row.
	if code, out := post(olly, "notes", fmt.Sprintf(`{"order_id":%d,"text":"mine now"}`, oid)); code != http.StatusNotFound || out["code"] != "invalid_reference" {
		t.Errorf("reference to another's order = %d %v", code, out)
	}
	if code, _ := post(vera, "notes", fmt.Sprintf(`{"order_id":%d,"text":"ring the bell"}`, oid)); code != http.StatusCreated {
		t.Errorf("reference to own order = %d", code)
	}
	// A referenced row cannot go.
	if rec := w.siteReq(owner, "shop", http.MethodDelete, fmt.Sprintf("%s/%d", rows("products"), pid), nil, ""); rec.Code != http.StatusConflict {
		t.Errorf("delete referenced product = %d %s", rec.Code, rec.Body)
	}
	// The owner on the site sees every order with who placed it and sets a
	// status; vera sees it.
	ownerPage := storageTestCaller{"owner-on-site", "alice", false}
	rec := w.siteReq(ownerPage, "shop", http.MethodGet, rows("orders")+"?where.status=new", nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"added_by":"vera"`) {
		t.Errorf("owner list = %d %s", rec.Code, rec.Body)
	}
	if rec := w.siteReq(ownerPage, "shop", http.MethodPatch, fmt.Sprintf("%s/%d", rows("orders"), oid), []byte(`{"status":"sent"}`), "application/json"); rec.Code != http.StatusOK {
		t.Errorf("owner status = %d %s", rec.Code, rec.Body)
	}
	if rec := w.siteReq(vera, "shop", http.MethodGet, fmt.Sprintf("%s/%d", rows("orders"), oid), nil, ""); !strings.Contains(rec.Body.String(), `"status":"sent"`) || strings.Contains(rec.Body.String(), "added_by") {
		t.Errorf("vera reads her order = %s", rec.Body)
	}
	// Filters: equality on declared columns only, at most three.
	for q, want := range map[string]int{
		"?where.qty=2":              http.StatusOK,
		"?where.nope=1":             http.StatusBadRequest,
		"?where.qty=two":            http.StatusBadRequest,
		"?order=qty&desc=1&limit=5": http.StatusOK,
		"?order=data":               http.StatusBadRequest,
		"?limit=0":                  http.StatusBadRequest,
		"?sql=select":               http.StatusBadRequest,
	} {
		if rec := w.siteReq(ownerPage, "shop", http.MethodGet, rows("orders")+q, nil, ""); rec.Code != want {
			t.Errorf("list %s = %d %s, want %d", q, rec.Code, rec.Body, want)
		}
	}
	// A page never reaches nobody, even the owner's.
	if rec := w.settings("alice", http.MethodPut, "shop", "vault", map[string]any{"kind": "kv", "preset": "private", "read": "nobody", "add": "nobody", "edit": "nobody", "delete": "nobody"}); rec.Code != http.StatusCreated {
		t.Fatalf("vault = %d %s", rec.Code, rec.Body)
	}
	if rec := w.siteReq(ownerPage, "shop", http.MethodGet, "/api/site/storage/kv/vault/keys", nil, ""); rec.Code != http.StatusForbidden {
		t.Errorf("owner page on nobody = %d", rec.Code)
	}
	if rec := w.siteReq(owner, "shop", http.MethodPut, "/api/site/storage/kv/vault/keys/k", []byte(`{"value":1}`), "application/json"); rec.Code != http.StatusCreated {
		t.Errorf("owner tool on nobody = %d %s", rec.Code, rec.Body)
	}
	// The site's data limit.
	prev := oplimits.Get()
	t.Cleanup(func() { oplimits.Set(prev) })
	small := prev
	small.SiteStorageMaxBytes = 2000
	oplimits.Set(small)
	big := `{"value":"` + strings.Repeat("x", 3000) + `"}`
	if rec := w.siteReq(owner, "shop", http.MethodPut, "/api/site/storage/kv/vault/keys/big", []byte(big), "application/json"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over the limit = %d %s", rec.Code, rec.Body)
	}
	oplimits.Set(prev)
	// The listing tells the page who it is.
	rec = w.siteReq(ownerPage, "shop", http.MethodGet, "/api/site/storage", nil, "")
	if !strings.Contains(rec.Body.String(), `"site_owner":true`) {
		t.Errorf("owner you = %s", rec.Body)
	}
	rec = w.siteReq(vera, "shop", http.MethodGet, "/api/site/storage", nil, "")
	if !strings.Contains(rec.Body.String(), `"site_owner":false`) {
		t.Errorf("vera you = %s", rec.Body)
	}
	// Bytes are counted and freed.
	var bytesNow int64
	_ = w.database.QueryRow(`SELECT bytes FROM site_storage_resources WHERE name = 'shop'`).Scan(&bytesNow)
	if bytesNow <= 0 {
		t.Errorf("bytes = %d", bytesNow)
	}
}

func TestStorageFilesServeAndQuota(t *testing.T) {
	w := newAccessWorldWith(t, UploadQuota{MaxBytes: 4000}, nil)
	w.storageSite("gallery", db.AccessCompany)
	if rec := w.settings("alice", http.MethodPut, "gallery", "photos", map[string]any{"kind": "files", "preset": "wall"}); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	vera := storageTestCaller{"author", "vera", false}
	olly := storageTestCaller{"colleague", "olly", false}
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 100)...)
	rec := w.siteReq(vera, "gallery", http.MethodPut, "/api/site/storage/files/photos/objects/2026/cake.png", png, "image/png")
	if rec.Code != http.StatusCreated {
		t.Fatalf("put = %d %s", rec.Code, rec.Body)
	}
	rec = w.siteReq(olly, "gallery", http.MethodGet, "/api/site/storage/files/photos/objects/2026/cake.png", nil, "")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || !bytes.Equal(rec.Body.Bytes(), png) {
		t.Errorf("get = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	// HTML is never stored.
	if rec := w.siteReq(vera, "gallery", http.MethodPut, "/api/site/storage/files/photos/objects/x.html", []byte("<html><script>alert(1)</script>"), "text/plain"); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("html = %d %s", rec.Code, rec.Body)
	}
	// Bad paths.
	for _, p := range []string{"a//b", "a/../b", "./a"} {
		if rec := w.siteReq(vera, "gallery", http.MethodPut, "/api/site/storage/files/photos/objects/"+p, png, "image/png"); rec.Code == http.StatusCreated {
			t.Errorf("path %q accepted", p)
		}
	}
	// olly cannot delete vera's photo (wall: own); vera can.
	if rec := w.siteReq(olly, "gallery", http.MethodDelete, "/api/site/storage/files/photos/objects/2026/cake.png", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("olly delete = %d", rec.Code)
	}
	// The owner's quota counts stored files.
	bigPNG := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 5000)...)
	if rec := w.siteReq(vera, "gallery", http.MethodPut, "/api/site/storage/files/photos/objects/big.png", bigPNG, "image/png"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("over quota = %d %s", rec.Code, rec.Body)
	}
	usage, err := db.OwnerUsageOf(context.Background(), w.database, w.users["alice"])
	if err != nil || usage.Bytes != int64(len(png)) {
		t.Errorf("owner usage = %+v %v", usage, err)
	}
	if rec := w.siteReq(vera, "gallery", http.MethodDelete, "/api/site/storage/files/photos/objects/2026/cake.png", nil, ""); rec.Code != http.StatusNoContent {
		t.Errorf("vera delete = %d", rec.Code)
	}
	var queued int
	_ = w.database.QueryRow(`SELECT count(*) FROM storage_retired`).Scan(&queued)
	if queued == 0 {
		t.Error("the deleted file's object was not queued for the sweep")
	}
}

// Sessions, origins, kinds and hosts keep storage to its own site.
func TestStorageCrossSite(t *testing.T) {
	w := newAccessWorld(t)
	w.storageSite("one", db.AccessCompany)
	w.storageSite("two", db.AccessCompany)
	if rec := w.settings("alice", http.MethodPut, "one", "board", map[string]any{"kind": "kv", "preset": "board"}); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	vera := storageTestCaller{"author", "vera", false}
	// A session minted for site two does not work on site one.
	host := "one.alice." + accessBase
	r := httptest.NewRequest(http.MethodPut, "https://"+host+"/api/site/storage/kv/board/keys/k", strings.NewReader(`{"value":1}`))
	r.AddCookie(w.cookie("vera", "two.alice."+accessBase))
	r.Header.Set("Origin", "https://"+host)
	if rec := w.do(r); rec.Code != http.StatusUnauthorized {
		t.Errorf("other site's session = %d", rec.Code)
	}
	// A write from another origin is refused.
	r = httptest.NewRequest(http.MethodPut, "https://"+host+"/api/site/storage/kv/board/keys/k", strings.NewReader(`{"value":1}`))
	r.AddCookie(w.cookie("vera", host))
	r.Header.Set("Origin", "https://two.alice."+accessBase)
	if rec := w.do(r); rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin write = %d", rec.Code)
	}
	// Site two has no such resource.
	if rec := w.siteReq(vera, "two", http.MethodGet, "/api/site/storage/kv/board/keys", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("other site's resource = %d", rec.Code)
	}
	// A key of the wrong kind is not found.
	if rec := w.siteReq(vera, "one", http.MethodGet, "/api/site/storage/files/board/objects", nil, ""); rec.Code != http.StatusNotFound {
		t.Errorf("wrong kind = %d", rec.Code)
	}
	// The base host never answers the storage routes.
	r = httptest.NewRequest(http.MethodGet, "https://"+accessBase+"/api/sites/one/storage", nil)
	r.Header.Set("X-API-Key", w.apiKeys["alice"])
	if rec := w.do(r); rec.Code == http.StatusOK {
		t.Errorf("base host storage = %d", rec.Code)
	}
}

// Review round 1: the byte count matches what Postgres stores, whatever the
// edits; a number that grows when stored counts at its stored size; a table
// re-sent without a preset keeps its own; a removed column's values go; the
// listing shows a non-owner only what they may use.
func TestStorageReviewFixes(t *testing.T) {
	w := newAccessWorld(t)
	w.storageSite("shop", db.AccessCompany)
	tables := map[string]any{
		"products": map[string]any{"preset": "public", "columns": map[string]any{"name": "text", "secret": "text"}},
		"orders":   map[string]any{"preset": "records", "columns": map[string]any{"note": "json"}},
	}
	if rec := w.settings("alice", http.MethodPut, "shop", "shop", map[string]any{"kind": "db", "preset": "board", "tables": tables}); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	vera := storageTestCaller{"author", "vera", false}
	olly := storageTestCaller{"colleague", "olly", false}
	owner := storageTestCaller{"owner-tool", "alice", true}
	rows := "/api/site/storage/db/shop/tables/orders/rows"
	counted := func() (int64, int64) {
		var c, actual int64
		_ = w.database.QueryRow(`SELECT bytes FROM site_storage_resources WHERE name = 'shop'`).Scan(&c)
		_ = w.database.QueryRow(`SELECT COALESCE(sum(octet_length(data::text)), 0) FROM site_storage_rows WHERE resource = 'shop'`).Scan(&actual)
		return c, actual
	}
	rec := w.siteReq(vera, "shop", http.MethodPost, rows, []byte(`{"note":{"a":1,"b":[1,2,3],"c":"x"}}`), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", rec.Code, rec.Body)
	}
	var row map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &row)
	id := int64(row["id"].(float64))
	for i := 0; i < 5; i++ {
		owned := storageTestCaller{"owner-on-site", "alice", false}
		if rec := w.siteReq(owned, "shop", http.MethodPatch, fmt.Sprintf("%s/%d", rows, id), []byte(`{"note":{"a":1,"b":[1,2,3],"c":"x"}}`), "application/json"); rec.Code != http.StatusOK {
			t.Fatalf("edit = %d %s", rec.Code, rec.Body)
		}
	}
	if c, actual := counted(); c != actual || c == 0 {
		t.Errorf("counted %d bytes, stored %d", c, actual)
	}
	// 1e100000 is 8 bytes sent and 100001 digits stored: refused before
	// the database builds it. 1e300 stays within range and is stored at its
	// full 301 digits, counted at that size.
	if rec := w.siteReq(vera, "shop", http.MethodPost, rows, []byte(`{"note":[1e100000]}`), "application/json"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_number") {
		t.Errorf("amplified number = %d %s", rec.Code, rec.Body)
	}
	if rec := w.siteReq(vera, "shop", http.MethodPost, rows, []byte(`{"note":[1e300,1E-300]}`), "application/json"); rec.Code != http.StatusCreated {
		t.Errorf("in-range exponents = %d %s", rec.Code, rec.Body)
	}
	if rec := w.siteReq(owner, "shop", http.MethodPut, "/api/site/storage/kv/nope/keys/k", []byte(`{"value":1e999}`), "application/json"); rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
		t.Errorf("kv exponent = %d", rec.Code)
	}
	// The stored-size limit itself: a row whose stored form passes the
	// value limit is refused after the write and rolled back.
	prev := oplimits.Get()
	small := prev
	small.SiteStorageValueMaxBytes = 1024
	oplimits.Set(small)
	big := `{"note":[` + strings.TrimSuffix(strings.Repeat("1e300,", 6), ",") + `]}`
	if rec := w.siteReq(vera, "shop", http.MethodPost, rows, []byte(big), "application/json"); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("stored size over the value limit = %d %s", rec.Code, rec.Body)
	}
	oplimits.Set(prev)
	if c, actual := counted(); c != actual {
		t.Errorf("after the refused add: counted %d, stored %d", c, actual)
	}
	// Re-sending the tables without presets keeps each table's own, and
	// dropping a column clears its values.
	if rec := w.siteReq(owner, "shop", http.MethodPost, "/api/site/storage/db/shop/tables/products/rows", []byte(`{"name":"bread","secret":"cost 2"}`), "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("product = %d %s", rec.Code, rec.Body)
	}
	resend := map[string]any{
		"products": map[string]any{"columns": map[string]any{"name": "text"}},
		"orders":   map[string]any{"columns": map[string]any{"note": "json"}},
	}
	rec = w.settings("alice", http.MethodPut, "shop", "shop", map[string]any{"tables": resend})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"cleared_columns":{"products":["secret"]}`) {
		t.Fatalf("resend = %d %s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	tbls := out["resource"].(map[string]any)["tables"].(map[string]any)
	if tbls["orders"].(map[string]any)["preset"] != "records" || tbls["products"].(map[string]any)["preset"] != "public" {
		t.Errorf("tables lost their presets: %v", tbls)
	}
	if c, actual := counted(); c != actual {
		t.Errorf("after clearing a column: counted %d, stored %d", c, actual)
	}
	resend["products"] = map[string]any{"columns": map[string]any{"name": "text", "secret": "boolean"}}
	if rec := w.settings("alice", http.MethodPut, "shop", "shop", map[string]any{"tables": resend}); rec.Code != http.StatusOK {
		t.Fatalf("re-add = %d %s", rec.Code, rec.Body)
	}
	rec = w.siteReq(olly, "shop", http.MethodGet, "/api/site/storage/db/shop/tables/products/rows", nil, "")
	if !strings.Contains(rec.Body.String(), `"secret":null`) {
		t.Errorf("a re-added column shows old values: %s", rec.Body)
	}
	// The listing: olly does not see a private resource at all.
	if rec := w.settings("alice", http.MethodPut, "shop", "costs", map[string]any{"kind": "kv", "preset": "private"}); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body)
	}
	rec = w.siteReq(olly, "shop", http.MethodGet, "/api/site/storage", nil, "")
	if strings.Contains(rec.Body.String(), `"costs"`) || !strings.Contains(rec.Body.String(), `"shop"`) {
		t.Errorf("olly's listing = %s", rec.Body)
	}
	rec = w.siteReq(storageTestCaller{"owner-on-site", "alice", false}, "shop", http.MethodGet, "/api/site/storage", nil, "")
	if !strings.Contains(rec.Body.String(), `"costs"`) {
		t.Errorf("owner's listing = %s", rec.Body)
	}
}
