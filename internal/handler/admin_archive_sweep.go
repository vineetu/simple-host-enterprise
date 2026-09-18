package handler

import (
	"log"
	"net/http"
	"strconv"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
)

// defaultArchiveSweepLimit bounds one sweep when no limit is given. Small on
// purpose: this is a manual, observable operation over real user data, not a
// job that should try to finish in one go.
const defaultArchiveSweepLimit = 20

// maxArchiveSweepLimit caps a single request so one call cannot occupy the
// site locks for an unbounded stretch.
const maxArchiveSweepLimit = 200

type archiveSweepResponse struct {
	Scanned      int    `json:"scanned"`
	Archived     int    `json:"archived"`
	Failed       int    `json:"failed"`
	BytesBefore  int64  `json:"bytes_before"`
	BytesAfter   int64  `json:"bytes_after"`
	BytesSaved   int64  `json:"bytes_saved"`
	MoreRemain   bool   `json:"more_remaining"`
	FirstFailure string `json:"first_failure,omitempty"`
}

// archiveVersions compresses a bounded batch of superseded version directories.
//
// Deploying already compresses the version it replaces, so this exists only for
// the backlog: sites whose history predates that behaviour and which may never
// deploy again. Run it repeatedly until more_remaining is false.
func (h *AdminHandler) archiveVersions(w http.ResponseWriter, r *http.Request) {
	if h.diskStorage == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "archive sweep unavailable"})
		return
	}

	limit := defaultArchiveSweepLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "limit must be a positive integer"})
			return
		}
		limit = min(parsed, maxArchiveSweepLimit)
	}

	result, err := h.diskStorage.ArchiveIdleVersions(limit)
	if err != nil {
		log.Printf("archive sweep: %v", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "archive sweep failed"})
		return
	}

	log.Printf("archive sweep: scanned=%d archived=%d failed=%d before=%d after=%d",
		result.Scanned, result.Archived, result.Failed, result.BytesBefore, result.BytesAfter)

	actorKind, keyID := auditActorKind(r.Context())
	actorID := ""
	if actor := auth.GetUser(r.Context()); actor != nil {
		actorID = actor.ID
	}
	h.audit.Record(r.Context(), audit.Event{
		ActorID: actorID, ActorKind: actorKind, KeyID: keyID,
		Action:    "admin_archive_versions",
		RequestID: auditRequestID(r.Context()),
		Extra: map[string]any{
			"scanned": result.Scanned, "archived": result.Archived, "failed": result.Failed,
			"bytes_saved": result.BytesBefore - result.BytesAfter,
		},
	})

	writeJSON(w, http.StatusOK, archiveSweepResponse{
		Scanned:      result.Scanned,
		Archived:     result.Archived,
		Failed:       result.Failed,
		BytesBefore:  result.BytesBefore,
		BytesAfter:   result.BytesAfter,
		BytesSaved:   result.BytesBefore - result.BytesAfter,
		MoreRemain:   result.ReachedLimit,
		FirstFailure: result.FirstFailure,
	})
}
