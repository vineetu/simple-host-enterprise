package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vsriram/simple-host/internal/auth"
	db "github.com/vsriram/simple-host/internal/db"
)

func TestMintRefusesLifetimeBeyondTheMaximum(t *testing.T) {
	h := NewKeysHandler(nil, nil, HostModel{}, "https://example.com").WithMaxKeyDays(30)
	for _, body := range []string{`{"name":"ci","expires_in_days":31}`, `{"name":"ci","expires_in_days":-1}`} {
		request := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(body))
		request = request.WithContext(auth.ContextWithTestAuth(request.Context(), &db.User{ID: "u1"}, "s1", ""))
		response := httptest.NewRecorder()
		h.mint(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", body, response.Code)
		}
	}
}

func TestGeneratedKeysCarryTheScannerPrefix(t *testing.T) {
	key, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "shk_") || len(key) != 4+64 {
		t.Fatalf("key %q", key[:6])
	}
}
