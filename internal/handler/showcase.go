package handler

import (
	"database/sql"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/auth"
	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/sitetype"
)

// ShowcaseHandler renders /showcase — a public, unauth gallery that uses
// the same visual language as /admin (users alphabetically, sites under
// each). Only sites with public=true appear; users with no public sites
// are hidden entirely.
type ShowcaseHandler struct {
	database *sql.DB
	// hosts decides which address each gallery entry links to.
	hosts       HostModel
	signingKeys []auth.SigningKey
	sessionIdle time.Duration
	// sessionUser resolves the caller's base-host session, or nil if there
	// is none or it is not valid. A field, defaulting to the real check
	// below, so a test can substitute a fake signed-in caller without
	// faking the sessions/users join through the database/sql/driver fake
	// every other showcase test already drives.
	sessionUser func(*http.Request) *db.User
}

func NewShowcaseHandler(database *sql.DB, hosts HostModel, signingKeys []auth.SigningKey, sessionIdle time.Duration) *ShowcaseHandler {
	h := &ShowcaseHandler{database: database, hosts: hosts, signingKeys: signingKeys, sessionIdle: sessionIdle}
	h.sessionUser = h.realSessionUser
	return h
}

func (h *ShowcaseHandler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /showcase", h.page)
}

// realSessionUser resolves the base-host session cookie — the same check
// DashboardHandler.optionalUser makes, duplicated rather than shared because
// the two handlers have no other coupling and this is three lines.
func (h *ShowcaseHandler) realSessionUser(r *http.Request) *db.User {
	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	verified, err := auth.VerifyBaseSessionCookie(h.signingKeys, c.Value)
	if err != nil {
		return nil
	}
	withUser, err := db.GetValidSession(r.Context(), h.database, verified.SessionID, h.sessionIdle)
	if err != nil || withUser.Session.UserID != verified.UserID {
		return nil
	}
	user := withUser.User
	return &user
}

