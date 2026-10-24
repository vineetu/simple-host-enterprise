package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vsriram/simple-host/internal/audit"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oplimits"
	"github.com/vsriram/simple-host/internal/safepath"
	"github.com/vsriram/simple-host/internal/storage"
	sp "github.com/vsriram/simple-host/internal/storagepolicy"
)

// Site storage, the site-facing half (docs/designs/storage-presets.md): the
// routes under .../storage on a site's own host. The host gate has already
// established who is calling and that they may open the site; every route
// here then applies the resource's matrix to that caller. The settings
// routes are site_storage_admin.go.
//
// Answers that reveal nothing: an entry the caller may not read, or (for own
// edits and deletes) did not add, is 404 like an entry that does not exist.

const (
	storageListDefault = 50
	storageListMax     = 200
	storageMaxOffset   = 10_000
	storageKeyMaxBytes = 256
	storageMaxFilters  = 3
)

type storageError struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

func writeStorageError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, storageError{Error: message, Code: code})
}

func storageInternal(w http.ResponseWriter, what string, err error) {
	log.Printf("site storage: %s: %v", what, err)
	writeStorageError(w, http.StatusInternalServerError, "internal", "internal server error")
}

// storageCaller is who the matrix is applied to.
func (h *SiteAPIHandler) storageCaller(ctx context.Context, call siteAPICall) (sp.Caller, error) {
	if call.ActorUserID == "" {
		return sp.Caller{}, nil
	}
	owner, err := db.SiteOwnerOrMember(ctx, h.database, call.SiteID, call.ActorUserID)
	if err != nil {
		return sp.Caller{}, err
	}
	return sp.Caller{UserID: call.ActorUserID, Owner: owner, Tool: call.SessionID == ""}, nil
}

// refuse writes the answer for an action the caller may not take at all.
func refuseStorage(w http.ResponseWriter, c sp.Caller, a sp.Action, who sp.Who) {
	if c.UserID == "" {
		writeStorageError(w, http.StatusUnauthorized, "sign_in_required", "sign in to "+string(a)+" here: open the site with ?signin")
		return
	}
	switch who {
	case sp.Nobody:
		writeStorageError(w, http.StatusForbidden, "forbidden", "pages cannot "+string(a)+" here; only the owner's own tools can")
	default:
		writeStorageError(w, http.StatusForbidden, "forbidden", "only the site's owner can "+string(a)+" here")
	}
}

// onlyAddedBy is the person an own decision limits the statement to.
func onlyAddedBy(d sp.Decision, c sp.Caller) string {
	if d == sp.OwnOnly {
		return c.UserID
	}
	return ""
}

// Storage dispatches every route under .../storage. rest is what follows
// "storage" in the path ("" or "/kv/...").
func (h *SiteAPIHandler) Storage(w http.ResponseWriter, r *http.Request, call siteAPICall, rest string) {
	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	if write {
		if decision := h.limits.allowHTTP(w, stateClientPolicy, clientLimitKey(r)); !decision.Allowed {
			writeRateLimit(w, decision)
			return
		}
		if decision := h.limits.allowHTTP(w, stateSitePolicy, siteLimitKey(call.Owner, call.SiteName)); !decision.Allowed {
			writeRateLimit(w, decision)
			return
		}
	} else {
		if decision := h.limits.allowHTTP(w, stateReadClientPolicy, clientLimitKey(r)); !decision.Allowed {
			writeRateLimit(w, decision)
			return
		}
		if decision := h.limits.allowHTTP(w, stateReadSitePolicy, siteLimitKey(call.Owner, call.SiteName)); !decision.Allowed {
			writeRateLimit(w, decision)
			return
		}
	}
	caller, err := h.storageCaller(r.Context(), call)
	if err != nil {
		storageInternal(w, "caller", err)
		return
	}
	if rest == "" || rest == "/" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		h.listStorage(w, r, call, caller)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(rest, "/"), "/", 4)
	if len(parts) < 3 || !sp.ResourceNamePattern.MatchString(parts[1]) {
		writeStorageError(w, http.StatusNotFound, "not_found", "no such storage route")
		return
	}
	kind, name, sub := parts[0], parts[1], parts[2]
	tail := ""
	if len(parts) == 4 {
		tail = parts[3]
	}
	res, err := db.GetStorageResource(r.Context(), h.database, call.SiteID, name)
	if errors.Is(err, db.ErrStorageNotFound) || (err == nil && res.Kind != kind) {
		writeStorageError(w, http.StatusNotFound, "no_resource", "this site has no "+kind+" storage named "+name+"; its owner creates it first")
		return
	}
	if err != nil {
		storageInternal(w, "resource", err)
		return
	}
	switch {
	case kind == "kv" && sub == "keys":
		h.storageKV(w, r, call, caller, res, tail)
	case kind == "files" && sub == "objects":
		h.storageFiles(w, r, call, caller, res, tail)
	case kind == "db" && sub == "tables":
		table, rowsPart, _ := strings.Cut(tail, "/")
		rowsSeg, id, hasID := strings.Cut(rowsPart, "/")
		t, ok := res.Settings.Tables[table]
		if rowsSeg != "rows" || !ok || (hasID && id == "") {
			writeStorageError(w, http.StatusNotFound, "no_table", "database "+name+" has no table named "+table)
			return
		}
		h.storageRows(w, r, call, caller, res, table, t, id, hasID)
	default:
		writeStorageError(w, http.StatusNotFound, "not_found", "no such storage route")
	}
}

type storageResourceView struct {
	Name   string                     `json:"name"`
	Kind   string                     `json:"kind"`
	Preset string                     `json:"preset"`
	Base   string                     `json:"base,omitempty"`
	Access sp.Matrix                  `json:"access"`
	Tables map[string]storageTableOut `json:"tables,omitempty"`
}

type storageTableOut struct {
	Preset  string               `json:"preset"`
	Base    string               `json:"base,omitempty"`
	Access  sp.Matrix            `json:"access"`
	Columns map[string]sp.Column `json:"columns"`
}

func storageView(res db.StorageResource) storageResourceView {
	v := storageResourceView{Name: res.Name, Kind: res.Kind, Preset: res.Preset, Base: res.Settings.Base, Access: res.Access}
	if res.Kind == "db" {
		v.Tables = map[string]storageTableOut{}
		for name, t := range res.Settings.Tables {
			v.Tables[name] = storageTableOut{Preset: t.Preset, Base: t.Base, Access: t.Access, Columns: t.Columns}
		}
	}
	return v
}

