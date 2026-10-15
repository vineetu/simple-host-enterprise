package handler

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/vsriram/simple-host/internal/oplimits"
)

func TestDeployKeepQuery(t *testing.T) {
	for _, tc := range []struct {
		query  string
		keep   bool
		remove []string
		ok     bool
	}{
		{"", false, nil, true},
		{"keep=1", true, nil, true},
		{"keep=1&remove=old.html&remove=./img/a.png", true, []string{"old.html", "img/a.png"}, true},
		{"remove=old.html", false, nil, false},
		{"keep=yes", false, nil, false},
		{"keep=1&remove=../etc/passwd", false, nil, false},
		{"keep=1&remove=/abs.html", false, nil, false},
	} {
		rec := httptest.NewRecorder()
		keep, remove, ok := deployKeep(rec, httptest.NewRequest("PUT", "/api/sites/demo?"+tc.query, nil))
		if ok != tc.ok || keep != tc.keep {
			t.Errorf("%q: keep=%v ok=%v, want %v %v", tc.query, keep, ok, tc.keep, tc.ok)
			continue
		}
		for _, p := range tc.remove {
			if !remove[p] {
				t.Errorf("%q: %s not in remove %v", tc.query, p, remove)
			}
		}
		if !ok && rec.Code != 400 {
			t.Errorf("%q: status %d, want 400", tc.query, rec.Code)
		}
	}
}

func TestKeepLiveFilesMerges(t *testing.T) {
	store := newTestStore(t)
	siteID := testSiteID("alice", "demo")
	store.publish(t, "alice", "demo", siteID, 1, map[string]string{
		"index.html": "home", "about.html": "old about", "old.html": "gone", "img/a.txt": "a",
	})
	h := &SiteHandler{store: store.Store}
	ctx := context.Background()

	files := map[string][]byte{"about.html": []byte("new about")}
	if err := h.keepLiveFiles(ctx, siteID, 1, files, map[string]bool{"old.html": true}); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"index.html": "home", "about.html": "new about", "img/a.txt": "a"}
	if len(files) != len(want) {
		t.Fatalf("merged %d files, want %d: %v", len(files), len(want), files)
	}
	for name, body := range want {
		if string(files[name]) != body {
			t.Errorf("%s = %q, want %q", name, files[name], body)
		}
	}

	if err := h.keepLiveFiles(ctx, siteID, 1, map[string][]byte{"about.html": nil}, map[string]bool{"index.html": true}); !errors.Is(err, errMergedNoIndex) {
		t.Errorf("removing index.html: %v, want errMergedNoIndex", err)
	}
	// A site with no live version keeps nothing; the upload must stand alone.
	if err := h.keepLiveFiles(ctx, siteID, 0, map[string][]byte{"about.html": nil}, nil); !errors.Is(err, errMergedNoIndex) {
		t.Errorf("no live version: %v, want errMergedNoIndex", err)
	}

	previous := oplimits.Get()
	limits := previous
	limits.MaxFilesPerSite = 2
	oplimits.Set(limits)
	defer oplimits.Set(previous)
	if err := h.keepLiveFiles(ctx, siteID, 1, map[string][]byte{"about.html": nil}, nil); !errors.Is(err, errMergedTooLarge) {
		t.Errorf("over the entry limit: %v, want errMergedTooLarge", err)
	}
}
