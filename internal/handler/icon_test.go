package handler

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/vsriram/simple-host/internal/auth"
	plugin "github.com/vsriram/simple-host/simple-host-plugin"
)

// The four icon addresses, and the Content-Type http.ServeContent derives
// from each file name.
var productIconFiles = []struct {
	path        string
	contentType string
}{
	{"/favicon.svg", "image/svg+xml"},
	{"/favicon.ico", "image/vnd.microsoft.icon"},
	{"/favicon-32.png", "image/png"},
	{"/apple-touch-icon.png", "image/png"},
}

// Root-relative links, as a page on the app host writes them.
var productIconHrefs = []string{
	`href="/favicon.svg?v=b"`,
	`href="/favicon-32.png?v=b"`,
	`href="/favicon.ico?v=b"`,
	`href="/apple-touch-icon.png?v=b"`,
}

func productIconBytes(t *testing.T) map[string][]byte {
	t.Helper()
	out := make(map[string][]byte, len(productIconFiles))
	for _, icon := range productIconFiles {
		data, err := fs.ReadFile(staticFiles, "static"+icon.path)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			t.Fatalf("%s is empty", icon.path)
		}
		out[icon.path] = data
	}
	return out
}

func iconResponse(handler http.Handler, host, iconPath string, credential bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "https://"+host+iconPath+"?v=b", nil)
	request.Host = host
	if credential {
		request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "not-a-session"})
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, request)
	return rec
}

func assertRelativeIconLinks(t *testing.T, where, body string) {
	t.Helper()
	for _, href := range productIconHrefs {
		if !strings.Contains(body, href) {
			t.Errorf("%s is missing %s", where, href)
		}
	}
}

func assertAbsoluteIconLinks(t *testing.T, where, body, origin string) {
	t.Helper()
	for _, href := range productIconHrefs {
		absolute := strings.Replace(href, `href="`, `href="`+origin, 1)
		if !strings.Contains(body, absolute) {
			t.Errorf("%s is missing %s", where, absolute)
		}
		// A root-relative link would ask the host that served the page.
		// Owner hosts and site hosts do not serve this mark.
		if strings.Contains(body, href) {
			t.Errorf("%s has a root-relative icon link %s", where, href)
		}
	}
}

func assertNotProductIcon(t *testing.T, where string, rec *httptest.ResponseRecorder, mark []byte) {
	t.Helper()
	if bytes.Equal(rec.Body.Bytes(), mark) {
		t.Fatalf("%s returned the product icon (status %d, %d bytes, cache %q)",
			where, rec.Code, rec.Body.Len(), rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("Cache-Control") == "public, max-age=86400" && rec.Code == http.StatusOK {
		t.Fatalf("%s answered like the app-host icon route", where)
	}
}

// The app host serves each embedded icon with a day of caching. An owner
// host and a site host do not: a site keeps its own favicon, and a site
// without one gets the same miss it got before these routes existed.
func TestProductIconServedOnBaseHostOnly(t *testing.T) {
	marks := productIconBytes(t)
	store := newTestStore(t)
	store.publish(t, "alice", "my-site", testSiteID("alice", "my-site"), 1, map[string]string{
		"index.html":  "hello",
		"favicon.ico": "SITE-FAVICON",
		"favicon.svg": "SITE-SVG",
	})

	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "https://foo.example")
	gated := testHostGate(t, store, testHostModel(t)).wrap(mux)
	wrapped := SecurityHeaders(mux, true, testHostModel(t))

	for _, icon := range productIconFiles {
		for _, handler := range []struct {
			name    string
			handler http.Handler
			cred    bool
		}{
			{"mux", mux, false},
			{"gate", gated, false},
			{"security headers with a cookie", wrapped, true},
		} {
			rec := iconResponse(handler.handler, "foo.example", icon.path, handler.cred)
			where := handler.name + " " + icon.path
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200", where, rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != icon.contentType {
				t.Errorf("%s: Content-Type = %q, want %q", where, got, icon.contentType)
			}
			if got := rec.Header().Get("Cache-Control"); got != "public, max-age=86400" {
				t.Errorf("%s: Cache-Control = %q, want public, max-age=86400", where, got)
			}
			if !bytes.Equal(rec.Body.Bytes(), marks[icon.path]) {
				t.Errorf("%s: body is %d bytes, want the embedded file (%d bytes)", where, rec.Body.Len(), len(marks[icon.path]))
			}
		}
	}

	// Owner host, certificate ready: the path is a site address and redirects.
	// It never reaches the app-host file routes.
	for _, icon := range productIconFiles {
		rec := iconResponse(gated, "alice.foo.example", icon.path, false)
		if rec.Code != http.StatusMovedPermanently {
			t.Errorf("ready owner host %s: status = %d, want 301", icon.path, rec.Code)
		}
		assertNotProductIcon(t, "ready owner host "+icon.path, rec, marks[icon.path])
	}

	// Owner host, certificate not ready: the path is looked up as a site
	// name. Nothing is published under it, so it is a miss, not this mark.
	notReady := testHostGate(t, store, testHostModel(t).WithOwnerReadiness(func(string) bool { return false })).wrap(mux)
	for _, icon := range productIconFiles {
		rec := iconResponse(notReady, "alice.foo.example", icon.path, false)
		if rec.Code != http.StatusNotFound {
			t.Errorf("not-ready owner host %s: status = %d, want 404", icon.path, rec.Code)
		}
		assertNotProductIcon(t, "not-ready owner host "+icon.path, rec, marks[icon.path])
	}

	// Site host, signed in: the site's own files, including a favicon the
	// site published. The two icons the site did not publish stay missing.
	for _, icon := range productIconFiles {
		rec := gateAuthedRequest(t, gated, http.MethodGet, aliceSiteHost, icon.path)
		switch icon.path {
		case "/favicon.ico":
			if rec.Code != http.StatusOK || rec.Body.String() != "SITE-FAVICON" {
				t.Fatalf("site favicon.ico: status %d body %q", rec.Code, rec.Body.String())
			}
		case "/favicon.svg":
			if rec.Code != http.StatusOK || rec.Body.String() != "SITE-SVG" {
				t.Fatalf("site favicon.svg: status %d body %q", rec.Code, rec.Body.String())
			}
		default:
			if rec.Code == http.StatusOK {
				t.Errorf("site %s: status 200, want a miss", icon.path)
			}
		}
		assertNotProductIcon(t, "site host "+icon.path, rec, marks[icon.path])
	}

	// Signed out, and a site host that names no site: neither returns the mark.
	for _, host := range []string{aliceSiteHost, "missing.alice.foo.example"} {
		for _, icon := range productIconFiles {
			rec := iconResponse(gated, host, icon.path, false)
			assertNotProductIcon(t, host+" "+icon.path, rec, marks[icon.path])
			if rec.Code == http.StatusOK {
				t.Errorf("%s %s: status 200", host, icon.path)
			}
		}
	}
}

