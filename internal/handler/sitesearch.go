package handler

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/db"
)

// filterSiteList always sorts (default: newest edited first), and additionally
// filters and pages the result when a query, date bound, limit or offset
// parameter is present; the no-argument size is otherwise untouched.
func filterSiteList[T siteListItem](sites []T, params url.Values) ([]T, *errorResponse) {
	sorted, orderErr := orderSites(sites, params.Get("order"))
	if orderErr != nil {
		return nil, orderErr
	}

	active := false
	for _, key := range []string{"query", "created_after", "created_before", "updated_after", "updated_before", "limit", "offset"} {
		active = active || params.Has(key)
	}
	if !active {
		return sorted, nil
	}
	type dateBound struct {
		key string
		at  time.Time
	}
	var bounds []dateBound
	for _, key := range []string{"created_after", "created_before", "updated_after", "updated_before"} {
		if !params.Has(key) {
			continue
		}
		at, err := time.Parse(time.RFC3339, params.Get(key))
		if err != nil {
			at, err = time.Parse("2006-01-02", params.Get(key))
			if err != nil {
				return nil, &errorResponse{Error: key + " must be RFC3339 or YYYY-MM-DD", Code: "bad_date"}
			}
			if strings.HasSuffix(key, "_before") {
				at = at.AddDate(0, 0, 1).Add(-time.Nanosecond)
			}
		}
		bounds = append(bounds, dateBound{key, at})
	}
	limit, offset := 200, 0
	for _, p := range []struct {
		key  string
		dest *int
	}{{"limit", &limit}, {"offset", &offset}} {
		if !params.Has(p.key) {
			continue
		}
		n, err := strconv.Atoi(params.Get(p.key))
		if err != nil || n < 0 {
			return nil, &errorResponse{Error: p.key + " must be a non-negative integer", Code: "bad_pagination"}
		}
		*p.dest = n
	}
	if limit > 500 {
		limit = 500
	}
	words := strings.Fields(strings.ToLower(params.Get("query")))
	matches := make([]T, 0, len(sorted))
	for _, item := range sorted {
		site, domain := item.searchFields()
		haystack := strings.ToLower(strings.Join([]string{site.Name, domain, site.Summary.Title, site.Summary.Description, site.Summary.Headings, site.Summary.Text}, " "))
		match := true
		for _, word := range words {
			if !strings.Contains(haystack, word) {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		for _, bound := range bounds {
			at := site.CreatedAt
			if strings.HasPrefix(bound.key, "updated_") {
				at = site.UpdatedAt
			}
			if strings.HasSuffix(bound.key, "_after") && at.Before(bound.at) || strings.HasSuffix(bound.key, "_before") && at.After(bound.at) {
				match = false
				break
			}
		}
		if match {
			matches = append(matches, item)
		}
	}
	// matches is already in `sorted`'s order (the loop above only drops
	// entries, never reorders), so no further sort is needed here.
	if offset >= len(matches) {
		return matches[:0], nil
	}
	matches = matches[offset:]
	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

// orderSites sorts a copy of sites by the requested key: "updated" (default,
// last edited first, ties broken by created descending), "created" (newest
// created first), or "name" (alphabetical). The input is never mutated.
func orderSites[T siteListItem](sites []T, order string) ([]T, *errorResponse) {
	sorted := make([]T, len(sites))
	copy(sorted, sites)
	key := func(item T) db.Site { s, _ := item.searchFields(); return s }
	switch order {
	case "", "updated":
		sort.SliceStable(sorted, func(i, j int) bool {
			a, b := key(sorted[i]), key(sorted[j])
			if !a.UpdatedAt.Equal(b.UpdatedAt) {
				return a.UpdatedAt.After(b.UpdatedAt)
			}
			return a.CreatedAt.After(b.CreatedAt)
		})
	case "created":
		sort.SliceStable(sorted, func(i, j int) bool {
			a, b := key(sorted[i]), key(sorted[j])
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.After(b.CreatedAt)
			}
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		})
	case "name":
		sort.SliceStable(sorted, func(i, j int) bool {
			a, b := key(sorted[i]), key(sorted[j])
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		})
	default:
		return nil, &errorResponse{Error: "order must be updated, created, or name", Code: "bad_order"}
	}
	return sorted, nil
}

// Filtering runs only after authorization and projection, including shared
// viewer entries. The domain is the actual installation's serving address.
type siteListItem interface{ searchFields() (db.Site, string) }

func deployedTime(site db.Site) *time.Time {
	if site.LastDeployedAt.IsZero() {
		return nil
	}
	return &site.LastDeployedAt
}
func searchFields(name, address string, summary db.SiteSummary, created, updated time.Time, deployed *time.Time) (db.Site, string) {
	site := db.Site{Name: name, Summary: summary, CreatedAt: created, UpdatedAt: updated}
	if deployed != nil {
		site.LastDeployedAt = *deployed
	}
	u, _ := url.Parse(address)
	domain := ""
	if u != nil {
		domain = u.Hostname()
	}
	return site, domain
}
func (s siteResponse) searchFields() (db.Site, string) {
	return searchFields(s.Name, s.URL, s.Summary, s.CreatedAt, s.UpdatedAt, s.DeployedAt)
}
func (s collaborationSiteResponse) searchFields() (db.Site, string) {
	return searchFields(s.Name, s.URL, s.Summary, s.CreatedAt, s.UpdatedAt, s.DeployedAt)
}
