package mcp

import (
	"embed"
	"encoding/json"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/vsriram/simple-host/internal/auth"
)

const appMIME = "text/html;profile=mcp-app"

//go:embed apps/*.html apps/*.js apps/*.css
var appFiles embed.FS

type appScreen struct{ ID, Flag, URI, Tool, Label, HTML string }

var appScreens = []appScreen{
	{"my-sites", "MCP_APP_MY_SITES", "ui://simple-host/my-sites", "list_sites", "My sites", "my-sites.html"},
	{"dashboard", "MCP_APP_DASHBOARD", "ui://simple-host/dashboard", "", "Weekly overview", "my-sites.html"},
	{"photo-picker", "MCP_APP_PHOTO_PICKER", "ui://simple-host/photo-picker", "add_photos", "Photo picker", "photo-picker.html"},
	{"site-preview", "MCP_APP_SITE_PREVIEW", "ui://simple-host/site-preview", "get_site", "Site preview", "site-preview.html"},
}

func (s *Server) WithApps(origin string, flags map[string]string) *Server {
	s.appOrigin = strings.TrimRight(origin, "/")
	s.appFlags = flags
	return s
}
func appFlag(value, handle string) bool {
	if value == "on" {
		return true
	}
	if handle == "" || !strings.HasPrefix(value, "accounts:") {
		return false
	}
	for _, allowed := range strings.Split(strings.TrimPrefix(value, "accounts:"), ",") {
		if strings.TrimSpace(allowed) == handle {
			return true
		}
	}
	return false
}
func (s *Server) appEnabled(r *http.Request, flag string) bool {
	handle := ""
	if user := auth.GetUser(r.Context()); user != nil {
		handle = user.Username
	}
	return appFlag(s.appFlags[flag], handle)
}
func (s *Server) enabledScreens(r *http.Request) []appScreen {
	var screens []appScreen
	for _, screen := range appScreens {
		if s.appEnabled(r, screen.Flag) {
			screens = append(screens, screen)
		}
	}
	return screens
}
func (s *Server) appCapabilities(r *http.Request) map[string]any {
	caps := map[string]any{"tools": map[string]any{"listChanged": false}}
	if len(s.enabledScreens(r)) > 0 {
		caps["resources"] = map[string]any{"listChanged": false, "subscribe": false}
		caps["extensions"] = map[string]any{"io.modelcontextprotocol/ui": map[string]any{"mimeTypes": []string{appMIME}}}
	}
	return caps
}
func (s *Server) appTools(r *http.Request) []Tool {
	out := make([]Tool, 0, len(s.tools))
	screens := s.enabledScreens(r)
	for _, tool := range s.tools {
		// Preserve the pre-Apps inventory when every screen is off. The fallback
		// tool remains callable by name, including on hosts without UI support.
		if tool.Name == "add_photos" && len(screens) == 0 {
			continue
		}
		if tool.Name == "site_activity" && !s.appEnabled(r, "MCP_APP_DASHBOARD") {
			original := tool.InputSchema["properties"].(map[string]any)
			props := map[string]any{}
			for key, value := range original {
				if key != "days" {
					props[key] = value
				}
			}
			tool.InputSchema = object(props, "site", "owner")
		}
		for _, screen := range screens {
			if screen.Tool == tool.Name {
				tool.Meta = map[string]any{"ui": map[string]any{"resourceUri": screen.URI}}
			}
		}
		out = append(out, tool)
	}
	return out
}
func (s *Server) appMeta(screen appScreen) map[string]any {
	resources := []string{}
	if screen.ID == "photo-picker" {
		resources = append(resources, s.appOrigin)
	}
	return map[string]any{"ui": map[string]any{"prefersBorder": true, "csp": map[string][]string{
		"connectDomains": {}, "resourceDomains": resources, "frameDomains": {}, "baseUriDomains": {},
	}}}
}
func (s *Server) appResources(r *http.Request) []any {
	resources := []any{}
	for _, screen := range s.enabledScreens(r) {
		resources = append(resources, map[string]any{"uri": screen.URI, "name": screen.Label, "mimeType": appMIME, "_meta": s.appMeta(screen)})
	}
	return resources
}
func (s *Server) readApp(req request, r *http.Request) response {
	var params struct {
		URI string `json:"uri"`
	}
	if json.Unmarshal(req.Params, &params) != nil {
		return failure(req.ID, codeInvalidParams, "invalid resource params", nil)
	}
	for _, screen := range s.enabledScreens(r) {
		if params.URI != screen.URI {
			continue
		}
		asset := func(name string) string { b, _ := appFiles.ReadFile("apps/" + name); return string(b) }
		cfg, _ := json.Marshal(map[string]any{"assetOrigin": s.appOrigin, "maxBodyBytes": maxRequestBytes, "maxInlineBytes": maxInlineBytes, "maxBytes": 1000000, "maxDimension": 1600})
		page := strings.NewReplacer("__STYLE__", asset("shared.css"), "__BRIDGE__", asset("bridge.js"),
			"__SCREEN__", asset(strings.TrimSuffix(screen.HTML, ".html")+".js"), "__CONFIG__", string(cfg),
			"__DASHBOARD__", strconv.FormatBool(s.appEnabled(r, "MCP_APP_DASHBOARD")),
			"__ASSET_ORIGIN__", html.EscapeString(s.appOrigin)).Replace(asset(screen.HTML))
		return result(req.ID, s.withMeta(map[string]any{"resultType": "complete", "ttlMs": cacheTTLMillis, "cacheScope": "private", "contents": []any{map[string]any{"uri": screen.URI, "mimeType": appMIME, "text": page, "_meta": s.appMeta(screen)}}}))
	}
	return failure(req.ID, codeInvalidParams, "resource not available", nil)
}
func (s *Server) appResult(r *http.Request, name string, resp response) response {
	for _, screen := range s.enabledScreens(r) {
		if screen.Tool == name {
			payload := resp.Result.(map[string]any)
			payload["_meta"] = map[string]any{"ui": map[string]any{"resourceUri": screen.URI}}
		}
	}
	return resp
}
