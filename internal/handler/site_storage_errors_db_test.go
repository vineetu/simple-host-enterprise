package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oplimits"
)

// Every storage refusal says what happened and what to do, with a stable
// code. The owner sees the declared names; a visitor never sees the tables
// or columns of a table they cannot read.
func TestStorageErrorsSayWhatAndWhatToDo(t *testing.T) {
	w := newAccessWorld(t)
	w.storageSite("shop", db.AccessNetwork)
	tables := map[string]any{
		"customers": map[string]any{"preset": "board", "columns": map[string]any{
			"email": map[string]any{"type": "text", "required": true}, "name": map[string]any{"type": "text", "required": true},
			"age": "integer", "parent_id": map[string]any{"type": "integer", "references": "parents"}}},
		"parents":      map[string]any{"preset": "private", "columns": map[string]any{"name": "text"}},
		"leads":        map[string]any{"preset": "inbox", "columns": map[string]any{"note": map[string]any{"type": "text", "required": true}}},
		"menu":         map[string]any{"preset": "public", "columns": map[string]any{"item": "text"}},
		"topics":       map[string]any{"preset": "wall", "columns": map[string]any{"title": "text"}},
		"secret_notes": map[string]any{"preset": "private", "columns": map[string]any{"topic_id": map[string]any{"type": "integer", "references": "topics"}}},
	}
	if rec := w.settings("alice", http.MethodPut, "shop", "shop", map[string]any{"kind": "db", "tables": tables}); rec.Code != http.StatusCreated {
		t.Fatalf("create db = %d %s", rec.Code, rec.Body)
	}
	if rec := w.settings("alice", http.MethodPut, "shop", "vault", map[string]any{"kind": "kv", "preset": "private", "read": "nobody", "add": "nobody", "edit": "nobody", "delete": "nobody"}); rec.Code != http.StatusCreated {
		t.Fatalf("create kv = %d %s", rec.Code, rec.Body)
	}
	if rec := w.settings("alice", http.MethodPut, "shop", "photos", map[string]any{"kind": "files", "preset": "wall"}); rec.Code != http.StatusCreated {
		t.Fatalf("create files = %d %s", rec.Code, rec.Body)
	}
	tool := storageTestCaller{"owner-tool", "alice", true}
	ownerPage := storageTestCaller{"owner-on-site", "alice", false}
	olly := storageTestCaller{"colleague", "olly", false}
	vera := storageTestCaller{"author", "vera", false}
	anon := storageTestCaller{"anonymous", "", false}
	rows := func(table string) string { return "/api/site/storage/db/shop/tables/" + table + "/rows" }

	check := func(label string, rec *httptest.ResponseRecorder, status int, code string, has []string, hasNot []string) {
		t.Helper()
		var out storageError
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code != status || out.Code != code {
			t.Errorf("%s = %d %s, want %d %s", label, rec.Code, rec.Body, status, code)
			return
		}
		for _, s := range has {
			if !strings.Contains(out.Error, s) {
				t.Errorf("%s: %q does not say %q", label, out.Error, s)
			}
		}
		// Never Postgres text, SQL, or internal paths.
		for _, s := range append(hasNot, "pq:", "SQLSTATE", "SELECT", "jsonb", "sites/", "internal") {
			if strings.Contains(out.Error, s) {
				t.Errorf("%s: %q says %q", label, out.Error, s)
			}
		}
	}
	post := func(c storageTestCaller, table, body string) *httptest.ResponseRecorder {
		return w.siteReq(c, "shop", http.MethodPost, rows(table), []byte(body), "application/json")
	}

	// Unknown table: the owner sees the tables, a visitor does not.
	check("owner unknown table", w.siteReq(tool, "shop", http.MethodGet, rows("orders"), nil, ""), http.StatusNotFound, "no_table",
		[]string{"database shop has no table named orders", "Tables here: customers, leads, menu, parents, secret_notes, topics"}, nil)
	check("visitor unknown table", w.siteReq(olly, "shop", http.MethodGet, rows("orders"), nil, ""), http.StatusNotFound, "no_table",
		[]string{"no table named orders", "GET .../storage lists the tables you may use"}, []string{"Tables here", "parents", "secret_notes"})

	// Unknown resource: the owner sees what there is.
	check("owner unknown resource", w.siteReq(tool, "shop", http.MethodGet, "/api/site/storage/kv/nope/keys", nil, ""), http.StatusNotFound, "no_resource",
		[]string{"no kv storage named nope", "Storage here: ", "shop (db)", "vault (kv)", "storage_set_resource"}, nil)
	check("visitor unknown resource", w.siteReq(olly, "shop", http.MethodGet, "/api/site/storage/kv/nope/keys", nil, ""), http.StatusNotFound, "no_resource",
		[]string{"no kv storage named nope", "its owner creates it first"}, []string{"vault", "shop (db)"})
	check("bad route", w.siteReq(tool, "shop", http.MethodGet, "/api/site/storage/db/shop/tables/customers/bogus", nil, ""), http.StatusNotFound, "not_found",
		[]string{"no such storage route: use kv/{name}/keys/{key}"}, nil)

	// Unknown column: the owner and readers see the columns; a visitor who
	// cannot read the table does not.
	check("owner unknown column", post(tool, "customers", `{"email":"a@x","name":"A","phone":"1"}`), http.StatusBadRequest, "unknown_column",
		[]string{"customers has no column named phone", "Columns in customers: age, email, name, parent_id"}, nil)
	check("reader unknown column", post(olly, "customers", `{"email":"a@x","name":"A","phone":"1"}`), http.StatusBadRequest, "unknown_column",
		[]string{"Columns in customers: age, email, name, parent_id"}, nil)
	check("inbox visitor unknown column", post(olly, "leads", `{"note":"hi","phone":"1"}`), http.StatusBadRequest, "unknown_column",
		[]string{"leads has no column named phone", "ask the site's owner to declare it"}, []string{"Columns in", "note"})
	check("where unknown column", w.siteReq(olly, "shop", http.MethodGet, rows("customers")+"?where.phone=1", nil, ""), http.StatusBadRequest, "unknown_column",
		[]string{"where.phone: customers has no column named phone", "Columns in customers"}, nil)
	check("where wrong type", w.siteReq(olly, "shop", http.MethodGet, rows("customers")+"?where.age=two", nil, ""), http.StatusBadRequest, "invalid_query",
		[]string{`where.age: age is integer, and "two" is not a valid integer value`}, nil)
	check("bad order", w.siteReq(olly, "shop", http.MethodGet, rows("customers")+"?order=phone", nil, ""), http.StatusBadRequest, "invalid_query",
		[]string{"order must be id, created_at, updated_at, or a column of customers", "Columns in customers"}, nil)
	check("unknown parameter", w.siteReq(olly, "shop", http.MethodGet, rows("customers")+"?sql=select", nil, ""), http.StatusBadRequest, "invalid_query",
		[]string{"unknown query parameter sql: use where.<column>, order, desc, limit, or offset"}, nil)

	// Wrong type, required, server columns.
	check("wrong type", post(tool, "customers", `{"email":"a@x","name":"A","age":"4"}`), http.StatusBadRequest, "invalid_value",
		[]string{"customers.age is integer: send a whole number such as 42, without quotes"}, nil)
	check("wrong type text", post(tool, "customers", `{"email":5,"name":"A"}`), http.StatusBadRequest, "invalid_value",
		[]string{`customers.email is text: send text in quotes`}, nil)
	check("required missing", post(tool, "customers", `{"email":"a@x"}`), http.StatusBadRequest, "required_missing",
		[]string{"customers.name needs a value (required): include it, and do not send null for it"}, nil)
	check("required null", post(tool, "customers", `{"email":"a@x","name":null}`), http.StatusBadRequest, "required_missing",
		[]string{"customers.name needs a value"}, nil)
	check("server column", post(tool, "customers", `{"email":"a@x","name":"A","id":7}`), http.StatusBadRequest, "server_column",
		[]string{"id is set by the server; leave it out"}, nil)

	// References: the owner sees the referenced table; a visitor who
	// cannot read it does not.
	check("owner missing reference", post(tool, "customers", `{"email":"a@x","name":"A","parent_id":99999}`), http.StatusNotFound, "invalid_reference",
		[]string{"customers.parent_id must be the id of an existing row of parents", "Add that row first"}, nil)
	check("visitor missing reference", post(olly, "customers", `{"email":"a@x","name":"A","parent_id":99999}`), http.StatusNotFound, "invalid_reference",
		[]string{"customers.parent_id must be the id of an existing row that you can read"}, []string{"of parents"})

	// A row other rows point at: the owner sees which; a visitor not
	// allowed to read the pointing table does not.
	var parent, topic map[string]any
	rec := post(tool, "parents", `{"name":"P"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &parent)
	if rec.Code != http.StatusCreated {
		t.Fatalf("parent = %d %s", rec.Code, rec.Body)
	}
	if rec := post(tool, "customers", fmt.Sprintf(`{"email":"a@x","name":"A","parent_id":%v}`, parent["id"])); rec.Code != http.StatusCreated {
		t.Fatalf("customer = %d %s", rec.Code, rec.Body)
	}
	check("owner referenced", w.siteReq(tool, "shop", http.MethodDelete, fmt.Sprintf("%s/%v", rows("parents"), parent["id"]), nil, ""), http.StatusConflict, "referenced",
		[]string{"other rows still point at this row (customers.parent_id holds its id): delete or change those rows first"}, nil)
	rec = post(vera, "topics", `{"title":"T"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &topic)
	if rec.Code != http.StatusCreated {
		t.Fatalf("topic = %d %s", rec.Code, rec.Body)
	}
	if rec := post(tool, "secret_notes", fmt.Sprintf(`{"topic_id":%v}`, topic["id"])); rec.Code != http.StatusCreated {
		t.Fatalf("secret note = %d %s", rec.Code, rec.Body)
	}
	check("visitor referenced", w.siteReq(vera, "shop", http.MethodDelete, fmt.Sprintf("%s/%v", rows("topics"), topic["id"]), nil, ""), http.StatusConflict, "referenced",
		[]string{"other rows still point at this row: delete or change those rows first"}, []string{"secret_notes", "topic_id"})

	// Rows, keys and files that are not there.
	check("no row", w.siteReq(olly, "shop", http.MethodGet, rows("customers")+"/99999", nil, ""), http.StatusNotFound, "not_found",
		[]string{"customers has no row 99999 that you can see: list its rows"}, nil)
	check("bad row id", w.siteReq(olly, "shop", http.MethodGet, rows("customers")+"/abc", nil, ""), http.StatusNotFound, "not_found",
		[]string{`a row id is a whole number from 1`, `"abc" is not one`}, nil)
	check("no key", w.siteReq(tool, "shop", http.MethodGet, "/api/site/storage/kv/vault/keys/theme", nil, ""), http.StatusNotFound, "not_found",
		[]string{`vault has no key "theme" that you can see`}, nil)
	check("no file", w.siteReq(olly, "shop", http.MethodGet, "/api/site/storage/files/photos/objects/missing.txt", nil, ""), http.StatusNotFound, "not_found",
		[]string{`photos has no file "missing.txt" that you can see`}, nil)

	// Refusals name the rule and its value.
	check("owner-only add", post(olly, "parents", `{"name":"x"}`), http.StatusForbidden, "forbidden",
		[]string{"the add rule of table parents is owner: only the site's owner or its team can add here"}, nil)
	// (An anonymous write never reaches storage: the host gate asks for
	// sign-in first, with the 401 every site API route answers.)
	check("anonymous read", w.siteReq(anon, "shop", http.MethodGet, rows("customers"), nil, ""), http.StatusUnauthorized, "sign_in_required",
		[]string{"sign in to read here: the read rule of table customers is signed-in", "?signin"}, nil)
	check("nobody read", w.siteReq(ownerPage, "shop", http.MethodGet, "/api/site/storage/kv/vault/keys", nil, ""), http.StatusForbidden, "forbidden",
		[]string{"the read rule of key-value storage vault is nobody: pages cannot read here, only the owner's own tools"}, nil)
	check("delete refused", w.siteReq(olly, "shop", http.MethodDelete, fmt.Sprintf("%s/%v", rows("customers"), parent["id"]), nil, ""), http.StatusForbidden, "forbidden",
		[]string{"the delete rule of table customers is owner"}, nil)

	// Body shapes.
	check("array body", post(tool, "customers", `[1]`), http.StatusBadRequest, "invalid_body",
		[]string{"the body must be one JSON object of column values"}, nil)
	check("broken body", post(tool, "customers", `{bad`), http.StatusBadRequest, "invalid_body",
		[]string{"the body is not valid JSON (at byte", "one JSON object of column values"}, nil)
	check("empty body", post(tool, "customers", ``), http.StatusBadRequest, "invalid_body",
		[]string{"the body is empty"}, nil)
	check("null body", post(tool, "customers", `null`), http.StatusBadRequest, "invalid_body",
		[]string{"the body must be one JSON object of column values"}, nil)
	check("kv no value", w.siteReq(tool, "shop", http.MethodPut, "/api/site/storage/kv/vault/keys/k", []byte(`{}`), "application/json"), http.StatusBadRequest, "invalid_body",
		[]string{`the body has no value: send {"value": <any JSON>}`}, nil)

	// Size limits.
	prev := oplimits.Get()
	t.Cleanup(func() { oplimits.Set(prev) })
	small := prev
	small.SiteStorageValueMaxBytes = 100
	oplimits.Set(small)
	big := strings.Repeat("x", 500)
	check("kv too large", w.siteReq(tool, "shop", http.MethodPut, "/api/site/storage/kv/vault/keys/k", []byte(`{"value":"`+big+`"}`), "application/json"), http.StatusRequestEntityTooLarge, "too_large",
		[]string{"the value is larger than 100 bytes: store less under one key"}, nil)
	check("row too large", post(tool, "menu", `{"item":"`+big+`"}`), http.StatusRequestEntityTooLarge, "too_large",
		[]string{"the row is larger than 100 bytes: shorten its values, or split them across rows"}, nil)
	small = prev
	small.SiteStorageMaxBytes = 10
	oplimits.Set(small)
	check("storage full", post(tool, "menu", `{"item":"a longer menu item"}`), http.StatusRequestEntityTooLarge, "storage_full",
		[]string{"would pass its 10-byte limit: delete entries it no longer needs"}, nil)
}
