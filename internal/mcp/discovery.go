package mcp

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
)

// The common building and sharing operations. Other tools stay callable.
var coreToolNames = []string{
	"get_account", "list_sites", "get_site", "deploy_site", "list_site_files", "read_site_file",
	"list_teams", "find_users", "set_site_access", "grant_site_viewer",
	"get_state", "update_state", "storage_list_resources", "storage_set_resource",
	"storage_get_kv", "storage_put_kv", "storage_list_rows", "storage_add_row",
	"find_tools", "run_tool",
}

var toolAliases = map[string][]string{
	"add_photos":                {"add photos", "upload pictures from my phone", "put my photos on the site"},
	"list_site_viewers":         {"viewers", "who has access", "allowed emails"},
	"revoke_site_viewer":        {"remove viewer", "revoke access", "uninvite"},
	"rollback_site":             {"undo", "revert", "restore previous", "go back", "broke", "previous version", "last change", "put back", "yesterday"},
	"list_site_versions":        {"versions", "history", "revisions"},
	"preview_version":           {"preview", "draft", "older version", "see old version", "without publishing"},
	"site_activity":             {"visitors", "visits", "traffic", "views", "stats", "numbers", "how many people", "who visited"},
	"export_site":               {"download", "backup", "copy", "archive", "entire website", "download website"},
	"delete_site":               {"delete", "delete site", "remove site", "trash"},
	"restore_site":              {"undelete", "recently deleted", "bring back", "deleted site", "recover", "recover website"},
	"list_deleted_sites":        {"undelete", "recently deleted", "trash list", "show trash"},
	"rename_site":               {"rename", "new name", "change name"},
	"set_home_page":             {"home page", "homepage", "profile landing"},
	"set_bio":                   {"bio", "biography", "profile description"},
	"set_showcase_site":         {"pin", "showcase", "profile order"},
	"keep_site":                 {"keep site", "renew", "expiry", "stop expiring", "never expire", "permanent"},
	"storage_list_resources":    {"saved data", "presets", "who can read", "access", "resources", "database permissions", "visitors read"},
	"storage_list_file_objects": {"uploads", "uploaded photos", "files", "photos visitors uploaded", "pictures", "images", "show uploads"},
	"storage_list_kv_keys":      {"keys", "saved settings", "list values"},
	"storage_delete_resource":   {"delete resource", "drop database", "remove bucket"},
	"storage_delete_kv":         {"delete key", "remove value", "erase setting", "remove saved setting"},
	"storage_delete_file":       {"delete file", "remove photo", "erase upload"},
	"create_team":               {"create team", "new team"},
	"add_team_member":           {"invite colleague", "add team member"},
	"remove_team_member":        {"remove team member", "remove colleague"},
	"transfer_site":             {"move site", "hand over", "transfer to team"},
	"search_sites":              {"company showcase", "find listed sites", "colleague websites"},
	"delete_site_asset":         {"delete uploaded asset", "remove uploaded asset"},
	"storage_delete_row":        {"delete row", "remove record"},
}

// Operations get a fixed bonus, independent of how rare their words are. Match
// unstemmed query words so "deleted" describes an object, not a delete request.
var actionAliases = map[string][]string{
	"delete":   {"delete", "remove", "erase", "trash"},
	"remove":   {"delete", "remove", "erase", "trash", "disconnect", "unlink"},
	"restore":  {"restore", "recover", "undelete", "bring back"},
	"list":     {"list", "show", "see"},
	"get":      {"get", "show"},
	"set":      {"set"},
	"connect":  {"connect", "link", "attach"},
	"revoke":   {"revoke", "remove", "uninvite"},
	"rollback": {"rollback", "undo", "revert"},
	"export":   {"export", "download", "backup"},
	"rename":   {"rename"},
	"keep":     {"keep", "renew"},
	"read":     {"read"},
	"create":   {"create"},
	"update":   {"update"},
	"preview":  {"preview"},
}