func (h *ShowcaseHandler) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	// The showcase lives on the base host behind the base
	// session — there is no more anonymous viewing of anything this server
	// serves, the showcase included.
	if h.sessionUser(r) == nil {
		http.Redirect(w, r, "/auth/login?to="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	hosts := h.hosts

	// Cache-Control: no-store is set once, for every authenticated base-host
	// response, in SecurityHeaders — this used to be set here only when a
	// query was present, missing the
	// plain page view.
	queryValues := r.URL.Query()["q"]
	query := ""
	if len(queryValues) > 0 {
		query = queryValues[0]
	}

	users, err := db.ListAllUsers(r.Context(), h.database)
	if err != nil {
		http.Error(w, "failed to load users", http.StatusInternalServerError)
		return
	}
	sites, err := db.ListAllSites(r.Context(), h.database)
	if err != nil {
		http.Error(w, "failed to load sites", http.StatusInternalServerError)
		return
	}
	// A restricted site is not public, whatever its own
	// public flag says. Filtered here, alongside the public-flag check
	// below, rather than in ListAllSites, which several other unrelated
	// callers (the admin dashboard, rankings) also use unfiltered.
	restrictedSiteIDs, err := db.ListRestrictedSiteIDs(r.Context(), h.database)
	if err != nil {
		log.Printf("showcase restricted sites: %v", err)
		restrictedSiteIDs = map[string]bool{}
	}
	analyticsBySiteID, err := db.ListSiteAnalyticsSummaries(r.Context(), h.database, 7)
	if err != nil {
		log.Printf("showcase analytics: %v", err)
		analyticsBySiteID = make(map[string]db.SiteAnalyticsSummary)
	}
	// Types are optional decoration. A failure here costs the chip row, not the
	// page, so it degrades to the list exactly as it was before this feature.
	typeBySiteID, err := db.ListPublicSiteTypes(r.Context(), h.database)
	typesAvailable := err == nil
	if err != nil {
		log.Printf("showcase site types: %v", err)
		typeBySiteID = map[string]string{}
	}

	// One flat list of sites. The showcase is browsed for interesting sites, not
	// for a directory of people, so the owner is a column and one of the sort
	// options rather than the structure of the page.
	usernameByID := make(map[string]string, len(users))
	for _, u := range users {
		usernameByID[u.ID] = u.Username
	}

	entries := make([]showcaseEntry, 0, len(sites))
	for _, s := range sites {
		if !s.Public || restrictedSiteIDs[s.ID] {
			continue
		}
		owner, known := usernameByID[s.UserID]
		if !known {
			continue
		}
		summary := analyticsBySiteID[s.ID]
		entries = append(entries, showcaseEntry{
			site:     s,
			owner:    owner,
			views7d:  summary.Last7Pageviews,
			views:    summary.TodayPageviews,
			visits:   summary.TodayVisits,
			siteType: sitetype.Parse(typeBySiteID[s.ID]),
		})
	}

	// Counted over every public site, never the narrowed set: the chips show
	// these while a type is selected, and a count that shrank to match the
	// current view would be wrong.
	typeCounts := map[sitetype.Type]int{}
	unsorted := 0
	for _, e := range entries {
		if e.siteType == "" {
			unsorted++
			continue
		}
		typeCounts[e.siteType]++
	}

	// With no type data — the transient state of a binary running ahead of its
	// migration — a ?type= link narrows to nothing. Ignore the filter instead
	// of showing an empty page for a link that used to work.
	activeType := sitetype.Parse(r.URL.Query().Get("type"))
	unsortedSelected := r.URL.Query().Get("type") == unsortedTypeKey
	if !typesAvailable {
		activeType, unsortedSelected = "", false
	}
	selectedTypeKey := ""
	if unsortedSelected {
		selectedTypeKey = unsortedTypeKey
	} else if activeType != "" {
		selectedTypeKey = string(activeType)
	}
	if activeType != "" || unsortedSelected {
		kept := entries[:0]
		for _, e := range entries {
			if (unsortedSelected && e.siteType == "") || (activeType != "" && e.siteType == activeType) {
				kept = append(kept, e)
			}
		}
		entries = kept
	}

	// Sorted on the server so the order is right without JavaScript; the select
	// re-sorts in place after that.
	activeSort := parseShowcaseSort(r.URL.Query().Get("sort"))
	rankBySiteID := showcaseRanks(entries)
	sortShowcaseEntries(entries, activeSort)

	var b strings.Builder
	b.WriteString(showcaseHeadHTML)
	b.WriteString(`<a class="site-skip-link" href="#main-content">Skip to main content</a>
<header class="site-header">
  <div class="site-header__inner">
    <a class="site-header__wordmark" href="/">Simple Host</a>
    <nav class="site-header__nav" aria-label="Primary navigation">
      <a href="/showcase" aria-current="page">Showcase</a>
      <a href="/capabilities.html">Capabilities</a>
      <a href="/changelog.html">What's New</a>
      <a href="/install.html">Install</a>
      <a href="/docs.html">API</a>
    </nav>
  </div>
</header>
<main id="main-content" tabindex="-1">`)
	b.WriteString(`<section class="filter-panel" aria-labelledby="showcase-filter-heading">
  <div class="filter-copy">
    <h1 id="showcase-filter-heading">Filter sites</h1>
    <p>Narrow the sites already listed by owner or site name.</p>
  </div>
	<form class="filter-form" action="/showcase" method="get">
    <label for="showcase-filter">Filter sites</label>
    <div class="filter-controls">
      <input id="showcase-filter" type="search" name="q" maxlength="200" value="`)
	b.WriteString(html.EscapeString(query))
	b.WriteString(`" placeholder="Owner or site name" autocomplete="off">
      <button type="submit">Filter</button>
      <button id="showcase-filter-clear" class="filter-clear" type="button" hidden>Clear</button>
    </div>
    <input type="hidden" name="sort" value="`)
	b.WriteString(html.EscapeString(string(activeSort)))
	b.WriteString(`"><input type="hidden" name="type" value="`)
	b.WriteString(html.EscapeString(selectedTypeKey))
	b.WriteString(`">
  </form>
</section>`)

	renderTypeChips(&b, typeCounts, unsorted, selectedTypeKey, query, activeSort)

	b.WriteString(`<div class="list-toolbar"><div class="list-count" id="showcase-count">`)
	fmt.Fprintf(&b, `%s`, pluralize(len(entries), "1 site", fmt.Sprintf("%d sites", len(entries))))
	b.WriteString(`</div><p class="sr-only" id="showcase-announcer" role="status" aria-live="polite"></p>`)
	if len(entries) > 0 {
		// Its own form, and a submit button, so the control still works with no
		// JavaScript — a select on its own submits nothing. The script hides the
		// button and sorts in place instead. The current filter rides along so
		// submitting here does not discard it.
		b.WriteString(`<form class="sort-form" action="/showcase" method="get"><input type="hidden" name="q" value="`)
		b.WriteString(html.EscapeString(query))
		b.WriteString(`"><input type="hidden" name="type" value="`)
		b.WriteString(html.EscapeString(selectedTypeKey))
		b.WriteString(`"><label class="sort-label" for="showcase-sort">Sort</label><select id="showcase-sort" name="sort">`)
		for _, option := range showcaseSorts {
			selected := ""
			if option.key == activeSort {
				selected = ` selected`
			}
			fmt.Fprintf(&b, `<option value="%s"%s>%s</option>`,
				html.EscapeString(string(option.key)), selected, html.EscapeString(option.label))
		}
		b.WriteString(`</select><button type="submit" class="sort-go" id="showcase-sort-go">Sort</button></form>`)
	}
	b.WriteString(`</div>`)

	if len(entries) == 0 {
		b.WriteString(`<div class="empty">No public sites yet. Sites are unlisted by default — ask their owners to opt into the showcase via the simple-host skill.</div>`)
	} else {
		b.WriteString(`<div class="sites" id="showcase-sites">`)

		for _, e := range entries {
			publicPath := hosts.SiteURL(e.owner, e.site.Name, false)
			ranks := rankBySiteID[e.site.ID]
			fmt.Fprintf(&b, `<div class="site" data-filter-text="%s"%s>
  <div class="site-id">
    <div class="site-name"><a href="%s" target="_blank" rel="noopener">%s</a></div>
    <div class="site-owner"><a href="%s" target="_blank" rel="noopener">%s</a></div>
  </div>
  <div class="site-version"><span class="chip">v%d</span></div>
  <div class="site-traffic"><b>%s</b> views / <b>%s</b> visits today <span>%s views 7d</span></div>
</div>`,
				html.EscapeString(e.owner+" "+e.site.Name),
				ranks,
				html.EscapeString(publicPath),
				html.EscapeString(e.site.Name),
				html.EscapeString(hosts.OwnerPageURL(e.owner)),
				html.EscapeString(e.owner),
				e.site.ActiveVersion,
				formatCount(e.views),
				formatCount(e.visits),
				formatCount(e.views7d),
			)
		}
		b.WriteString(`</div>`)
	}

	b.WriteString(showcaseFilterScript)
	b.WriteString(`</main></body></html>`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

// showcaseSortKey names one ordering of the showcase list.
type showcaseSortKey string

const (
	showcaseSortViews   showcaseSortKey = "views"
	showcaseSortUpdated showcaseSortKey = "updated"
	showcaseSortNewest  showcaseSortKey = "newest"
	showcaseSortOwner   showcaseSortKey = "owner"
	showcaseSortName    showcaseSortKey = "name"
)

// showcaseSorts is the offered order, first entry being the default. Views
// leads because the page exists to surface what people are actually reading.
var showcaseSorts = []struct {
	key   showcaseSortKey
	label string
}{
	{showcaseSortViews, "Most viewed (7d)"},
	{showcaseSortUpdated, "Recently updated"},
	{showcaseSortNewest, "Newest"},
	{showcaseSortOwner, "Owner A–Z"},
	{showcaseSortName, "Site name A–Z"},
}

// parseShowcaseSort maps the query parameter onto a known ordering, falling
// back to the default rather than rejecting an unknown value: this is a public
// page reached from links that may outlive a sort key.
func parseShowcaseSort(raw string) showcaseSortKey {
	for _, option := range showcaseSorts {
		if raw == string(option.key) {
			return option.key
		}
	}
	return showcaseSorts[0].key
}

type showcaseEntry struct {
	site     db.Site
	owner    string
	views7d  int64
	views    int64
	visits   int64
	siteType sitetype.Type
}

// unsortedTypeKey selects sites with no classification. It is not a member of
// the closed set — an unclassified site is the absence of a judgement, not a
// kind of site — so it travels as its own query value.
const unsortedTypeKey = "unsorted"

// showcaseRanks precomputes each site's position under every ordering and
// returns them as ready-to-emit attributes.
//
// The rows carry a rank rather than the values they were sorted on. That is
// deliberate: sites.updated_at is bumped by writes to a site's public state
// backend, and those writes are unauthenticated, so a second-precision
// timestamp on an unauthenticated page would tell anyone when the last comment
// or form submission on any listed site arrived. A rank exposes only the
// relative order, which is what choosing "Recently updated" reveals anyway.
//
// It also makes the two sorts agree by construction. The browser no longer
// compares anything — it reorders by an integer the server assigned — so
// Go's byte-wise comparison and JavaScript's localeCompare can no longer
// disagree on names outside ASCII.
func showcaseRanks(entries []showcaseEntry) map[string]string {
	ranked := make(map[string][]string, len(entries))
	ordered := make([]showcaseEntry, len(entries))

	for _, option := range showcaseSorts {
		copy(ordered, entries)
		sortShowcaseEntries(ordered, option.key)
		for position, e := range ordered {
			ranked[e.site.ID] = append(ranked[e.site.ID],
				fmt.Sprintf(` data-rank-%s="%d"`, option.key, position))
		}
	}

	attributes := make(map[string]string, len(ranked))
	for siteID, parts := range ranked {
		attributes[siteID] = strings.Join(parts, "")
	}
	return attributes
}

// sortShowcaseEntries orders the list in place. Every ordering ends with the
// same owner/name tiebreak so equal values — which is most of them, since most
// sites have no traffic — do not shuffle between requests.
func sortShowcaseEntries(entries []showcaseEntry, key showcaseSortKey) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		switch key {
		case showcaseSortUpdated:
			if !a.site.UpdatedAt.Equal(b.site.UpdatedAt) {
				return a.site.UpdatedAt.After(b.site.UpdatedAt)
			}
		case showcaseSortNewest:
			if !a.site.CreatedAt.Equal(b.site.CreatedAt) {
				return a.site.CreatedAt.After(b.site.CreatedAt)
			}
		case showcaseSortName:
			if nameA, nameB := strings.ToLower(a.site.Name), strings.ToLower(b.site.Name); nameA != nameB {
				return nameA < nameB
			}
		case showcaseSortOwner:
			// falls through to the shared tiebreak, which is owner then name
		default:
			if a.views7d != b.views7d {
				return a.views7d > b.views7d
			}
		}
		// Compared on the same lowered values that decide the result: EqualFold
		// and ToLower do not agree on every input (İ folds to i under one and
		// not the other), and a disagreement leaves a pair with no ordering at
		// all, so the tiebreak below would be skipped and sort.Slice would
		// order them arbitrarily.
		ownerA, ownerB := strings.ToLower(a.owner), strings.ToLower(b.owner)
		if ownerA != ownerB {
			return ownerA < ownerB
		}
		nameA, nameB := strings.ToLower(a.site.Name), strings.ToLower(b.site.Name)
		if nameA != nameB {
			return nameA < nameB
		}
		// Two sites can share an owner and a case-insensitive name only across
		// different owners' trees; fall back to the id so the order is stable.
		return a.site.ID < b.site.ID
	})
}

const showcaseHeadHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Simple Host · Showcase</title>
<link rel="stylesheet" href="/ps-blue.css">
<link rel="stylesheet" href="/site-header.css">
<style>
  body { min-height: 100vh; }
  main{
    padding:32px 56px 64px;
    display:grid;gap:36px;
    max-width:960px;margin:0 auto;
  }
  .list-toolbar{
    display:flex;align-items:center;gap:12px;
    padding-bottom:12px;
  }
  .list-count{
    font-family:var(--font-mono);font-weight:500;font-size:12px;
    color:var(--ink-muted);letter-spacing:0.08em;text-transform:uppercase;
    margin-right:auto;min-height:20px;
    /* The message that replaces the tally is longer. Without a floor on the
       basis the flex row rewraps as the user types and moves the select. */
    flex:1 1 12ch;min-width:0;
  }
  /* The same element also carries prose ("No sites match this filter."), which
     uppercase letterspaced mono is the wrong treatment for. */
  .list-count.is-message{
    font-family:var(--font-sans);font-size:14px;letter-spacing:normal;
    text-transform:none;color:var(--ink-soft);
  }
  .sort-form{display:flex;align-items:center;gap:10px}
  .type-chips{
    display:flex;flex-wrap:wrap;gap:8px;
    padding:20px 0 4px;
  }
  .type-chip{
    display:inline-flex;align-items:baseline;gap:7px;
    padding:6px 13px;border:1px solid var(--surface-line);
    border-radius:999px;background:var(--surface);
    color:var(--ink);text-decoration:none;
    font:500 13px/1.2 var(--font-sans);white-space:nowrap;
  }
  .type-chip:hover{border-color:var(--ps-blue-700);color:var(--ps-blue-800)}
  .type-chip:focus-visible{outline:3px solid var(--ps-blue-300);outline-offset:2px}
  .type-chip.is-active{
    background:var(--ps-blue-800);border-color:var(--ps-blue-800);color:#fff;
  }
  .type-chip-count{
    font-family:var(--font-mono);font-size:11px;font-variant-numeric:tabular-nums;
    color:var(--ink-muted);
  }
  .type-chip.is-active .type-chip-count{color:rgba(255,255,255,.75)}
  /* Announcements go to their own hidden node so the visible tally can update
     on every keystroke without a screen reader narrating each letter. */
  .sr-only{
    position:absolute;width:1px;height:1px;margin:-1px;padding:0;
    overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap;border:0;
  }
  .sort-label{font-size:14px;font-weight:600;color:var(--ink)}
  .sort-go{
    height:38px;padding:6px 14px;border:1px solid var(--ps-blue-800);
    border-radius:var(--radius-sm);background:var(--ps-blue-800);color:#fff;
    font-family:var(--font-mono);font-size:12px;font-weight:600;
    letter-spacing:0.08em;text-transform:uppercase;cursor:pointer;
  }
  .sort-go[hidden]{display:none}
  .list-toolbar select{
    height:38px;padding:6px 10px;
    border:1px solid var(--surface-line);border-radius:var(--radius-sm);
    background:var(--surface);color:var(--ink);
    font:500 14px/1.4 var(--font-sans);cursor:pointer;
  }
  .list-toolbar select:focus{outline:3px solid var(--ps-blue-300);outline-offset:2px}
  .sites{display:grid;gap:0}
  .site{
    display:grid;grid-template-columns:minmax(0,1fr) auto auto;align-items:baseline;
    gap:24px;padding:14px 0;
    border-top:1px solid var(--surface-line);
    transition:background-color 150ms ease, box-shadow 150ms ease;
  }
  .site:first-child{border-top:none}
  .site[hidden]{display:none}
  .site-id{min-width:0}
  .site-owner{
    font-family:var(--font-mono);font-weight:500;font-size:12px;
    color:var(--ink-muted);letter-spacing:0.04em;margin-top:2px;
    overflow:hidden;text-overflow:ellipsis;white-space:nowrap;
  }
  /* The owner's name links to their own page. It stays the muted caption it
     has always been and only reads as a link on hover, so crediting the
     author does not compete with the site name above it. */
  .site-owner a{color:inherit;text-decoration:none}
  .site-owner a:hover{color:var(--ps-blue-800);text-decoration:underline}
  .site-owner a:focus-visible{outline:3px solid var(--ps-blue-300);outline-offset:2px;border-radius:2px}
  .site:hover{background:var(--ps-blue-100);box-shadow:var(--shadow-xs);border-radius:var(--radius-md)}
  .site-name{font-family:var(--font-sans);font-size:18px;font-weight:600;letter-spacing:-0.005em;
    min-width:0}
  .site-name a{display:block;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
  .site-name a{color:var(--ink);text-decoration:none}
  .site-name a:hover{color:var(--ps-blue-800)}
  .site-version{font-variant-numeric:tabular-nums}
  .site-version .chip{font-size:12px;letter-spacing:0.14em;padding:3px 10px}
  .site-traffic{
    font-family:var(--font-mono);font-weight:500;font-size:12px;
    color:var(--ink-muted);letter-spacing:0.04em;font-variant-numeric:tabular-nums;
    white-space:nowrap;
  }
  .site-traffic b{color:var(--ink);font-weight:600}
  .site-traffic span{margin-left:8px;color:var(--ink-soft)}
  .empty{
    text-align:center;padding:80px 20px;
    font-family:var(--font-sans);font-size:18px;font-weight:500;
    color:var(--ink-muted);max-width:560px;margin:0 auto;line-height:1.55;
  }
  .filter-panel{
    display:grid;grid-template-columns:minmax(0, 1fr) minmax(280px, 430px);
    align-items:end;gap:32px;padding-bottom:28px;
    border-bottom:1px solid var(--surface-line);
  }
  .filter-copy h1{
    margin:0 0 5px;font-size:22px;line-height:1.15;letter-spacing:-0.02em;
  }
  .filter-copy p{margin:0;color:var(--ink-soft);font-size:14px}
  .filter-form{display:grid;gap:7px}
  .filter-form label{font-size:14px;font-weight:600;color:var(--ink)}
  .filter-controls{display:flex;align-items:stretch;gap:8px}
  .filter-form input{
    min-width:0;width:100%;height:42px;padding:8px 12px;
    border:1px solid var(--surface-line);border-radius:var(--radius-sm);
    background:var(--surface);color:var(--ink);font:500 15px/1.4 var(--font-sans);
  }
  .filter-form input::placeholder{color:var(--ink-muted)}
  .filter-form button{
    min-height:42px;padding:8px 17px;border:1px solid var(--ps-blue-800);
    border-radius:var(--radius-sm);background:var(--ps-blue-800);color:#fff;
    font-family:var(--font-mono);font-size:12px;font-weight:600;
    letter-spacing:0.08em;text-transform:uppercase;cursor:pointer;
    transition:background-color 150ms ease,border-color 150ms ease;
  }
  .filter-form button:hover{background:var(--ps-blue-700);border-color:var(--ps-blue-700)}
  .filter-form .filter-clear{background:var(--surface);color:var(--ps-blue-800)}
  .filter-form .filter-clear:hover{background:var(--ps-blue-100);border-color:var(--ps-blue-700)}
  .filter-form input:focus,
  .filter-form button:focus{
    outline:3px solid var(--ps-blue-300);outline-offset:2px;
  }
  @media (max-width: 780px) {
    main{padding:28px 20px 48px}
    .filter-panel{grid-template-columns:1fr;align-items:start;gap:16px}
    .site{grid-template-columns:1fr;gap:6px;padding:16px 0}
    .list-toolbar{flex-wrap:wrap;gap:8px}
    .site-traffic{white-space:normal}
    .site-traffic span{white-space:nowrap}
  }
  @media (max-width: 440px) {
    .filter-controls{display:grid;grid-template-columns:1fr}
    .filter-form button{width:100%}
  }
  @media (prefers-reduced-motion: reduce) {
    .site,.filter-form button{transition:none}
  }
</style>
</head>
<body>
`

const showcaseFilterScript = `<script>
(function () {
  "use strict";

  var form = document.querySelector(".filter-form");
  var input = document.getElementById("showcase-filter");
  var clear = document.getElementById("showcase-filter-clear");
  var list = document.getElementById("showcase-sites");
  var sortSel = document.getElementById("showcase-sort");
  var count = document.getElementById("showcase-count");
  var sortGo = document.getElementById("showcase-sort-go");
  var announcer = document.getElementById("showcase-announcer");
  var sortLabel = "";
  var announceTimer = null;
  var announced = "";
  var sites = Array.prototype.slice.call(document.querySelectorAll(".site[data-filter-text]"));

  function num(el, key) {
    var parsed = parseInt(el.getAttribute(key), 10);
    return isNaN(parsed) ? 0 : parsed;
  }

  // Every ordering was decided by the server and emitted as a rank, so the
  // browser only has to reorder by an integer. Nothing here can disagree with
  // the order the page arrived in, and no sort key needs a tiebreak.
  var SORTS = ["views", "updated", "newest", "owner", "name"];

  function rankAttr(mode) {
    return "data-rank-" + (SORTS.indexOf(mode) === -1 ? SORTS[0] : mode);
  }

  function applySort(announce) {
    if (!list || !sortSel) { return; }
    var chosen = sortSel.options[sortSel.selectedIndex];
    sortLabel = chosen ? chosen.text : "";
    var key = rankAttr(sortSel.value);
    sites.slice().sort(function (a, b) {
      return num(a, key) - num(b, key);
    }).forEach(function (site) {
      list.appendChild(site);
    });
    // Keep the address bar shareable without reloading the page. Guarded on
    // window itself so the script can be exercised outside a browser.
    if (typeof window !== "undefined" && window.history && window.history.replaceState) {
      var url = new URL(window.location.href);
      url.searchParams.set("sort", sortSel.value);
      window.history.replaceState({}, "", url);
    }
    // Reordering the list is silent to a screen reader, so the live region
    // beside the control says what the order now is.
    if (announce) { render(); }
  }

  function applyFilter() {
    var filterText = input.value.trim().toLowerCase();
    sites.forEach(function (site) {
      site.hidden = site.dataset.filterText.toLowerCase().indexOf(filterText) === -1;
    });

    clear.hidden = input.value.length === 0;
    render();
  }

  // One live region for the tally, the empty case and the current order: it
  // sits with the sort control, and a second copy elsewhere said the same thing
  // twice to sighted users and screen readers alike.
  function render() {
    if (!count) { return; }
    var shown = 0;
    sites.forEach(function (site) { if (!site.hidden) { shown += 1; } });

    var message = "";
    if (shown === 0 && input.value.trim() !== "") {
      message = "No sites match this filter.";
    } else if (shown === sites.length) {
      message = sites.length === 1 ? "1 site" : sites.length + " sites";
    } else {
      message = shown + " of " + sites.length + " sites";
    }
    if (sortLabel && shown > 0) {
      message += ", sorted by " + sortLabel.toLowerCase();
    }
    count.textContent = message;
    if (count.classList) {
      count.classList.toggle("is-message", shown === 0 && input.value.trim() !== "");
    }
    if (!announcer || message === announced) { return; }
    announced = message;
    // Announcing on every keystroke makes a screen reader narrate each letter
    // typed. Let the typing settle, then say the result once.
    if (announceTimer) { clearTimeout(announceTimer); }
    announceTimer = setTimeout(function () {
      announcer.textContent = message;
      announceTimer = null;
    }, 250);
  }

  input.addEventListener("input", applyFilter);
  form.addEventListener("submit", function (event) {
    event.preventDefault();
    applyFilter();
  });
  clear.addEventListener("click", function () {
    input.value = "";
    applyFilter();
    input.focus();
  });
  if (sortSel) {
    sortSel.addEventListener("change", function () { applySort(true); });
  }
  // The submit button exists only for the no-JavaScript path.
  if (sortGo) { sortGo.hidden = true; }

  applySort(false);
  applyFilter();
}());
</script>`

// showcaseTypeHref builds a chip link that keeps the filter text and sort the
// visitor already chose. Chips are links rather than client-side toggles so
// they work without JavaScript and so the counts beside them, which are
// computed server-side over every public site, cannot drift out of step with
// what is listed.
func showcaseTypeHref(typeKey, query string, sort showcaseSortKey) string {
	values := url.Values{}
	if query != "" {
		values.Set("q", query)
	}
	values.Set("sort", string(sort))
	if typeKey != "" {
		values.Set("type", typeKey)
	}
	return "/showcase?" + values.Encode()
}

// renderTypeChips draws the type filter row.
//
// It renders nothing at all when no site carries a type. That is the state
// between deploying this and running the backfill, and a row of chips all
// reading zero would look broken; the page is simply what it was before.
func renderTypeChips(
	b *strings.Builder,
	counts map[sitetype.Type]int,
	unsorted int,
	selected, query string,
	sort showcaseSortKey,
) {
	classified := 0
	for _, n := range counts {
		classified += n
	}
	if classified == 0 {
		return
	}

	chip := func(key, label string, n int) {
		if n == 0 {
			return
		}
		class := "type-chip"
		aria := ""
		if key == selected {
			class += " is-active"
			aria = ` aria-current="true"`
		}
		fmt.Fprintf(b, `<a class="%s" href="%s"%s>%s<span class="type-chip-count">%d</span></a>`,
			class, html.EscapeString(showcaseTypeHref(key, query, sort)), aria,
			html.EscapeString(label), n)
	}

	b.WriteString(`<nav class="type-chips" aria-label="Filter by type">`)
	chip("", "All", classified+unsorted)
	for _, known := range sitetype.All {
		chip(string(known.Type), known.Label, counts[known.Type])
	}
	chip(unsortedTypeKey, "Unsorted", unsorted)
	b.WriteString(`</nav>`)
}
