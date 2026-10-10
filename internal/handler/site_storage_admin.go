package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oplimits"
	sp "github.com/vsriram/simple-host/internal/storagepolicy"
)

// Site storage, the owner's settings (docs/designs/storage-presets.md):
// base-host routes, owner or team member only, the shape of the asset
// routes in assets_admin.go. The data itself is read and changed through the
// site-facing routes (site_storage.go), where the owner's tools bypass the
// matrix.

func (h *SiteHandler) registerStorageAdminRoutes(mux *http.ServeMux, ownerMutation func(http.Handler) http.Handler, browserWrite func(http.Handler) http.Handler) {
	mux.Handle("GET /api/collaboration/sites/{owner}/{sitename}/storage", ownerMutation(http.HandlerFunc(h.listStorageResources)))
	mux.Handle("PUT /api/collaboration/sites/{owner}/{sitename}/storage/resources/{name}", browserWrite(ownerMutation(http.HandlerFunc(h.putStorageResource))))
	mux.Handle("DELETE /api/collaboration/sites/{owner}/{sitename}/storage/resources/{name}", browserWrite(ownerMutation(http.HandlerFunc(h.deleteStorageResource))))
}

type storageResourceAdminView struct {
	storageResourceView
	Bytes int64 `json:"bytes"`
}

func (h *SiteHandler) listStorageResources(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok || !requireOwnerRole(w, access) {
		return
	}
	list, err := db.ListStorageResources(r.Context(), h.database, access.Site.ID)
	if err != nil {
		storageInternal(w, "list resources", err)
		return
	}
	data, files, err := db.SiteStorageBytes(r.Context(), h.database, access.Site.ID)
	if err != nil {
		storageInternal(w, "usage", err)
		return
	}
	out := make([]storageResourceAdminView, 0, len(list))
	for _, res := range list {
		out = append(out, storageResourceAdminView{storageView(res), res.Bytes})
	}
	op := oplimits.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"resources": out,
		"usage": map[string]int64{
			"data_bytes": data, "data_max_bytes": int64(op.SiteStorageMaxBytes),
			"files_bytes": files, "files_max_bytes": int64(op.SiteStorageFilesMaxBytes),
			"file_max_bytes": int64(op.SiteStorageFileMaxBytes), "value_max_bytes": int64(op.SiteStorageValueMaxBytes),
			"max_resources": int64(op.SiteStorageMaxResources),
		},
		"presets": sp.Presets,
	})
}

type storageTableRequest struct {
	sp.Spec
	Columns map[string]sp.Column `json:"columns"`
}

type storageResourceRequest struct {
	Kind string `json:"kind"`
	sp.Spec
	Tables map[string]storageTableRequest `json:"tables"`
}

func invalidAccess(w http.ResponseWriter, e *sp.RuleError, where string) {
	msg := e.Hint
	if where != "" {
		msg = where + ": " + msg
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg, "code": "invalid_access", "rule": e.Code})
}

