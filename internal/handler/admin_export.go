package handler

import (
	"encoding/csv"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
)

var auditEventCSVHeader = []string{
	"id", "at", "request_id", "actor_id", "actor_kind", "key_id", "action",
	"owner_id", "site_id", "team_id", "via_site_label", "via_site_name",
	"via_site_observed", "ip", "user_agent", "detail",
}

var accessLogCSVHeader = []string{
	"id", "at", "user_id", "session_id", "owner_label", "site_name",
	"path", "method", "status", "bytes", "ip", "user_agent", "client_kind",
}

// exportAuditOrAccess answers GET /api/admin/export (design.md 8.3): admin
// only, streamed, itself audited as admin_export. kind selects audit_events
// or access_log; format selects CSV or newline-delimited JSON. Both kinds
// are admin-scoped reads (every owner, no namespace restriction) — the
// scoping /api/audit and /api/access apply for a non-admin caller does not
// apply here, since only an admin ever reaches this route.
func (h *AdminHandler) exportAuditOrAccess(w http.ResponseWriter, r *http.Request) {
	if h.auditReader == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "export unavailable"})
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "audit" && kind != "access" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: `kind must be "audit" or "access"`})
		return
	}
	format := r.URL.Query().Get("format")
	if format != "csv" && format != "jsonl" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: `format must be "csv" or "jsonl"`})
		return
	}
	from, ok := parseAuditTimeParam(w, r, "from")
	if !ok {
		return
	}
	to, ok := parseAuditTimeParam(w, r, "to")
	if !ok {
		return
	}

	actor := auth.GetUser(r.Context())
	actorID := ""
	if actor != nil {
		actorID = actor.ID
	}
	actorKind, keyID := auditActorKind(r.Context())
	h.audit.Record(r.Context(), audit.Event{
		ActorID: actorID, ActorKind: actorKind, KeyID: keyID,
		Action:    "admin_export",
		RequestID: auditRequestID(r.Context()),
		Extra:     map[string]any{"kind": kind, "format": format},
	})

	filename := "simple-host-" + kind + "-export." + format
	if format == "jsonl" {
		w.Header().Set("Content-Type", "application/x-ndjson")
	} else {
		w.Header().Set("Content-Type", "text/csv")
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	var csvWriter *csv.Writer
	if format == "csv" {
		csvWriter = csv.NewWriter(w)
		header := auditEventCSVHeader
		if kind == "access" {
			header = accessLogCSVHeader
		}
		if err := csvWriter.Write(header); err != nil {
			log.Printf("admin export: write %s CSV header: %v", kind, err)
			return
		}
	}

	cursor := ""
	for {
		var nextCursor string
		if kind == "audit" {
			page, err := h.auditReader.ListAuditEvents(r.Context(), audit.AuditQuery{
				Admin: true, From: from, To: to, Cursor: cursor,
			})
			if err != nil {
				log.Printf("admin export: list audit events: %v", err)
				return
			}
			for _, e := range page.Events {
				row := toAuditEventResponse(e)
				if format == "csv" {
					if err := writeAuditEventCSVRow(csvWriter, row); err != nil {
						log.Printf("admin export: write audit CSV row: %v", err)
						return
					}
				} else if err := writeJSONLine(w, row); err != nil {
					log.Printf("admin export: write audit JSONL row: %v", err)
					return
				}
			}
			nextCursor = page.NextCursor
		} else {
			page, err := h.auditReader.ListAccess(r.Context(), audit.AccessQuery{
				Admin: true, From: from, To: to, Cursor: cursor,
			})
			if err != nil {
				log.Printf("admin export: list access log: %v", err)
				return
			}
			for _, e := range page.Entries {
				row := toAccessLogEntryResponse(e)
				if format == "csv" {
					if err := writeAccessLogCSVRow(csvWriter, row); err != nil {
						log.Printf("admin export: write access CSV row: %v", err)
						return
					}
				} else if err := writeJSONLine(w, row); err != nil {
					log.Printf("admin export: write access JSONL row: %v", err)
					return
				}
			}
			nextCursor = page.NextCursor
		}
		if csvWriter != nil {
			csvWriter.Flush()
		}
		if flusher != nil {
			flusher.Flush()
		}
		if nextCursor == "" || nextCursor == cursor {
			return
		}
		cursor = nextCursor
	}
}

func writeAuditEventCSVRow(w *csv.Writer, e auditEventResponse) error {
	detail := ""
	if len(e.Detail) > 0 {
		if b, err := json.Marshal(e.Detail); err == nil {
			detail = string(b)
		}
	}
	return writeSafeCSVRow(w, []string{
		strconv.FormatInt(e.ID, 10), e.At.Format("2006-01-02T15:04:05.000Z07:00"), e.RequestID, e.ActorID,
		e.ActorKind, e.KeyID, e.Action, e.OwnerID, e.SiteID, e.TeamID, e.ViaSiteLabel, e.ViaSiteName,
		strconv.FormatBool(e.ViaSiteObserved), e.IP, e.UserAgent, detail,
	})
}

// writeSafeCSVRow neutralises spreadsheet formulas: a cell starting with
// = + - @ tab or carriage return is prefixed with ' so a spreadsheet shows
// it as text. User-Agent, paths and details are attacker-controlled.
func writeSafeCSVRow(w *csv.Writer, cells []string) error {
	for i, cell := range cells {
		if cell != "" && strings.ContainsRune("=+-@\t\r", rune(cell[0])) {
			cells[i] = "'" + cell
		}
	}
	return w.Write(cells)
}

func writeAccessLogCSVRow(w *csv.Writer, e accessLogEntryResponse) error {
	return writeSafeCSVRow(w, []string{
		strconv.FormatInt(e.ID, 10), e.At.Format("2006-01-02T15:04:05.000Z07:00"), e.UserID, e.SessionID,
		e.OwnerLabel, e.SiteName, e.Path, e.Method, strconv.Itoa(e.Status), strconv.FormatInt(e.Bytes, 10),
		e.IP, e.UserAgent, e.ClientKind,
	})
}

func writeJSONLine(w http.ResponseWriter, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}
