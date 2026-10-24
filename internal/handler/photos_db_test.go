package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io/fs"
	"math/rand"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/db"
)

func photoRequest(w *accessWorld, user, owner string, names []string, data [][]byte, session bool, origin string, etags ...string) *httptest.ResponseRecorder {
	body, kind := photoMultipart(w.t, names, data)
	r := httptest.NewRequest("POST", "https://"+accessBase+"/v1/sites/demo/photos?owner="+owner, strings.NewReader(body))
	r.Header.Set("Content-Type", kind)
	if len(etags) > 0 {
		r.Header.Set("If-Match", etags[0])
	}
	r.Header.Set("X-Simple-Host-Client", "control-ui")
	if session {
		r.AddCookie(w.cookie(user, ""))
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
	} else {
		r.Header.Set("X-API-Key", w.apiKeys[user])
	}
	return w.do(r)
}
func TestPhotosVersionAccessAndSession(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	jpg := photoJPEG(t, 8, 6)
	for _, user := range []string{"vera", "root"} {
		rec := photoRequest(w, user, "alice", []string{"front.jpg"}, [][]byte{jpg}, false, "")
		if rec.Code != 404 {
			t.Fatalf("non-owner %s: %d %s", user, rec.Code, rec.Body)
		}
	}
	for _, origin := range []string{"", "https://demo.alice." + accessBase} {
		rec := photoRequest(w, "alice", "alice", []string{"front.jpg"}, [][]byte{jpg}, true, origin)
		if rec.Code != 403 {
			t.Fatalf("cross-origin write: %d %s", rec.Code, rec.Body)
		}
	}
	rec := photoRequest(w, "alice", "alice", []string{"front.jpg", "front.jpg"}, [][]byte{jpg, jpg}, true, "https://"+accessBase)
	if rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	var result struct {
		Version int
		Photos  []photoResult
	}
	json.Unmarshal(rec.Body.Bytes(), &result)
	if result.Version != 2 || len(result.Photos) != 2 || result.Photos[1].Path != "images/front-2.jpg" {
		t.Fatalf("result: %s", rec.Body)
	}
	site, err := db.GetSite(context.Background(), w.database, w.users["alice"], "demo")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := w.store.OpenVersion(context.Background(), site.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if b, err := fs.ReadFile(lease.FS(), "index.html"); err != nil || string(b) != "<h1>hi</h1>" {
		t.Fatal("existing index lost")
	}
	if b, err := fs.ReadFile(lease.FS(), "images/front.jpg"); err != nil || string(b) != string(jpg) {
		t.Fatal("photo bytes changed")
	}
	var updates int
	if err := w.database.QueryRow(`SELECT count(*) FROM audit_events WHERE site_id=$1 AND action='site_update'`, site.ID).Scan(&updates); err != nil || updates != 1 {
		t.Fatalf("audit: %d %v", updates, err)
	}
}
func TestPhotosTeamAndQuota(t *testing.T) {
	w := newAccessWorldWith(t, UploadQuota{MaxBytes: 2000}, nil)
	if rec := w.api("mo", "POST", "/api/teams", map[string]any{"name": "sales"}); rec.Code != 201 {
		t.Fatal(rec.Body)
	}
	w.deploy("mo", "/api/collaboration/sites/team-sales/demo")
	jpg := photoJPEG(t, 8, 6)
	if rec := photoRequest(w, "mo", "team-sales", []string{"front.jpg"}, [][]byte{jpg}, false, ""); rec.Code != 200 {
		t.Fatalf("team: %d %s", rec.Code, rec.Body)
	}
	if rec := photoRequest(w, "alice", "team-sales", []string{"front.jpg"}, [][]byte{jpg}, false, ""); rec.Code != 404 {
		t.Fatalf("non-member: %d", rec.Code)
	}
	// Many valid photos exceed the small quota without changing the live version.
	var noisy bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	rng := rand.New(rand.NewSource(1))
	rng.Read(img.Pix)
	if err := jpeg.Encode(&noisy, img, nil); err != nil {
		t.Fatal(err)
	}
	jpg = noisy.Bytes()
	names := []string{}
	data := [][]byte{}
	for i := 0; i < 10; i++ {
		names = append(names, "front.jpg")
		data = append(data, jpg)
	}
	rec := photoRequest(w, "mo", "team-sales", names, data, false, "")
	if rec.Code != 413 || !strings.Contains(rec.Body.String(), "storage_quota") {
		t.Fatalf("quota: %d %s", rec.Code, rec.Body)
	}
	var active int
	if err := w.database.QueryRow(`SELECT active_version FROM sites s JOIN users u ON u.id=s.user_id WHERE u.username='team-sales' AND s.name='demo'`).Scan(&active); err != nil || active != 2 {
		t.Fatalf("quota changed version: %d %v", active, err)
	}
}
func TestPhotosInvalidBatchIsAtomic(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	for _, names := range [][]string{{"front.jpg", "bad.jpg"}, {"../front.jpg"}} {
		data := [][]byte{photoJPEG(t, 8, 6)}
		if len(names) > 1 {
			data = append(data, []byte("bad"))
		}
		if rec := photoRequest(w, "alice", "alice", names, data, false, ""); rec.Code != 400 {
			t.Fatalf("invalid: %d %s", rec.Code, rec.Body)
		}
	}
	site, err := db.GetSite(context.Background(), w.database, w.users["alice"], "demo")
	if err != nil || site.ActiveVersion != 1 {
		t.Fatal("invalid batch created a version")
	}
}
func TestPhotosPageOIDCAndCSP(t *testing.T) {
	w := newAccessWorld(t)
	h := NewDashboardHandler(w.database, w.keys, time.Hour)
	for _, signed := range []bool{false, true} {
		r := httptest.NewRequest("GET", "https://"+accessBase+"/photos?site=demo&account=alice", nil)
		if signed {
			r.AddCookie(w.cookie("alice", ""))
		}
		rec := httptest.NewRecorder()
		h.photos(rec, r)
		if !signed {
			if rec.Code != 303 || !strings.HasPrefix(rec.Header().Get("Location"), "/auth/login?to=") {
				t.Fatal("missing company sign-in")
			}
			continue
		}
		if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "worker-src 'self'") || strings.Contains(rec.Body.String(), "__PHOTO") {
			t.Fatal("bad photo page")
		}
	}
}

