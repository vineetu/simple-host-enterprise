package handler

import (
	"fmt"
	"html"
	"net/url"
	"sort"
	"strings"
	"time"
)

// rankTop is how many rows each ranking card shows.
const rankTop = 10

// rankMetric selects which column a ranking is ordered by. The zero value is
// not valid; use parseRankMetric, which falls back to views.
type rankMetric string

// There is deliberately no downloads metric here. Downloads are still recorded
// per file, but the number only means "somebody took this file" on a site that
// publishes one — an installer or a PDF. On an ordinary site the page fetching
// its own data trips the same rule, so the same column meant two different
// things depending on the site and could not be ranked against itself.
const (
	metricViews   rankMetric = "views"
	metricSize    rankMetric = "size"
	metricUpdated rankMetric = "updated"
)

var userMetrics = []struct {
	metric rankMetric
	label  string
}{
	{metricViews, "Views"},
	{metricSize, "Storage"},
}

var siteMetrics = []struct {
	metric rankMetric
	label  string
}{
	{metricViews, "Views"},
	{metricUpdated, "Updated"},
	{metricSize, "Storage"},
}

func parseRankMetric(raw string, allowed []struct {
	metric rankMetric
	label  string
}) rankMetric {
	for _, option := range allowed {
		if raw == string(option.metric) {
			return option.metric
		}
	}
	return metricViews
}

// userRank is one row of the per-user ranking. Every field is filled for every
// user so the card can be re-sorted by any metric without reloading.
type userRank struct {
	username   string
	siteCount  int
	views      int64
	totalBytes uint64
	liveBytes  uint64
}

// siteRank is one row of the per-site ranking.
type siteRank struct {
	name       string
	owner      string
	views      int64
	totalBytes uint64
	liveBytes  uint64
	updatedAt  time.Time
}

func sortUserRanks(rows []userRank, metric rankMetric) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch metric {
		case metricSize:
			if a.totalBytes != b.totalBytes {
				return a.totalBytes > b.totalBytes
			}
		default:
			if a.views != b.views {
				return a.views > b.views
			}
		}
		if a.siteCount != b.siteCount {
			return a.siteCount > b.siteCount
		}
		return a.username < b.username
	})
}

func sortSiteRanks(rows []siteRank, metric rankMetric) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch metric {
		case metricSize:
			if a.totalBytes != b.totalBytes {
				return a.totalBytes > b.totalBytes
			}
		case metricUpdated:
			if !a.updatedAt.Equal(b.updatedAt) {
				return a.updatedAt.After(b.updatedAt)
			}
		default:
			if a.views != b.views {
				return a.views > b.views
			}
		}
		if a.name != b.name {
			return a.name < b.name
		}
		return a.owner < b.owner
	})
}

// rankingURL builds a link to /admin that changes one ranking's metric while
// preserving the reporting range and the other card's metric.
func rankingURL(days int, usersMetric, sitesMetric rankMetric) string {
	query := url.Values{}
	query.Set("days", fmt.Sprintf("%d", days))
	query.Set("users", string(usersMetric))
	query.Set("sites", string(sitesMetric))
	return "/admin?" + query.Encode()
}

func renderRankTabs(b *strings.Builder, options []struct {
	metric rankMetric
	label  string
}, active rankMetric, href func(rankMetric) string) {
	b.WriteString(`<div class="rank-tabs" role="tablist">`)
	for _, option := range options {
		class := "rank-tab"
		aria := "false"
		if option.metric == active {
			class += " is-active"
			aria = "true"
		}
		fmt.Fprintf(b, `<a class="%s" role="tab" aria-selected="%s" href="%s">%s</a>`,
			class, aria, html.EscapeString(href(option.metric)), html.EscapeString(option.label))
	}
	b.WriteString(`</div>`)
}

// rankCell is one row split into the two things a ranking row needs: the single
// number it is ordered by, and the context that number needs to make sense.
//
// Keeping them apart is what fixes the layout. Packing "450.4 MiB · 124.8 MiB
// live · 12 sites" into one right-aligned cell left nothing able to shrink, so
// the text ran past the edge of the card.
type rankCell struct {
	value string // the ranked number, e.g. "450.4 MiB" or "4,636"
	unit  string // short qualifier shown beside it, e.g. "views"; may be empty
	sub   string // context under the name, e.g. "124.8 MiB live · 12 sites"
	// valueHTML replaces value when the ranked thing is not a number the server
	// can format on its own — a timestamp has to reach the browser as markup so
	// it can be shown in the reader's timezone.
	valueHTML string
}

// metricIsRanged reports whether a metric is bounded by the reporting range, in
// which case the card header says so once instead of every row repeating it.
func metricIsRanged(metric rankMetric) bool {
	return metric == metricViews
}

func userMetricCell(row userRank, metric rankMetric) rankCell {
	sites := pluralize(row.siteCount, "1 site", fmt.Sprintf("%d sites", row.siteCount))
	switch metric {
	case metricSize:
		return rankCell{
			value: formatBytes(row.totalBytes),
			sub:   fmt.Sprintf("%s live · %s", formatBytes(row.liveBytes), sites),
		}
	default:
		return rankCell{value: formatCount(row.views), unit: "views", sub: sites}
	}
}

