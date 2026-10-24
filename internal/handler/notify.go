package handler

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oplimits"
)

type notificationEngine struct {
	client notifyClient
	wake   chan struct{}
	start  sync.Once
}

func (h *SiteHandler) notifyEngine() *notificationEngine {
	h.notifyOnce.Do(func() {
		h.notifications = &notificationEngine{client: notifyClient{secrets: &h.notifyCipher, allowed: h.notifyAllowed}, wake: make(chan struct{}, 1)}
	})
	return h.notifications
}
func notifyRandom(prefix string, n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b)
}
func notifyEventID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "evt_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}
func (e *notificationEngine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func notifyTruncate(data map[string]any) {
	fields, ok := data["fields"].(map[string]any)
	if !ok {
		return
	}
	truncated := []string{}
	var cut func(any, string) any
	cut = func(value any, path string) any {
		switch v := value.(type) {
		case string:
			if len(v) > oplimits.Get().NotifyFieldMaxBytes {
				n := oplimits.Get().NotifyFieldMaxBytes
				for n > 0 && !utf8.ValidString(v[:n]) {
					n--
				}
				truncated = append(truncated, path)
				return v[:n]
			}
		case map[string]any:
			for k, x := range v {
				v[k] = cut(x, path+"."+k)
			}
		case []any:
			for i, x := range v {
				v[i] = cut(x, fmt.Sprintf("%s.%d", path, i))
			}
		}
		return value
	}
	for key, value := range fields {
		fields[key] = cut(value, key)
	}
	if len(truncated) > 0 {
		sort.Strings(truncated)
		data["truncated"] = truncated
	}

}
func notifyValue(raw json.RawMessage) any {
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if !json.Valid(raw) || dec.Decode(&value) != nil {
		return string(raw)
	}
	return value
}
func notifyFields(raw json.RawMessage) map[string]any {
	value := notifyValue(raw)
	if fields, ok := value.(map[string]any); ok {
		return fields
	}
	return map[string]any{"value": value}
}
func notifyBody(d db.NotificationDelivery, cursor int64) []byte {
	body := map[string]any{"eventId": d.EventID, "name": d.Name, "timestamp": d.CreatedAt.UTC().Format(time.RFC3339), "cursor": fmt.Sprintf("c_%d", cursor), "data": d.Data}
	raw, _ := json.Marshal(body)
	if len(raw) > oplimits.Get().NotifyBodyMaxBytes {
		data := notifyFields(d.Data)
		delete(data, "fields")
		data["truncated"] = []string{"fields"}
		body["data"] = data
		raw, _ = json.Marshal(body)
		if len(raw) > oplimits.Get().NotifyBodyMaxBytes {
			truncated := []string{"fields"}
			for _, key := range []string{"path", "key"} {
				if value, ok := data[key].(string); ok && len(value) > 256 {
					n := 256
					for n > 0 && !utf8.ValidString(value[:n]) {
						n--
					}
					data[key] = value[:n]
					truncated = append(truncated, key)
				}
			}
			data["truncated"] = truncated
			raw, _ = json.Marshal(body)
		}
	}
	return raw
}
func (h *SiteHandler) StartNotifications(ctx context.Context) {
	if !h.NotificationsReady() {
		return
	}
	e := h.notifyEngine()
	e.start.Do(func() {
		h.sweepNotifications(ctx)
		for i := 0; i < oplimits.Get().NotifyWorkers; i++ {
			go func() {
				tick := time.NewTicker(time.Second)
				defer tick.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-tick.C:
					case <-e.wake:
					}
					for ctx.Err() == nil && h.deliverNotification(ctx) {
					}
				}
			}()
		}
		go func() {
			tick := time.NewTicker(time.Hour)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					h.sweepNotifications(ctx)
				}
			}
		}()
	})
}
func (h *SiteHandler) deliverNotification(ctx context.Context) bool {
	l := oplimits.Get()
	d, err := db.ClaimNotificationDelivery(ctx, h.database, l.NotifyTimeout+30*time.Second, l.NotifyAttempts, l.NotifyHistoryDays, l.NotifySuspendWindow)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		if ctx.Err() == nil {
			log.Print("notifications: claim failed")
		}
		return false
	}
	return h.processNotification(ctx, d)
}
func (h *SiteHandler) processNotification(ctx context.Context, d db.NotificationDelivery) bool {
	l := oplimits.Get()
	ctx, cancel := context.WithDeadline(ctx, d.LeaseUntil)
	defer cancel()
	n, err := db.GetNotification(ctx, h.database, d.SubscriptionID)
	if err != nil {
		return true
	}
	// A pause or expiry may have happened after the claim.
	if n.State != "active" || n.ExpiresAt != nil && !n.ExpiresAt.After(time.Now()) {
		return true
	}
	start := time.Now()
	result := notifyResult{Result: "site_unavailable"}
	state := "skipped"
	allowed, err := db.NotificationAllowed(ctx, h.database, n.ID)
	if err != nil {
		return true
	}
	if allowed {
		cursor, e := db.NotificationCursor(ctx, h.database, n.ID, n.SiteID)
		if e != nil {
			return true
		}
		held, err := db.NotificationLeaseHeld(ctx, h.database, d)
		if err != nil || !held {
			return true
		}
		engine := h.notifyEngine()
		acquired, err := db.AcquireNotificationSend(ctx, h.database, n, d, l.NotifyOwnerConcurrency, l.NotifyPerMinute, l.NotifyPerDay, l.NotifyPerURLPerMinute)
		if err != nil || !acquired {
			if err := db.DeferNotificationDelivery(ctx, h.database, d); err != nil {
				log.Print("notifications: defer failed")
			}
			return true
		}
		result = engine.client.send(ctx, n, d.EventID, notifyBody(d, cursor))
		state = "failed"
		if result.Delivered {
			state = "delivered"
		} else if d.Attempts < l.NotifyAttempts && result.Status != 410 && result.Status != 413 && result.Result != "refused_address" {
			state = "pending"
		}
	}
	delays := []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute, 7 * time.Minute}
	i := d.Attempts - 1
	if i >= len(delays) {
		i = len(delays) - 1
	}
	next := time.Now().Add(delays[i])
	_, err = db.FinishNotificationDelivery(ctx, h.database, d, state, result.Result, result.Status, int(time.Since(start).Milliseconds()), next, l.NotifySuspendWindow, l.NotifySuspendAttempts, l.NotifySuspendFailurePercent, l.NotifySuspendAfter)
	if err != nil {
		log.Printf("notifications: result update failed for subscription %s", n.ID)
	}
	return true
}
func (h *SiteHandler) sweepNotifications(ctx context.Context) {
	if err := db.SweepNotifications(ctx, h.database, oplimits.Get().NotifyHistoryDays); err != nil {
		log.Print("notifications: history sweep failed")
	}
	_, _ = h.database.ExecContext(ctx, `DELETE FROM notification_budgets WHERE window_start<now()-interval '2 days'`)
}
