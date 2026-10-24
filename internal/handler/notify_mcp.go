package handler

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/db"
	mcp "github.com/vsriram/simple-host/internal/notifyevents"
	"github.com/vsriram/simple-host/internal/oplimits"
)

type notificationBackend struct{ h *SiteHandler }

func (h *SiteHandler) EventsBackend() mcp.EventsBackend {
	return notificationBackend{h}
}

// EventsAvailability hides discovery while retaining a clear subscribe error.
func (b notificationBackend) EventsAvailability() error {
	if b.h.NotificationsReady() {
		return nil
	}
	return &mcp.EventError{Code: mcp.EventCodeForbidden, Message: "The server has no NOTIFY_ENC_KEY; new notifications are unavailable.", Data: map[string]any{"reason": "notifications_not_enabled"}}
}
func notifyMCPError(code int, kind, value string) error {
	return &mcp.EventError{Code: code, Message: strings.ReplaceAll(value, "_", " "), Data: map[string]any{kind: value}}
}
func notifyMCPID(owner, grant, url, name, args string) string {
	// Length-delimited JSON avoids ambiguous keys when URLs contain separators.
	raw, _ := json.Marshal([]string{owner, grant, url, name, args})
	sum := sha256.Sum256(raw)
	return "sub_" + hex.EncodeToString(sum[:12])
}
func (h *SiteHandler) notifyChallengeClient(ctx context.Context, owner, raw string) (notifyClient, error) {
	e := h.notifyEngine()
	c := e.client
	u, err := url.Parse(raw)
	if err != nil {
		return c, notifyFailure("invalid_url")
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	if !h.notifyBudget(ctx, "challenge-owner:"+owner, 10, time.Minute) || !h.notifyBudget(ctx, "challenge-host:"+host, 10, time.Minute) {
		return c, notifyFailure("rate_limited")
	}
	addrs, err := c.addresses(ctx, host)
	if err != nil {
		return c, err
	}
	for _, addr := range addrs {
		if !h.notifyBudget(ctx, "challenge-ip:"+addr.IP.String(), 10, time.Minute) {
			return c, notifyFailure("rate_limited")
		}
	}
	// Dial exactly the addresses whose shared destination budgets we checked.
	c.resolver = func(context.Context, string) ([]net.IPAddr, error) { return addrs, nil }
	return c, nil
}
func (b notificationBackend) site(ctx context.Context, c mcp.Caller, owner, name, storage string) (db.Site, error) {
	if c.DeployOnly || c.Owner == "" || c.OAuthGrantID == "" {
		return db.Site{}, notifyMCPError(mcp.EventCodeForbidden, "kind", "site")
	}
	if owner == "" {
		owner = c.Username
	}
	access, err := db.ResolveSiteAccess(ctx, b.h.database, c.Owner, owner, name)
	if err != nil {
		return db.Site{}, notifyMCPError(mcp.EventCodeForbidden, "kind", "site")
	}
	if storage != "" {
		if _, err = db.GetStorageResource(ctx, b.h.database, access.Site.ID, storage); err != nil {
			return db.Site{}, notifyMCPError(mcp.EventCodeNotFound, "kind", "storage")
		}
	}
	return access.Site, nil
}
func (h *SiteHandler) notifyBudget(ctx context.Context, key string, cap int, window time.Duration) bool {
	ok, err := db.NotificationBudget(ctx, h.database, key, cap, window)
	return err == nil && ok
}
func (b notificationBackend) Subscribe(ctx context.Context, c mcp.Caller, req mcp.EventSubscribeRequest) (mcp.EventSubscribeResult, error) {
	out := mcp.EventSubscribeResult{}
	if err := b.EventsAvailability(); err != nil {
		return out, err
	}
	site, err := b.site(ctx, c, req.Owner, req.Site, req.Storage)
	if err != nil {
		return out, err
	}
	if !validNotifyEvents([]string{req.Name}) {
		return out, notifyMCPError(mcp.EventCodeNotFound, "kind", "event")
	}
	if _, err = notifySecretKey(req.Secret); err != nil {
		return out, notifyMCPError(-32602, "reason", "invalid_secret")
	}
	var cursor *int64
	if req.Cursor != nil {
		raw, ok := strings.CutPrefix(*req.Cursor, "c_")
		seq, e := strconv.ParseInt(raw, 10, 64)
		if !ok || e != nil || seq < 0 {
			return out, notifyMCPError(-32602, "reason", "invalid_cursor")
		}
		cursor = &seq
	}
	ctx, cancel := context.WithTimeout(ctx, oplimits.Get().NotifyTimeout)
	defer cancel()
	if _, err = b.h.notifyEngine().client.validate(ctx, req.URL); err != nil {
		code := mcp.EventCodeCallbackEndpoint
		if errors.Is(err, notifyFailure("invalid_url")) {
			code = -32602
		}
		return out, notifyMCPError(code, "reason", notifyCategory(err))
	}
	var grantExpiry time.Time
	if err = b.h.database.QueryRowContext(ctx, `SELECT c.client_name,max(t.expires_at) FROM oauth_grants g JOIN oauth_clients c ON c.client_id=g.client_id JOIN oauth_tokens t ON t.grant_id=g.id WHERE g.id=$1 AND g.user_id=$2 AND t.expires_at>now() GROUP BY c.client_name`, c.OAuthGrantID, c.Owner).Scan(&c.AppName, &grantExpiry); err != nil {
		return out, notifyMCPError(mcp.EventCodeForbidden, "kind", "grant")
	}
	now := time.Now()
	n := db.Notification{ID: notifyMCPID(c.Owner, c.OAuthGrantID, req.URL, req.Name, req.CanonicalArgs), SiteID: site.ID, OwnerID: c.Owner, Source: "mcp", URL: req.URL, Events: []string{req.Name}, Storage: req.Storage, ArgsCanonical: req.CanonicalArgs, Secret: req.Secret, AppName: c.AppName, VerifiedAt: &now}
	if c.OAuthGrantID != "" {
		n.GrantID = &c.OAuthGrantID
	}
	ttl := oplimits.Get().NotifyDefaultTTL
	if req.TTLSet {
		if req.TTL == nil {
			ttl = 0
		} else {
			ttl = *req.TTL
			if ttl < 5*time.Minute {
				ttl = 5 * time.Minute
			}
		}
	}
	if ttl == 0 || now.Add(ttl).After(grantExpiry) {
		ttl = grantExpiry.Sub(now)
	}
	expiry := now.Add(ttl)
	n.ExpiresAt = &expiry
	old, e := db.GetNotification(ctx, b.h.database, n.ID)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	siteChanged := e == nil && old.SiteID != n.SiteID
	if siteChanged {
		if err = b.Unsubscribe(ctx, c, mcp.EventUnsubscribeRequest{Name: req.Name, CanonicalArgs: req.CanonicalArgs, URL: req.URL}); err != nil {
			return out, err
		}
		old = db.Notification{}
	}
	verified := e == nil && !siteChanged && old.VerifiedAt != nil && old.VerifiedAt.After(time.Now().Add(-30*24*time.Hour))
	if !verified {
		client, err := b.h.notifyChallengeClient(ctx, c.Owner, req.URL)
		if errors.Is(err, notifyFailure("rate_limited")) {
			return out, &mcp.EventError{Code: mcp.EventCodeResourceExhausted, Message: "verification rate limit reached", Data: map[string]any{"limit": "challenges", "max": 10}}
		}
		if err != nil {
			return out, notifyMCPError(mcp.EventCodeCallbackEndpoint, "reason", notifyCategory(err))
		}
		random := make([]byte, 24)
		if _, err = rand.Read(random); err != nil {
			return out, err
		}
		challenge := base64.RawURLEncoding.EncodeToString(random)
		body, _ := json.Marshal(map[string]string{"type": "verification", "challenge": challenge})
		result := client.send(ctx, n, notifyRandom("msg_verification_", 16), body)
		if !result.Delivered {
			return out, notifyMCPError(mcp.EventCodeCallbackEndpoint, "reason", result.Result)
		}
		var echo struct {
			Challenge string          `json:"challenge"`
			Type      json.RawMessage `json:"type"`
		}
		if json.Unmarshal(result.body, &echo) != nil || echo.Type != nil || subtle.ConstantTimeCompare([]byte(echo.Challenge), []byte(challenge)) != 1 {
			return out, notifyMCPError(mcp.EventCodeCallbackEndpoint, "reason", "challenge_failed")
		}
	} else {
		// A refresh does not extend the proof's 30-day lifetime without a challenge.
		var verifiedAt time.Time
		if err = b.h.database.QueryRowContext(ctx, `SELECT verified_at FROM site_notifications WHERE id=$1`, n.ID).Scan(&verifiedAt); err != nil {
			return out, err
		}
		n.VerifiedAt = &verifiedAt
	}
	refreshed, truncated, err := db.SaveNotification(ctx, b.h.database, n, oplimits.Get().NotifyMaxPerSite, cursor, oplimits.Get().NotifyHistoryDays, b.h.notificationSecrets(), b.h.notificationAudit(ctx, c.Owner, site, n.ID, "notification_subscribe", auditRequestID(ctx)))
	if errors.Is(err, db.ErrNotificationCap) {
		return out, &mcp.EventError{Code: mcp.EventCodeResourceExhausted, Message: "too many subscriptions", Data: map[string]any{"limit": "subscriptions", "max": oplimits.Get().NotifyMaxPerSite}}
	}
	if err != nil {
		return out, err
	}
	seq, err := db.NotificationCursor(ctx, b.h.database, n.ID, n.SiteID)
	if err != nil {
		return out, err
	}
	watermark := fmt.Sprintf("c_%d", seq)
	b.h.notifyEngine().signal()
	if old.LastResult == "ok" {
		old.LastResult = ""
	}
	return mcp.EventSubscribeResult{ID: n.ID, RefreshBefore: n.ExpiresAt, Cursor: &watermark, Truncated: truncated, Refreshed: refreshed, Active: true, LastError: old.LastResult}, nil
}

// Unsubscribe is idempotent and can name only the caller's own grant.
func (b notificationBackend) Unsubscribe(ctx context.Context, c mcp.Caller, req mcp.EventUnsubscribeRequest) error {
	if c.Owner == "" || c.DeployOnly || c.OAuthGrantID == "" {
		return notifyMCPError(mcp.EventCodeForbidden, "reason", "grant_required")
	}
	id := notifyMCPID(c.Owner, c.OAuthGrantID, req.URL, req.Name, req.CanonicalArgs)
	n, err := db.GetNotification(ctx, b.h.database, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}

	// The grant may remove its own subscription after a rename or membership
	// loss. It cannot read or replace data from the site's new namespace.
	tx, err := b.h.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer audit.Rollback(tx)
	if _, err = db.DeleteNotification(ctx, tx, id); err != nil {
		return err
	}
	kind, keyID := auditActorKind(ctx)
	if err = b.h.audit.RecordTx(ctx, tx, audit.Event{ActorID: c.Owner, ActorKind: kind, KeyID: keyID, OwnerID: n.SiteOwnerID, SiteID: n.SiteID, Action: "notification_delete", RequestID: auditRequestID(ctx), Extra: map[string]any{"subscription_id": id}}); err != nil {
		return err
	}
	return audit.Commit(tx)
}
