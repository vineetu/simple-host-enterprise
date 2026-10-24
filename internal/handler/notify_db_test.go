package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/db"
	mcp "github.com/vsriram/simple-host/internal/notifyevents"
	"github.com/vsriram/simple-host/internal/oplimits"
	sp "github.com/vsriram/simple-host/internal/storagepolicy"
)

func notificationWorld(t *testing.T) (*accessWorld, *notificationReceiver, string) {
	t.Helper()
	w := newAccessWorld(t)
	h := w.notifications.WithNotifications(bytes.Repeat([]byte{3}, 32), []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	w.siteAPI.WithNotifications(h)
	x := newNotificationReceiver(t)
	h.notifyEngine().client = x.client()
	h.notifyEngine().client.secrets = &h.notifyCipher
	site := w.storageSite("notify", db.AccessCompany)
	if _, err := w.database.Exec(`UPDATE sites SET active_version=1 WHERE id=$1`, site); err != nil {
		t.Fatal(err)
	}
	tx, err := w.database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, kind := range []string{"kv", "db", "files"} {
		res := db.StorageResource{SiteID: site, Name: kind, Kind: kind, Preset: "personal", Access: sp.Presets["personal"]}
		if kind == "db" {
			res.Settings.Tables = map[string]db.StorageTable{"items": {Preset: "personal", Access: sp.Presets["personal"], Columns: map[string]sp.Column{"text": {Type: "text"}}}}
		}
		if _, err = db.PutStorageResource(context.Background(), tx, res); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return w, x, site
}
func addTestNotification(t *testing.T, w *accessWorld, x *notificationReceiver) string {
	t.Helper()
	r := w.api("alice", "POST", "/api/sites/notify/notifications", map[string]any{"url": x.server.URL, "events": []string{"record.added", "record.changed", "file.uploaded", "file.deleted"}, "secret": notifyTestSecret, "auth_header": map[string]string{"name": "Authorization", "value": "Bearer test-header"}})
	if r.Code != 201 {
		t.Fatalf("create: %d %s", r.Code, r.Body.String())
	}
	var out struct {
		Notification struct {
			ID string `json:"id"`
		} `json:"notification"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Notification.ID
}
func TestNotificationVisitorMutationsAndAtomicOutbox(t *testing.T) {
	w, x, site := notificationWorld(t)
	id := addTestNotification(t, w, x)
	call := storageTestCaller{user: "vera"}
	req := func(method, path, body, kind string) {
		t.Helper()
		r := w.siteReq(call, "notify", method, "/api/site/storage/"+path, []byte(body), kind)
		if r.Code < 200 || r.Code >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, r.Code, r.Body.String())
		}
	}
	req("PUT", "kv/kv/keys/a", `{"value":{"number":9007199254740993}}`, "application/json")
	req("PUT", "kv/kv/keys/a", `{"value":{"number":9007199254740994}}`, "application/json")
	req("DELETE", "kv/kv/keys/a", "", "")
	req("POST", "db/db/tables/items/rows", `{"text":"first"}`, "application/json")
	var row int64
	if err := w.database.QueryRow(`SELECT id FROM site_storage_rows WHERE site_id=$1`, site).Scan(&row); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("db/db/tables/items/rows/%d", row)
	req("PATCH", path, `{"text":"edited"}`, "application/json")
	req("DELETE", path, "", "")
	req("PUT", "files/files/objects/a.txt", "first", "text/plain")
	req("PUT", "files/files/objects/a.txt", "edited", "text/plain")
	req("DELETE", "files/files/objects/a.txt", "", "")
	if got := countRows(t, w.database, `SELECT count(*) FROM site_events WHERE site_id=$1`, site); got != 9 {
		t.Fatalf("events=%d, want 9", got)
	}
	call.user = "alice"
	req("PUT", "kv/kv/keys/owner", `{"value":1}`, "application/json")
	if got := countRows(t, w.database, `SELECT count(*) FROM site_events WHERE site_id=$1`, site); got != 9 {
		t.Fatal("owner emitted")
	}
	// Atomic rollback includes the save, its outbox row, and its event budget.
	tx, err := w.database.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = db.EmitSiteEventTx(context.Background(), tx, site, "evt_rollback", "record.added", "kv", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	if got := countRows(t, w.database, `SELECT count(*) FROM site_events WHERE event_id='evt_rollback'`); got != 0 {
		t.Fatal("rolled-back event persisted")
	}
	// Two independent handler instances share leases and budgets through Postgres.
	other := NewSiteHandler(w.notifications.database, w.store, "https://"+accessBase, w.notifications.hosts).WithNotifications(bytes.Repeat([]byte{3}, 32), nil)
	other.notifyEngine().client = w.notifications.notifyEngine().client
	var workers sync.WaitGroup
	for _, h := range []*SiteHandler{w.notifications, other} {
		workers.Add(1)
		go func(h *SiteHandler) {
			defer workers.Done()
			for h.deliverNotification(context.Background()) {
			}
		}(h)
	}
	workers.Wait()
	if got := countRows(t, w.database, `SELECT count(*) FROM site_event_deliveries WHERE subscription_id=$1 AND state='delivered'`, id); got != 9 {
		t.Fatalf("delivered %d of 9", got)
	}
	received := x.snapshot()
	if len(received) != 9 {
		t.Fatalf("requests %d", len(received))
	}
	for _, r := range received {
		if r.duplicate {
			t.Fatal("duplicate delivery without lease expiry")
		}
		raw, _ := json.Marshal(r.body)
		if bytes.Contains(raw, []byte("@example.com")) {
			t.Fatal("event carried email")
		}
	}
	if got := countRows(t, w.database, `SELECT count(*) FROM site_events WHERE data->'fields'->>'number'='9007199254740993'`); got != 1 {
		t.Fatal("number precision lost")
	}
	if got := countRows(t, w.database, `SELECT count(*) FROM audit_events WHERE action='notification_create'`); got != 1 {
		t.Fatal("subscription audit absent")
	}
	// Network-open sites emit nothing, even for a signed-in visitor.
	if _, err = w.database.Exec(`UPDATE sites SET access='network' WHERE id=$1`, site); err != nil {
		t.Fatal(err)
	}
	call.user = "vera"
	req("PUT", "kv/kv/keys/network", `{"value":1}`, "application/json")
	if got := countRows(t, w.database, `SELECT count(*) FROM site_events WHERE site_id=$1`, site); got != 9 {
		t.Fatal("network site emitted")
	}
}

func TestNotificationSealingRotationAndDisabled(t *testing.T) {
	w, x, site := notificationWorld(t)
	id := addTestNotification(t, w, x)
	ctx := context.Background()
	n, err := db.GetNotification(ctx, w.database, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(n.Secret, "enc1:") || !strings.HasPrefix(n.AuthHeaderValue, "enc1:") {
		t.Fatal("plaintext secret stored")
	}
	if _, err = w.notifications.notifyCipher.openNotification(id+":header", n.Secret); err == nil {
		t.Fatal("associated data not bound")
	}
	r := w.api("alice", "POST", "/api/sites/notify/notifications/"+id+"/rotate-secret", nil)
	if r.Code != 200 {
		t.Fatalf("rotate %d", r.Code)
	}
	n, err = db.GetNotification(ctx, w.database, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(n.PrevSecret, "enc1:") || n.PrevSecretUntil == nil {
		t.Fatal("rotation lost sealed prior key")
	}
	secret, err := w.notifications.notifyCipher.openNotification(id, n.Secret)
	if err != nil {
		t.Fatal(err)
	}
	x.keys = []string{notifyTestSecret, secret}
	r = w.api("alice", "POST", "/api/sites/notify/notifications/"+id+"/test", nil)
	if r.Code != 200 || !strings.Contains(r.Body.String(), `"delivered":true`) {
		t.Fatalf("test %d %s", r.Code, r.Body.String())
	}
	wrong := (&SiteHandler{}).WithNotifications(bytes.Repeat([]byte{9}, 32), nil)
	if _, err = wrong.notifyCipher.openNotification(id, n.Secret); err == nil {
		t.Fatal("wrong key opened")
	}
	until, err := db.RotateNotification(ctx, w.notifications.database, id, notifyTestSecret, wrong.notificationSecrets())
	if err != nil || until != nil {
		t.Fatal("unreadable prior key retained")
	}
	// A missing encryption key refuses the whole REST surface and discovery.
	w.notifications.notifyCipher = notificationCipher{}
	if r = w.api("alice", "GET", "/api/sites/notify/notifications", nil); r.Code != 409 {
		t.Fatalf("disabled %d", r.Code)
	}
	if w.notifications.EventsBackend().(interface{ EventsAvailability() error }).EventsAvailability() == nil {
		t.Fatal("disabled events offered")
	}
	_ = site
}

func TestNotificationLeasesCapsAndRetries(t *testing.T) {
	w, x, site := notificationWorld(t)
	id := addTestNotification(t, w, x)
	ctx := context.Background()
	h := w.notifications
	l := oplimits.Get()
	tx, _ := w.database.Begin()
	if err := db.EmitSiteEventTx(ctx, tx, site, "evt_retry", "record.added", "kv", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	d, err := db.ClaimNotificationDelivery(ctx, h.database, time.Minute, l.NotifyAttempts, 7, l.NotifySuspendWindow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ClaimNotificationDelivery(ctx, h.database, time.Minute, l.NotifyAttempts, 7, l.NotifySuspendWindow); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("double claim")
	}
	n, _ := db.GetNotification(ctx, h.database, id)
	ok, err := db.AcquireNotificationSend(ctx, h.database, n, d, 1, 1, 1, 1)
	if err != nil || !ok {
		t.Fatal("send budget refused", err)
	}
	if ok, err = db.AcquireNotificationSend(ctx, h.database, n, d, 1, 1, 1, 1); err != nil || ok {
		t.Fatal("duplicate send admitted")
	}
	x.setStatus(500)
	result := h.notifyEngine().client.send(ctx, n, d.EventID, notifyBody(d, 0))
	if result.Delivered {
		t.Fatal("failed response accepted")
	}
	if _, err = db.FinishNotificationDelivery(ctx, h.database, d, "pending", result.Result, 500, 1, time.Now().Add(time.Minute), l.NotifySuspendWindow, 100, 95, 72*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ClaimNotificationDelivery(ctx, h.database, time.Minute, l.NotifyAttempts, 7, l.NotifySuspendWindow); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("backoff ignored")
	}
	if _, err = w.database.Exec(`UPDATE site_event_deliveries SET next_attempt_at=now() WHERE subscription_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	next, err := db.ClaimNotificationDelivery(ctx, h.database, time.Minute, l.NotifyAttempts, 7, l.NotifySuspendWindow)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err = db.AcquireNotificationSend(ctx, h.database, n, next, 1, 1, 1, 1); err != nil || ok {
		t.Fatal("shared cap exceeded")
	}
	if _, err = db.FinishNotificationDelivery(ctx, h.database, d, "delivered", "ok", 200, 1, time.Now(), l.NotifySuspendWindow, 100, 95, 72*time.Hour); err != nil {
		t.Fatal(err)
	}
	held, err := db.NotificationLeaseHeld(ctx, h.database, next)
	if err != nil || !held {
		t.Fatal("stale worker overwrote current lease")
	}
	if err = db.DeferNotificationDelivery(ctx, h.database, next); err != nil {
		t.Fatal(err)
	}
	// Subscription cap is serialized across independent connections.
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := db.SaveNotification(ctx, h.database, db.Notification{ID: notifyRandom("sub_", 8), SiteID: site, OwnerID: w.users["alice"], Source: "owner", URL: x.server.URL, Events: []string{"record.added"}, Secret: notifyTestSecret}, 2, nil, 7, h.notificationSecrets())
			if err == nil {
				mu.Lock()
				created++
				mu.Unlock()
			} else if !errors.Is(err, db.ErrNotificationCap) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Fatalf("cap allowed %d additional subscriptions", created)
	}
	// A crashed worker's final lease expires without leaving the event pending.
	if _, err = w.database.Exec(`UPDATE site_event_deliveries SET attempts=$2,lease_until=now()-interval '1 second',send_started=true WHERE subscription_id=$1`, id, l.NotifyAttempts); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ClaimNotificationDelivery(ctx, h.database, time.Minute, l.NotifyAttempts, 7, l.NotifySuspendWindow); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("exhausted lease was reclaimed", err)
	}
	if got := countRows(t, w.database, `SELECT count(*) FROM site_event_deliveries WHERE subscription_id=$1 AND state='failed' AND NOT send_started AND lease_until IS NULL`, id); got != 1 {
		t.Fatal("expired final lease was not released")
	}
}

func TestNotificationMCPGrantAndTeamAccess(t *testing.T) {
	w, x, site := notificationWorld(t)
	ctx := context.Background()
	h := w.notifications
	grant := connectApp(t, w.database, w.users["alice"], "notify-app", "Receiver")
	c := mcp.Caller{Owner: w.users["alice"], Username: "alice", OAuthGrantID: grant}
	req := mcp.EventSubscribeRequest{Name: "record.added", Site: "notify", CanonicalArgs: `{"site":"notify"}`, URL: x.server.URL, Secret: notifyTestSecret}
	sub, err := h.EventsBackend().Subscribe(ctx, c, req)
	if err != nil {
		t.Fatal(err)
	}
	reflected := req
	reflected.URL += "/reflect"
	x.reflectBody = true
	if _, err = h.EventsBackend().Subscribe(ctx, c, reflected); err == nil {
		t.Fatal("reflecting callback passed challenge")
	}
	x.reflectBody = false
	if sub.RefreshBefore == nil {
		t.Fatal("grant expiry not bounded")
	}
	if sub, err = h.EventsBackend().Subscribe(ctx, c, req); err != nil || !sub.Refreshed {
		t.Fatal("refresh failed", err)
	}
	if len(x.snapshot()) != 2 {
		t.Fatal("refresh challenged within proof period")
	}
	foreign := c
	foreign.OAuthGrantID = connectApp(t, w.database, c.Owner, "second-app", "Second")
	if err = h.EventsBackend().Unsubscribe(ctx, foreign, mcp.EventUnsubscribeRequest{Name: req.Name, Site: req.Site, CanonicalArgs: req.CanonicalArgs, URL: req.URL}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.GetNotification(ctx, h.database, sub.ID); err != nil {
		t.Fatal("another grant removed subscription")
	}
	foreign.Owner = w.users["olly"]
	foreign.Username = "olly"
	req.Owner = "alice"
	if _, err = h.EventsBackend().Subscribe(ctx, foreign, req); err == nil {
		t.Fatal("stranger subscribed")
	}
	if _, err = w.database.Exec(`DELETE FROM oauth_grants WHERE id=$1`, grant); err != nil {
		t.Fatal(err)
	}
	if _, err = db.GetNotification(ctx, h.database, sub.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("revocation left subscription")
	}
	// Team ownership is supported; leaving the team stops pending delivery.
	var team string
	if err = w.database.QueryRow(`INSERT INTO users(username,kind) VALUES('team-notify','team') RETURNING id`).Scan(&team); err != nil {
		t.Fatal(err)
	}
	if _, err = w.database.Exec(`INSERT INTO team_members(team_id,user_id) VALUES($1,$2)`, team, w.users["alice"]); err != nil {
		t.Fatal(err)
	}
	if _, err = w.database.Exec(`UPDATE sites SET user_id=$1 WHERE id=$2`, team, site); err != nil {
		t.Fatal(err)
	}
	c.OAuthGrantID = connectApp(t, w.database, c.Owner, "team-app", "Team")
	req.Owner = "team-notify"
	req.CanonicalArgs = `{"owner":"team-notify","site":"notify"}`
	sub, err = h.EventsBackend().Subscribe(ctx, c, req)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := db.NotificationAllowed(ctx, h.database, sub.ID); err != nil || !ok {
		t.Fatal("team notification refused", err)
	}
	if _, err = w.database.Exec(`DELETE FROM team_members WHERE team_id=$1 AND user_id=$2`, team, c.Owner); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.NotificationAllowed(ctx, h.database, sub.ID); err != nil || ok {
		t.Fatal("removed member still delivers", err)
	}

	if err = h.EventsBackend().Unsubscribe(ctx, c, mcp.EventUnsubscribeRequest{Name: req.Name, Site: req.Site, Owner: req.Owner, CanonicalArgs: req.CanonicalArgs, URL: req.URL}); err != nil {
		t.Fatal("own grant could not remove after membership loss", err)
	}
}

func TestNotificationChangesReachAuditStream(t *testing.T) {
	w, x, _ := notificationWorld(t)
	var buf bytes.Buffer
	stream := audit.NewStream(slog.New(slog.NewJSONHandler(&buf, nil)), 0)
	t.Cleanup(func() { stream.Close(time.Second) })
	recorder := audit.NewDBRecorder(w.notifications.database)
	recorder.SetStream(stream)
	w.notifications.WithAudit(recorder)
	id := addTestNotification(t, w, x)
	base := "/api/sites/notify/notifications/" + id
	for _, step := range []struct {
		method, path string
		body         any
	}{
		{"PATCH", base, map[string]bool{"paused": true}},
		{"PATCH", base, map[string]bool{"paused": false}},
		{"POST", base + "/rotate-secret", nil},
		{"DELETE", base, nil},
	} {
		r := w.api("alice", step.method, step.path, step.body)
		if r.Code != 200 {
			t.Fatalf("subscription change failed %d", r.Code)
		}
	}
	stream.Close(time.Second)
	text := buf.String()
	for _, action := range []string{"notification_create", "notification_pause", "notification_resume", "notification_rotate", "notification_delete"} {
		if !strings.Contains(text, action) {
			t.Fatalf("audit stream missing %s", action)
		}
	}
	if strings.Contains(text, notifyTestSecret) || strings.Contains(text, "test-header") || strings.Contains(text, x.server.URL) {
		t.Fatal("audit stream contained notification secrets")
	}
}

func TestNotificationBrowserOriginAndScope(t *testing.T) {
	w, x, _ := notificationWorld(t)
	body, _ := json.Marshal(map[string]any{"url": x.server.URL, "events": []string{"record.added"}})
	call := func(origin string) int {
		r := httptest.NewRequest("POST", "https://"+accessBase+"/api/sites/notify/notifications", bytes.NewReader(body))
		r.AddCookie(w.cookie("alice", ""))
		r.Header.Set("X-Simple-Host-Client", "control-ui")
		r.Header.Set("Content-Type", "application/json")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return w.do(r).Code
	}
	if code := call(""); code != 403 {
		t.Fatalf("missing Origin %d", code)
	}
	if code := call("https://foreign.example"); code != 403 {
		t.Fatalf("foreign Origin %d", code)
	}
	if code := call("https://" + accessBase); code != 201 {
		t.Fatalf("dashboard create %d", code)
	}
	r := w.api("olly", "GET", "/api/collaboration/sites/alice/notify/notifications", nil)
	if r.Code != 404 {
		t.Fatalf("stranger read %d", r.Code)
	}
	if _, err := w.database.Exec(`UPDATE api_keys SET scope='publish' WHERE user_id=$1`, w.users["alice"]); err != nil {
		t.Fatal(err)
	}
	r = w.api("alice", "GET", "/api/sites/notify/notifications", nil)
	if r.Code != 403 {
		t.Fatalf("publish key read %d", r.Code)
	}
}
