package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

const homepageLinks = `</index.md>; rel="alternate"; type="text/markdown", </.well-known/api-catalog>; rel="api-catalog", </openapi.yaml>; rel="service-desc", </.well-known/ard.json>; rel="discovery", </.well-known/mcp/server-card.json>; rel="mcp-server-card"`

// Only explicit markdown and HTML preferences matter here. Equal preferences
// favor markdown; a browser's usual HTML/wildcard Accept still gets HTML.
func acceptsMarkdown(r *http.Request) bool {
	var markdown, htmlQuality float64
	for _, value := range strings.Split(strings.Join(r.Header.Values("Accept"), ","), ",") {
		media, params, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err != nil {
			continue
		}
		quality := 1.0
		if q, ok := params["q"]; ok {
			quality, err = strconv.ParseFloat(q, 64)
			if err != nil || quality < 0 || quality > 1 {
				continue
			}
		}
		switch media {
		case "text/markdown":
			markdown = max(markdown, quality)
		case "text/html":
			htmlQuality = max(htmlQuality, quality)
		}
	}
	return markdown > 0 && markdown >= htmlQuality
}

// homepageMarkdown reads only the landing's marked copy, excluding the film
// player, scripts, and dashboard. The HTML remains the single source of copy.
// This deliberately supports just the tags used in that section.
func homepageMarkdown(page []byte) ([]byte, error) {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, err
	}
	var content *html.Node
	var find func(*html.Node)
	find = func(n *html.Node) {
		for _, a := range n.Attr {
			if a.Key == "id" && a.Val == "main-content" {
				content = n
				return
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			find(child)
		}
	}
	find(doc)
	if content == nil {
		return nil, fmt.Errorf("homepage-content missing from landing page")
	}
	var render func(*html.Node) string
	render = func(n *html.Node) string {
		if n.Type == html.TextNode {
			return n.Data
		}
		if n.Type != html.ElementNode || n.Data == "script" || n.Data == "style" || n.Data == "button" || n.Data == "details" || n.Data == "form" {
			return ""
		}
		for _, attr := range n.Attr {
			if attr.Key == "hidden" {
				return ""
			}
		}
		var b strings.Builder
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			b.WriteString(render(child))
		}
		text := b.String()
		switch n.Data {
		case "h1", "h2", "h3":
			return "\n\n" + strings.Repeat("#", int(n.Data[1]-'0')) + " " + strings.TrimSpace(text) + "\n\n"
		case "p", "div":
			return "\n\n" + strings.TrimSpace(text) + "\n\n"
		case "br":
			return "\n"
		case "a":
			for _, a := range n.Attr {
				if a.Key == "href" {
					return "[" + strings.TrimSpace(text) + "](" + a.Val + ") "
				}
			}
		}
		return text
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(render(content)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" || len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, line)
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func homepageMarkdownHandler(publicBaseURL string) http.HandlerFunc {
	page, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		panic(err)
	}
	body, err := homepageMarkdown([]byte(expandServedText(string(page))))
	if err != nil {
		panic(err)
	}
	frontmatter := "---\ntitle: Simple Host Enterprise\ndescription: Publish and share sites within your organisation.\ncanonical: " + publicBaseURL + "/\n---\n\n"
	body = append([]byte(frontmatter), body...)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	}
}