func siteMetricCell(row siteRank, metric rankMetric) rankCell {
	switch metric {
	case metricSize:
		return rankCell{
			value: formatBytes(row.totalBytes),
			sub:   fmt.Sprintf("%s · %s live", row.owner, formatBytes(row.liveBytes)),
		}
	case metricUpdated:
		return rankCell{valueHTML: localTimeHTML(row.updatedAt, "datetime"), sub: row.owner}
	default:
		return rankCell{value: formatCount(row.views), unit: "views", sub: row.owner}
	}
}

// writeRankRow emits one ranked row: position, an identity column that
// ellipsizes rather than wrapping, and the value column.
func writeRankRow(b *strings.Builder, position int, href, name string, cell rankCell) {
	unit := ""
	if cell.unit != "" {
		unit = `<span class="rank-unit">` + html.EscapeString(cell.unit) + `</span>`
	}
	value := html.EscapeString(cell.value)
	if cell.valueHTML != "" {
		value = cell.valueHTML
	}
	fmt.Fprintf(b, `<div class="rank-row rank-row--ranked">
  <span class="rank-num">%d</span>
  <span class="rank-id">
    <a class="rank-name" href="%s" target="_blank" rel="noopener" title="%s">%s</a>
    <span class="rank-sub">%s</span>
  </span>
  <span class="rank-value">%s%s</span>
</div>`,
		position,
		html.EscapeString(href),
		html.EscapeString(name),
		html.EscapeString(name),
		html.EscapeString(cell.sub),
		value,
		unit,
	)
}

// writeRankCardHead writes the card title, the reporting range when the active
// metric is bounded by it, and the tab strip.
func writeRankCardHead(b *strings.Builder, title string, options []struct {
	metric rankMetric
	label  string
}, active rankMetric, days int, href func(rankMetric) string) {
	fmt.Fprintf(b, `<div class="overview-card"><h2 class="section-title">%s`, html.EscapeString(title))
	if metricIsRanged(active) {
		// Said once here rather than repeated on all ten rows.
		fmt.Fprintf(b, `<span class="card-count">%s</span>`, html.EscapeString(analyticsRangeCompact(days)))
	}
	b.WriteString(`</h2>`)
	renderRankTabs(b, options, active, href)
	b.WriteString(`<div class="rank-list">`)
}

// renderUserRankingCard writes the "Top users" card with its metric tabs.
func renderUserRankingCard(b *strings.Builder, hosts HostModel, rows []userRank, metric, sitesMetric rankMetric, days int) {
	writeRankCardHead(b, "Top users", userMetrics, metric, days, func(m rankMetric) string {
		return rankingURL(days, m, sitesMetric)
	})

	if len(rows) == 0 {
		b.WriteString(`<div class="rank-empty">No users yet.</div>`)
	}
	for i, row := range rows {
		if i >= rankTop {
			break
		}
		writeRankRow(b, i+1, hosts.OwnerPageURL(row.username), row.username, userMetricCell(row, metric))
	}
	b.WriteString(`</div></div>`)
}

// renderSiteRankingCard writes the "Top sites" card with its metric tabs.
// hosts decides the site link; the user card beside it links the management
// view, which stays on the base host whatever the cutover says.
func renderSiteRankingCard(b *strings.Builder, hosts HostModel, rows []siteRank, metric, usersMetric rankMetric, days int) {
	writeRankCardHead(b, "Top sites", siteMetrics, metric, days, func(m rankMetric) string {
		return rankingURL(days, usersMetric, m)
	})

	if len(rows) == 0 {
		b.WriteString(`<div class="rank-empty">No sites yet.</div>`)
	}
	for i, row := range rows {
		if i >= rankTop {
			break
		}
		writeRankRow(b, i+1, hosts.SiteURL(row.owner, row.name), row.name, siteMetricCell(row, metric))
	}
	b.WriteString(`</div></div>`)
}

// newUser is one row of the "New users" card: who joined, when, and how far
// they have got since. Ordered newest first, so position 1 is the newest
// account — somebody registering, or a team somebody created.
type newUser struct {
	username  string
	joined    time.Time
	siteCount int
	// disabled is a person an admin has offboarded. A team
	// is never disabled — it is marked as a team instead.
	disabled bool
	team     bool
}

// renderNewUsersCard writes the "New users" card. It has no metric tabs: there
// is only one order that answers "who just joined".
func renderNewUsersCard(b *strings.Builder, hosts HostModel, rows []newUser, total int) {
	fmt.Fprintf(b, `<div class="overview-card"><h2 class="section-title">New users<span class="card-count">%d total</span></h2><div class="rank-list">`, total)
	if len(rows) == 0 {
		b.WriteString(`<div class="rank-empty">No users yet.</div>`)
	}
	for i, row := range rows {
		if i >= rankTop {
			break
		}
		sub := pluralize(row.siteCount, "1 site", fmt.Sprintf("%d sites", row.siteCount))
		if row.team {
			sub += " · team"
		}
		if row.disabled {
			sub += " · disabled"
		}
		writeRankRow(b, i+1, hosts.OwnerPageURL(row.username), row.username, rankCell{
			valueHTML: localTimeHTML(row.joined, "date"),
			sub:       sub,
		})
	}
	b.WriteString(`</div></div>`)
}