func TestPhotoRetryMetadataAndPrecondition(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	current := w.api("alice", "GET", "/api/collaboration/sites/alice/demo", nil)
	var site collaborationSiteResponse
	if err := json.Unmarshal(current.Body.Bytes(), &site); err != nil {
		t.Fatal(err)
	}
	jpg := photoJPEG(t, 8, 6)
	if rec := photoRequest(w, "alice", "alice", []string{"front.jpg"}, [][]byte{jpg}, false, "", site.ETag); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	// A stale reservation cannot silently receive a collision suffix.
	if rec := photoRequest(w, "alice", "alice", []string{"front.jpg"}, [][]byte{jpg}, false, "", site.ETag); rec.Code != 412 {
		t.Fatal(rec.Code, rec.Body)
	}
	path := "/api/collaboration/sites/alice/demo/versions/2/files?sha256=images%2Ffront.jpg"
	rec := w.api("alice", "GET", path, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), fmt.Sprintf("%x", sha256.Sum256(jpg))) || !strings.Contains(rec.Body.String(), "index.html") {
		t.Fatal(rec.Code, rec.Body)
	}
	for _, actor := range []string{"vera", "root"} {
		if denied := w.api(actor, "GET", path, nil); denied.Code != 404 {
			t.Fatal(actor, denied.Code)
		}
	}
	if missing := w.api("alice", "GET", "/api/collaboration/sites/alice/demo/versions/99/files", nil); missing.Code != 404 {
		t.Fatal(missing.Code)
	}
	// Requested hashes never escape the retained version or open arbitrary paths.
	if safe := w.api("alice", "GET", path+"&sha256=../../outside", nil); safe.Code != 200 || strings.Contains(safe.Body.String(), "outside") {
		t.Fatal(safe.Code, safe.Body)
	}
}
