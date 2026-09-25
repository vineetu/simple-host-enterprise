package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vsriram/simple-host/internal/auth"
)

// The overwhelming majority of requests are anonymous visitors, and they must
// reach the analytics write without the check touching the database at all.
// A nil *sql.DB here is the assertion: any query would panic.
func TestIsSelfTrafficSkipsTheDatabaseForAnonymousVisitors(t *testing.T) {
	tests := []struct {
		name   string
		cookie *http.Cookie
	}{
		{"no cookies at all", nil},
		{"session cookie present but empty", &http.Cookie{Name: auth.SessionCookieName, Value: ""}},
		{"session cookie present but unverifiable", &http.Cookie{Name: auth.SessionCookieName, Value: "garbage"}},
		{"some other cookie", &http.Cookie{Name: "sh_visit_alice_site", Value: "1"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/portfolio/", nil)
			if tc.cookie != nil {
				r.AddCookie(tc.cookie)
			}
			if isSelfTraffic(r, nil, nil, 0, "alice", "portfolio") {
				t.Error("anonymous request was treated as the site's own traffic")
			}
		})
	}
}

// A nil database means analytics are not being recorded anyway; the check must
// not claim the traffic is self-traffic and must not panic.
func TestIsSelfTrafficWithoutDatabase(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/portfolio/", nil)
	r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "some-value"})

	if isSelfTraffic(r, nil, nil, 0, "alice", "portfolio") {
		t.Error("expected false when there is no database to resolve against")
	}
}
