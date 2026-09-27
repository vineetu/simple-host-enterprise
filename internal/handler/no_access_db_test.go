package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
)

// open sends a browser navigation to a site host, signed in as user there.
func (w *accessWorld) open(user, host string) *httptest.ResponseRecorder {
	w.t.Helper()
	r := httptest.NewRequest(http.MethodGet, "https://"+host+"/", nil)
	r.Header.Set("Accept", "text/html")
	r.Header.Set("Sec-Fetch-Dest", "document")
	if user != "" {
		r.AddCookie(w.cookie(user, host))
	}
	return w.do(r)
}

// A site the person may not open and a site that does not exist answer a
// browser with the same page, naming who is signed in; scripts still get a
// bare 404, and a signed-out browser is sent to sign in for both.
func TestNoAccessPageIsTheSameForMissingAndUnshared(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	unshared := w.open("vera", "demo.alice."+accessBase)
	missing := w.open("vera", "nope.alice."+accessBase)
	if unshared.Code != http.StatusNotFound || missing.Code != http.StatusNotFound {
		t.Fatalf("status unshared %d, missing %d, want 404", unshared.Code, missing.Code)
	}
	body := unshared.Body.String()
	for _, want := range []string{"This site doesn't exist or isn't shared with you", "You're signed in as <strong>vera (vera@example.com)</strong>", "Ask the person who sent you the link to share it with you.", `href="/auth/switch"`} {
		if !strings.Contains(body, want) {
			t.Errorf("no-access page lacks %q:\n%s", want, body)
		}
	}
	if missing.Body.String() != body {
		t.Errorf("missing and unshared pages differ:\n%s\n---\n%s", missing.Body, body)
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'access_denied'`); n != 1 {
		t.Errorf("access_denied rows = %d, want 1 (the unshared site only)", n)
	}

	// Signed out: both are sent through the sign-in hand-off.
	for _, host := range []string{"demo.alice." + accessBase, "nope.alice." + accessBase} {
		rec := w.open("", host)
		if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "/auth/handoff") {
			t.Errorf("signed-out %s = %d %q, want the hand-off", host, rec.Code, rec.Header().Get("Location"))
		}
	}

	// A script's request keeps the plain 404.
	r := httptest.NewRequest(http.MethodGet, "https://nope.alice."+accessBase+"/", nil)
	r.Header.Set("Sec-Fetch-Dest", "empty")
	r.AddCookie(w.cookie("vera", "nope.alice."+accessBase))
	if rec := w.do(r); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "shared with you") {
		t.Errorf("fetch to a missing site = %d %s", rec.Code, rec.Body)
	}
	// The owner still sees the site.
	if rec := w.open("alice", "demo.alice."+accessBase); rec.Code != http.StatusOK {
		t.Errorf("owner = %d", rec.Code)
	}
}

// Switch account: the site host drops its cookie and sends the person to
// the dashboard, which offers to sign out and come back; signing out with
// that address returns there, and nowhere off this server.
func TestSwitchAccountFlow(t *testing.T) {
	w := newAccessWorld(t)
	host := "demo.alice." + accessBase
	rec := w.open("vera", host)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("setup = %d", rec.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "https://"+host+"/auth/switch", nil)
	r.AddCookie(w.cookie("vera", host))
	rec = w.do(r)
	want := "https://" + accessBase + "/dashboard?switch=" + url.QueryEscape("https://"+host+"/")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != want {
		t.Fatalf("switch = %d %q, want 303 %q", rec.Code, rec.Header().Get("Location"), want)
	}
	if c := rec.Result().Cookies(); len(c) != 1 || c[0].MaxAge >= 0 {
		t.Fatalf("switch did not clear the host cookie: %v", c)
	}

	hosts, err := NewHostModel("https://" + accessBase)
	if err != nil {
		t.Fatal(err)
	}
	dashboard := http.NewServeMux()
	NewDashboardHandler(w.database, w.keys, time.Hour).WithHosts(hosts).Register(dashboard, nil)
	page := func(query string) string {
		req := httptest.NewRequest(http.MethodGet, "https://"+accessBase+"/dashboard?"+query, nil)
		req.AddCookie(w.cookie("vera", ""))
		out := httptest.NewRecorder()
		dashboard.ServeHTTP(out, req)
		return out.Body.String()
	}
	if got := page("switch=" + url.QueryEscape("https://"+host+"/")); !strings.Contains(got, `name="to" value="https://`+host+`/"`) || !strings.Contains(got, "Sign out and switch") {
		t.Errorf("dashboard switch notice missing")
	}
	if got := page("switch=" + url.QueryEscape("https://evil.example/")); strings.Contains(got, "Sign out and switch") {
		t.Errorf("dashboard offered to switch to another server")
	}

	auth := &AuthHandler{database: w.database, hosts: hosts, audit: audit.NoOp{}}
	logout := func(to string) string {
		req := httptest.NewRequest(http.MethodPost, "https://"+accessBase+"/auth/logout", strings.NewReader(url.Values{"to": {to}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		out := httptest.NewRecorder()
		auth.logout(out, req)
		return out.Header().Get("Location")
	}
	if got := logout("https://" + host + "/"); got != "https://"+host+"/" {
		t.Errorf("logout back to the site = %q", got)
	}
	if got := logout("https://evil.example/"); got != "/dashboard" {
		t.Errorf("logout to another server = %q, want /dashboard", got)
	}
}
