package handler

import (
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionFilesListsLargeContentWithoutReadingIt(t *testing.T) {
	root := t.TempDir()
	// Sparse on disk: prove listing does not read a 65 MiB payload or hash it
	// when it exceeds the per-file hash cap.
	f, err := os.Create(filepath.Join(root, "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(65 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := os.WriteFile(filepath.Join(root, "photo.jpg"), []byte("photo"), 0600); err != nil {
		t.Fatal(err)
	}
	h := &SiteHandler{}
	rec := httptest.NewRecorder()
	h.writeVersionFiles(rec, httptest.NewRequest("GET", "/?sha256=large.bin&sha256=photo.jpg", nil), os.DirFS(root), 7)
	if rec.Code != 200 || rec.Body.Len() > 512 || !strings.Contains(rec.Body.String(), `"size":68157440`) || !strings.Contains(rec.Body.String(), fmt.Sprintf("%x", sha256.Sum256([]byte("photo")))) {
		t.Fatal(rec.Code, rec.Body)
	}
	if strings.Count(rec.Body.String(), "sha256") != 1 || rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal(rec.Header(), rec.Body)
	}
	rec = httptest.NewRecorder()
	h.writeVersionFiles(rec, httptest.NewRequest("GET", "/?"+strings.Repeat("sha256=photo.jpg&", 31), nil), os.DirFS(root), 7)
	if rec.Code != 400 {
		t.Fatal(rec.Code)
	}
}
