package reqlog

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMiddlewareLogsAndEchoesTheID(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	handler := Middleware(logger, ProbePaths)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		SetUser(r.Context(), "user-1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("hello"))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "https://alice.example.com/site/", nil)
	req.RemoteAddr = "10.0.0.7:4242"
	handler.ServeHTTP(rec, req)

	id := rec.Header().Get(Header)
	if len(id) != 32 {
		t.Fatalf("X-Request-Id = %q, want a 32-hex id", id)
	}
	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("log line is not JSON: %v: %s", err, out.String())
	}
	for key, want := range map[string]any{
		"request_id": id, "method": "POST", "host": "alice.example.com", "path": "/site/",
		"status": float64(201), "bytes": float64(5), "remote": "10.0.0.7", "user_id": "user-1",
	} {
		if line[key] != want {
			t.Errorf("%s = %v, want %v", key, line[key], want)
		}
	}
}

func TestMiddlewareSkipsHealthyProbesButNotFailingOnes(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	status := http.StatusOK
	handler := Middleware(logger, ProbePaths)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if out.Len() != 0 {
		t.Fatalf("healthy probe was logged: %s", out.String())
	}
	status = http.StatusServiceUnavailable
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if !strings.Contains(out.String(), `"status":503`) {
		t.Fatalf("failing probe was not logged: %s", out.String())
	}
}

func TestInboundRequestIDIsNotTrusted(t *testing.T) {
	handler := Middleware(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(Header, "attacker-chosen")
	handler.ServeHTTP(rec, req)
	if got := rec.Header().Get(Header); got == "attacker-chosen" || got == "" {
		t.Fatalf("X-Request-Id = %q, want a server-generated id", got)
	}
}