func (h *SiteAPIHandler) listStorage(w http.ResponseWriter, r *http.Request, call siteAPICall, c sp.Caller) {
	list, err := db.ListStorageResources(r.Context(), h.database, call.SiteID)
	if err != nil {
		storageInternal(w, "list resources", err)
		return
	}
	// A caller who is not the owner sees only what they may act on: a
	// resource or table where every action is refused to them is left
	// out, so the list does not map the site's private data for them.
	usable := func(m sp.Matrix) bool {
		for _, a := range sp.Actions {
			if sp.Decide(m, a, c) != sp.Refused {
				return true
			}
		}
		return false
	}
	out := make([]storageResourceView, 0, len(list))
	for _, res := range list {
		v := storageView(res)
		if !c.Owner {
			if res.Kind == "db" {
				for name, t := range res.Settings.Tables {
					if !usable(t.Access) {
						delete(v.Tables, name)
					}
				}
				if len(v.Tables) == 0 {
					continue
				}
				// A visible column must not name a hidden table.
				for name, t := range v.Tables {
					cols := map[string]sp.Column{}
					for col, spec := range t.Columns {
						if _, shown := v.Tables[spec.References]; spec.References != "" && !shown {
							spec.References = ""
						}
						cols[col] = spec
					}
					t.Columns = cols
					v.Tables[name] = t
				}
			} else if !usable(res.Access) {
				continue
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resources": out,
		"you":       map[string]bool{"signed_in": c.UserID != "", "site_owner": c.Owner},
	})
}

// entryMeta is what every entry answers besides its value: mine for
// everyone signed in, and who added it for the owner only.
func entryMeta(out map[string]any, m db.StorageEntryMeta, c sp.Caller) {
	if c.UserID != "" {
		out["mine"] = m.AddedBy != "" && m.AddedBy == c.UserID
	}
	if c.Owner {
		out["added_by"] = m.AddedByUsername
	}
	out["created_at"] = m.CreatedAt.UTC().Format(time.RFC3339)
	if m.UpdatedAt != nil {
		out["updated_at"] = m.UpdatedAt.UTC().Format(time.RFC3339)
	} else {
		out["updated_at"] = nil
	}
}

func listPage(r *http.Request) (db.ListPage, string, bool) {
	q := r.URL.Query()
	limit := storageListDefault
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > storageListMax {
			return db.ListPage{}, "limit must be 1 to " + strconv.Itoa(storageListMax), false
		}
		limit = n
	}
	return db.ListPage{Prefix: q.Get("prefix"), After: q.Get("after"), Limit: limit}, "", true
}

// beginStorageWrite opens the write transaction and takes, in order, the
// writer's row, the owner's quota lock when files may grow, and the site's
// storage lock; then it re-reads the resource under the lock.
func (h *SiteAPIHandler) beginStorageWrite(ctx context.Context, call siteAPICall, name, kind string, quotaOwner bool) (*sql.Tx, db.StorageResource, string, error) {
	tx, err := h.database.BeginTx(ctx, nil)
	if err != nil {
		return nil, db.StorageResource{}, "", err
	}
	fail := func(err error) (*sql.Tx, db.StorageResource, string, error) {
		audit.Rollback(tx)
		return nil, db.StorageResource{}, "", err
	}
	if err := lockWriter(ctx, tx, call.ActorUserID); err != nil {
		return fail(err)
	}
	ownerID := ""
	if quotaOwner {
		// The quota is the site's owner's now, not the name the request
		// arrived under (a transfer may have moved the site since).
		if err := tx.QueryRowContext(ctx, `SELECT user_id::text FROM sites WHERE id = $1::uuid`, call.SiteID).Scan(&ownerID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fail(db.ErrStorageNotFound)
			}
			return fail(err)
		}
		if err := db.LockOwnerQuota(ctx, tx, ownerID); err != nil {
			return fail(err)
		}
	}
	if err := db.LockSiteStorage(ctx, tx, call.SiteID); err != nil {
		return fail(err)
	}
	res, err := db.GetStorageResource(ctx, tx, call.SiteID, name)
	if err != nil {
		return fail(err)
	}
	// Re-read under the lock: the resource may have been deleted and
	// created again as another kind since the request looked it up.
	if res.Kind != kind {
		return fail(db.ErrStorageNotFound)
	}
	return tx, res, ownerID, nil
}

func (h *SiteAPIHandler) commitStorage(w http.ResponseWriter, r *http.Request, tx *sql.Tx, call siteAPICall, action string, extra map[string]any) bool {
	if err := h.queueStorageNotification(r.Context(), tx, call, action, extra); err != nil {
		storageInternal(w, "queue notification", err)
		return false
	}

	if err := h.audit.RecordTx(r.Context(), tx, h.auditEvent(r.Context(), tx, call, action, extra)); err != nil {
		storageInternal(w, "audit "+action, err)
		return false
	}
	if err := audit.Commit(tx); err != nil {
		storageInternal(w, "commit "+action, err)
		return false
	}
	markSiteUsed(h.use, h.database, call.Owner, call.SiteName)
	return true
}

// checkDataRoom refuses a change that would take the site's key-value and
// table data past SITE_STORAGE_MAX_BYTES.
func checkDataRoom(ctx context.Context, w http.ResponseWriter, tx *sql.Tx, siteID string, delta int64) bool {
	if delta <= 0 {
		return true
	}
	data, _, err := db.SiteStorageBytes(ctx, tx, siteID)
	if err != nil {
		storageInternal(w, "usage", err)
		return false
	}
	limit := int64(oplimits.Get().SiteStorageMaxBytes)
	if data+delta > limit {
		writeStorageError(w, http.StatusRequestEntityTooLarge, "storage_full", "this site's saved data would pass its "+oplimits.Count(int(limit))+"-byte limit")
		return false
	}
	return true
}

// tooLargeStored refuses one value or row whose stored form passes
// SITE_STORAGE_VALUE_MAX_BYTES (measured after the write, inside the
// transaction the refusal rolls back).
func tooLargeStored(w http.ResponseWriter, stored int64) bool {
	if stored <= int64(oplimits.Get().SiteStorageValueMaxBytes)+storageKeyMaxBytes {
		return false
	}
	writeStorageError(w, http.StatusRequestEntityTooLarge, "too_large", "the value is larger than "+oplimits.Count(oplimits.Get().SiteStorageValueMaxBytes)+" bytes as stored")
	return true
}

// --- key-value ---