// Every app-host page links the four versioned icon addresses. Pages that
// are actually served on an owner host or a site host use the app origin,
// because those hosts do not serve the files.
func TestPagesLinkProductIcon(t *testing.T) {
	mux := http.NewServeMux()
	RegisterUIRoutes(mux, "https://foo.example")
	// The file server redirects /index.html to /, which is the landing page.
	// The other static pages are served as themselves.
	for _, page := range []string{"/", "/install.html", "/docs.html", "/capabilities.html", "/changelog.html"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, page, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", page, rec.Code)
		}
		assertRelativeIconLinks(t, page, rec.Body.String())
	}
	for _, name := range []string{"index.html", "install.html", "docs.html", "capabilities.html", "changelog.html"} {
		body, err := fs.ReadFile(staticFiles, "static/"+name)
		if err != nil {
			t.Fatal(err)
		}
		assertRelativeIconLinks(t, name, string(body))
	}
	indexRedirect := httptest.NewRecorder()
	mux.ServeHTTP(indexRedirect, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	if indexRedirect.Code != http.StatusMovedPermanently {
		t.Fatalf("/index.html: status = %d, want 301 to /", indexRedirect.Code)
	}

	dash := http.NewServeMux()
	NewDashboardHandler(nil, nil, 0).Register(dash, nil)
	rec := httptest.NewRecorder()
	dash.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: status = %d, want 200", rec.Code)
	}
	assertRelativeIconLinks(t, "dashboard", rec.Body.String())

	rec = httptest.NewRecorder()
	writeAdminSignInPrompt(rec, "")
	assertRelativeIconLinks(t, "admin sign-in", rec.Body.String())
	// The signed-in admin page, the sessions page, and the connect consent
	// page all write one of these two heads.
	assertRelativeIconLinks(t, "admin head", adminHeadHTML)
	assertRelativeIconLinks(t, "dashboard head", dashboardHeadHTML)
	assertRelativeIconLinks(t, "showcase head", showcaseHeadHTML)

	showcase := newTestShowcaseHandler(openShowcaseTestDB(t, showcaseGalleryTestScript()), testHostModel(t))
	rec = httptest.NewRecorder()
	showcase.page(rec, httptest.NewRequest(http.MethodGet, "/showcase", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("showcase: status = %d, want 200", rec.Code)
	}
	assertRelativeIconLinks(t, "showcase", rec.Body.String())

	rec = httptest.NewRecorder()
	writeAuthError(rec, http.StatusBadRequest, "try again")
	assertRelativeIconLinks(t, "sign-in problem", rec.Body.String())

	rec = httptest.NewRecorder()
	writeConnectError(rec, "try again")
	assertRelativeIconLinks(t, "connect problem", rec.Body.String())

	owner := ownerIndexGate(t, "user-1")
	rec = gateAuthedRequest(t, owner, http.MethodGet, "alice.foo.example", "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner index: status = %d, want 200", rec.Code)
	}
	assertAbsoluteIconLinks(t, "owner index", rec.Body.String(), "https://foo.example")

	gate := &hostGate{hosts: testHostModel(t)}
	req := httptest.NewRequest(http.MethodGet, "https://"+aliceSiteHost+"/", nil)
	req.Host = aliceSiteHost
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Sec-Fetch-Dest", "document")
	rec = httptest.NewRecorder()
	gate.noAccess(rec, req, "user-1")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no access: status = %d, want 404", rec.Code)
	}
	assertAbsoluteIconLinks(t, "no access", rec.Body.String(), "https://foo.example")
	wantCSP := noAccessCSP + "; img-src https://foo.example"
	if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Fatalf("no access CSP = %q, want %q", got, wantCSP)
	}

	// No usable app origin: leave the icon off, and do not open img-src.
	if strings.Contains(renderNoAccess("vera", ""), "favicon") {
		t.Fatal("no-access page with an empty origin linked an icon")
	}
	if strings.Contains(renderOwnerIndex("alice", nil, true, ""), "favicon") {
		t.Fatal("owner index with an empty origin linked an icon")
	}
	for _, origin := range []string{"", "https://evil.example/path", "https://user:pass@foo.example", "https://foo.example?x=1", "javascript:alert(1)"} {
		if got := noAccessPolicy(origin); got != noAccessCSP {
			t.Errorf("noAccessPolicy(%q) = %q, want the page policy with no img-src", origin, got)
		}
		if strings.Contains(absoluteProductIconLinks(origin), "favicon") {
			t.Errorf("absoluteProductIconLinks(%q) emitted an icon link", origin)
		}
	}
}