func discoveryTools() []Tool {
	return []Tool{
		{
			Name: "find_tools", Title: "Find more tools",
			Description: "Find tools for a job, including teams, rollback, rename, delete, restore, visits, export, and storage. Returns arguments and annotations; call the chosen tool with run_tool.",
			InputSchema: object(map[string]any{
				"query": str("The job in a few words; empty lists the other tools."),
				"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 10, "default": 3},
			}, "query"),
			OutputSchema: outObject(map[string]any{
				"tools": outArray("Matching tools or the catalog.", outObject(map[string]any{
					"name": outString("Tool name."), "title": outString("Tool title."),
					"description": outString("What the tool does."),
					"arguments":   map[string]any{"type": "object", "additionalProperties": true},
					"annotations": map[string]any{"type": "object", "additionalProperties": true},
					"listed":      outBool("Already in tools/list."),
				}, "name", "title")),
				"next": outString("How to call it."),
			}, "tools", "next"),
			Annotations: readOnly(), family: familyAccount,
		},
		{
			Name: "run_tool", Title: "Run a tool",
			Description: "Run a tool named by find_tools, a skill, or a result. Pass its arguments unchanged. The target tool's authorization and ETag checks apply; follow its confirmation requirements.",
			InputSchema: object(map[string]any{
				"name":      str("The tool name."),
				"arguments": map[string]any{"type": "object", "additionalProperties": true, "description": "The tool's arguments; defaults to {}."},
			}, "name"),
			Annotations: writes(true, false), family: familyAccount,
		},
	}
}

// listedTools keeps the full direct-call inventory intact, including Apps
// metadata. Enabled photo picking is an additional listed building tool.
func (s *Server) listedTools(r *http.Request) []Tool {
	available := s.appTools(r)
	byName := map[string]Tool{}
	for _, tool := range available {
		byName[tool.Name] = tool
	}
	out := make([]Tool, 0, len(coreToolNames)+1)
	for _, name := range coreToolNames {
		if tool, ok := byName[name]; ok {
			out = append(out, tool)
		}
	}
	if tool, ok := byName["add_photos"]; ok {
		out = append(out, tool)
	}
	return out
}

func (s *Server) findTools(r *http.Request, req request, args map[string]any) response {
	query, ok := args["query"].(string)
	if !ok {
		return toolResult(req.ID, "query must be a string", nil, true)
	}
	limit := 3
	if raw, present := args["limit"]; present {
		n, ok := raw.(float64)
		if !ok || n < 1 || n > 10 || n != math.Trunc(n) {
			return toolResult(req.ID, "limit must be an integer from 1 to 10", nil, true)
		}
		limit = int(n)
	}
	available := s.appTools(r)
	if len(s.enabledScreens(r)) == 0 {
		available = append(available, s.byName["add_photos"])
	}
	search := newToolSearch(available)
	listed := map[string]bool{}
	for _, tool := range s.listedTools(r) {
		listed[tool.Name] = true
	}
	for i := range search.tools {
		search.tools[i].listed = listed[search.tools[i].tool.Name]
	}
	text, structured := search.find(query, limit)
	return toolResult(req.ID, text, structured, false)
}

type searchableTool struct {
	tool    Tool
	weights map[string]float64
	phrases []string
	actions []string
	listed  bool
}

type toolSearch struct {
	tools []searchableTool
	idf   map[string]float64
}

func searchTerms(text string) []string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') })
	terms := make([]string, 0, len(words))
	for _, word := range words {
		switch word {
		case "a", "an", "the", "my", "our", "me", "i", "it", "to", "for", "of", "on", "in", "is", "are", "and", "from", "with", "this":
			continue
		}
		if len(word) > 4 && strings.HasSuffix(word, "ies") {
			word = strings.TrimSuffix(word, "ies") + "y"
		} else if len(word) > 3 && strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss") {
			word = strings.TrimSuffix(word, "s")
		}
		if word == "expiry" {
			word = "expire"
		}
		switch word {
		case "website":
			word = "site"
		case "photo", "picture", "image":
			word = "file"
		}
		inflected := false
		switch {
		case len(word) > 4 && strings.HasSuffix(word, "ied"):
			word = strings.TrimSuffix(word, "ied") + "y"
		case len(word) > 5 && strings.HasSuffix(word, "ing"):
			word = strings.TrimSuffix(word, "ing")
			inflected = true
		case len(word) > 4 && strings.HasSuffix(word, "ed"):
			word = strings.TrimSuffix(word, "ed")
			inflected = true
		}
		// Undo consonant doubling in forms such as pinned and stopping.
		if n := len(word); inflected && n > 2 && word[n-1] == word[n-2] && strings.ContainsRune("bdgmnprt", rune(word[n-1])) {
			word = word[:n-1]
		}
		if len(word) > 3 {
			word = strings.TrimSuffix(word, "e")
		}
		terms = append(terms, word)
	}
	return terms
}

