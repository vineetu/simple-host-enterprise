package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/oplimits"
)

func (h *SiteAPIHandler) WithNotifications(n *SiteHandler) *SiteAPIHandler {
	h.notifications = n
	return h
}
func (h *SiteAPIHandler) queueStorageNotification(ctx context.Context, tx *sql.Tx, call siteAPICall, action string, extra map[string]any) error {
	if h.notifications == nil || !h.notifications.NotificationsReady() || call.ActorUserID == "" || call.SessionID == "" {
		return nil
	}
	owner, err := db.SiteOwnerOrMember(ctx, tx, call.SiteID, call.ActorUserID)
	if err != nil || owner {
		return err
	}
	var access string
	if err = tx.QueryRowContext(ctx, `SELECT access FROM sites WHERE id=$1`, call.SiteID).Scan(&access); err != nil {
		return err
	}
	if access == db.AccessNetwork {
		return nil
	}
	yes, err := db.HasNotifications(ctx, tx, call.SiteID)
	if err != nil || !yes {
		return err
	}
	l := oplimits.Get()
	for _, b := range []struct {
		key    string
		cap    int
		window time.Duration
	}{
		{"emit-minute:" + call.SiteID, l.NotifyPerMinute, time.Minute}, {"emit-day:" + call.SiteID, l.NotifyPerDay, 24 * time.Hour},
	} {
		ok, e := db.NotificationBudget(ctx, tx, b.key, b.cap, b.window)
		if e != nil || !ok {
			return e
		}
	}
	name, _ := extra["resource"].(string)
	res, err := db.GetStorageResource(ctx, tx, call.SiteID, name)
	if err != nil {
		return err
	}
	data := map[string]any{"v": 1, "site": call.SiteName, "site_url": h.hosts.SiteURL(call.Owner, call.SiteName), "storage": name, "kind": res.Kind, "preset": res.Preset, "visitor": call.ActorUserID, "untrusted": []string{}}
	if res.Preset == "custom" {
		data["preset"] = res.Settings.Base
	}
	event := "record.changed"
	if action == "storage_add" {
		event = "record.added"
	} else if action == "storage_delete" {
		data["change"] = "deleted"
	} else {
		data["change"] = "edited"
	}
	if key, ok := extra["key"].(string); ok {
		data["key"] = key
		data["untrusted"] = []string{"key", "fields"}
		if action != "storage_delete" {
			value, e := db.GetStorageKV(ctx, tx, call.SiteID, name, key)
			if e != nil {
				return e
			}
			data["fields"] = notifyFields(value.Value)
		}
	}
	if table, ok := extra["table"].(string); ok {
		id := extra["id"].(int64)
		if settings, ok := res.Settings.Tables[table]; ok {
			data["preset"] = settings.Preset
			if settings.Preset == "custom" {
				data["preset"] = settings.Base
			}
		}
		data["table"] = table
		data["row_id"] = id
		data["untrusted"] = []string{"fields"}
		if action != "storage_delete" {
			row, e := db.GetStorageRow(ctx, tx, call.SiteID, name, table, id)
			if e != nil {
				return e
			}
			fields := notifyFields(row.Data)
			delete(fields, "visitor_id")
			data["fields"] = fields
		}
	}
	if path, ok := extra["path"].(string); ok {
		delete(data, "change")
		data["path"] = path
		data["untrusted"] = []string{"path"}
		event = "file.uploaded"
		if action == "storage_delete" {
			event = "file.deleted"
		} else {
			f, e := db.GetStorageFile(ctx, tx, call.SiteID, name, path)
			if e != nil {
				return e
			}
			data["size"] = f.Size
			data["content_type"] = f.ContentType
		}
	}
	untrusted := []string{}
	for _, key := range []string{"fields", "key", "path"} {
		if _, ok := data[key]; ok {
			untrusted = append(untrusted, key)
		}
	}
	data["untrusted"] = untrusted
	notifyTruncate(data)
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if len(raw) > l.NotifyBodyMaxBytes {
		delete(data, "fields")
		data["truncated"] = []string{"fields"}
		raw, err = json.Marshal(data)
		if err != nil {
			return err
		}
	}
	return db.EmitSiteEventTx(ctx, tx, call.SiteID, notifyEventID(), event, name, raw)
}