func (h *SiteHandler) putStorageResource(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !sp.ResourceNamePattern.MatchString(name) {
		writeStorageError(w, http.StatusBadRequest, "invalid_name", "a resource name is 1 to 63 lowercase letters, digits, - and _, starting with a letter or digit")
		return
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok || !requireOwnerRole(w, access) {
		return
	}
	var req storageResourceRequest
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeStorageError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return
	}

	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		storageInternal(w, "begin resource", err)
		return
	}
	defer audit.Rollback(tx)
	if err := db.LockSiteStorage(r.Context(), tx, access.Site.ID); err != nil {
		storageInternal(w, "lock", err)
		return
	}
	existing, err := db.GetStorageResource(r.Context(), tx, access.Site.ID, name)
	exists := err == nil
	if err != nil && !errors.Is(err, db.ErrStorageNotFound) {
		storageInternal(w, "resource lookup", err)
		return
	}
	kind := req.Kind
	if kind == "" && exists {
		kind = existing.Kind
	}
	switch kind {
	case "kv", "db", "files":
	case "":
		writeStorageError(w, http.StatusBadRequest, "invalid_kind", "kind is required: kv, db or files")
		return
	default:
		writeStorageError(w, http.StatusBadRequest, "invalid_kind", "kind must be kv, db or files")
		return
	}
	if exists && kind != existing.Kind {
		writeStorageError(w, http.StatusConflict, "kind_mismatch", "this resource is "+existing.Kind+"; a kind cannot change (delete it and create it again)")
		return
	}
	if !exists {
		n, err := db.CountStorageResources(r.Context(), tx, access.Site.ID)
		if err != nil {
			storageInternal(w, "count resources", err)
			return
		}
		if n >= oplimits.Get().SiteStorageMaxResources {
			writeStorageError(w, http.StatusConflict, "too_many_resources", "a site can have at most "+oplimits.Count(oplimits.Get().SiteStorageMaxResources)+" storage resources")
			return
		}
	}

	// The resource's own matrix: on a change, what is not stated stays.
	fallback := sp.Default()
	if exists {
		fallback = sp.Resolved{Matrix: existing.Access, Preset: existing.Preset, Base: existing.Settings.Base}
	}
	resolved, ruleErr := sp.Resolve(req.Spec, fallback)
	if ruleErr != nil {
		invalidAccess(w, ruleErr, "")
		return
	}
	res := db.StorageResource{SiteID: access.Site.ID, Name: name, Kind: kind, Preset: resolved.Preset, Access: resolved.Matrix, Settings: db.StorageSettings{Base: resolved.Base}}

	var removed []string
	stripped := map[string][]string{}
	if kind == "db" {
		if len(req.Tables) == 0 {
			writeStorageError(w, http.StatusBadRequest, "invalid_tables", `a database needs its tables: {"tables": {"orders": {"preset": "records", "columns": {"item": "text"}}}}`)
			return
		}
		if len(req.Tables) > sp.MaxTables {
			writeStorageError(w, http.StatusBadRequest, "invalid_tables", "a database has at most 20 tables")
			return
		}
		names := map[string]bool{}
		for t := range req.Tables {
			if !sp.ValidIdentifier(t) {
				writeStorageError(w, http.StatusBadRequest, "invalid_tables", "table "+t+": use lowercase letters, digits and _ (starting with a letter)")
				return
			}
			names[t] = true
		}
		res.Settings.Tables = map[string]db.StorageTable{}
		for _, t := range sp.SortedKeys(req.Tables) {
			spec := req.Tables[t]
			if err := sp.CheckColumns(t, spec.Columns, names); err != nil {
				writeStorageError(w, http.StatusBadRequest, "invalid_columns", err.Error())
				return
			}
			// An existing table keeps its own matrix unless this change
			// states one; a new table starts from the database's.
			fallback := resolved
			if old, ok := existing.Settings.Tables[t]; exists && ok {
				fallback = sp.Resolved{Matrix: old.Access, Preset: old.Preset, Base: old.Base}
			}
			tr, ruleErr := sp.Resolve(spec.Spec, fallback)
			if ruleErr != nil {
				invalidAccess(w, ruleErr, "table "+t)
				return
			}
			res.Settings.Tables[t] = db.StorageTable{Preset: tr.Preset, Base: tr.Base, Access: tr.Matrix, Columns: spec.Columns}
		}
		if exists {
			for t, old := range existing.Settings.Tables {
				if !names[t] {
					removed = append(removed, t)
					continue
				}
				// A column removed or given another type loses its stored
				// values, so one added back later starts empty.
				for col, c := range old.Columns {
					if now, ok := res.Settings.Tables[t].Columns[col]; !ok || now.Type != c.Type || now.References != c.References {
						stripped[t] = append(stripped[t], col)
					}
				}
			}
		}
	} else if req.Tables != nil {
		writeStorageError(w, http.StatusBadRequest, "invalid_tables", "only a db resource has tables")
		return
	}

	created, err := db.PutStorageResource(r.Context(), tx, res)
	if err != nil {
		storageInternal(w, "put resource", err)
		return
	}
	for _, t := range removed {
		freed, err := db.DeleteStorageTableRows(r.Context(), tx, access.Site.ID, name, t)
		if err != nil {
			storageInternal(w, "drop table rows", err)
			return
		}
		if err := db.AddStorageBytes(r.Context(), tx, access.Site.ID, name, -freed); err != nil {
			storageInternal(w, "table bytes", err)
			return
		}
	}
	for _, t := range sp.SortedKeys(stripped) {
		freed, err := db.StripStorageColumns(r.Context(), tx, access.Site.ID, name, t, stripped[t])
		if err != nil {
			storageInternal(w, "strip columns", err)
			return
		}
		if err := db.AddStorageBytes(r.Context(), tx, access.Site.ID, name, -freed); err != nil {
			storageInternal(w, "column bytes", err)
			return
		}
	}
	actorID := ""
	if user := auth.GetUser(r.Context()); user != nil {
		actorID = user.ID
	}
	actorKind, keyID := auditActorKind(r.Context())
	extra := map[string]any{"resource": name, "kind": kind, "preset": res.Preset, "access": res.Access, "created": created}
	if kind == "db" {
		tables := map[string]any{}
		for t, tt := range res.Settings.Tables {
			tables[t] = map[string]any{"preset": tt.Preset, "access": tt.Access}
		}
		extra["tables"] = tables
		if len(removed) > 0 {
			extra["removed_tables"] = removed
		}
		if len(stripped) > 0 {
			extra["cleared_columns"] = stripped
		}
	}
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: actorID, ActorKind: actorKind, KeyID: keyID, Action: "storage_resource_set",
		OwnerID: access.OwnerID, SiteID: access.Site.ID, RequestID: auditRequestID(r.Context()), Extra: extra,
	}); err != nil {
		storageInternal(w, "audit resource", err)
		return
	}
	if err := audit.Commit(tx); err != nil {
		storageInternal(w, "commit resource", err)
		return
	}
	saved, err := db.GetStorageResource(r.Context(), h.database, access.Site.ID, name)
	if err != nil {
		storageInternal(w, "reread resource", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	out := map[string]any{"resource": storageResourceAdminView{storageView(saved), saved.Bytes}, "created": created}
	if len(removed) > 0 {
		out["removed_tables"] = removed
	}
	if len(stripped) > 0 {
		out["cleared_columns"] = stripped
	}
	notes := storageNotes(saved)
	if kind == "db" && exists && req.Spec != (sp.Spec{}) {
		// The database's own access only seeds new tables: say which
		// existing ones kept theirs, so a change meant for all is not
		// silently partial.
		for _, t := range sp.SortedKeys(req.Tables) {
			if spec := req.Tables[t].Spec; spec == (sp.Spec{}) {
				if _, existed := existing.Settings.Tables[t]; existed {
					notes = append(notes, "table "+t+" kept its own access ("+saved.Settings.Tables[t].Preset+"); the database's preset applies to new tables only, so name a preset on this table to change it")
				}
			}
		}
	}
	if len(notes) > 0 {
		out["notes"] = notes
	}
	writeJSON(w, status, out)
}

// storageNotes says, in plain words, what the saved matrix means for pages,
// so an agent reads it back before it builds on it.
func storageNotes(res db.StorageResource) []string {
	var notes []string
	check := func(where string, m sp.Matrix) {
		if m.Add == sp.Anyone {
			notes = append(notes, where+"add is anyone: every colleague who can open the site may add; anonymous visitors to a network-approved site still cannot")
		}
		if m.Read == sp.Nobody && m.Add == sp.Nobody && m.Edit == sp.Nobody && m.Delete == sp.Nobody {
			notes = append(notes, where+"every action is nobody: pages cannot use it at all, only your own tools")
		}
	}
	if res.Kind == "db" {
		for _, t := range sp.SortedKeys(res.Settings.Tables) {
			check("table "+t+": ", res.Settings.Tables[t].Access)
		}
	} else {
		check("", res.Access)
	}
	return notes
}

func (h *SiteHandler) deleteStorageResource(w http.ResponseWriter, r *http.Request) {
	ownerUsername, siteName, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !sp.ResourceNamePattern.MatchString(name) {
		writeStorageError(w, http.StatusNotFound, "no_resource", "no such storage resource")
		return
	}
	access, ok := h.resolveCollaborationAccess(w, r, ownerUsername, siteName)
	if !ok || !requireOwnerRole(w, access) {
		return
	}
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		storageInternal(w, "begin delete resource", err)
		return
	}
	defer audit.Rollback(tx)
	if err := db.LockSiteStorage(r.Context(), tx, access.Site.ID); err != nil {
		storageInternal(w, "lock", err)
		return
	}
	existing, err := db.GetStorageResource(r.Context(), tx, access.Site.ID, name)
	if errors.Is(err, db.ErrStorageNotFound) {
		writeStorageError(w, http.StatusNotFound, "no_resource", "no such storage resource")
		return
	}
	if err != nil {
		storageInternal(w, "resource lookup", err)
		return
	}
	objects, err := db.DeleteStorageResource(r.Context(), tx, access.Site.ID, name)
	if err != nil {
		storageInternal(w, "delete resource", err)
		return
	}
	for _, id := range objects {
		if err := retireAssetObject(r.Context(), tx, access.Site.ID, id); err != nil {
			storageInternal(w, "retire file", err)
			return
		}
	}
	actorID := ""
	if user := auth.GetUser(r.Context()); user != nil {
		actorID = user.ID
	}
	actorKind, keyID := auditActorKind(r.Context())
	if err := h.audit.RecordTx(r.Context(), tx, audit.Event{
		ActorID: actorID, ActorKind: actorKind, KeyID: keyID, Action: "storage_resource_delete",
		OwnerID: access.OwnerID, SiteID: access.Site.ID, RequestID: auditRequestID(r.Context()),
		Extra: map[string]any{"resource": name, "kind": existing.Kind, "bytes": existing.Bytes, "files": len(objects)},
	}); err != nil {
		storageInternal(w, "audit delete resource", err)
		return
	}
	if err := audit.Commit(tx); err != nil {
		log.Printf("site storage: commit delete %s/%s %s: %v", ownerUsername, siteName, name, err)
		writeStorageError(w, http.StatusInternalServerError, "internal", "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
