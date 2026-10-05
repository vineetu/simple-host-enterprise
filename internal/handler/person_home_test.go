package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/vsriram/simple-host/internal/db"
)

func personalGate(t *testing.T, ownerID string) *hostGate {
	store := newTestStore(t)
	writeGateSite(t, store, "alice", "gantt", "gantt-index")
	g := testHostGate(t, store, testHostModel(t))
	g.ownerIndex = func(context.Context, string) (string, []db.Site, map[string]bool, error) {
		return ownerID, ownerIndexTestSites(), ownerIndexTestRestricted, nil
	}
	g.personHome = func(context.Context, string) (string, error) { return "gantt", nil }
	g.presentation = func(context.Context, string) (db.PersonPresentation, map[string]db.ShowcasePreference, error) {
		return db.PersonPresentation{Bio: "<script>bio</script>"}, map[string]db.ShowcasePreference{"gantt": {Pinned: true, Order: 5}, "draft-notes": {Pinned: true, Order: 1}}, nil
	}
	return g
}
func TestPersonHomeUsesExistingSiteOrigin(t *testing.T) {
	g := personalGate(t, "user-1")
	h := g.wrap(newHostGateTestMux())
	r := gateAuthedRequest(t, h, http.MethodGet, "alice.foo.example", "/?a=b")
	if r.Code != 302 || r.Header().Get("Location") != "https://gantt.alice.foo.example/?a=b" {
		t.Fatalf("home: %d %s", r.Code, r.Header().Get("Location"))
	}
	if r := gateRequest(h, http.MethodGet, "alice.foo.example", "/"); r.Code == 302 && strings.Contains(r.Header().Get("Location"), "gantt") {
		t.Fatal("home disclosed without sign-in")
	}
}
func TestPersonHomeDeniedFallsBack(t *testing.T) {
	g := personalGate(t, "user-2")
	g.viewerAllowed = func(*http.Request, string, string) (bool, error) { return false, nil }
	r := gateAuthedRequest(t, g.wrap(newHostGateTestMux()), http.MethodGet, "alice.foo.example", "/")
	if r.Code != 200 || r.Header().Get("Location") != "" {
		t.Fatalf("fallback: %d", r.Code)
	}
	if strings.Contains(r.Body.String(), "<script>bio") {
		t.Fatal("bio was not escaped")
	}
}
func TestPersonShowcaseFeedVisibilityAndOrder(t *testing.T) {
	for _, tc := range []struct {
		ownerID string
		names   string
	}{{"user-1", "draft-notes,gantt,payroll"}, {"user-2", "gantt"}} {
		g := personalGate(t, tc.ownerID)
		h := g.wrap(newHostGateTestMux())
		r := gateAuthedRequest(t, h, http.MethodGet, "alice.foo.example", "/showcase.json")
		var out struct {
			Sites []struct {
				Name string `json:"name"`
			} `json:"sites"`
		}
		if r.Code != 200 || json.Unmarshal(r.Body.Bytes(), &out) != nil {
			t.Fatalf("feed: %d %s", r.Code, r.Body.String())
		}
		var names []string
		for _, s := range out.Sites {
			names = append(names, s.Name)
		}
		if strings.Join(names, ",") != tc.names {
			t.Fatalf("feed names: %v", names)
		}
		if !strings.Contains(r.Header().Get("Cache-Control"), "no-store") {
			t.Fatal("feed cached")
		}
		if r := gateRequest(h, http.MethodGet, "alice.foo.example", "/showcase.json"); r.Code == 200 {
			t.Fatal("feed exposed without sign-in")
		}
	}
}
