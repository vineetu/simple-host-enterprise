package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

// An admin's API key does not reach /api/admin/*: admin actions need a
// browser session.
func TestAdminAPIRefusesAPIKeys(t *testing.T) {
	keyAuth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := auth.ContextWithTestAuth(r.Context(), &db.User{ID: "admin-1", IsAdmin: true}, "", "key-1")
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
	passthrough := func(next http.Handler) http.Handler { return next }
	mux := http.NewServeMux()
	NewAdminHandler(nil, "https://example.com", HostModel{}, CookiePolicy{}, nil, time.Hour, nil).Register(mux, keyAuth, passthrough)

	request := httptest.NewRequest(http.MethodGet, "/api/admin/export?kind=audit&format=jsonl", nil)
	request.Header.Set("X-API-Key", "shk_whatever")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("admin export with an API key: status = %d, want 403", response.Code)
	}
}
