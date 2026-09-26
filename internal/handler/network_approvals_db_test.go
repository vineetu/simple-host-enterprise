package handler

import (
	"net/http"
	"strings"
	"testing"
)

func (w *accessWorld) networkAudit() string {
	w.t.Helper()
	rows, err := w.database.Query(`SELECT action, COALESCE(detail->>'approval', ''), COALESCE(detail->>'required', '') FROM audit_events WHERE action LIKE 'network_access_%' ORDER BY id`)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var action, approval, required string
		_ = rows.Scan(&action, &approval, &required)
		if approval != "" {
			action += " " + approval + "/" + required
		}
		out = append(out, action)
	}
	return strings.Join(out, ",")
}

// TestNetworkApprovalSelfRefusedOneAdmin: with the default of one approval,
// an admin who requested network access for their own site cannot approve
// it; another admin can.
func TestNetworkApprovalSelfRefusedOneAdmin(t *testing.T) {
	w := newAccessWorldApprovals(t, 1)
	w.deploy("root", "/api/sites/mine")
	got := w.setAccess("root", "/api/sites/mine/access", map[string]any{"level": "network", "reason": "event"}, http.StatusAccepted)
	if req, _ := got["network_request"].(map[string]any); req == nil || req["approvals"] != float64(0) || req["approvals_required"] != float64(1) {
		t.Fatalf("network_request = %v", got["network_request"])
	}
	if rec := w.admin(http.MethodPost, "/api/admin/access-requests/root/mine/approve"); rec.Code != http.StatusForbidden {
		t.Fatalf("self-approval = %d %s, want 403", rec.Code, rec.Body)
	}
	if page := w.admin(http.MethodGet, "/admin").Body.String(); !strings.Contains(page, "your request") || strings.Contains(page, "/root/mine/approve") {
		t.Fatal("admin page offers the requester an approve button")
	}
	if rec := w.adminAs("ada", http.MethodPost, "/api/admin/access-requests/root/mine/approve"); rec.Code != http.StatusOK {
		t.Fatalf("other admin approval = %d %s", rec.Code, rec.Body)
	}
	if code := w.view("", "root", "mine", false); code != http.StatusOK {
		t.Fatalf("anonymous view after approval = %d", code)
	}
	if got := w.networkAudit(); got != "network_access_requested,network_access_approved 1/1" {
		t.Fatalf("audit = %s", got)
	}
}

// TestNetworkApprovalTwoAdminsEndToEnd: with two approvals required, the
// first admin leaves the request pending (the owner sees 1 of 2, the admin
// page names the approver and hides that admin's approve button), the same
// admin again changes nothing, and a second admin opens the site. A
// decline clears partial approvals.
func TestNetworkApprovalTwoAdminsEndToEnd(t *testing.T) {
	w := newAccessWorldApprovals(t, 2)
	w.deploy("alice", "/api/sites/demo")
	w.setAccess("alice", "/api/sites/demo/access", map[string]any{"level": "company"}, http.StatusOK)
	got := w.setAccess("alice", "/api/sites/demo/access", map[string]any{"level": "network", "reason": "event"}, http.StatusAccepted)
	if note, _ := got["note"].(string); !strings.Contains(note, "Two admins") {
		t.Fatalf("note = %q", note)
	}

	if rec := w.admin(http.MethodPost, "/api/admin/access-requests/alice/demo/approve"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "approval 1 of 2") {
		t.Fatalf("first approval = %d %s", rec.Code, rec.Body)
	}
	if code := w.view("", "alice", "demo", false); code == http.StatusOK {
		t.Fatal("one of two approvals opened the site")
	}
	if rec := w.admin(http.MethodPost, "/api/admin/access-requests/alice/demo/approve"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "already approved") {
		t.Fatalf("same admin again = %d %s", rec.Code, rec.Body)
	}
	rec := w.api("alice", http.MethodGet, "/api/collaboration/sites/alice/demo", nil)
	if !strings.Contains(rec.Body.String(), `"approvals":1`) || !strings.Contains(rec.Body.String(), `"approvals_required":2`) {
		t.Fatalf("owner's view = %d %s", rec.Code, rec.Body)
	}
	page := w.admin(http.MethodGet, "/admin").Body.String()
	if !strings.Contains(page, "1 of 2 approvals: root@example.com") || !strings.Contains(page, "you approved") || strings.Contains(page, "/alice/demo/approve") {
		t.Fatal("admin page for the admin who approved: progress or button wrong")
	}
	if page := w.adminAs("ada", http.MethodGet, "/admin").Body.String(); !strings.Contains(page, "/alice/demo/approve") {
		t.Fatal("admin page hides the approve button from the second admin")
	}

	// A decline clears the approval; a new request starts from zero.
	if rec := w.adminAs("ada", http.MethodPost, "/api/admin/access-requests/alice/demo/decline"); rec.Code != http.StatusOK {
		t.Fatalf("decline = %d %s", rec.Code, rec.Body)
	}
	w.setAccess("alice", "/api/sites/demo/access", map[string]any{"level": "network", "reason": "again"}, http.StatusAccepted)
	if rec := w.adminAs("ada", http.MethodPost, "/api/admin/access-requests/alice/demo/approve"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "approval 1 of 2") {
		t.Fatalf("approval after decline = %d %s", rec.Code, rec.Body)
	}
	if rec := w.admin(http.MethodPost, "/api/admin/access-requests/alice/demo/approve"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "approved:") {
		t.Fatalf("second approval = %d %s", rec.Code, rec.Body)
	}
	if code := w.view("", "alice", "demo", false); code != http.StatusOK {
		t.Fatalf("anonymous view after two approvals = %d", code)
	}
	want := "network_access_requested,network_access_approval_added 1/2,network_access_declined," +
		"network_access_requested,network_access_approval_added 1/2,network_access_approved 2/2"
	if got := w.networkAudit(); got != want {
		t.Fatalf("audit = %s\nwant    %s", got, want)
	}
}