func validKVKey(key string) bool {
	if key == "" || len(key) > storageKeyMaxBytes || !utf8.ValidString(key) || strings.Contains(key, "/") {
		return false
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func (h *SiteAPIHandler) kvEntry(e db.StorageKV, c sp.Caller) map[string]any {
	out := map[string]any{"key": e.Key, "value": e.Value}
	entryMeta(out, e.StorageEntryMeta, c)
	return out
}

func (h *SiteAPIHandler) storageKV(w http.ResponseWriter, r *http.Request, call siteAPICall, c sp.Caller, res db.StorageResource, key string) {
	if key == "" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		d := sp.Decide(res.Access, sp.Read, c)
		if d == sp.Refused {
			refuseStorage(w, c, sp.Read, res.Access.Read)
			return
		}
		page, msg, ok := listPage(r)
		if !ok {
			writeStorageError(w, http.StatusBadRequest, "invalid_query", msg)
			return
		}
		page.OnlyAddedBy = onlyAddedBy(d, c)
		entries, err := db.ListStorageKV(r.Context(), h.database, call.SiteID, res.Name, page)
		if err != nil {
			storageInternal(w, "list kv", err)
			return
		}
		out := make([]map[string]any, 0, len(entries))
		for _, e := range entries {
			out = append(out, h.kvEntry(e, c))
		}
		var next any
		if len(entries) == page.Limit {
			next = entries[len(entries)-1].Key
		}
		writeJSON(w, http.StatusOK, map[string]any{"keys": out, "next_after": next})
		return
	}
	if !validKVKey(key) {
		writeStorageError(w, http.StatusBadRequest, "invalid_key", "a key is 1 to 256 bytes of text with no / or control characters")
		return
	}
	switch r.Method {
	case http.MethodGet:
		d := sp.Decide(res.Access, sp.Read, c)
		if d == sp.Refused {
			refuseStorage(w, c, sp.Read, res.Access.Read)
			return
		}
		e, err := db.GetStorageKV(r.Context(), h.database, call.SiteID, res.Name, key)
		if errors.Is(err, db.ErrStorageNotFound) || (err == nil && d == sp.OwnOnly && e.AddedBy != c.UserID) {
			writeStorageError(w, http.StatusNotFound, "not_found", "no such key")
			return
		}
		if err != nil {
			storageInternal(w, "get kv", err)
			return
		}
		writeJSON(w, http.StatusOK, h.kvEntry(e, c))
	case http.MethodPut:
		h.putKV(w, r, call, c, res.Name, key)
	case http.MethodDelete:
		h.deleteKV(w, r, call, c, res.Name, key)
	default:
		methodNotAllowed(w, "GET, PUT, DELETE")
	}
}

// boundedNumbers refuses a body whose numbers would grow past the value
// limit when stored. Postgres stores numbers exactly, so 1e100000 is 8 bytes
// sent and 100,001 digits built in the database, and many 1e-308s are each
// 6 bytes that become 310. Each exponent must be within ±308 (a float's
// range), and the digits the exponents add, summed over the body, must stay
// within SITE_STORAGE_VALUE_MAX_BYTES, so a small body cannot make the
// database build a large value before the stored-size check refuses it.
func boundedNumbers(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	budget := oplimits.Get().SiteStorageValueMaxBytes
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return true
		}
		if err != nil {
			return false
		}
		n, ok := tok.(json.Number)
		if !ok {
			continue
		}
		s := strings.ToLower(string(n))
		i := strings.IndexByte(s, 'e')
		if i < 0 {
			continue
		}
		exp, err := strconv.Atoi(strings.TrimPrefix(s[i+1:], "+"))
		if err != nil || exp > 308 || exp < -308 {
			return false
		}
		if exp < 0 {
			exp = -exp
		}
		if budget -= exp; budget < 0 {
			return false
		}
	}
}

func readStorageJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	limit := int64(oplimits.Get().SiteStorageValueMaxBytes) + 1024
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeStorageError(w, http.StatusRequestEntityTooLarge, "too_large", "the value is larger than "+oplimits.Count(oplimits.Get().SiteStorageValueMaxBytes)+" bytes")
			return false
		}
		writeStorageError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil || dec.More() {
		writeStorageError(w, http.StatusBadRequest, "invalid_body", "the body must be one JSON object")
		return false
	}
	if !boundedNumbers(body) {
		writeStorageError(w, http.StatusBadRequest, "invalid_number", "numbers written with exponents (1e300) would be too long as stored: write them out, each exponent within ±308")
		return false
	}
	return true
}

func (h *SiteAPIHandler) putKV(w http.ResponseWriter, r *http.Request, call siteAPICall, c sp.Caller, name, key string) {
	var body struct {
		Value json.RawMessage `json:"value"`
	}
	if !readStorageJSON(w, r, &body) {
		return
	}
	if len(body.Value) == 0 {
		writeStorageError(w, http.StatusBadRequest, "invalid_body", `send {"value": <any JSON>}`)
		return
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, body.Value); err != nil {
		writeStorageError(w, http.StatusBadRequest, "invalid_body", "value must be JSON")
		return
	}
	value := json.RawMessage(compact.Bytes())
	if len(value) > oplimits.Get().SiteStorageValueMaxBytes {
		writeStorageError(w, http.StatusRequestEntityTooLarge, "too_large", "the value is larger than "+oplimits.Count(oplimits.Get().SiteStorageValueMaxBytes)+" bytes")
		return
	}
	tx, res, _, err := h.beginStorageWrite(r.Context(), call, name, "kv", false)
	if err != nil {
		if errors.Is(err, db.ErrStorageNotFound) {
			writeStorageError(w, http.StatusNotFound, "no_resource", "this storage was just removed")
			return
		}
		storageInternal(w, "begin kv put", err)
		return
	}
	defer audit.Rollback(tx)
	// A caller who may neither add nor edit learns nothing about the key.
	if sp.Decide(res.Access, sp.Add, c) == sp.Refused && sp.Decide(res.Access, sp.Edit, c) == sp.Refused {
		refuseStorage(w, c, sp.Add, res.Access.Add)
		return
	}
	existing, err := db.GetStorageKV(r.Context(), tx, call.SiteID, name, key)
	exists := err == nil
	if err != nil && !errors.Is(err, db.ErrStorageNotFound) {
		storageInternal(w, "kv lookup", err)
		return
	}
	if !exists {
		if d := sp.Decide(res.Access, sp.Add, c); d == sp.Refused {
			refuseStorage(w, c, sp.Add, res.Access.Add)
			return
		}
		stored, err := db.InsertStorageKV(r.Context(), tx, call.SiteID, name, key, value, c.UserID)
		if err != nil {
			storageInternal(w, "kv insert", err)
			return
		}
		if tooLargeStored(w, stored) || !checkDataRoom(r.Context(), w, tx, call.SiteID, stored) {
			return
		}
		if err := db.AddStorageBytes(r.Context(), tx, call.SiteID, name, stored); err != nil {
			storageInternal(w, "kv bytes", err)
			return
		}
		if !h.commitStorage(w, r, tx, call, "storage_add", map[string]any{"resource": name, "kind": "kv", "key": key}) {
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"key": key, "added": true})
		return
	}
	// An edit: it may not be visible to the caller at all (read), and an
	// own edit is limited to their own key in the statement itself.
	read := sp.Decide(res.Access, sp.Read, c)
	d := sp.Decide(res.Access, sp.Edit, c)
	if read == sp.Refused || (read == sp.OwnOnly && existing.AddedBy != c.UserID) {
		// A key the caller cannot see is taken all the same.
		writeStorageError(w, http.StatusConflict, "key_taken", "this key is already used; pick another (random keys avoid this)")
		return
	}
	if d == sp.Refused {
		refuseStorage(w, c, sp.Edit, res.Access.Edit)
		return
	}
	stored, err := db.UpdateStorageKV(r.Context(), tx, call.SiteID, name, key, value, onlyAddedBy(d, c))
	if err != nil {
		if errors.Is(err, db.ErrStorageNotFound) {
			writeStorageError(w, http.StatusNotFound, "not_found", "no such key")
			return
		}
		storageInternal(w, "kv update", err)
		return
	}
	delta := stored - existing.Stored
	if tooLargeStored(w, stored) || !checkDataRoom(r.Context(), w, tx, call.SiteID, delta) {
		return
	}
	if err := db.AddStorageBytes(r.Context(), tx, call.SiteID, name, delta); err != nil {
		storageInternal(w, "kv bytes", err)
		return
	}
	if !h.commitStorage(w, r, tx, call, "storage_edit", map[string]any{"resource": name, "kind": "kv", "key": key}) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": key, "added": false})
}

