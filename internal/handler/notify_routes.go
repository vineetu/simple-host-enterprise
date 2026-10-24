package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oplimits"
)

func (h *SiteHandler) registerNotifications(mux *http.ServeMux, ownerMutation, browserWrite func(http.Handler) http.Handler) {
	mux.Handle("GET /api/notifications", ownerMutation(http.HandlerFunc(h.notificationRoute)))
	mux.Handle("GET /api/sites/{sitename}/notifications", ownerMutation(http.HandlerFunc(h.notificationRoute)))
	mux.Handle("POST /api/sites/{sitename}/notifications", browserWrite(ownerMutation(http.HandlerFunc(h.notificationRoute))))
	mux.Handle("PATCH /api/sites/{sitename}/notifications/{id}", browserWrite(ownerMutation(http.HandlerFunc(h.notificationRoute))))
	mux.Handle("DELETE /api/sites/{sitename}/notifications/{id}", browserWrite(ownerMutation(http.HandlerFunc(h.notificationRoute))))
	mux.Handle("POST /api/sites/{sitename}/notifications/{id}/test", browserWrite(ownerMutation(http.HandlerFunc(h.notificationRoute))))
	mux.Handle("POST /api/sites/{sitename}/notifications/{id}/rotate-secret", browserWrite(ownerMutation(http.HandlerFunc(h.notificationRoute))))
	mux.Handle("GET /api/sites/{sitename}/notifications/{id}/deliveries", ownerMutation(http.HandlerFunc(h.notificationRoute)))
	mux.Handle("GET /api/collaboration/sites/{owner}/{sitename}/notifications", ownerMutation(http.HandlerFunc(h.notificationRoute)))
	mux.Handle("POST /api/collaboration/sites/{owner}/{sitename}/notifications", browserWrite(ownerMutation(http.HandlerFunc(h.notificationRoute))))
	mux.Handle("PATCH /api/collaboration/sites/{owner}/{sitename}/notifications/{id}", browserWrite(ownerMutation(http.HandlerFunc(h.notificationRoute))))
	mux.Handle("DELETE /api/collaboration/sites/{owner}/{sitename}/notifications/{id}", browserWrite(ownerMutation(http.HandlerFunc(h.notificationRoute))))
	mux.Handle("POST /api/collaboration/sites/{owner}/{sitename}/notifications/{id}/test", browserWrite(ownerMutation(http.HandlerFunc(h.notificationRoute))))
	mux.Handle("POST /api/collaboration/sites/{owner}/{sitename}/notifications/{id}/rotate-secret", browserWrite(ownerMutation(http.HandlerFunc(h.notificationRoute))))
	mux.Handle("GET /api/collaboration/sites/{owner}/{sitename}/notifications/{id}/deliveries", ownerMutation(http.HandlerFunc(h.notificationRoute)))
}

func (h *SiteHandler) notifyLimits() map[string]any {
	l := oplimits.Get()
	return map[string]any{"enabled": h.NotificationsReady(), "per_site": l.NotifyMaxPerSite, "per_minute": l.NotifyPerMinute, "per_day": l.NotifyPerDay, "history_days": l.NotifyHistoryDays}
}
func notificationView(n db.Notification) map[string]any {
	state := n.State
	if n.ExpiresAt != nil && !n.ExpiresAt.After(time.Now()) {
		state = "expired"
	} else if n.Source == "mcp" && n.VerifiedAt == nil {
		state = "pending"
	}
	return map[string]any{"id": n.ID, "site": n.Site, "url": n.URL, "events": n.Events, "storage": n.Storage, "label": n.Label, "source": n.Source, "app_name": n.AppName, "state": state, "created_at": n.CreatedAt, "expires_at": n.ExpiresAt, "last_attempt_at": n.LastAttemptAt, "last_success_at": n.LastSuccessAt, "last_result": n.LastResult, "failing_since": n.FailingSince, "auth_header_name": n.AuthHeaderName}
}
func notifyError(w http.ResponseWriter, status int, code string) {
	writeStorageError(w, status, code, strings.ReplaceAll(code, "_", " "))
}