// RegisterDiscovery publishes only installation metadata, on the app host.
func RegisterDiscovery(mux *http.ServeMux, base, version string, maxKeyDays int64, protocolVersion string, inventory []DiscoveryTool) {
	base = strings.TrimRight(base, "/")
	origin, err := url.Parse(base)
	if err != nil {
		panic(err)
	}
	document := func(value any, contentType string) http.HandlerFunc {
		body, err := json.Marshal(value)
		if err != nil {
			panic(err)
		}
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", contentType)
			w.Header().Set("Cache-Control", "public, max-age=60")
			if r.Method != http.MethodHead {
				_, _ = w.Write(body)
			}
		}
	}
	mux.HandleFunc("GET /.well-known/mcp/server-card.json", document(map[string]any{
		"$schema": "https://static.modelcontextprotocol.io/schemas/v1/server-card.schema.json",
		"name":    origin.Hostname() + "/mcp", "title": "Simple Host Enterprise", "version": version,
		"description": "Publish and manage organisation sites with company sign-in.",
		"icons":       []any{map[string]any{"src": base + "/favicon.svg"}},
		"remotes":     []any{map[string]any{"type": "streamable-http", "url": base + "/mcp"}},
	}, "application/json"))
	summaries := make([]DiscoveryTool, len(inventory))
	for i, tool := range inventory {
		summaries[i] = DiscoveryTool{Name: tool.Name, Description: agentCardSummary(tool.Description)}
	}
	mux.HandleFunc("GET /.well-known/agent-card.json", document(map[string]any{
		"name": "Simple Host Enterprise", "version": version, "protocol": "mcp", "protocolVersion": protocolVersion,
		"description": "Manage sites and storage for a signed-in person or their teams.",
		"endpoint":    base + "/mcp", "tools": summaries,
		"auth": map[string]string{"type": "oauth2", "authorization_server": base + "/.well-known/oauth-authorization-server", "protected_resource": base + "/.well-known/oauth-protected-resource/mcp"},
	}, "application/json"))
	entries := []map[string]string{}
	for _, entry := range [][3]string{
		{"/.well-known/mcp/server-card.json", "Simple Host Enterprise MCP server", "application/mcp-server-card+json"},
		{"/.well-known/agent-skills/index.json", "Simple Host Enterprise skills", "application/json"},
		{"/openapi.yaml", "Simple Host Enterprise REST API", "application/yaml"},
		{"/auth.md", "Authentication", "text/markdown"},
	} {
		entries = append(entries, map[string]string{"identifier": base + entry[0], "url": base + entry[0], "displayName": entry[1], "type": entry[2]})
	}
	mux.HandleFunc("GET /.well-known/ard.json", document(map[string]any{"specVersion": "1.0", "host": map[string]string{"displayName": "Simple Host Enterprise", "identifier": base}, "entries": entries}, "application/json"))
	spec := []any{map[string]string{"href": base + "/openapi.yaml", "type": "application/yaml", "title": "Simple Host Enterprise REST API"}}
	mux.HandleFunc("GET /.well-known/api-catalog", document(map[string]any{"linkset": []any{map[string]any{
		"anchor": base + "/.well-known/api-catalog", "item": spec, "service-desc": spec,
		"service-doc": []any{map[string]string{"href": base + "/llms.txt", "type": "text/plain", "title": "Quickstart"}},
	}}}, "application/linkset+json"))
	mux.HandleFunc("GET /auth.md", serveDiscoveryText("auth.md", "text/markdown; charset=utf-8", base, maxKeyDays))
}

func serveDiscoveryText(name, contentType, base string, maxKeyDays int64) http.HandlerFunc {
	raw, err := staticFiles.ReadFile("static/" + name)
	if err != nil {
		panic(err)
	}
	body := expandServedText(strings.NewReplacer("{{BASE_URL}}", strings.TrimRight(base, "/"), "{{API_KEY_MAX_DAYS}}", strconv.FormatInt(maxKeyDays, 10)).Replace(string(raw)))
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		if r.Method != http.MethodHead {
			_, _ = w.Write([]byte(body))
		}
	}
}

type DiscoveryTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Capability cards carry short summaries; tools/list holds the full contract.
func agentCardSummary(description string) string {
	description = strings.Join(strings.Fields(description), " ")
	if end := strings.Index(description, ". "); end >= 0 {
		description = description[:end+1]
	}
	chars := []rune(description)
	if len(chars) > 120 {
		return string(chars[:117]) + "..."
	}
	return description
}
