package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A cookie's host binding is checked before any database read, so these run
// with no database: a cookie that passes the binding would panic on the nil
// *sql.DB, which is itself the failure signal.
func TestMiddlewareRefusesCookieBoundToAnotherHost(t *testing.T) {
	keys := []SigningKey{testKey("k1", 1)}
	exp := time.Now().Add(time.Hour)
	hostCookie, err := SignHostSession(keys, "s1", "u1", "mallory.example.com", exp)
	if err != nil {
		t.Fatal(err)
	}
	baseCookie, err := SignSession(keys, "s1", "u1", exp)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, cookie, expectedHost string
	}{
		{"owner-host cookie on the base host", hostCookie, ""},
		{"owner-host cookie on another owner host", hostCookie, "alice.example.com"},
		{"base-host cookie on an owner host", baseCookie, "mallory.example.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reached := false
			handler := Middleware(nil, keys, time.Hour)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
			request := httptest.NewRequest(http.MethodGet, "/api/me", nil)
			if test.expectedHost != "" {
				request = request.WithContext(WithExpectedSessionHost(request.Context(), test.expectedHost))
			}
			request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: test.cookie})
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if reached || response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, reached = %v; want 401", response.Code, reached)
			}
		})
	}
}

func TestVerifyBaseSessionCookieRefusesHostBoundCookie(t *testing.T) {
	keys := []SigningKey{testKey("k1", 1)}
	exp := time.Now().Add(time.Hour)
	hostCookie, _ := SignHostSession(keys, "s1", "u1", "mallory.example.com", exp)
	if _, err := VerifyBaseSessionCookie(keys, hostCookie); err == nil {
		t.Fatal("a host-bound cookie verified as a base-host session")
	}
	baseCookie, _ := SignSession(keys, "s1", "u1", exp)
	if _, err := VerifyBaseSessionCookie(keys, baseCookie); err != nil {
		t.Fatalf("base cookie: %v", err)
	}
}
