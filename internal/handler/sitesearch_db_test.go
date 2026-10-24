package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vsriram/simple-host/internal/db"
)

func TestSiteSearchAuthorizedInventoryAndLiveSummaries(t *testing.T) {
	w := newAccessWorld(t)
	ctx := context.Background()
	tx, _ := w.database.Begin()
	team, err := db.CreateTeam(ctx, tx, "team-research", w.users["mo"])
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Alice is a member of research as well as an owner and a named viewer.
	if _, err = w.database.Exec(`INSERT INTO team_members(team_id,user_id) VALUES($1,$2)`, team.ID, w.users["alice"]); err != nil {
		t.Fatal(err)
	}
	deploy := func(user, method, path, title, etag string) map[string]any {
		t.Helper()
		var buf bytes.Buffer
		z := zip.NewWriter(&buf)
		f, _ := z.Create("index.html")
		f.Write([]byte("<title>" + title + "</title><meta name=description content='Coastal homes'><h1>For sale</h1><p>Sunny apartments</p>"))
		z.Close()
		r := httptest.NewRequest(method, "https://"+accessBase+path, &buf)
		r.Header.Set("X-API-Key", w.apiKeys[user])
		r.Header.Set("X-Simple-Host-Client", "control-ui")
		if etag != "" {
			r.Header.Set("If-Match", etag)
		}
		out := w.do(r)
		if out.Code != 200 && out.Code != 201 {
			t.Fatalf("deploy %s: %d %s", path, out.Code, out.Body)
		}
		var data map[string]any
		json.Unmarshal(out.Body.Bytes(), &data)
		data["etag"] = out.Header().Get("ETag")
		return data
	}
	deploy("alice", "POST", "/api/sites/own", "Real owned", "")
	deploy("mo", "POST", "/api/collaboration/sites/team-research/teamwork", "Real team", "")
	shared := deploy("vera", "POST", "/api/sites/shared", "Real shared", "")
	deploy("olly", "POST", "/api/sites/hidden", "Real hidden", "")
	deploy("olly", "POST", "/api/sites/company", "Real company", "")
	for _, path := range []string{"shared"} {
		w.setAccess("vera", "/api/collaboration/sites/vera/"+path+"/access", map[string]any{"level": "specific"}, 200)
		w.setAccess("vera", "/api/collaboration/sites/vera/"+path+"/viewers", map[string]any{"usernames": []string{"alice"}}, 200)
	}
	w.setAccess("olly", "/api/collaboration/sites/olly/company/access", map[string]any{"level": "listed"}, 200)
	list := func(user, query string) []collaborationSiteResponse {
		t.Helper()
		out := w.api(user, "GET", "/api/collaboration/sites?include=shared&"+query, nil)
		if out.Code != 200 {
			t.Fatalf("list %d %s", out.Code, out.Body)
		}
		var items []collaborationSiteResponse
		if err := json.Unmarshal(out.Body.Bytes(), &items); err != nil {
			t.Fatal(err)
		}
		return items
	}
	items := list("alice", "query=REAL+coastal")
	if len(items) != 3 {
		t.Fatalf("wanted owner/member/viewer, got %+v", items)
	}
	roles := map[string]db.CollaborationRole{}
	for _, item := range items {
		roles[item.Name] = item.AccessRole
		if item.Summary.Description != "Coastal homes" {
			t.Fatalf("missing summary: %+v", item)
		}
	}
	if roles["own"] != "owner" || roles["teamwork"] != "member" || roles["shared"] != "viewer" {
		t.Fatal(roles)
	}
	if got := list("alice", "query=hidden"); len(got) != 0 {
		t.Fatal("private site leaked")
	}
	if got := list("alice", "query=company"); len(got) != 0 {
		t.Fatal("listed discovery mixed into private inventory")
	}
	if got := list("alice", "query=real&limit=1&offset=2"); len(got) != 1 {
		t.Fatal("paging authorized inventory")
	}
	// A held deploy must be stored but cannot leak its text to a viewer.
	held := deploy("vera", "PUT", "/api/collaboration/sites/vera/shared?publish=false", "Unpublished secret", shared["etag"].(string))
	if got := list("alice", "query=unpublished"); len(got) != 0 {
		t.Fatal("held deploy leaked")
	}
	if got := list("alice", "query=real+shared"); len(got) != 1 {
		t.Fatal("lost live summary")
	}
	r := httptest.NewRequest("POST", "https://"+accessBase+"/api/collaboration/sites/vera/shared/rollback", strings.NewReader(`{"version":2}`))
	r.Header.Set("X-API-Key", w.apiKeys["vera"])
	r.Header.Set("X-Simple-Host-Client", "control-ui")
	r.Header.Set("If-Match", held["etag"].(string))
	r.Header.Set("Content-Type", "application/json")
	if out := w.do(r); out.Code != 200 {
		t.Fatalf("rollback %d %s", out.Code, out.Body)
	}
	if got := list("alice", "query=unpublished"); len(got) != 1 {
		t.Fatal("rollback did not select version summary")
	}
	if _, err := w.database.Exec(`DELETE FROM team_members WHERE team_id=$1 AND user_id=$2`, team.ID, w.users["alice"]); err != nil {
		t.Fatal(err)
	}
	if got := list("alice", "query=teamwork"); len(got) != 0 {
		t.Fatal("revoked member leaked")
	}
	out := w.api("alice", "GET", "/api/sites?query=REAL+coastal", nil)
	if out.Code != 200 {
		t.Fatalf("owned list: %d %s", out.Code, out.Body)
	}
	var owned []siteResponse
	json.Unmarshal(out.Body.Bytes(), &owned)
	if len(owned) != 1 || owned[0].Name != "own" {
		t.Fatal("owned list search")
	}
}
