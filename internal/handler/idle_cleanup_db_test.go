package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/vsriram/simple-host/internal/audit"
	db "github.com/vsriram/simple-host/internal/db"
)

type fakeMailer struct {
	mu   sync.Mutex
	sent map[string][]string // subject -> recipients
}

func (m *fakeMailer) Send(_ context.Context, to []string, subject, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sent == nil {
		m.sent = map[string][]string{}
	}
	m.sent[subject] = append([]string(nil), to...)
	return nil
}

func (w *accessWorld) idleSince(name string) (marked, kept, deleted bool) {
	w.t.Helper()
	if err := w.database.QueryRow(`SELECT idle_since IS NOT NULL, idle_keep, deleted_at IS NOT NULL FROM sites WHERE name = $1`, name).Scan(&marked, &kept, &deleted); err != nil {
		w.t.Fatal(err)
	}
	return marked, kept, deleted
}

// The idle cleanup, end to end: unused sites are marked (owner and team
// told on the dashboard and by email), a visit unmarks one, Keep takes one
// out for good, and a site still unused 30 days after it was marked moves
// to Recently deleted, from where it can be restored. Every step audited.
func TestIdleCleanup(t *testing.T) {
	w := newAccessWorld(t)
	ctx := context.Background()
	w.deploy("alice", "/api/sites/old")
	w.deploy("alice", "/api/sites/fresh")
	w.newTeam("crew", "mo", "olly")
	w.deploy("mo", "/api/collaboration/sites/team-crew/board")
	if _, err := w.database.Exec(`UPDATE sites SET created_at = now() - interval '100 days', updated_at = now() - interval '100 days' WHERE name IN ('old', 'board')`); err != nil {
		t.Fatal(err)
	}
	mailer := &fakeMailer{}
	cleanup := NewIdleCleanup(w.database, audit.NewDBRecorder(w.database), 60, mailer, "https://"+accessBase)
	if err := cleanup.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"old": true, "board": true, "fresh": false} {
		if marked, _, _ := w.idleSince(name); marked != want {
			t.Fatalf("%s marked = %v, want %v", name, marked, want)
		}
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_idle_marked'`); n != 2 {
		t.Fatalf("site_idle_marked events = %d, want 2", n)
	}
	var teamMail []string
	for subject, to := range mailer.sent {
		if strings.HasPrefix(subject, "team-crew/board ") {
			teamMail = to
		}
		if strings.HasPrefix(subject, "alice/old ") && (len(to) != 1 || to[0] != "alice@example.com") {
			t.Fatalf("alice/old mail went to %v", to)
		}
	}
	sort.Strings(teamMail)
	if strings.Join(teamMail, ",") != "mo@example.com,olly@example.com" {
		t.Fatalf("team site mail went to %v (all mail: %v)", teamMail, mailer.sent)
	}
	// A second run the same hour marks nothing again.
	if err := cleanup.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_idle_marked'`); n != 2 {
		t.Fatalf("site_idle_marked events after a second run = %d, want 2", n)
	}

	// The dashboard tells the owner and each team member; /admin lists both.
	olly, _ := db.GetUserByUsername(ctx, w.database, "olly")
	if notice := idleNoticeHTML(ctx, w.database, &olly); !strings.Contains(notice, "team-crew/board") || !strings.Contains(notice, "Will move to Recently deleted on") || strings.Contains(notice, "alice/old") {
		t.Fatalf("olly's notice = %q", notice)
	}
	vera, _ := db.GetUserByUsername(ctx, w.database, "vera")
	if notice := idleNoticeHTML(ctx, w.database, &vera); notice != "" {
		t.Fatalf("someone with no idle sites sees %q", notice)
	}
	r := httptest.NewRequest(http.MethodGet, "https://"+accessBase+"/admin", nil)
	r.AddCookie(w.cookie("root", ""))
	if page := w.do(r).Body.String(); !strings.Contains(page, "Not used lately") || !strings.Contains(page, "alice/old") || !strings.Contains(page, "team-crew/board") {
		t.Fatal("/admin does not list the idle sites")
	}

	// Keep: a team member keeps the team's site; a stranger cannot.
	if rec := w.api("vera", http.MethodPost, "/api/collaboration/sites/team-crew/board/keep", map[string]any{"keep": true}); rec.Code == http.StatusOK {
		t.Fatalf("a stranger kept a team's site: %s", rec.Body)
	}
	if rec := w.api("olly", http.MethodPost, "/api/collaboration/sites/team-crew/board/keep", map[string]any{"keep": true}); rec.Code != http.StatusOK {
		t.Fatalf("member keep = %d %s", rec.Code, rec.Body)
	}
	if marked, kept, _ := w.idleSince("board"); marked || !kept {
		t.Fatalf("kept site: marked %v kept %v", marked, kept)
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_idle_keep' AND detail->>'keep' = 'true'`); n != 1 {
		t.Fatalf("site_idle_keep events = %d", n)
	}

	// A visit unmarks; unused again, it is marked again.
	id := w.siteID("alice", "old")
	if _, err := w.database.Exec(`INSERT INTO site_daily_analytics (site_id, day, pageviews, visits, last_seen_at) VALUES ($1, current_date, 1, 1, now())`, id); err != nil {
		t.Fatal(err)
	}
	if err := cleanup.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if marked, _, _ := w.idleSince("old"); marked {
		t.Fatal("a visited site is still marked")
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_idle_cleared' AND site_id = $1`, id); n != 1 {
		t.Fatalf("site_idle_cleared events = %d", n)
	}
	if _, err := w.database.Exec(`DELETE FROM site_daily_analytics WHERE site_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := cleanup.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if marked, _, _ := w.idleSince("old"); !marked {
		t.Fatal("an unused site was not marked again")
	}

	// 30 days after it was marked, still unused: Recently deleted.
	if _, err := w.database.Exec(`UPDATE sites SET idle_since = now() - interval '31 days' WHERE name IN ('old', 'board')`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.database.Exec(`UPDATE sites SET idle_keep = true, idle_since = now() - interval '31 days' WHERE name = 'board'`); err != nil {
		t.Fatal(err)
	}
	if err := cleanup.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if marked, _, deleted := w.idleSince("old"); !deleted || marked {
		t.Fatalf("due site: deleted %v, still marked %v", deleted, marked)
	}
	if _, _, deleted := w.idleSince("board"); deleted {
		t.Fatal("a kept site was deleted")
	}
	if n := w.count(`SELECT count(*) FROM audit_events WHERE action = 'site_delete' AND site_id = $1 AND actor_kind = 'system' AND detail->>'reason' = 'idle'`, id); n != 1 {
		t.Fatalf("idle site_delete events = %d", n)
	}
	var deletedList []map[string]any
	_ = json.Unmarshal(w.api("alice", http.MethodGet, "/api/deleted-sites", nil).Body.Bytes(), &deletedList)
	if len(deletedList) != 1 || deletedList[0]["site"] != "old" {
		t.Fatalf("alice's Recently deleted = %v", deletedList)
	}
	if rec := w.api("alice", http.MethodPost, "/api/sites/old/restore", nil); rec.Code != http.StatusOK {
		t.Fatalf("restore an idle-deleted site = %d %s", rec.Code, rec.Body)
	}
	if err := cleanup.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if marked, _, deleted := w.idleSince("old"); marked || deleted {
		t.Fatalf("restored site: marked %v deleted %v; a restore counts as use", marked, deleted)
	}
}
