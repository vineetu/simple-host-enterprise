package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oplimits"
	"github.com/vsriram/simple-host/internal/safepath"
)

// Person settings use the same origin, authentication and audit transaction as site management.
func (h *SiteHandler) registerPersonHome(mux *http.ServeMux, owner func(http.Handler) http.Handler, browserWrite func(http.Handler) http.Handler) {
	mux.Handle("GET /api/me/home", owner(http.HandlerFunc(h.personHome)))
	mux.Handle("PUT /api/me/home", browserWrite(owner(http.HandlerFunc(h.personHome))))
	mux.Handle("GET /api/me/bio", owner(http.HandlerFunc(h.personBio)))
	mux.Handle("PUT /api/me/bio", browserWrite(owner(http.HandlerFunc(h.personBio))))
	mux.Handle("GET /api/sites/{sitename}/showcase", owner(http.HandlerFunc(h.personShowcaseSite)))
	mux.Handle("PUT /api/sites/{sitename}/showcase", browserWrite(owner(http.HandlerFunc(h.personShowcaseSite))))
}
func (h *SiteHandler) presentationRead(w http.ResponseWriter, r *http.Request) (db.PersonPresentation, bool) {
	u := auth.GetUser(r.Context())
	p, err := db.GetPersonPresentation(r.Context(), h.database, u.ID)
	if err != nil {
		writeJSON(w, 500, errorResponse{Error: "failed to load settings"})
		return p, false
	}
	return p, true
}
func (h *SiteHandler) presentationWrite(w http.ResponseWriter, r *http.Request, action string, change func(*sql.Tx) error) bool {
	u := auth.GetUser(r.Context())
	tx, err := h.database.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, 500, errorResponse{Error: "failed to save settings"})
		return false
	}
	defer audit.Rollback(tx)
	err = change(tx)
	if err == sql.ErrNoRows {
		writeJSON(w, 404, errorResponse{Error: "site not found"})
		return false
	}
	if err == nil {
		err = h.audit.RecordTx(r.Context(), tx, audit.Event{ActorID: u.ID, OwnerID: u.ID, Action: action, Extra: map[string]any{}})
	}
	if err == nil {
		err = audit.Commit(tx)
	}
	if err != nil {
		writeJSON(w, 500, errorResponse{Error: "failed to save settings"})
		return false
	}
	return true
}
func (h *SiteHandler) personHome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == "PUT" {
		var body map[string]json.RawMessage
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		if dec.Decode(&body) != nil || len(body) != 1 || body["site"] == nil {
			writeJSON(w, 400, errorResponse{Error: "send site as a name or null"})
			return
		}
		var name *string
		if json.Unmarshal(body["site"], &name) != nil || (name != nil && !safepath.IsSegment(*name)) {
			writeJSON(w, 400, errorResponse{Error: "send site as a name or null"})
			return
		}
		if !h.presentationWrite(w, r, "home_page", func(tx *sql.Tx) error { return db.SetPersonHome(r.Context(), tx, auth.GetUser(r.Context()).ID, name) }) {
			return
		}
	}
	if p, ok := h.presentationRead(w, r); ok {
		writeJSON(w, 200, map[string]any{"site": p.Home})
	}
}
func (h *SiteHandler) personBio(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	max := oplimits.Get().ShowcaseBioMaxLength
	if r.Method == "PUT" {
		var body struct {
			Bio *string `json:"bio"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
		dec.DisallowUnknownFields()
		if dec.Decode(&body) != nil || body.Bio == nil {
			writeJSON(w, 400, errorResponse{Error: "send a plain-text bio"})
			return
		}
		bio := strings.TrimSpace(*body.Bio)
		if utf8.RuneCountInString(bio) > max {
			writeJSON(w, 400, errorResponse{Error: "bio is too long", Code: "bio_too_long"})
			return
		}
		if !h.presentationWrite(w, r, "showcase_bio", func(tx *sql.Tx) error { return db.SetPersonBio(r.Context(), tx, auth.GetUser(r.Context()).ID, bio) }) {
			return
		}
	}
	if p, ok := h.presentationRead(w, r); ok {
		writeJSON(w, 200, map[string]any{"bio": p.Bio, "max_length": max})
	}
}
func (h *SiteHandler) personShowcaseSite(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	u := auth.GetUser(r.Context())
	name := r.PathValue("sitename")
	if r.Method == "PUT" {
		var body struct {
			Pinned *bool `json:"pinned"`
			Order  *int  `json:"order"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if dec.Decode(&body) != nil || (body.Pinned == nil && body.Order == nil) || (body.Order != nil && (*body.Order < 0 || *body.Order > 1000000)) {
			writeJSON(w, 400, errorResponse{Error: "send pinned and/or order (0 to 1000000)"})
			return
		}
		if !h.presentationWrite(w, r, "showcase_site", func(tx *sql.Tx) error {
			_, err := db.SetShowcasePreference(r.Context(), tx, u.ID, name, body.Pinned, body.Order)
			return err
		}) {
			return
		}
	}
	prefs, err := db.GetShowcasePreferences(r.Context(), h.database, u.ID)
	if err != nil {
		writeJSON(w, 500, errorResponse{Error: "failed to load settings"})
		return
	}
	pref, ok := prefs[name]
	if !ok {
		writeJSON(w, 404, errorResponse{Error: "site not found"})
		return
	}
	writeJSON(w, 200, map[string]any{"site": name, "pinned": pref.Pinned, "order": pref.Order})
}
