package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const serveTestSiteID = "0a0a0a0a-0000-4000-8000-000000000001"

func TestSiteFileHandlerServesTheLiveVersion(t *testing.T) {
	store := newTestStore(t)
	store.publish(t, "alice", "demo", serveTestSiteID, 1, map[string]string{
		"index.html":        "site home",
		"downloads/app.dmg": "download",
	})

	mux := newSiteFileTestMux(store)
	for _, test := range []struct {
		path string
		body string
	}{
		{path: "/demo/", body: "site home"},
		{path: "/demo/downloads/app.dmg", body: "download"},
	} {
		response := serveRequest(mux, test.path)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, body %q", test.path, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), test.body) {
			t.Fatalf("GET %s body = %q", test.path, response.Body.String())
		}
	}
}

// The database decides what is live: a deploy on another replica is served
// here as soon as the index says so, and a rollback likewise.
func TestSiteFileHandlerFollowsTheIndex(t *testing.T) {
	store := newTestStore(t)
	store.publish(t, "alice", "demo", serveTestSiteID, 1, map[string]string{"index.html": "first"})
	mux := newSiteFileTestMux(store)
	if body := serveRequest(mux, "/demo/").Body.String(); !strings.Contains(body, "first") {
		t.Fatalf("v1 body = %q", body)
	}
	store.publish(t, "alice", "demo", serveTestSiteID, 2, map[string]string{"index.html": "second"})
	if body := serveRequest(mux, "/demo/").Body.String(); !strings.Contains(body, "second") {
		t.Fatalf("v2 body = %q", body)
	}
	store.index.set("alice", "demo", serveTestSiteID, 1)
	if body := serveRequest(mux, "/demo/").Body.String(); !strings.Contains(body, "first") {
		t.Fatalf("rolled-back body = %q", body)
	}
	store.index.remove("alice", "demo")
	if response := serveRequest(mux, "/demo/"); response.Code != http.StatusNotFound {
		t.Fatalf("deleted site status = %d, want 404", response.Code)
	}
}

func TestSiteFileHandlerLiveVersionMissingFromBucketIs404(t *testing.T) {
	store := newTestStore(t)
	store.index.set("alice", "demo", serveTestSiteID, 3)
	if response := serveRequest(newSiteFileTestMux(store), "/demo/"); response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}

func TestSiteFileHandlerRejectsEncodedIdentityTraversal(t *testing.T) {
	store := newTestStore(t)
	store.publish(t, "alice", "demo", serveTestSiteID, 1, map[string]string{"index.html": "site home"})
	mux := newSiteFileTestMux(store)
	for _, requestPath := range []string{
		"/%2e%2e/",
		"/demo%2Fother/",
		"/demo%0A/",
		"/demo/%2e%2e/%2e%2e/etc/passwd",
	} {
		response := serveRequest(mux, requestPath)
		if response.Code == http.StatusOK {
			t.Errorf("GET %s status = %d, want not found", requestPath, response.Code)
		}
	}
}

func TestSiteFileHandlerDoesNotListDirectories(t *testing.T) {
	store := newTestStore(t)
	store.publish(t, "alice", "demo", serveTestSiteID, 1, map[string]string{"assets/app.js": "app"})
	response := serveRequest(newSiteFileTestMux(store), "/demo/assets/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if strings.Contains(response.Body.String(), "app.js") {
		t.Fatal("directory listing exposed app.js")
	}
}

// newSiteFileTestMux serves alice's sites the way the host gate does on her
// own host: /{sitename}/..., with no host session to record.
func newSiteFileTestMux(store *testStore) *http.ServeMux {
	files := NewSiteFiles(store.Store, nil, CookiePolicy{}, nil, 0)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{sitename}/", func(w http.ResponseWriter, r *http.Request) {
		siteName := r.PathValue("sitename")
		files.serveSite(w, r, "alice", siteName, "/"+siteName, "", "")
	})
	return mux
}

func serveRequest(handler http.Handler, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, target, nil).WithContext(context.Background())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
