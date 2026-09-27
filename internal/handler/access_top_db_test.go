package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	db "github.com/vsriram/simple-host/internal/db"
)

// The Visitors view's top pages and referring domains: people's page views
// only (no assets, bots or errors), the referrer as a domain only, counts
// only for the owner, and the aggregate for an admin who asks for it.
func TestAccessTopPagesAndReferrers(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	now := time.Now().UTC()
	ev := func(user, path string, status int, kind, ref string) db.AccessLogEvent {
		return db.AccessLogEvent{At: now, UserID: w.users[user], OwnerLabel: "alice", SiteName: "demo", Path: path, Method: "GET", Status: status, ClientKind: kind, ReferrerDomain: ref, IP: "10.0.0.9"}
	}
	if err := db.InsertAccessLogBatch(context.Background(), w.database, []db.AccessLogEvent{
		ev("vera", "/", 200, "human", "news.example.com"),
		ev("olly", "/", 200, "human", "news.example.com"),
		ev("vera", "/about.html", 200, "human", ""),
		ev("vera", "/guide", 200, "human", "wiki.example.org"),
		ev("vera", "/app.js", 200, "human", ""),
		ev("vera", "/style.css", 200, "human", ""),
		ev("vera", "/missing.html", 404, "human", ""),
		ev("", "/", 200, "bot", "spam.example.net"),
	}); err != nil {
		t.Fatal(err)
	}

	rec := w.api("alice", http.MethodGet, "/api/access?owner=alice&site=demo", nil)
	var counts struct {
		TopPages []struct {
			Path  string `json:"path"`
			Views int64  `json:"views"`
		} `json:"top_pages"`
		TopReferrers []struct {
			Domain string `json:"domain"`
			Views  int64  `json:"views"`
		} `json:"top_referrers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &counts); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("owner counts = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), w.users["vera"]) || strings.Contains(rec.Body.String(), "10.0.0.9") {
		t.Fatalf("owner counts name a visitor: %s", rec.Body)
	}
	if len(counts.TopPages) != 3 || counts.TopPages[0].Path != "/" || counts.TopPages[0].Views != 2 {
		t.Fatalf("top pages = %+v, want / (2), then /about.html and /guide; no assets, errors or bots", counts.TopPages)
	}
	if len(counts.TopReferrers) != 2 || counts.TopReferrers[0].Domain != "news.example.com" || counts.TopReferrers[0].Views != 2 || counts.TopReferrers[1].Domain != "wiki.example.org" {
		t.Fatalf("top referrers = %+v, want news.example.com (2), wiki.example.org (1)", counts.TopReferrers)
	}

	// An admin's session gets rows by default and the aggregate on request.
	rec = w.admin(http.MethodGet, "/api/access?owner=alice&site=demo")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"referrer_domain":"news.example.com"`) {
		t.Fatalf("admin rows = %d %s, want the referrer domain on each row", rec.Code, rec.Body)
	}
	rec = w.admin(http.MethodGet, "/api/access?owner=alice&site=demo&summary=counts")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"top_referrers"`) || strings.Contains(rec.Body.String(), `"entries"`) {
		t.Fatalf("admin summary = %d %s, want the aggregate", rec.Code, rec.Body)
	}
}

// A real visit records the referrer's domain, never its path or query.
func TestSiteVisitRecordsReferrerDomainOnly(t *testing.T) {
	w := newAccessWorld(t)
	hosts, err := NewHostModel("https://" + accessBase)
	if err != nil {
		t.Fatal(err)
	}
	writer := audit.NewAccessWriter(w.database)
	w.files.WithAccessWriter(writer).WithHosts(hosts)
	w.deploy("alice", "/api/sites/demo")
	host := "demo.alice." + accessBase
	visit := func(referer string) {
		r := httptest.NewRequest(http.MethodGet, "https://"+host+"/", nil)
		r.Header.Set("Accept", "text/html")
		r.Header.Set("Referer", referer)
		r.AddCookie(w.cookie("alice", host))
		if rec := w.do(r); rec.Code != http.StatusOK {
			t.Fatalf("visit = %d", rec.Code)
		}
	}
	visit("https://news.example.com/item?id=42&secret=abc")
	writer.Close() // flushes
	var ref string
	if err := w.database.QueryRow(`SELECT referrer_domain FROM access_log WHERE owner_label = 'alice' AND site_name = 'demo' ORDER BY at DESC LIMIT 1`).Scan(&ref); err != nil || ref != "news.example.com" {
		t.Fatalf("referrer_domain = %q (%v), want news.example.com", ref, err)
	}
}

// A person an admin renamed keeps their Visitors history: visits recorded
// under the old label count under the new one, for the person themselves.
func TestAccessCountsCarryAcrossRename(t *testing.T) {
	w := newAccessWorld(t)
	w.deploy("alice", "/api/sites/demo")
	now := time.Now().UTC()
	if err := db.InsertAccessLogBatch(context.Background(), w.database, []db.AccessLogEvent{
		{At: now, UserID: w.users["vera"], OwnerLabel: "alice", SiteName: "demo", Path: "/", Method: "GET", Status: 200, ClientKind: "human", ReferrerDomain: "news.example.com"},
	}); err != nil {
		t.Fatal(err)
	}
	if rec := w.adminForm("/api/admin/users/alice/rename", map[string][]string{"name": {"alicia"}}); rec.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body)
	}
	if err := db.InsertAccessLogBatch(context.Background(), w.database, []db.AccessLogEvent{
		{At: now, UserID: w.users["olly"], OwnerLabel: "alicia", SiteName: "demo", Path: "/", Method: "GET", Status: 200, ClientKind: "human"},
	}); err != nil {
		t.Fatal(err)
	}
	rec := w.api("alice", http.MethodGet, "/api/access?owner=alicia&site=demo&summary=counts", nil)
	var counts struct {
		UniqueViewers int64 `json:"unique_viewers"`
		TopReferrers  []struct {
			Domain string `json:"domain"`
		} `json:"top_referrers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &counts); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("counts after rename = %d %s", rec.Code, rec.Body)
	}
	if counts.UniqueViewers != 2 || len(counts.TopReferrers) != 1 || counts.TopReferrers[0].Domain != "news.example.com" {
		t.Fatalf("counts after rename = %s, want both visits (one before the rename)", rec.Body)
	}
	// Somebody else cannot ask for the old label: it is not theirs.
	if rec := w.api("vera", http.MethodGet, "/api/access?owner=alice&site=demo&summary=counts", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("vera reading the old label = %d", rec.Code)
	}
}
