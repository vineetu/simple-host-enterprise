package handler

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteAuthErrorCard(t *testing.T) {
	message := `say <script>alert("1")</script> & 100% of "it"`
	for _, status := range []int{http.StatusBadRequest, http.StatusBadGateway, http.StatusForbidden} {
		rec := httptest.NewRecorder()
		writeAuthError(rec, status, message)
		if rec.Code != status {
			t.Errorf("status = %d, want %d", rec.Code, status)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Errorf("content-type = %q", ct)
		}
		body := rec.Body.String()
		for _, want := range []string{
			`<meta name="viewport" content="width=device-width,initial-scale=1">`,
			`<meta name="robots" content="noindex">`,
			`<p class="eyebrow">Sign in</p>`,
			`<h1>Sign-in didn't complete</h1>`,
			`<p class="lead">` + html.EscapeString(message) + `</p>`,
			`<a class="primary" href="/auth/login">Sign in again</a>`,
			`<p class="fine">If this keeps happening, ask your IT team.</p>`,
			`href="/favicon.svg?v=b"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("status %d page lacks %q\n%s", status, want, body)
			}
		}
		assertSignInCard(t, body)
	}
}

func TestRenderNoAccessCard(t *testing.T) {
	name := `Vera <b>x</b> & "y"`
	host := `notes.alice.example/<script>`
	body := renderNoAccess(name, "https://foo.example", host)
	for _, want := range []string{
		`<p class="eyebrow">` + html.EscapeString(host) + `</p>`,
		`<h1>This site doesn't exist or isn't shared with you</h1>`,
		`<p class="who">You're signed in as <strong>` + html.EscapeString(name) + `</strong>.</p>`,
		`<p class="lead">Ask the person who sent you the link to share it with you.</p>`,
		`<a class="primary" href="` + switchAccountPath + `">Switch account</a>`,
		`href="https://foo.example/favicon.svg?v=b"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("no-access page lacks %q\n%s", want, body)
		}
	}
	assertSignInCard(t, body)
	if strings.Contains(body, "<b>") || strings.Contains(body, "<script") {
		t.Fatalf("name or host was not escaped:\n%s", body)
	}

	// No host, no eyebrow. The wording does not change.
	plain := renderNoAccess(name, "", "  ")
	if strings.Contains(plain, `class="eyebrow"`) {
		t.Fatalf("blank host still wrote an eyebrow:\n%s", plain)
	}
	if !strings.Contains(plain, `<h1>This site doesn't exist or isn't shared with you</h1>`) {
		t.Fatal("heading changed when the host was left off")
	}

	// No name, no signed-in row.
	anon := renderNoAccess("", "https://foo.example", "notes.alice.example")
	if strings.Contains(anon, "You're signed in") || strings.Contains(anon, "<strong>") {
		t.Fatalf("empty name still named someone:\n%s", anon)
	}

	// A missing site and an unshared site are the same page. Only the host
	// they asked for differs.
	missing := renderNoAccess("vera", "https://foo.example", "nope.alice.example")
	unshared := renderNoAccess("vera", "https://foo.example", "demo.alice.example")
	if strings.ReplaceAll(missing, "nope.alice.example", "HOST") != strings.ReplaceAll(unshared, "demo.alice.example", "HOST") {
		t.Fatalf("pages differ by more than the host:\n%s\n---\n%s", missing, unshared)
	}
}

func TestNoAccessCardKeepsCSP(t *testing.T) {
	if strings.Contains(noAccessCSP, "script-src") || !strings.Contains(noAccessCSP, "style-src 'unsafe-inline'") || !strings.HasPrefix(noAccessCSP, "sandbox;") {
		t.Fatalf("noAccessCSP = %q", noAccessCSP)
	}
	name := `Ann <script>alert(1)</script>`
	gate := &hostGate{
		hosts: testHostModel(t),
		userLabel: func(context.Context, string) string {
			return name
		},
	}
	req := httptest.NewRequest(http.MethodGet, "https://"+aliceSiteHost+"/", nil)
	req.Host = aliceSiteHost
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Sec-Fetch-Dest", "document")
	rec := httptest.NewRecorder()
	gate.noAccess(rec, req, "user-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got, want := rec.Header().Get("Content-Security-Policy"), noAccessPolicy(gate.hosts.origin); got != want {
		t.Fatalf("CSP = %q, want %q", got, want)
	}
	body := rec.Body.String()
	assertSignInCard(t, body)
	if !strings.Contains(body, `<p class="eyebrow">`+aliceSiteHost+`</p>`) {
		t.Errorf("eyebrow:\n%s", body)
	}
	if !strings.Contains(body, `<p class="who">You're signed in as <strong>`+html.EscapeString(name)+`</strong>.</p>`) {
		t.Errorf("signed-in row:\n%s", body)
	}
	if !strings.Contains(body, `<a class="primary" href="/auth/switch">Switch account</a>`) {
		t.Errorf("switch button:\n%s", body)
	}
}

func assertSignInCard(t *testing.T, body string) {
	t.Helper()
	lower := strings.ToLower(body)
	if strings.Contains(lower, "<script") {
		t.Fatalf("page has a script:\n%s", body)
	}
	if strings.Contains(lower, "javascript:") {
		t.Fatalf("page has a javascript URL:\n%s", body)
	}
	if strings.Contains(body, `rel="stylesheet"`) {
		t.Fatalf("page loads a stylesheet, which the no-access policy would block:\n%s", body)
	}
	if !strings.Contains(body, "<style>") {
		t.Fatal("page has no inline style")
	}
	for _, want := range []string{"max-width:400px", "background:#f5f6f8", "#1b4ee0", "border-radius:14px", "#d6d9e0", "font-weight:650"} {
		if !strings.Contains(body, want) {
			t.Errorf("card style lacks %q", want)
		}
	}
	if strings.Contains(body, "prefers-color-scheme") || strings.Contains(body, "data-theme") {
		t.Error("card has a dark theme")
	}
}
