package handler

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestRaceDeployDuringTeamDelete: while alice deletes team "crew" (which owns
// site "board"), mo concurrently tries to deploy a new version to the same
// site. Neither outcome should leave storage_retired without covering
// whatever mo managed to write, and the site must end up consistently
// either fully gone or fully present -- never a site row deleted while a
// version mo just wrote sits unretired in the object store, and never a
// site somehow surviving team deletion.
func TestRaceDeployDuringTeamDelete(t *testing.T) {
	w := newAccessWorld(t)
	w.newTeam("crew", "mo", "alice")
	w.deploy("mo", "/api/collaboration/sites/crew/board")
	ids := w.teamSiteIDs("crew")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("index.html")
	_, _ = f.Write([]byte("<h1>racing deploy</h1>"))
	_ = zw.Close()
	payload := buf.Bytes()
	etag := `"site-` + ids[0] + `-v1"`

	var wg sync.WaitGroup
	var deleteCode, deployCode int
	var deleteBody, deployBody string
	start := make(chan struct{})

	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		rec := w.api("alice", http.MethodDelete, "/api/teams/crew?confirm_name=crew", nil)
		deleteCode, deleteBody = rec.Code, rec.Body.String()
	}()
	go func() {
		defer wg.Done()
		<-start
		req := httptest.NewRequest(http.MethodPut, "https://"+accessBase+"/api/collaboration/sites/crew/board", bytes.NewReader(payload))
		req.Header.Set("X-API-Key", w.apiKeys["mo"])
		req.Header.Set("X-Simple-Host-Client", "control-ui")
		req.Header.Set("If-Match", etag)
		rec := w.do(req)
		deployCode, deployBody = rec.Code, rec.Body.String()
	}()
	close(start)
	wg.Wait()

	t.Logf("delete: %d %s", deleteCode, deleteBody)
	t.Logf("deploy: %d %s", deployCode, deployBody)

	teamGone := !w.teamExists("crew")
	t.Logf("team exists after race: %v", !teamGone)

	if len(ids) != 1 {
		t.Fatalf("setup: want 1 site, got %d", len(ids))
	}
	siteID := ids[0]
	siteRowExists := w.count(`SELECT count(*) FROM sites WHERE id = $1`, siteID) == 1

	if teamGone && siteRowExists {
		t.Fatalf("team deleted but its site row survived: orphan site")
	}

	// However this resolved, count how many storage_retired rows target this
	// site's prefix. If the deploy committed a NEW version after the delete's
	// retire-queue row was inserted, and the site is nonetheless gone, that
	// new version's objects need to be covered somehow.
	n := w.count(`SELECT count(*) FROM storage_retired WHERE object_key = $1`, "sites/"+siteID+"/")
	t.Logf("storage_retired rows for site prefix: %d", n)
	if teamGone && n == 0 {
		t.Fatalf("team+site deleted but nothing queued for storage retirement: leaked bucket objects")
	}
	if !teamGone && deployCode != http.StatusOK && deployCode != http.StatusConflict && deployCode != http.StatusPreconditionFailed {
		t.Fatalf("team survived (delete lost the race) but deploy got unexpected status %d", deployCode)
	}
}
