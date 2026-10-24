package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
)

func notificationTools() []Tool {
	props := func(extra map[string]any, required ...string) map[string]any {
		p := map[string]any{"site": str("Site name."), "owner": str("Namespace username or team name.")}
		for k, v := range extra {
			p[k] = v
		}
		return object(p, required...)
	}
	schema := func(keys ...string) map[string]any {
		p := map[string]any{}
		for _, k := range keys {
			p[k] = anyJSON("Notification " + k + ".")
		}
		return outObject(p, keys...)
	}
	specs := []struct {
		Name, title, description, method, suffix string
		extra                                    map[string]any
		required                                 []string
		output                                   map[string]any
		read, destructive                        bool
	}{
		{
			Name:        "list_notifications",
			title:       "List notifications",
			description: "List subscriptions and delivery status for your sites and teams. Callback paths and secrets are omitted.",
			method:      "GET",
			suffix:      "",
			extra:       nil,
			required:    nil,
			output:      schema("notifications", "limits"),
			read:        true,
			destructive: false,
		},
		{
			Name:        "create_notification",
			title:       "Create notification",
			description: "Create a signed HTTPS webhook for visitor saves. The signing secret is returned once. Notifications require operator configuration.",
			method:      "POST",
			suffix:      "",
			extra:       map[string]any{"url": str("HTTPS receiver URL."), "events": map[string]any{"type": "array", "items": outEnum("Event.", "record.added", "record.changed", "file.uploaded", "file.deleted")}, "storage": str("Optional resource filter."), "label": str("Label."), "auth_header": object(map[string]any{"name": str("Authorization or an X- header."), "value": str("Secret header value.")}, "name", "value")},
			required:    []string{"site", "url", "events"},
			output:      schema("notification", "secret"),
			read:        false,
			destructive: false,
		},
		{
			Name:        "pause_notification",
			title:       "Pause notification",
			description: "Pause or resume deliveries, retaining pending events within the history window.",
			method:      "PATCH",
			suffix:      "",
			extra:       map[string]any{"id": str("Subscription ID."), "paused": map[string]any{"type": "boolean"}},
			required:    []string{"site", "id", "paused"},
			output:      schema("notification"),
			read:        false,
			destructive: false,
		},
		{
			Name:        "remove_notification",
			title:       "Remove notification",
			description: "Remove a subscription and its deliveries.",
			method:      "DELETE",
			suffix:      "",
			extra:       map[string]any{"id": str("Subscription ID.")},
			required:    []string{"site", "id"},
			output:      schema("deleted"),
			read:        false,
			destructive: true,
		},
		{
			Name:        "test_notification",
			title:       "Test notification",
			description: "Send a signed test to an owner-created webhook.",
			method:      "POST",
			suffix:      "/test",
			extra:       map[string]any{"id": str("Subscription ID.")},
			required:    []string{"site", "id"},
			output:      schema("delivered", "status", "result"),
			read:        false,
			destructive: false,
		},
		{
			Name:        "rotate_notification_secret",
			title:       "Rotate notification secret",
			description: "Replace an owner webhook signing secret. The previous readable secret signs alongside it for 24 hours.",
			method:      "POST",
			suffix:      "/rotate-secret",
			extra:       map[string]any{"id": str("Subscription ID.")},
			required:    []string{"site", "id"},
			output:      schema("secret", "previous_valid_until"),
			read:        false,
			destructive: false,
		},
		{
			Name:        "notification_deliveries",
			title:       "Notification deliveries",
			description: "Read recent delivery results without payloads or secrets.",
			method:      "GET",
			suffix:      "/deliveries",
			extra:       map[string]any{"id": str("Subscription ID.")},
			required:    []string{"site", "id"},
			output:      schema("deliveries"),
			read:        true,
			destructive: false,
		},
	}
	var out []Tool
	for _, spec := range specs {
		spec := spec
		annotations := writes(spec.destructive, spec.method == "PATCH" || spec.method == "DELETE")
		if spec.read {
			annotations = readOnly()
		}
		if spec.Name == "create_notification" || spec.Name == "test_notification" {
			annotations["openWorldHint"] = true
		}
		out = append(out, Tool{Name: spec.Name, Title: spec.title, Description: spec.description, InputSchema: props(spec.extra, spec.required...), OutputSchema: spec.output, Annotations: annotations, family: familySite, call: func(args map[string]any) (upstream, error) {
			path := "/api/notifications"
			if _, present := args["site"]; present {
				site, err := stringArg(args, "site")
				if err != nil {
					return upstream{}, err
				}
				site, err = segment(site, "site")
				if err != nil {
					return upstream{}, err
				}
				path = "/api/sites/" + site + "/notifications"
				if _, present := args["owner"]; present {
					owner, err := stringArg(args, "owner")
					if err != nil {
						return upstream{}, err
					}
					owner, err = segment(owner, "owner")
					if err != nil {
						return upstream{}, err
					}
					path = "/api/collaboration/sites/" + owner + "/" + site + "/notifications"
				}
			} else if _, present := args["owner"]; present {
				return upstream{}, fmt.Errorf("owner requires site")
			}
			if _, present := args["id"]; present {
				id, err := stringArg(args, "id")
				if err != nil {
					return upstream{}, err
				}
				id, err = segment(id, "id")
				if err != nil {
					return upstream{}, err
				}
				path += "/" + id
			}
			up := upstream{Method: spec.method, Path: path + spec.suffix}
			if spec.Name == "create_notification" || spec.Name == "pause_notification" {
				body := map[string]any{}
				for key, value := range args {
					if key != "site" && key != "owner" && key != "id" {
						body[key] = value
					}
				}
				up.Body, _ = json.Marshal(body)
				up.ContentType = "application/json"
			}
			if spec.Name == "list_notifications" {
				up.Transform = redactNotificationResult
			}
			return up, nil
		}})
	}
	return out
}

func redactNotificationResult(body []byte) ([]byte, error) {
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	rows, _ := data["notifications"].([]any)
	for _, row := range rows {
		if item, ok := row.(map[string]any); ok {
			if raw, ok := item["url"].(string); ok {
				item["url"] = ""
				if u, err := url.Parse(raw); err == nil {
					item["url"] = u.Scheme + "://" + u.Host + "/..."
				}
			}
		}
	}
	return json.Marshal(data)
}

func isNotificationTool(name string) bool {
	switch name {
	case "list_notifications", "create_notification", "pause_notification", "remove_notification", "test_notification", "rotate_notification_secret", "notification_deliveries":
		return true
	}
	return false
}
