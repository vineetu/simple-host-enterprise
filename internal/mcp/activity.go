package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// siteActivity is site_activity: one site's versions (the primary read, which
// also settles that the caller may act on the site at all), its recorded
// changes from GET /api/audit, and its visit counts from GET /api/access.
func siteActivity(args map[string]any) (upstream, error) {
	up, err := collaborationSuffix(args, "versions", "GET", nil)
	if err != nil {
		return upstream{}, err
	}
	owner, site := args["owner"].(string), args["site"].(string)
	query := "owner=" + url.QueryEscape(owner) + "&site=" + url.QueryEscape(site)
	// The access log names a namespace by its host label, which is the
	// username folded as handler.ownerLabel folds it.
	label := strings.ReplaceAll(strings.ToLower(owner), ".", "-")
	up.Also = []upstream{
		{Method: "GET", Path: "/api/audit?" + query},
		{Method: "GET", Path: "/api/access?owner=" + url.QueryEscape(label) + "&site=" + url.QueryEscape(site) + "&summary=counts"},
	}
	up.Merge = func(versions []byte, also []partResult) ([]byte, error) {
		return mergeSiteActivity(owner, site, versions, also[0], also[1])
	}
	return up, nil
}

type activityVersion struct {
	VersionNumber int    `json:"version_number"`
	Live          bool   `json:"live"`
	UploadedBy    string `json:"uploaded_by,omitempty"`
	CreatedAt     string `json:"created_at"`
}

type activityChange struct {
	At        string         `json:"at"`
	Action    string         `json:"action"`
	Actor     string         `json:"actor,omitempty"`
	ActorKind string         `json:"actor_kind"`
	Detail    map[string]any `json:"detail,omitempty"`
}

type activityVisits struct {
	From          string `json:"from"`
	To            string `json:"to"`
	UniqueViewers int64  `json:"unique_viewers"`
	Days          []struct {
		Day           string `json:"day"`
		Views         int64  `json:"views"`
		UniqueViewers int64  `json:"unique_viewers"`
	} `json:"days"`
}

type activityResult struct {
	Owner       string            `json:"owner"`
	Site        string            `json:"site"`
	LiveVersion int               `json:"live_version"`
	Versions    []activityVersion `json:"versions"`
	Changes     []activityChange  `json:"changes,omitempty"`
	ChangesNote string            `json:"changes_note,omitempty"`
	Visits      *activityVisits   `json:"visits,omitempty"`
	VisitsNote  string            `json:"visits_note,omitempty"`
}

func mergeSiteActivity(owner, site string, versions []byte, audit, access partResult) ([]byte, error) {
	out := activityResult{Owner: owner, Site: site, Versions: []activityVersion{}}
	if err := json.Unmarshal(versions, &out.Versions); err != nil {
		return nil, fmt.Errorf("site_activity: the versions could not be read: %w", err)
	}
	for _, v := range out.Versions {
		if v.Live {
			out.LiveVersion = v.VersionNumber
		}
	}

	if audit.Status == http.StatusOK {
		var page struct {
			Events []activityChange `json:"events"`
		}
		if err := json.Unmarshal(audit.Body, &page); err != nil {
			out.ChangesNote = "The change history could not be read."
		} else {
			out.Changes = page.Events
			if len(out.Changes) == 0 {
				out.ChangesNote = "No changes are recorded for this site."
			}
		}
	} else {
		out.ChangesNote = partNote("The change history", audit)
	}

	if access.Status == http.StatusOK {
		var visits activityVisits
		if err := json.Unmarshal(access.Body, &visits); err != nil {
			out.VisitsNote = "Visitor numbers could not be read."
		} else {
			out.Visits = &visits
		}
	} else if access.Status == http.StatusForbidden && !strings.Contains(string(access.Body), "scope") {
		out.VisitsNote = "This server shows visitor numbers to admins only (ACCESS_LOG_VISIBILITY=admin)."
	} else {
		out.VisitsNote = partNote("Visitor numbers", access)
	}
	return json.Marshal(out)
}

// partNote says why a part of site_activity is missing, in words a model can
// pass on: a publish-scope key reads versions but not the logs.
func partNote(what string, part partResult) string {
	if part.Status == http.StatusForbidden && strings.Contains(string(part.Body), "scope") {
		return what + " needs a full-scope API key or a connected app; this key is publish-only. The versions above still show who published each one."
	}
	return fmt.Sprintf("%s could not be read (HTTP %d).", what, part.Status)
}