func newToolSearch(tools []Tool) *toolSearch {
	s := &toolSearch{idf: map[string]float64{}}
	listed := map[string]bool{}
	for _, name := range coreToolNames {
		listed[name] = true
	}
	for _, tool := range tools {
		if tool.Name == "find_tools" || tool.Name == "run_tool" {
			continue
		}
		entry := searchableTool{tool: tool, weights: map[string]float64{}, listed: listed[tool.Name]}
		add := func(text string, weight float64) {
			for _, term := range searchTerms(text) {
				if weight > entry.weights[term] {
					entry.weights[term] = weight
				}
			}
		}
		add(tool.Name, 4)
		add(tool.Title, 2)
		add(tool.Description, 0.5)
		for _, alias := range toolAliases[tool.Name] {
			add(alias, 3)
			terms := searchTerms(alias)
			if len(terms) > 1 {
				entry.phrases = append(entry.phrases, strings.Join(terms, " "))
			}
		}
		// Skip namespace/noun components; set_keep_versions also names "keep".
		for _, word := range strings.Split(tool.Name, "_") {
			entry.actions = append(entry.actions, actionAliases[word]...)
		}
		// Read-only tools can also answer "show"/"see" even when their names
		// are nouns, such as site_analytics and domain_status.
		if tool.Annotations["readOnlyHint"] == true {
			entry.actions = append(entry.actions, "show", "see")
		}
		for _, action := range entry.actions {
			add(action, 4)
		}
		for term := range entry.weights {
			s.idf[term]++
		}
		s.tools = append(s.tools, entry)
	}
	for term, count := range s.idf {
		s.idf[term] = math.Log(1 + float64(len(s.tools))/count)
	}
	return s
}

func (s *toolSearch) rank(query string, limit int) []searchableTool {
	terms := searchTerms(query)
	actionPhrase := " " + strings.Join(strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return r < 'a' || r > 'z'
	}), " ") + " "
	phrase := " " + strings.Join(terms, " ") + " "
	type match struct {
		entry searchableTool
		score float64
	}
	matches := []match{}
	for _, entry := range s.tools {
		score := 0.0
		for _, term := range terms {
			score += s.idf[term] * entry.weights[term]
		}
		for _, alias := range entry.phrases {
			if strings.Contains(phrase, " "+alias+" ") {
				score += 12
			}
		}
		for _, action := range entry.actions {
			if strings.Contains(actionPhrase, " "+action+" ") {
				score += 20
				break
			}
		}
		if strings.Contains(strings.ToLower(query), entry.tool.Name) {
			score += 1000
		}
		if score > 0 {
			matches = append(matches, match{entry, score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	if len(matches) > limit {
		matches = matches[:limit]
	}
	result := make([]searchableTool, 0, len(matches))
	for _, match := range matches {
		result = append(result, match.entry)
	}
	return result
}

func (s *toolSearch) find(query string, limit int) (string, map[string]any) {
	matches := s.rank(query, limit)
	catalog := strings.TrimSpace(query) == "" || len(matches) == 0
	lines := []string{}
	if catalog {
		matches = nil
		for _, entry := range s.tools {
			if !entry.listed {
				matches = append(matches, entry)
			}
		}
		if strings.TrimSpace(query) != "" {
			lines = append(lines, "Nothing matched. Describe the job differently or pick from this list.")
		}
	}
	items := make([]any, 0, len(matches))
	for _, entry := range matches {
		tool := entry.tool
		item := map[string]any{"name": tool.Name, "title": tool.Title}
		how := "run_tool"
		if entry.listed {
			how = "call it directly"
		}
		if !catalog {
			item["description"], item["arguments"], item["listed"] = tool.Description, tool.InputSchema, entry.listed
			item["annotations"] = tool.Annotations
			arguments, _ := json.Marshal(tool.InputSchema)
			lines = append(lines, fmt.Sprintf("%s (%s): %s\nArguments: %s", tool.Name, how, tool.Description, arguments))
		} else {
			lines = append(lines, fmt.Sprintf("%s: %s (%s)", tool.Name, tool.Title, how))
		}
		items = append(items, item)
	}
	return strings.Join(lines, "\n"), map[string]any{
		"tools": items, "next": `Call one with run_tool {"name": ..., "arguments": {...}}`,
	}
}
