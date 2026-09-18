package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/vsriram/simple-host/internal/db"
)

func TestCollaborationPreconditionBehavior(t *testing.T) {
	site := db.Site{ID: "4b256b3b-9d15-4ce5-bba7-b04ef26f49e5", ActiveVersion: 7}
	wantETag := `"site-4b256b3b-9d15-4ce5-bba7-b04ef26f49e5-v7"`

	tests := []struct {
		name       string
		required   bool
		ifMatch    string
		wantOK     bool
		wantStatus int
	}{
		{name: "optional missing", wantOK: true, wantStatus: http.StatusOK},
		{name: "required missing", required: true, wantStatus: http.StatusPreconditionRequired},
		{name: "matching", required: true, ifMatch: wantETag, wantOK: true, wantStatus: http.StatusOK},
		{name: "stale", required: true, ifMatch: `"site-old-v6"`, wantStatus: http.StatusPreconditionFailed},
		{name: "wildcard is not accepted", required: true, ifMatch: "*", wantStatus: http.StatusPreconditionFailed},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/", nil)
			request.Header.Set("If-Match", test.ifMatch)
			response := httptest.NewRecorder()
			gotOK := requireSitePrecondition(response, request, site, test.required)
			if gotOK != test.wantOK {
				t.Fatalf("requireSitePrecondition ok = %t, want %t", gotOK, test.wantOK)
			}
			if got := response.Header().Get("ETag"); got != wantETag {
				t.Fatalf("ETag = %q, want %q", got, wantETag)
			}
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, test.wantStatus)
			}
			if test.wantOK {
				return
			}
			var body collaborationPreconditionResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.ActiveVersion != 7 || body.ETag != wantETag {
				t.Fatalf("precondition response = %+v", body)
			}
		})
	}
}

func TestArchiveStreamKeepsWebsiteContentsAtZipRoot(t *testing.T) {
	root := fstest.MapFS{
		"index.html":        &fstest.MapFile{Data: []byte("home")},
		"assets":            &fstest.MapFile{Mode: fs.ModeDir},
		"assets/app.js":     &fstest.MapFile{Data: []byte("app")},
		"downloads/app.dmg": &fstest.MapFile{Data: []byte{0, 1, 2}},
	}
	entries, err := collectArchiveEntries(context.Background(), root)
	if err != nil {
		t.Fatalf("collectArchiveEntries: %v", err)
	}
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	if err := streamArchiveEntries(context.Background(), writer, root, entries); err != nil {
		t.Fatalf("streamArchiveEntries: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	contents := make(map[string]string)
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}
		opened, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(opened)
		closeErr := opened.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read %s: read=%v close=%v", file.Name, readErr, closeErr)
		}
		contents[file.Name] = string(body)
	}
	if contents["index.html"] != "home" || contents["assets/app.js"] != "app" {
		t.Fatalf("archive contents = %#v", contents)
	}
	for name := range contents {
		if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "v7/") {
			t.Fatalf("archive entry %q was not rooted at website contents", name)
		}
	}
}

func TestArchiveCollectionRejectsNonRegularEntriesAndCancellation(t *testing.T) {
	linkFS := fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("home")},
		"escape":     &fstest.MapFile{Mode: fs.ModeSymlink},
	}
	if _, err := collectArchiveEntries(context.Background(), linkFS); err == nil {
		t.Fatal("collectArchiveEntries accepted a symlink")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := collectArchiveEntries(ctx, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("home")}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled collect error = %v, want context.Canceled", err)
	}
}

func TestArchiveRouteUsesStreamingSafeSkillVersionMiddleware(t *testing.T) {
	handler := NewSiteHandler(nil, nil, nil, "", HostModel{})
	mux := http.NewServeMux()
	identity := func(next http.Handler) http.Handler { return next }
	notice := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Test-Notice", "applied")
			next.ServeHTTP(w, r)
		})
	}
	handler.Register(mux, identity, notice)

	metadata := httptest.NewRecorder()
	mux.ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, "/api/collaboration/sites/alice/demo", nil))
	if metadata.Header().Get("X-Test-Notice") != "applied" {
		t.Fatal("metadata route did not use skill version middleware")
	}

	archive := httptest.NewRecorder()
	mux.ServeHTTP(archive, httptest.NewRequest(http.MethodGet, "/api/collaboration/sites/alice/demo/versions/1/archive", nil))
	if archive.Header().Get("X-Test-Notice") != "applied" {
		t.Fatal("archive route did not use skill version middleware")
	}
	if archive.Code != http.StatusUnauthorized {
		t.Fatalf("archive without auth status = %d, want 401", archive.Code)
	}
}