var notifyHeaderRE = regexp.MustCompile(`^[Xx]-[A-Za-z0-9-]{1,62}$`)

func validNotifyHeader(name, value string) bool {
	if name == "" {
		return value == ""
	}
	if (!strings.EqualFold(name, "Authorization") && !notifyHeaderRE.MatchString(name)) || len(value) > 1024 {
		return false
	}
	lower := strings.ToLower(name)
	for _, prefix := range []string{"x-forwarded-", "x-mcp-"} {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	switch lower {
	case "x-real-ip", "x-client-ip", "x-originating-ip":
		return false
	}
	for _, c := range value {
		if c < 32 || c > 126 {
			return false
		}
	}
	return true
}
func validNotifyEvents(events []string) bool {
	if len(events) < 1 || len(events) > 4 {
		return false
	}
	seen := map[string]bool{}
	for _, e := range events {
		if e != "record.added" && e != "record.changed" && e != "file.uploaded" && e != "file.deleted" || seen[e] {
			return false
		}
		seen[e] = true
	}
	return true
}
func (h *SiteHandler) notificationRoute(w http.ResponseWriter, r *http.Request) {
	if !h.requireNotifications(w) {
		return
	}
	user := auth.GetUser(r.Context())
	if user == nil || user.DisabledAt != nil {
		notifyError(w, 403, "forbidden")
		return
	}
	ctx := r.Context()
	var site db.Site
	var err error
	if name := r.PathValue("sitename"); name != "" {
		owner := r.PathValue("owner")
		if owner == "" {
			owner = user.Username
		}
		access, e := db.ResolveSiteAccess(ctx, h.database, user.ID, owner, name)
		if e != nil {
			notifyError(w, 404, "not_found")
			return
		}
		site = access.Site
	}
	id := r.PathValue("id")
	if id == "" && r.Method == http.MethodGet {
		ns, err := db.ListNotifications(ctx, h.database, user.ID, site.ID)
		if err != nil {
			notifyError(w, 500, "internal_error")
			return
		}
		out := []map[string]any{}
		for _, n := range ns {
			out = append(out, notificationView(n))
			if auth.OAuthGrantID(ctx) != "" {
				redactNotificationURL(out[len(out)-1])
			}
		}
		writeJSON(w, 200, map[string]any{"notifications": out, "limits": h.notifyLimits()})
		return
	}
	if id == "" {
		h.createNotification(w, r, site)
		return
	}
	n, err := db.GetNotification(ctx, h.database, id)
	if err != nil || n.SiteID != site.ID {
		notifyError(w, 404, "not_found")
		return
	}
	switch {
	case r.Method == http.MethodDelete:
		if _, err = h.deleteNotification(ctx, user.ID, site, id, auditRequestID(ctx)); err != nil {
			notifyError(w, 500, "internal_error")
			return
		}
		writeJSON(w, 200, map[string]bool{"deleted": true})
	case r.Method == http.MethodPatch:
		var req struct {
			Paused *bool `json:"paused"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req) != nil || req.Paused == nil {
			notifyError(w, 400, "invalid_request")
			return
		}
		action := "notification_resume"
		if *req.Paused {
			action = "notification_pause"
		}
		if err = db.PauseNotification(ctx, h.database, id, *req.Paused, h.notificationAudit(ctx, user.ID, site, id, action, auditRequestID(ctx))); err != nil {
			notifyError(w, 500, "internal_error")
			return
		}
		n, err = db.GetNotification(ctx, h.database, id)
		if err != nil {
			notifyError(w, 500, "internal_error")
			return
		}
		h.notifyEngine().signal()
		writeJSON(w, 200, map[string]any{"notification": notificationView(n)})
	case strings.HasSuffix(r.URL.Path, "/rotate-secret"):
		if !h.requireNotifications(w) {
			return
		}
		if n.Source == "mcp" {
			notifyError(w, 409, "client_secret")
			return
		}
		secret, err := newNotifySecret()
		if err != nil {
			notifyError(w, 500, "internal_error")
			return
		}
		until, err := db.RotateNotification(ctx, h.database, id, secret, h.notificationSecrets(), h.notificationAudit(ctx, user.ID, site, id, "notification_rotate", auditRequestID(ctx)))
		if err != nil {
			notifyError(w, 500, "internal_error")
			return
		}
		writeJSON(w, 200, map[string]any{"secret": secret, "previous_valid_until": until})
	case strings.HasSuffix(r.URL.Path, "/deliveries"):
		deliveries, err := db.NotificationHistory(ctx, h.database, id, oplimits.Get().NotifyHistoryDays)
		if err != nil {
			notifyError(w, 500, "internal_error")
			return
		}
		writeJSON(w, 200, map[string]any{"deliveries": deliveries})
	case strings.HasSuffix(r.URL.Path, "/test"):
		if n.Source == "mcp" || n.ExpiresAt != nil && !n.ExpiresAt.After(time.Now()) {
			notifyError(w, 409, "client_subscription")
			return
		}
		ctx, cancel := context.WithTimeout(ctx, oplimits.Get().NotifyTimeout)
		defer cancel()
		client, err := h.notifyChallengeClient(ctx, user.ID, n.URL)
		if errors.Is(err, notifyFailure("rate_limited")) {
			notifyError(w, 429, "rate_limited")
			return
		}
		if err != nil {
			writeJSON(w, 200, notifyResult{Result: notifyCategory(err)})
			return
		}
		allowed, err := db.NotificationAllowed(ctx, h.database, n.ID)
		if err != nil || !allowed {
			writeJSON(w, 200, notifyResult{Result: "site_unavailable"})
			return
		}
		raw, _ := json.Marshal(map[string]any{"v": 1, "site": site.Name, "test": true})
		event := db.NotificationDelivery{EventID: notifyRandom("evt_test_", 16), Name: "notification.test", CreatedAt: time.Now(), Data: raw}
		cursor, err := db.NotificationCursor(ctx, h.database, n.ID, n.SiteID)
		if err != nil {
			notifyError(w, 500, "internal_error")
			return
		}
		writeJSON(w, 200, client.send(ctx, n, event.EventID, notifyBody(event, cursor)))
	}
}
func (h *SiteHandler) requireNotifications(w http.ResponseWriter) bool {
	if h.NotificationsReady() {
		return true
	}
	writeStorageError(w, http.StatusConflict, "notifications_not_enabled", "The server has no NOTIFY_ENC_KEY; new notifications are unavailable.")
	return false
}
func (h *SiteHandler) createNotification(w http.ResponseWriter, r *http.Request, site db.Site) {
	if !h.requireNotifications(w) {
		return
	}
	var req struct {
		URL        string   `json:"url"`
		Events     []string `json:"events"`
		Storage    string   `json:"storage"`
		Secret     string   `json:"secret"`
		Label      string   `json:"label"`
		AuthHeader struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"auth_header"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&req) != nil {
		notifyError(w, 400, "invalid_request")
		return
	}
	if !validNotifyEvents(req.Events) {
		notifyError(w, 400, "invalid_events")
		return
	}
	if !validNotifyHeader(req.AuthHeader.Name, req.AuthHeader.Value) {
		writeStorageError(w, 400, "invalid_header", "Use Authorization or X- followed by 1 to 62 letters, digits, or hyphens; X-Forwarded-*, X-Real-IP, X-MCP-*, X-Client-IP, and X-Originating-IP are reserved. Values must be printable ASCII, at most 1024 bytes.")
		return
	}
	if req.Storage != "" {
		if _, err := db.GetStorageResource(r.Context(), h.database, site.ID, req.Storage); err != nil {
			notifyError(w, 404, "storage_not_found")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), oplimits.Get().NotifyTimeout)
	defer cancel()
	if _, err := h.notifyEngine().client.validate(ctx, req.URL); err != nil {
		code := "invalid_url"
		if errors.Is(err, notifyFailure("refused_address")) {
			code = "refused_address"
		}
		notifyError(w, 400, code)
		return
	}
	if req.Secret == "" {
		var err error
		req.Secret, err = newNotifySecret()
		if err != nil {
			notifyError(w, 500, "internal_error")
			return
		}
	} else if _, err := notifySecretKey(req.Secret); err != nil {
		notifyError(w, 400, "invalid_secret")
		return
	}
	now := time.Now()
	n := db.Notification{ID: notifyRandom("sub_", 12), SiteID: site.ID, OwnerID: auth.GetUser(r.Context()).ID, Source: "owner", URL: req.URL, Events: req.Events, Storage: req.Storage, Secret: req.Secret, Label: req.Label, AuthHeaderName: req.AuthHeader.Name, AuthHeaderValue: req.AuthHeader.Value, VerifiedAt: &now}
	_, _, err := db.SaveNotification(ctx, h.database, n, oplimits.Get().NotifyMaxPerSite, nil, oplimits.Get().NotifyHistoryDays, h.notificationSecrets(), h.notificationAudit(ctx, n.OwnerID, site, n.ID, "notification_create", auditRequestID(ctx)))
	if errors.Is(err, db.ErrNotificationCap) {
		notifyError(w, 409, "too_many_notifications")
		return
	}
	if err != nil {
		notifyError(w, 500, "internal_error")
		return
	}
	n, err = db.GetNotification(ctx, h.database, n.ID)
	if err != nil {
		notifyError(w, 500, "internal_error")
		return
	}
	writeJSON(w, 201, map[string]any{"notification": notificationView(n), "secret": req.Secret})
}

// Subscription changes and their audit entry commit together. Never record
// callback URLs, signing keys, or header values in the audit log.
func (h *SiteHandler) notificationAudit(ctx context.Context, actor string, site db.Site, id, action, requestID string) db.NotificationMutation {
	return db.NotificationMutation{Commit: audit.Commit, Rollback: audit.Rollback, Record: func(tx *sql.Tx) error {
		var owner string
		if err := tx.QueryRowContext(ctx, `SELECT s.user_id FROM sites s JOIN users u ON u.id=$2 WHERE s.id=$1 AND s.deleted_at IS NULL AND u.disabled_at IS NULL FOR SHARE OF s,u`, site.ID, actor).Scan(&owner); err != nil {
			return err
		}
		if owner != site.UserID {
			return db.ErrNotificationSite
		}
		if owner != actor {
			var member string
			if err := tx.QueryRowContext(ctx, `SELECT user_id FROM team_members WHERE team_id=$1 AND user_id=$2 FOR SHARE`, owner, actor).Scan(&member); err != nil {
				return err
			}
		}
		kind, keyID := auditActorKind(ctx)
		return h.audit.RecordTx(ctx, tx, audit.Event{ActorID: actor, ActorKind: kind, KeyID: keyID, OwnerID: owner, SiteID: site.ID, Action: action, RequestID: requestID, Extra: map[string]any{"subscription_id": id}})
	},
	}
}
func (h *SiteHandler) deleteNotification(ctx context.Context, actor string, site db.Site, id, requestID string) (bool, error) {
	tx, err := h.database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer audit.Rollback(tx)
	if err = db.LockNotificationSite(ctx, tx, id); err != nil {
		return false, err
	}
	removed, err := db.DeleteNotification(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if err = h.notificationAudit(ctx, actor, site, id, "notification_delete", requestID).Record(tx); err != nil {
		return false, err
	}
	return removed, audit.Commit(tx)
}
func redactNotificationURL(item map[string]any) {
	raw, _ := item["url"].(string)
	item["url"] = ""
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		item["url"] = u.Scheme + "://" + u.Host + "/..."
	}
}
