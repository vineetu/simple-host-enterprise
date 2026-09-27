package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	db "github.com/vsriram/simple-host/internal/db"
)

// The "This instance" card: release, pending migrations, the bucket check,
// owners still waiting for a certificate (and for how long), and the
// limits in force.
func TestAdminInstanceStatus(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	w.deploy("vera", "/api/sites/notes")
	ctx := context.Background()
	if err := db.SetOwnerHostReady(ctx, w.database, "vera", true); err != nil {
		t.Fatal(err)
	}
	if _, err := w.database.Exec(`UPDATE sites SET created_at = now() - interval '3 hours' WHERE user_id = $1`, w.users["alice"]); err != nil {
		t.Fatal(err)
	}
	ready, waiting, err := db.OwnerHostReadiness(ctx, w.database)
	if err != nil || ready != 1 || len(waiting) != 1 || waiting[0].Label != "alice" {
		t.Fatalf("readiness = %d %+v %v, want vera ready and alice waiting", ready, waiting, err)
	}

	hosts, _ := NewHostModel("https://" + accessBase)
	checked := time.Now()
	h := NewAdminHandler(w.database, "https://"+accessBase, hosts, CookiePolicy{Secure: true}, w.keys, time.Hour, audit.NewDBRecorder(w.database)).
		WithInstanceStatus(InstanceStatus{
			Version: "v9.9.9", Commit: "0123456789abcdef0123", Schema: "0051",
			PendingMigrations: func(context.Context) ([]string, error) { return []string{"0052_next.sql"}, nil },
			Bucket:            func() (bool, bool, time.Time) { return false, true, checked },
			Limits:            []InstanceLimit{{Name: "Versions kept per site", Value: "5"}},
		})
	r := httptest.NewRequest(http.MethodGet, "https://"+accessBase+"/admin", nil)
	r.AddCookie(w.cookie("root", ""))
	rec := httptest.NewRecorder()
	h.dashboard(rec, r)
	page := rec.Body.String()
	for _, want := range []string{
		"This instance", "v9.9.9 · commit 0123456789ab · schema 0051",
		"1 waiting: 0052_next.sql", "failing", "1 ready · 1 waiting",
		"waiting: alice", "for 3 hours", "Versions kept per site",
		"Revoke a leaked key", "admin-activity-filter",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("admin page lacks %q", want)
		}
	}
	if strings.Contains(page, "waiting: vera") {
		t.Error("an owner whose certificate is ready is listed as waiting")
	}
}
