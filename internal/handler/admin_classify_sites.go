package handler

import (
	"log"
	"net/http"
	"strconv"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	"github.com/vsriram/simple-host/internal/sitetype"
)

// Bounds on one backfill request. Each site is a model call, so a request that
// tried to do all of them would outlive any sane HTTP timeout.
const (
	defaultClassifyLimit = 20
	maxClassifyLimit     = 100
)

type classifySitesResponse struct {
	Scanned  int  `json:"scanned"`
	Labelled int  `json:"labelled"`
	Skipped  int  `json:"skipped"`
	Failed   int  `json:"failed"`
	More     bool `json:"more_remaining"`
}

// classifySites labels a bounded batch of public sites that have no type yet.
//
// The background worker does this on its own; this exists so the existing
// sites can be done at once after a rollout, and so the operation is
// observable. Run it repeatedly until more_remaining is false.
func (h *AdminHandler) classifySites(w http.ResponseWriter, r *http.Request) {
	if h.siteTypes == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "site classification unavailable"})
		return
	}

	limit := defaultClassifyLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "limit must be a positive integer"})
			return
		}
		limit = min(parsed, maxClassifyLimit)
	}

	result, err := h.siteTypes.RunOnce(r.Context(), limit)
	if err != nil {
		log.Printf("classify sites: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "classification failed"})
		return
	}

	log.Printf("classify sites: scanned=%d labelled=%d skipped=%d failed=%d",
		result.Scanned, result.Labelled, result.Skipped, result.Failed)

	actorKind, keyID := auditActorKind(r.Context())
	actorID := ""
	if actor := auth.GetUser(r.Context()); actor != nil {
		actorID = actor.ID
	}
	h.audit.Record(r.Context(), audit.Event{
		ActorID: actorID, ActorKind: actorKind, KeyID: keyID,
		Action:    "admin_classify_sites",
		RequestID: auditRequestID(r.Context()),
		Extra: map[string]any{
			"scanned": result.Scanned, "labelled": result.Labelled,
			"skipped": result.Skipped, "failed": result.Failed,
		},
	})

	writeJSON(w, http.StatusOK, classifySitesResponse{
		Scanned:  result.Scanned,
		Labelled: result.Labelled,
		Skipped:  result.Skipped,
		Failed:   result.Failed,
		// A full batch means there may be more; the next call finds out.
		More: result.Scanned == limit,
	})
}

// WithSiteTypes attaches the classification worker, enabling the backfill
// endpoint. Optional: nil leaves the endpoint returning 503.
func (h *AdminHandler) WithSiteTypes(worker *sitetype.Worker) *AdminHandler {
	h.siteTypes = worker
	return h
}