func (h *SiteAPIHandler) deleteKV(w http.ResponseWriter, r *http.Request, call siteAPICall, c sp.Caller, name, key string) {
	tx, res, _, err := h.beginStorageWrite(r.Context(), call, name, "kv", false)
	if err != nil {
		if errors.Is(err, db.ErrStorageNotFound) {
			writeStorageError(w, http.StatusNotFound, "no_resource", "this storage was just removed")
			return
		}
		storageInternal(w, "begin kv delete", err)
		return
	}
	defer audit.Rollback(tx)
	d := sp.Decide(res.Access, sp.Delete, c)
	if d == sp.Refused {
		refuseStorage(w, c, sp.Delete, res.Access.Delete)
		return
	}
	existing, err := db.GetStorageKV(r.Context(), tx, call.SiteID, name, key)
	if errors.Is(err, db.ErrStorageNotFound) {
		writeStorageError(w, http.StatusNotFound, "not_found", "no such key")
		return
	}
	if err != nil {
		storageInternal(w, "kv lookup", err)
		return
	}
	if err := db.DeleteStorageKV(r.Context(), tx, call.SiteID, name, key, onlyAddedBy(d, c)); err != nil {
		if errors.Is(err, db.ErrStorageNotFound) {
			writeStorageError(w, http.StatusNotFound, "not_found", "no such key")
			return
		}
		storageInternal(w, "kv delete", err)
		return
	}
	if err := db.AddStorageBytes(r.Context(), tx, call.SiteID, name, -existing.Stored); err != nil {
		storageInternal(w, "kv bytes", err)
		return
	}
	if !h.commitStorage(w, r, tx, call, "storage_delete", map[string]any{"resource": name, "kind": "kv", "key": key}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- tables ---

func rowObject(row db.StorageRow, t db.StorageTable, c sp.Caller) map[string]any {
	out := map[string]any{"id": row.ID}
	var data map[string]json.RawMessage
	_ = json.Unmarshal(row.Data, &data)
	for col := range t.Columns {
		if v, ok := data[col]; ok {
			out[col] = v
		} else {
			out[col] = nil
		}
	}
	entryMeta(out, row.StorageEntryMeta, c)
	return out
}

func (h *SiteAPIHandler) storageRows(w http.ResponseWriter, r *http.Request, call siteAPICall, c sp.Caller, res db.StorageResource, table string, t db.StorageTable, idText string, hasID bool) {
	if !hasID {
		switch r.Method {
		case http.MethodGet:
			h.listRows(w, r, call, c, res, table, t)
		case http.MethodPost:
			if sp.Decide(t.Access, sp.Add, c) == sp.Refused {
				refuseStorage(w, c, sp.Add, t.Access.Add)
				return
			}
			replayCreate(w, r, h.database, c.UserID, call.SiteID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				h.addRow(w, r, call, c, res.Name, table)
			}))
		default:
			methodNotAllowed(w, "GET, POST")
		}
		return
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id < 1 || strconv.FormatInt(id, 10) != idText {
		writeStorageError(w, http.StatusNotFound, "not_found", "no such row")
		return
	}
	switch r.Method {
	case http.MethodGet:
		d := sp.Decide(t.Access, sp.Read, c)
		if d == sp.Refused {
			refuseStorage(w, c, sp.Read, t.Access.Read)
			return
		}
		row, err := db.GetStorageRow(r.Context(), h.database, call.SiteID, res.Name, table, id)
		if errors.Is(err, db.ErrStorageNotFound) || (err == nil && d == sp.OwnOnly && row.AddedBy != c.UserID) {
			writeStorageError(w, http.StatusNotFound, "not_found", "no such row")
			return
		}
		if err != nil {
			storageInternal(w, "get row", err)
			return
		}
		writeJSON(w, http.StatusOK, rowObject(row, t, c))
	case http.MethodPatch:
		h.editRow(w, r, call, c, res.Name, table, id)
	case http.MethodDelete:
		h.deleteRow(w, r, call, c, res.Name, table, id)
	default:
		methodNotAllowed(w, "GET, PATCH, DELETE")
	}
}

func (h *SiteAPIHandler) listRows(w http.ResponseWriter, r *http.Request, call siteAPICall, c sp.Caller, res db.StorageResource, table string, t db.StorageTable) {
	d := sp.Decide(t.Access, sp.Read, c)
	if d == sp.Refused {
		refuseStorage(w, c, sp.Read, t.Access.Read)
		return
	}
	q := r.URL.Query()
	rq := db.RowQuery{Limit: storageListDefault, OnlyAddedBy: onlyAddedBy(d, c)}
	for key, values := range q {
		switch key {
		case "order":
			o := values[0]
			if _, declared := t.Columns[o]; !declared && o != "id" && o != "created_at" && o != "updated_at" {
				writeStorageError(w, http.StatusBadRequest, "invalid_query", "order must be id, created_at, updated_at or a column of "+table)
				return
			}
			rq.Order = o
		case "desc":
			rq.Desc = values[0] == "1" || values[0] == "true"
		case "limit":
			n, err := strconv.Atoi(values[0])
			if err != nil || n < 1 || n > storageListMax {
				writeStorageError(w, http.StatusBadRequest, "invalid_query", "limit must be 1 to "+strconv.Itoa(storageListMax))
				return
			}
			rq.Limit = n
		case "offset":
			n, err := strconv.Atoi(values[0])
			if err != nil || n < 0 || n > storageMaxOffset {
				writeStorageError(w, http.StatusBadRequest, "invalid_query", "offset must be 0 to "+strconv.Itoa(storageMaxOffset))
				return
			}
			rq.Offset = n
		default:
			col, ok := strings.CutPrefix(key, "where.")
			if !ok {
				writeStorageError(w, http.StatusBadRequest, "invalid_query", "unknown query parameter "+key)
				return
			}
			spec, declared := t.Columns[col]
			if !declared {
				writeStorageError(w, http.StatusBadRequest, "invalid_query", "where."+col+": "+table+" has no such column")
				return
			}
			v, err := sp.FilterValue(col, spec, values[0])
			if err != nil {
				writeStorageError(w, http.StatusBadRequest, "invalid_query", err.Error())
				return
			}
			rq.Filters = append(rq.Filters, db.RowFilter{Column: col, Value: v})
		}
	}
	if len(rq.Filters) > storageMaxFilters {
		writeStorageError(w, http.StatusBadRequest, "invalid_query", "at most three where filters")
		return
	}
	rows, err := db.ListStorageRows(r.Context(), h.database, call.SiteID, res.Name, table, rq)
	if err != nil {
		storageInternal(w, "list rows", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, rowObject(row, t, c))
	}
	var next any
	if len(rows) == rq.Limit && rq.Offset+rq.Limit <= storageMaxOffset {
		next = rq.Offset + rq.Limit
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": out, "next_offset": next})
}

// readRowBody reads a row body. It runs before the write transaction opens,
// so a slow body never holds the site's storage lock or a connection.
func readRowBody(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	var values map[string]json.RawMessage
	if !readStorageJSON(w, r, &values) {
		return nil, false
	}
	if values == nil {
		writeStorageError(w, http.StatusBadRequest, "invalid_body", "the body must be one JSON object of column values")
		return nil, false
	}
	return values, true
}

// rowValues checks a row body against the table's columns, as read under the
// lock. With partial, required columns may be missing (an edit).
func rowValues(w http.ResponseWriter, values map[string]json.RawMessage, t db.StorageTable, partial bool) (map[string]json.RawMessage, bool) {
	for col, raw := range values {
		if sp.ServerColumns[col] {
			writeStorageError(w, http.StatusBadRequest, "server_column", col+" is set by the server; leave it out")
			return nil, false
		}
		spec, ok := t.Columns[col]
		if !ok {
			writeStorageError(w, http.StatusBadRequest, "unknown_column", "the table has no column named "+col)
			return nil, false
		}
		if err := sp.CheckValue(col, spec, raw); err != nil {
			writeStorageError(w, http.StatusBadRequest, "invalid_value", err.Error())
			return nil, false
		}
	}
	if !partial {
		for col, spec := range t.Columns {
			if _, ok := values[col]; spec.Required && !ok {
				writeStorageError(w, http.StatusBadRequest, "invalid_value", col+" is required")
				return nil, false
			}
		}
	}
	return values, true
}

// checkReferences makes sure every referenced row exists and the caller may
// read it under its own table's read value; otherwise 404 invalid_reference,
// the same answer for a row that is missing and one that is hidden.
func (h *SiteAPIHandler) checkReferences(w http.ResponseWriter, r *http.Request, tx *sql.Tx, call siteAPICall, c sp.Caller, res db.StorageResource, t db.StorageTable, values map[string]json.RawMessage) bool {
	for col, raw := range values {
		spec := t.Columns[col]
		if spec.References == "" || strings.TrimSpace(string(raw)) == "null" {
			continue
		}
		parentID, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		parent, ok := res.Settings.Tables[spec.References]
		if err != nil || !ok {
			writeStorageError(w, http.StatusNotFound, "invalid_reference", col+" names no row of "+spec.References)
			return false
		}
		row, err := db.GetStorageRow(r.Context(), tx, call.SiteID, res.Name, spec.References, parentID)
		if err != nil && !errors.Is(err, db.ErrStorageNotFound) {
			storageInternal(w, "reference", err)
			return false
		}
		d := sp.Decide(parent.Access, sp.Read, c)
		if err != nil || d == sp.Refused || (d == sp.OwnOnly && row.AddedBy != c.UserID) {
			writeStorageError(w, http.StatusNotFound, "invalid_reference", col+" names no row of "+spec.References)
			return false
		}
	}
	return true
}

func encodeRow(values map[string]json.RawMessage) (json.RawMessage, error) {
	clean := map[string]json.RawMessage{}
	for k, v := range values {
		var b bytes.Buffer
		if err := json.Compact(&b, v); err != nil {
			return nil, err
		}
		if b.String() == "null" {
			continue
		}
		clean[k] = b.Bytes()
	}
	return json.Marshal(clean)
}

func (h *SiteAPIHandler) addRow(w http.ResponseWriter, r *http.Request, call siteAPICall, c sp.Caller, name, table string) {
	body, ok := readRowBody(w, r)
	if !ok {
		return
	}
	tx, res, _, err := h.beginStorageWrite(r.Context(), call, name, "db", false)
	if err != nil {
		if errors.Is(err, db.ErrStorageNotFound) {
			writeStorageError(w, http.StatusNotFound, "no_resource", "this storage was just removed")
			return
		}
		storageInternal(w, "begin add row", err)
		return
	}
	defer audit.Rollback(tx)
	t, ok := res.Settings.Tables[table]
	if !ok {
		writeStorageError(w, http.StatusNotFound, "no_table", "database "+name+" has no table named "+table)
		return
	}
	if d := sp.Decide(t.Access, sp.Add, c); d == sp.Refused {
		refuseStorage(w, c, sp.Add, t.Access.Add)
		return
	}
	values, ok := rowValues(w, body, t, false)
	if !ok || !h.checkReferences(w, r, tx, call, c, res, t, values) {
		return
	}
	data, err := encodeRow(values)
	if err != nil {
		writeStorageError(w, http.StatusBadRequest, "invalid_body", "invalid JSON")
		return
	}
	if len(data) > oplimits.Get().SiteStorageValueMaxBytes {
		writeStorageError(w, http.StatusRequestEntityTooLarge, "too_large", "the row is larger than "+oplimits.Count(oplimits.Get().SiteStorageValueMaxBytes)+" bytes")
		return
	}
	row, err := db.InsertStorageRow(r.Context(), tx, call.SiteID, name, table, data, c.UserID)
	if err != nil {
		storageInternal(w, "insert row", err)
		return
	}
	if tooLargeStored(w, row.Stored) || !checkDataRoom(r.Context(), w, tx, call.SiteID, row.Stored) {
		return
	}
	if err := db.AddStorageBytes(r.Context(), tx, call.SiteID, name, row.Stored); err != nil {
		storageInternal(w, "row bytes", err)
		return
	}
	if !h.commitStorage(w, r, tx, call, "storage_add", map[string]any{"resource": name, "kind": "db", "table": table, "id": row.ID}) {
		return
	}
	writeJSON(w, http.StatusCreated, rowObject(row, t, c))
}

func (h *SiteAPIHandler) editRow(w http.ResponseWriter, r *http.Request, call siteAPICall, c sp.Caller, name, table string, id int64) {
	body, ok := readRowBody(w, r)
	if !ok {
		return
	}
	tx, res, _, err := h.beginStorageWrite(r.Context(), call, name, "db", false)
	if err != nil {
		if errors.Is(err, db.ErrStorageNotFound) {
			writeStorageError(w, http.StatusNotFound, "no_resource", "this storage was just removed")
			return
		}
		storageInternal(w, "begin edit row", err)
		return
	}
	defer audit.Rollback(tx)
	t, ok := res.Settings.Tables[table]
	if !ok {
		writeStorageError(w, http.StatusNotFound, "no_table", "database "+name+" has no table named "+table)
		return
	}
	d := sp.Decide(t.Access, sp.Edit, c)
	if d == sp.Refused {
		refuseStorage(w, c, sp.Edit, t.Access.Edit)
		return
	}
	row, err := db.GetStorageRow(r.Context(), tx, call.SiteID, name, table, id)
	if errors.Is(err, db.ErrStorageNotFound) || (err == nil && d == sp.OwnOnly && row.AddedBy != c.UserID) {
		writeStorageError(w, http.StatusNotFound, "not_found", "no such row")
		return
	}
	if err != nil {
		storageInternal(w, "row lookup", err)
		return
	}
	values, ok := rowValues(w, body, t, true)
	if !ok || !h.checkReferences(w, r, tx, call, c, res, t, values) {
		return
	}
	merged := map[string]json.RawMessage{}
	if err := json.Unmarshal(row.Data, &merged); err != nil {
		storageInternal(w, "row data", err)
		return
	}
	for k, v := range values {
		merged[k] = v
	}
	data, err := encodeRow(merged)
	if err != nil {
		writeStorageError(w, http.StatusBadRequest, "invalid_body", "invalid JSON")
		return
	}
	if len(data) > oplimits.Get().SiteStorageValueMaxBytes {
		writeStorageError(w, http.StatusRequestEntityTooLarge, "too_large", "the row is larger than "+oplimits.Count(oplimits.Get().SiteStorageValueMaxBytes)+" bytes")
		return
	}
	stored, err := db.UpdateStorageRow(r.Context(), tx, call.SiteID, name, table, id, data, onlyAddedBy(d, c))
	if err != nil {
		if errors.Is(err, db.ErrStorageNotFound) {
			writeStorageError(w, http.StatusNotFound, "not_found", "no such row")
			return
		}
		storageInternal(w, "update row", err)
		return
	}
	delta := stored - row.Stored
	if tooLargeStored(w, stored) || !checkDataRoom(r.Context(), w, tx, call.SiteID, delta) {
		return
	}
	if err := db.AddStorageBytes(r.Context(), tx, call.SiteID, name, delta); err != nil {
		storageInternal(w, "row bytes", err)
		return
	}
	updated, err := db.GetStorageRow(r.Context(), tx, call.SiteID, name, table, id)
	if err != nil {
		storageInternal(w, "row reread", err)
		return
	}
	if !h.commitStorage(w, r, tx, call, "storage_edit", map[string]any{"resource": name, "kind": "db", "table": table, "id": id}) {
		return
	}
	writeJSON(w, http.StatusOK, rowObject(updated, t, c))
}

func (h *SiteAPIHandler) deleteRow(w http.ResponseWriter, r *http.Request, call siteAPICall, c sp.Caller, name, table string, id int64) {
	tx, res, _, err := h.beginStorageWrite(r.Context(), call, name, "db", false)
	if err != nil {
		if errors.Is(err, db.ErrStorageNotFound) {
			writeStorageError(w, http.StatusNotFound, "no_resource", "this storage was just removed")
			return
		}
		storageInternal(w, "begin delete row", err)
		return
	}
	defer audit.Rollback(tx)
	t, ok := res.Settings.Tables[table]
	if !ok {
		writeStorageError(w, http.StatusNotFound, "no_table", "database "+name+" has no table named "+table)
		return
	}
	d := sp.Decide(t.Access, sp.Delete, c)
	if d == sp.Refused {
		refuseStorage(w, c, sp.Delete, t.Access.Delete)
		return
	}
	row, err := db.GetStorageRow(r.Context(), tx, call.SiteID, name, table, id)
	if errors.Is(err, db.ErrStorageNotFound) || (err == nil && d == sp.OwnOnly && row.AddedBy != c.UserID) {
		writeStorageError(w, http.StatusNotFound, "not_found", "no such row")
		return
	}
	if err != nil {
		storageInternal(w, "row lookup", err)
		return
	}
	for _, child := range sp.SortedKeys(res.Settings.Tables) {
		for _, col := range sp.SortedKeys(res.Settings.Tables[child].Columns) {
			if res.Settings.Tables[child].Columns[col].References != table {
				continue
			}
			referenced, err := db.StorageRowReferenced(r.Context(), tx, call.SiteID, name, child, col, id)
			if err != nil {
				storageInternal(w, "references", err)
				return
			}
			if referenced {
				writeStorageError(w, http.StatusConflict, "referenced", "rows of "+child+" still point at this row; delete them first")
				return
			}
		}
	}
	if err := db.DeleteStorageRow(r.Context(), tx, call.SiteID, name, table, id, onlyAddedBy(d, c)); err != nil {
		if errors.Is(err, db.ErrStorageNotFound) {
			writeStorageError(w, http.StatusNotFound, "not_found", "no such row")
			return
		}
		storageInternal(w, "delete row", err)
		return
	}
	if err := db.AddStorageBytes(r.Context(), tx, call.SiteID, name, -row.Stored); err != nil {
		storageInternal(w, "row bytes", err)
		return
	}
	if !h.commitStorage(w, r, tx, call, "storage_delete", map[string]any{"resource": name, "kind": "db", "table": table, "id": id}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- files ---

// validStoragePath is a file's path in its bucket: slash-separated
// segments, each a safe path segment.
func validStoragePath(p string) bool {
	if p == "" || len(p) > 512 {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if !safepath.IsSegment(seg) {
			return false
		}
	}
	return true
}

// fileURL is a file's address in the named form, which both the site's own
// host and the fallback owner host serve.
func (h *SiteAPIHandler) fileURL(call siteAPICall, name, path string) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	origin := h.siteURL(call)
	if u, err := url.Parse(origin); err == nil {
		origin = u.Scheme + "://" + u.Host
	}
	return origin + "/api/sites/" + url.PathEscape(call.SiteName) + "/storage/files/" + name + "/objects/" + strings.Join(parts, "/")
}

func (h *SiteAPIHandler) fileEntry(call siteAPICall, name string, f db.StorageFile, c sp.Caller) map[string]any {
	out := map[string]any{"path": f.Path, "content_type": f.ContentType, "size": f.Size, "url": h.fileURL(call, name, f.Path)}
	entryMeta(out, f.StorageEntryMeta, c)
	return out
}

func (h *SiteAPIHandler) storageFiles(w http.ResponseWriter, r *http.Request, call siteAPICall, c sp.Caller, res db.StorageResource, path string) {
	if path == "" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		d := sp.Decide(res.Access, sp.Read, c)
		if d == sp.Refused {
			refuseStorage(w, c, sp.Read, res.Access.Read)
			return
		}
		page, msg, ok := listPage(r)
		if !ok {
			writeStorageError(w, http.StatusBadRequest, "invalid_query", msg)
			return
		}
		page.OnlyAddedBy = onlyAddedBy(d, c)
		files, err := db.ListStorageFiles(r.Context(), h.database, call.SiteID, res.Name, page)
		if err != nil {
			storageInternal(w, "list files", err)
			return
		}
		out := make([]map[string]any, 0, len(files))
		for _, f := range files {
			out = append(out, h.fileEntry(call, res.Name, f, c))
		}
		var next any
		if len(files) == page.Limit {
			next = files[len(files)-1].Path
		}
		writeJSON(w, http.StatusOK, map[string]any{"files": out, "next_after": next})
		return
	}
	if !validStoragePath(path) {
		writeStorageError(w, http.StatusBadRequest, "invalid_path", "a path is up to 512 bytes of /-separated names, none empty, . or ..")
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		h.serveStorageFile(w, r, call, c, res, path)
	case http.MethodPut:
		h.putFile(w, r, call, c, res.Name, path)
	case http.MethodDelete:
		h.deleteFile(w, r, call, c, res.Name, path)
	default:
		methodNotAllowed(w, "GET, PUT, DELETE")
	}
}

func (h *SiteAPIHandler) serveStorageFile(w http.ResponseWriter, r *http.Request, call siteAPICall, c sp.Caller, res db.StorageResource, path string) {
	d := sp.Decide(res.Access, sp.Read, c)
	if d == sp.Refused {
		refuseStorage(w, c, sp.Read, res.Access.Read)
		return
	}
	f, err := db.GetStorageFile(r.Context(), h.database, call.SiteID, res.Name, path)
	if errors.Is(err, db.ErrStorageNotFound) || (err == nil && d == sp.OwnOnly && f.AddedBy != c.UserID) {
		writeStorageError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}
	if err != nil {
		storageInternal(w, "get file", err)
		return
	}
	openCtx, cancel := context.WithTimeout(r.Context(), siteOpenTimeout)
	defer cancel()
	lease, err := h.store.OpenAsset(openCtx, call.SiteID, f.ObjectID, max(f.Size, int64(oplimits.Get().SiteStorageFileMaxBytes)), f.SHA256)
	if err != nil {
		if errors.Is(err, storage.ErrAssetNotFound) {
			writeStorageError(w, http.StatusNotFound, "not_found", "no such file")
			return
		}
		log.Printf("site storage: open file %s/%s %s/%s: %v", call.Owner, call.SiteName, res.Name, path, err)
		http.Error(w, "file temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	defer lease.Close()
	w.Header().Set("Content-Type", f.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	name := path[strings.LastIndex(path, "/")+1:]
	if !isInlineAssetType(f.ContentType) {
		w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeAssetFilename(name)+`"`)
	}
	modified := f.CreatedAt
	if f.UpdatedAt != nil {
		modified = *f.UpdatedAt
	}
	http.ServeContent(w, r, name, modified, lease.File)
}

func (h *SiteAPIHandler) putFile(w http.ResponseWriter, r *http.Request, call siteAPICall, c sp.Caller, name, path string) {
	maxFile := int64(oplimits.Get().SiteStorageFileMaxBytes)
	r.Body = http.MaxBytesReader(w, r.Body, maxFile+1)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeStorageError(w, http.StatusRequestEntityTooLarge, "too_large", "a file is at most "+oplimits.Count(int(maxFile))+" bytes")
			return
		}
		writeStorageError(w, http.StatusBadRequest, "invalid_body", "invalid request body")
		return
	}
	// Refuse what the caller may not do before touching the bucket.
	res, err := db.GetStorageResource(r.Context(), h.database, call.SiteID, name)
	if err != nil {
		if errors.Is(err, db.ErrStorageNotFound) {
			writeStorageError(w, http.StatusNotFound, "no_resource", "this storage was just removed")
			return
		}
		storageInternal(w, "file resource", err)
		return
	}
	if sp.Decide(res.Access, sp.Add, c) == sp.Refused && sp.Decide(res.Access, sp.Edit, c) == sp.Refused {
		refuseStorage(w, c, sp.Add, res.Access.Add)
		return
	}
	if h.scanner != nil {
		if refusal := h.scanAsset(r, call, path, bytes.NewReader(body)); refusal != nil {
			refusal.write(w)
			return
		}
	}
	stored, err := h.store.CreateAsset(r.Context(), call.SiteID, r.Header.Get("Content-Type"), bytes.NewReader(body),
		storage.AssetLimits{MaxFileBytes: maxFile, MaxSiteBytes: 1 << 62, MaxSiteCount: 1 << 62})
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrAssetTooLarge):
			writeStorageError(w, http.StatusRequestEntityTooLarge, "too_large", "a file is at most "+oplimits.Count(int(maxFile))+" bytes")
		case errors.Is(err, storage.ErrAssetTypeNotAllowed):
			writeStorageError(w, http.StatusUnsupportedMediaType, "type_not_allowed", "this kind of file cannot be stored: images, audio, video, PDF, text, CSV, JSON, ZIP and gzip can")
		default:
			storageInternal(w, "store file", err)
		}
		return
	}
	keepObject := false
	defer func() {
		if !keepObject {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := h.store.DeleteAsset(ctx, call.SiteID, stored.ID); err != nil {
				log.Printf("site storage: discard uncommitted file object %s: %v", stored.ID, err)
			}
		}
	}()
	tx, res, ownerID, err := h.beginStorageWrite(r.Context(), call, name, "files", true)
	if err != nil {
		if errors.Is(err, db.ErrStorageNotFound) {
			writeStorageError(w, http.StatusNotFound, "no_resource", "this storage was just removed")
			return
		}
		storageInternal(w, "begin file put", err)
		return
	}
	defer audit.Rollback(tx)
	existing, err := db.GetStorageFile(r.Context(), tx, call.SiteID, name, path)
	exists := err == nil
	if err != nil && !errors.Is(err, db.ErrStorageNotFound) {
		storageInternal(w, "file lookup", err)
		return
	}
	f := db.StorageFile{Path: path, ObjectID: stored.ID, ContentType: stored.ContentType, Size: stored.Size, SHA256: stored.SHA256[:]}
	delta := stored.Size
	var d sp.Decision
	if exists {
		read := sp.Decide(res.Access, sp.Read, c)
		if read == sp.Refused || (read == sp.OwnOnly && existing.AddedBy != c.UserID) {
			writeStorageError(w, http.StatusConflict, "path_taken", "this path is already used; pick another (random names avoid this)")
			return
		}
		d = sp.Decide(res.Access, sp.Edit, c)
		if d == sp.Refused {
			refuseStorage(w, c, sp.Edit, res.Access.Edit)
			return
		}
		delta -= existing.Size
	} else if sp.Decide(res.Access, sp.Add, c) == sp.Refused {
		refuseStorage(w, c, sp.Add, res.Access.Add)
		return
	}
	if delta > 0 {
		_, files, err := db.SiteStorageBytes(r.Context(), tx, call.SiteID)
		if err != nil {
			storageInternal(w, "file usage", err)
			return
		}
		if files+delta > int64(oplimits.Get().SiteStorageFilesMaxBytes) {
			writeStorageError(w, http.StatusRequestEntityTooLarge, "storage_full", "this site's files would pass their "+oplimits.Count(oplimits.Get().SiteStorageFilesMaxBytes)+"-byte limit")
			return
		}
		if h.quota.MaxBytes > 0 {
			usage, err := db.OwnerUsageOf(r.Context(), tx, ownerID)
			if err != nil {
				storageInternal(w, "owner usage", err)
				return
			}
			if usage.Bytes+delta > h.quota.MaxBytes {
				storageQuotaRefusal(usage.Bytes, delta, h.quota.MaxBytes).write(w)
				return
			}
		}
	}
	action := "storage_add"
	if exists {
		action = "storage_edit"
		if err := db.UpdateStorageFile(r.Context(), tx, call.SiteID, name, f, onlyAddedBy(d, c)); err != nil {
			if errors.Is(err, db.ErrStorageNotFound) {
				writeStorageError(w, http.StatusNotFound, "not_found", "no such file")
				return
			}
			storageInternal(w, "update file", err)
			return
		}
		if err := retireAssetObject(r.Context(), tx, call.SiteID, existing.ObjectID); err != nil {
			storageInternal(w, "retire old file", err)
			return
		}
	} else if err := db.InsertStorageFile(r.Context(), tx, call.SiteID, name, f, c.UserID); err != nil {
		storageInternal(w, "insert file", err)
		return
	}
	if err := db.AddStorageBytes(r.Context(), tx, call.SiteID, name, delta); err != nil {
		storageInternal(w, "file bytes", err)
		return
	}
	if err := h.queueStorageNotification(r.Context(), tx, call, action, map[string]any{"resource": name, "kind": "files", "path": path}); err != nil {
		storageInternal(w, "queue notification", err)
		return
	}
	if err := h.audit.RecordTx(r.Context(), tx, h.auditEvent(r.Context(), tx, call, action, map[string]any{
		"resource": name, "kind": "files", "path": path, "size": stored.Size, "content_type": stored.ContentType,
	})); err != nil {
		storageInternal(w, "audit "+action, err)
		return
	}
	if err := audit.Commit(tx); err != nil {
		keepObject = true // the commit may have happened
		storageInternal(w, "commit "+action, err)
		return
	}
	keepObject = true
	markSiteUsed(h.use, h.database, call.Owner, call.SiteName)
	status := http.StatusCreated
	if exists {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"path": path, "added": !exists, "content_type": stored.ContentType, "size": stored.Size, "url": h.fileURL(call, name, path)})
}

