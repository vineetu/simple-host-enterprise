package mcp

import (
	"net/http"
	"strings"
	"testing"
)

// A failure is the only thing a model reads before deciding what to do next,
// so the wrong steering is worse than none: "call list_sites" on a team tool
// sends it looking for a thing that has nothing to do with what failed.
func TestExplainFailureSteersByCodeThenFamily(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tool    string
		status  int
		body    string
		want    string
		notWant string
	}{
		{
			name:   "code wins over status",
			tool:   "delete_team",
			status: http.StatusConflict,
			body:   `{"error":"delete the team's sites first","code":"team_has_sites"}`,
			want:   "still owns sites",
			// The status alone would have called this a stale ETag.
			notWant: "etag",
		},
		{
			name:    "last member is not an etag problem",
			tool:    "remove_team_member",
			status:  http.StatusConflict,
			body:    `{"error":"a team keeps at least one member","code":"last_member"}`,
			want:    "leave_team",
			notWant: "get_site",
		},
		{
			name:    "a delete that needs confirming asks the person, not the model",
			tool:    "leave_team",
			status:  http.StatusConflict,
			body:    `{"error":"You are the last active member, so leaving deletes team acme and its 2 sites permanently.","code":"confirm_team_delete","site_count":2}`,
			want:    "ask",
			notWant: "etag",
		},
		{
			name:    "a team 404 does not send the model to look for a site",
			tool:    "list_team_members",
			status:  http.StatusNotFound,
			body:    `{"error":"team not found","code":"not_found"}`,
			want:    "list_teams",
			notWant: "list_sites",
		},
		{
			name:    "the same code on a site tool is about the namespace",
			tool:    "delete_site",
			status:  http.StatusNotFound,
			body:    `{"error":"namespace not found","code":"not_found"}`,
			want:    "namespace",
			notWant: "list_team_members",
		},
		{
			name:    "a site limit is not an existing-site conflict",
			tool:    "deploy_site",
			status:  http.StatusConflict,
			body:    `{"error":"too many sites (1,000 of 1,000): delete a site to create another","code":"site_limit"}`,
			want:    "site limit",
			notWant: "etag",
		},
		{
			name:    "a full storage quota is not a smaller-files problem",
			tool:    "deploy_site",
			status:  http.StatusRequestEntityTooLarge,
			body:    `{"error":"storage quota exceeded (used 10.0 GiB of 10.0 GiB; this upload needs 2.0 MiB)","code":"storage_quota"}`,
			want:    "storage quota",
			notWant: "fewer or smaller",
		},
		{
			name:   "an infected file is never worked around",
			tool:   "deploy_site",
			status: http.StatusUnprocessableEntity,
			body:   `{"error":"upload rejected: a.exe is infected (Eicar-Test-Signature); nothing was stored","code":"malware_found"}`,
			want:   "Do not retry",
		},
		{
			name:   "a scanner outage is not the caller's fault",
			tool:   "deploy_site",
			status: http.StatusServiceUnavailable,
			body:   `{"error":"the malware scanner is unavailable","code":"scanner_unavailable"}`,
			want:   "Nothing in the arguments",
		},
		{
			name:   "a site 404 with no code keeps the site wording",
			tool:   "get_site",
			status: http.StatusNotFound,
			body:   `{"error":"not found"}`,
			want:   "list_sites",
		},
		{
			name:   "400 is steered rather than handed over raw",
			tool:   "create_team",
			status: http.StatusBadRequest,
			body:   `{"error":"team names use letters, numbers and hyphens only — no dots"}`,
			want:   "no dots",
		},
		{
			name:   "400 on any other tool still says not to repeat the call",
			tool:   "deploy_site",
			status: http.StatusBadRequest,
			body:   `{"error":"index.html is required"}`,
			want:   "correct the arguments",
		},
		{
			name:   "a body that is not JSON falls through to the status",
			tool:   "deploy_site",
			status: http.StatusRequestEntityTooLarge,
			body:   "request body too large",
			want:   "size limit",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, known := byName(tc.tool)
			if !known {
				t.Fatalf("tool %q is not registered", tc.tool)
			}
			got := explainFailure(tc.status, tc.body, tool)
			if !strings.Contains(strings.ToLower(got), strings.ToLower(tc.want)) {
				t.Errorf("explanation does not mention %q:\n%s", tc.want, got)
			}
			if tc.notWant != "" && strings.Contains(strings.ToLower(got), strings.ToLower(tc.notWant)) {
				t.Errorf("explanation should not mention %q:\n%s", tc.notWant, got)
			}
			if !strings.Contains(got, tc.body) {
				t.Errorf("the upstream body must survive into the explanation:\n%s", got)
			}
		})
	}
}
