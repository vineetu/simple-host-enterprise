package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oplimits"
)

// replayCreate is called only after authentication, authorization, and any
// preconditions. Request archives and authentication headers are never stored.
func replayCreate(w http.ResponseWriter, r *http.Request, database *sql.DB, account, resource string, next http.HandlerFunc) {
	key := r.Header.Get("Idempotency-Key")
	if r.Method != http.MethodPost || key == "" || account == "" {
		next(w, r)
		return
	}
	if len(key) > 255 {
		writeJSON(w, 400, errorResponse{Error: "Idempotency-Key is at most 255 bytes", Code: "invalid_idempotency_key"})
		return
	}
	body, err := os.CreateTemp("", "simple-host-replay-*")
	if err != nil {
		writeJSON(w, 500, errorResponse{Error: "could not read request body"})
		return
	}
	defer os.Remove(body.Name())
	defer body.Close()
	defer r.Body.Close()
	hash := sha256.New()
	limit := max(oplimits.Get().MaxArchiveBytes, int64(photoCountLimit*(photoMaxBytes+4096)+4096))
	if _, err = io.Copy(io.MultiWriter(body, hash), http.MaxBytesReader(w, r.Body, limit)); err != nil {
		status := 400
		if isMaxBytesError(err) {
			status = 413
		}
		writeJSON(w, status, errorResponse{Error: "invalid or oversized request body"})
		return
	}
	if _, err = body.Seek(0, io.SeekStart); err != nil {
		writeJSON(w, 500, errorResponse{Error: "could not read request body"})
		return
	}
	r.Body = body
	sum := sha256.Sum256([]byte(resource + "\x00" + r.Method + " " + r.URL.EscapedPath() + "?" + r.URL.Query().Encode() + "\x00" + key))
	requestKey := hex.EncodeToString(sum[:])
	limits := oplimits.Get()
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		replay, claimed, err := db.ClaimCreate(r.Context(), database, account, requestKey, hash.Sum(nil), limits.CreateReplayPerAccount, limits.CreateReplayTotal)
		switch {
		case errors.Is(err, db.ErrIdempotencyReused):
			writeJSON(w, 422, errorResponse{Error: "Idempotency-Key was already used with a different request body", Code: "idempotency_key_reused"})
			return
		case errors.Is(err, db.ErrIdempotencyFull):
			writeJSON(w, 429, errorResponse{Error: "too many Idempotency-Key entries; retry after existing keys expire", Code: "idempotency_cache_full"})
			return
		case err != nil:
			writeJSON(w, 503, errorResponse{Error: "create replay unavailable"})
			return
		case claimed:
			rec := &createResponseWriter{header: make(http.Header)}
			next(rec, r)
			if rec.status == 0 {
				rec.status = 200
			}
			result := db.CreateReplay{Status: rec.status, Header: rec.header, Body: rec.body.Bytes()}
			// A disconnected caller must still leave a replay for its retry.
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
			saved := result
			saved.Header = result.Header.Clone()
			// Admission headers describe this attempt, not the original one.
			for name := range saved.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(name), "Ratelimit") {
					saved.Header.Del(name)
				}
			}
			err = db.CompleteCreate(ctx, database, account, requestKey, saved)
			cancel()
			if err != nil {
				writeJSON(w, 503, errorResponse{Error: "create response could not be saved; check the resource before retrying", Code: "idempotency_in_progress"})
				return
			}
			writeCreateReplay(w, result)
			return
		case replay.Status != 0:
			writeCreateReplay(w, *replay)
			return
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-r.Context().Done():
			timer.Stop()
			return
		case <-deadline.C:
			timer.Stop()
			w.Header().Set("Retry-After", "1")
			writeJSON(w, 409, errorResponse{Error: "the original create has not completed; check the resource before retrying", Code: "idempotency_in_progress"})
			return
		case <-timer.C:
		}
	}
}

type createResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *createResponseWriter) Header() http.Header { return w.header }
func (w *createResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *createResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.body.Write(b)
}
func writeCreateReplay(w http.ResponseWriter, result db.CreateReplay) {
	for k, v := range result.Header {
		w.Header()[k] = append([]string(nil), v...)
	}
	w.WriteHeader(result.Status)
	_, _ = w.Write(result.Body)
}