func (h *SiteAPIHandler) deleteFile(w http.ResponseWriter, r *http.Request, call siteAPICall, c sp.Caller, name, path string) {
	tx, res, _, err := h.beginStorageWrite(r.Context(), call, name, "files", false)
	if err != nil {
		if errors.Is(err, db.ErrStorageNotFound) {
			writeStorageError(w, http.StatusNotFound, "no_resource", "this storage was just removed")
			return
		}
		storageInternal(w, "begin file delete", err)
		return
	}
	defer audit.Rollback(tx)
	d := sp.Decide(res.Access, sp.Delete, c)
	if d == sp.Refused {
		refuseStorage(w, c, sp.Delete, res.Access.Delete)
		return
	}
	existing, err := db.GetStorageFile(r.Context(), tx, call.SiteID, name, path)
	if errors.Is(err, db.ErrStorageNotFound) {
		writeStorageError(w, http.StatusNotFound, "not_found", "no such file")
		return
	}
	if err != nil {
		storageInternal(w, "file lookup", err)
		return
	}
	if err := db.DeleteStorageFile(r.Context(), tx, call.SiteID, name, path, onlyAddedBy(d, c)); err != nil {
		if errors.Is(err, db.ErrStorageNotFound) {
			writeStorageError(w, http.StatusNotFound, "not_found", "no such file")
			return
		}
		storageInternal(w, "delete file", err)
		return
	}
	if err := retireAssetObject(r.Context(), tx, call.SiteID, existing.ObjectID); err != nil {
		storageInternal(w, "retire file", err)
		return
	}
	if err := db.AddStorageBytes(r.Context(), tx, call.SiteID, name, -existing.Size); err != nil {
		storageInternal(w, "file bytes", err)
		return
	}
	if !h.commitStorage(w, r, tx, call, "storage_delete", map[string]any{"resource": name, "kind": "files", "path": path}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