// /plugin.zip carries the three icon PNGs unchanged, and both manifests
// name them. The skill archives stay skills only.
func TestPluginZipCarriesProductIcon(t *testing.T) {
	const base = "https://hosting.corp.test"
	data, err := buildPluginZip(base)
	if err != nil {
		t.Fatal(err)
	}
	entries := zipEntries(t, data)
	for _, name := range []struct {
		zipPath string
		fsPath  string
	}{
		{"simple-host/.claude-plugin/icon.png", ".claude-plugin/icon.png"},
		{"simple-host/assets/icon.png", "assets/icon.png"},
		{"simple-host/assets/logo.png", "assets/logo.png"},
	} {
		want, err := fs.ReadFile(plugin.FS, name.fsPath)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := entries[name.zipPath]
		if !ok {
			t.Fatalf("plugin.zip is missing %s", name.zipPath)
		}
		if !bytes.Equal([]byte(got), want) {
			t.Fatalf("%s differs from the embedded file (%d bytes vs %d)", name.zipPath, len(got), len(want))
		}
		if strings.Contains(got, "{{BASE_URL}}") || strings.Contains(got, "{{VERSION}}") {
			t.Fatalf("%s was rewritten as text", name.zipPath)
		}
	}

	var claudeManifest struct {
		Icon string `json:"icon"`
	}
	if err := json.Unmarshal([]byte(entries["simple-host/.claude-plugin/plugin.json"]), &claudeManifest); err != nil {
		t.Fatal(err)
	}
	if claudeManifest.Icon != "./.claude-plugin/icon.png" {
		t.Fatalf("agent-app manifest icon = %q", claudeManifest.Icon)
	}

	var agentManifest struct {
		Extensions map[string]struct {
			Interface struct {
				ComposerIcon string `json:"composerIcon"`
				Logo         string `json:"logo"`
			} `json:"interface"`
		} `json:"extensions"`
	}
	if err := json.Unmarshal([]byte(entries["simple-host/plugin.json"]), &agentManifest); err != nil {
		t.Fatal(err)
	}
	iface := agentManifest.Extensions["com.openai"].Interface
	if iface.ComposerIcon != "./assets/icon.png" || iface.Logo != "./assets/logo.png" {
		t.Fatalf("Agent Plugins interface icons = %#v", iface)
	}

	skills, err := buildSkillsZip(base)
	if err != nil {
		t.Fatal(err)
	}
	assertZipHasNoProductIcons(t, "skills.zip", zipEntries(t, skills))
	for _, skillName := range agentSkillsDiscoveryAllowlist {
		archive, err := buildAgentSkillArchive(skillName, base)
		if err != nil {
			t.Fatal(err)
		}
		assertZipHasNoProductIcons(t, skillName+".zip", zipEntries(t, archive))
	}
}

func assertZipHasNoProductIcons(t *testing.T, label string, entries map[string]string) {
	t.Helper()
	for name := range entries {
		base := path.Base(name)
		if base == "icon.png" || base == "logo.png" || strings.Contains(name, ".claude-plugin") {
			t.Errorf("%s includes %s", label, name)
		}
	}
}
