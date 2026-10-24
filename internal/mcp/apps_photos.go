package mcp

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/vsriram/simple-host/internal/auth"
)

func (s *Server) photoLinkResult(r *http.Request, req request, args map[string]any, body []byte) response {
	var sites []struct {
		Name  string `json:"name"`
		Owner string `json:"owner_username"`
		Role  string `json:"access_role"`
	}
	if json.Unmarshal(body, &sites) != nil {
		return toolResult(req.ID, "Could not read sites.", nil, true)
	}
	requested := args["site"].(string)
	selected := ""
	for _, site := range sites {
		qualified := site.Owner + "/" + site.Name
		if site.Role != "owner" && site.Role != "member" {
			continue
		}
		if requested != qualified && requested != site.Name {
			continue
		}
		if selected != "" {
			return toolResult(req.ID, "More than one namespace has this site. Pass owner/site from list_sites.", nil, true)
		}
		selected = qualified
	}
	if selected == "" {
		return toolResult(req.ID, "No site with this name that you can edit. Call list_sites.", nil, true)
	}
	query := url.Values{"site": {selected}}
	if user := auth.GetUser(r.Context()); user != nil {
		query.Set("account", user.Username)
	}
	address := strings.TrimRight(s.appOrigin, "/") + "/photos?" + query.Encode()
	return s.appResult(r, "add_photos", toolResult(req.ID, address, map[string]any{"site": selected, "photos_url": address}, false))
}
