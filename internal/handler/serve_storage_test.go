package handler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vsriram/simple-host/internal/storage"
)

func TestSiteFileHandlerServesAuthorizedRootedSite(t *testing.T) {
	store, _ := newServeTestStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{
		"index.html":        []byte("site home"),
		"downloads/app.dmg": []byte("download"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentVersion("alice", "demo", 1); err != nil {
		t.Fatal(err)
	}

	mux := newSiteFileTestMux(store)
	for _, test := range []struct {
		path string
		body string
	}{
		{path: "/sites/alice/demo/", body: "site home"},
		{path: "/sites/alice/demo/downloads/app.dmg", body: "download"},
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

func TestSiteFileHandlerRejectsEncodedIdentityTraversal(t *testing.T) {
	store, _ := newServeTestStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("site home")}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentVersion("alice", "demo", 1); err != nil {
		t.Fatal(err)
	}
	mux := newSiteFileTestMux(store)
	for _, requestPath := range []string{
		"/sites/alice/%2e%2e/",
		"/sites/alice%2Fother/demo/",
		"/sites/alice/demo%2Fother/",
		"/sites/alice/demo%0A/",
		"/sites/alice%09/demo/",
	} {
		response := serveRequest(mux, requestPath)
		if response.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404", requestPath, response.Code)
		}
	}
}

func TestSiteFileHandlerCannotReadOutsideVersionRoot(t *testing.T) {
	store, base := newServeTestStorage(t)
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("site home")}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentVersion("alice", "demo", 1); err != nil {
		t.Fatal(err)
	}
	versionDir := filepath.Join(base, "alice", "demo", "v1")
	relativeTarget, err := filepath.Rel(versionDir, outsideFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relativeTarget, filepath.Join(versionDir, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	mux := newSiteFileTestMux(store)
	for _, requestPath := range []string{
		"/sites/alice/demo/escape.txt",
		"/sites/alice/demo/%2e%2e/%2e%2e/secret.txt",
	} {
		response := serveRequest(mux, requestPath)
		if strings.Contains(response.Body.String(), "outside-secret") {
			t.Fatalf("GET %s exposed outside file", requestPath)
		}
		if response.Code == http.StatusOK {
			t.Fatalf("GET %s unexpectedly succeeded", requestPath)
		}
	}
}

func TestSiteFileHandlerRejectsUnsafeCurrentTarget(t *testing.T) {
	store, base := newServeTestStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"index.html": []byte("demo")}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(base, "alice", "other", "v1")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "index.html"), []byte("sibling-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../other/v1", filepath.Join(base, "alice", "demo", "current")); err != nil {
		t.Fatal(err)
	}
	response := serveRequest(newSiteFileTestMux(store), "/sites/alice/demo/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if strings.Contains(response.Body.String(), "sibling-secret") {
		t.Fatal("unsafe current target served sibling site")
	}
}

func TestSiteFileHandlerDoesNotListDirectories(t *testing.T) {
	store, _ := newServeTestStorage(t)
	if err := store.WriteFiles("alice", "demo", 1, map[string][]byte{"assets/app.js": []byte("app")}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrentVersion("alice", "demo", 1); err != nil {
		t.Fatal(err)
	}
	response := serveRequest(newSiteFileTestMux(store), "/sites/alice/demo/assets/")
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	if strings.Contains(response.Body.String(), "app.js") {
		t.Fatal("directory listing exposed app.js")
	}
}

func newSiteFileTestMux(store *storage.DiskStorage) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /sites/{user}/{sitename}/", siteFileHandler(store, nil, CookiePolicy{}, nil, 0))
	return mux
}

func serveRequest(handler http.Handler, target string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func newServeTestStorage(t *testing.T) (*storage.DiskStorage, string) {
	t.Helper()
	base := t.TempDir()
	store, err := storage.NewDiskStorage(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return store, base
}
