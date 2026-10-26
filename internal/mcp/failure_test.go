package mcp

import (
	"fmt"
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

// A storage refusal already says what happened and what to do, so it reaches
// the model as the server wrote it, with nothing appended. A 404 no_table on
// a storage tool once read "No such site for this account", and a row's
// not_found read as a namespace problem.
func TestStorageFailuresPassThroughWithoutHint(t *testing.T) {
	for _, tc := range []struct {
		tool   string
		status int
		body   string
	}{
		{"storage_list_rows", http.StatusNotFound, `{"error":"database shop has no table named orders. Tables here: customers","code":"no_table"}`},
		{"storage_add_row", http.StatusBadRequest, `{"error":"customers has no column named phone; leave it out. Columns in customers: email, name","code":"unknown_column"}`},
		{"storage_add_row", http.StatusBadRequest, `{"error":"customers.age is integer: send a whole number such as 42, without quotes","code":"invalid_value"}`},
		{"storage_add_row", http.StatusBadRequest, `{"error":"customers.name needs a value (required): include it, and do not send null for it","code":"required_missing"}`},
		{"storage_add_row", http.StatusNotFound, `{"error":"customers.parent_id must be the id of an existing row of parents that you can read","code":"invalid_reference"}`},
		{"storage_delete_row", http.StatusConflict, `{"error":"other rows still point at this row: delete or change those rows first","code":"referenced"}`},
		{"storage_get_row", http.StatusNotFound, `{"error":"customers has no row 7 that you can see: list its rows for the ids there are","code":"not_found"}`},
		{"storage_list_kv_keys", http.StatusForbidden, `{"error":"the read rule of key-value storage vault is nobody","code":"forbidden"}`},
		{"storage_put_kv", http.StatusRequestEntityTooLarge, `{"error":"the value is larger than 100 bytes: store less under one key","code":"too_large"}`},
		{"storage_set_resource", http.StatusBadRequest, `{"error":"edit and delete cannot be anyone","code":"invalid_access","rule":"anonymous_change"}`},
	} {
		tool, known := byName(tc.tool)
		if !known {
			t.Fatalf("tool %q is not registered", tc.tool)
		}
		got := explainFailure(tc.status, tc.body, tool)
		if want := fmt.Sprintf("%s failed (HTTP %d): %s", tc.tool, tc.status, tc.body); got != want {
			t.Errorf("%s %s:\n got %s\nwant %s", tc.tool, tc.body, got, want)
		}
	}
	// On the settings tools not_found is about the namespace and keeps its
	// hint; a code outside storage keeps its hint on a storage tool too.
	tool, _ := byName("storage_set_resource")
	if got := explainFailure(http.StatusNotFound, `{"error":"namespace not found","code":"not_found"}`, tool); !strings.Contains(got, "namespace") || !strings.Contains(got, "list_teams") {
		t.Errorf("settings not_found lost its hint:\n%s", got)
	}
	tool, _ = byName("storage_list_rows")
	if got := explainFailure(http.StatusTooManyRequests, `{"error":"rate limit exceeded"}`, tool); !strings.Contains(got, "Rate limited") {
		t.Errorf("rate limit lost its hint:\n%s", got)
	}
}
