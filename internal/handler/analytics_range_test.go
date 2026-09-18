package handler

import (
	"net/http/httptest"
	"strings"
	"testing"

	dbstore "github.com/vsriram/simple-host/internal/db"
)

func TestAnalyticsDaysForRequestAllowlistAndPublicOverride(t *testing.T) {
	tests := []struct {
		name           string
		query          string
		allowSelection bool
		want           int
	}{
		{name: "default", want: 7, allowSelection: true},
		{name: "seven", query: "?days=7", want: 7, allowSelection: true},
		{name: "thirty", query: "?days=30", want: 30, allowSelection: true},
		{name: "six weeks", query: "?days=42", want: 42, allowSelection: true},
		{name: "six months", query: "?days=180", want: 180, allowSelection: true},
		{name: "unsupported", query: "?days=365", want: 7, allowSelection: true},
		{name: "malformed", query: "?days=30days", want: 7, allowSelection: true},
		{name: "public override ignored", query: "?days=180", want: 7, allowSelection: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://simple-host.test/sites/alice/"+test.query, nil)
			if got := analyticsDaysForRequest(r, test.allowSelection); got != test.want {
				t.Fatalf("analyticsDaysForRequest() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestUserAnalyticsRangeSelectorIsOwnerOnly(t *testing.T) {
	var public strings.Builder
	renderUserAnalyticsRangeSelector(&public, "alice", 180, false)
	if public.Len() != 0 {
		t.Fatalf("public listing rendered reporting-range controls: %s", public.String())
	}

	var owner strings.Builder
	renderUserAnalyticsRangeSelector(&owner, "alice", 180, true)
	html := owner.String()
	for _, want := range []string{
		`action="/sites/alice/"`,
		`name="days"`,
		`value="7"`,
		`value="30"`,
		`value="42"`,
		`value="180" selected`,
		`Analytics reporting range: 6 months`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("owner selector missing %q: %s", want, html)
		}
	}
}

func TestLongAnalyticsChartKeepsDailyDataWithoutClutter(t *testing.T) {
	series := normalizeAnalyticsSeries(nil, 180)
	for i := range series {
		series[i].Pageviews = int64(i + 1)
		series[i].Visits = int64(i % 11)
	}

	var chart strings.Builder
	renderTrendChart(&chart, series, "alice/site", "site-chart", 180)
	html := chart.String()
	if !strings.Contains(html, `alice/site 6-month traffic trend`) {
		t.Fatalf("chart title does not identify the selected range: %s", html)
	}
	if got := strings.Count(html, `class="x-label"`); got > maxAnalyticsChartXLabels {
		t.Fatalf("chart rendered %d x-axis labels, want at most %d", got, maxAnalyticsChartXLabels)
	}
	if strings.Contains(html, `<circle`) {
		t.Fatalf("180-day chart rendered per-day point markers")
	}

	maxValue := chartMax(180)
	wantPath := chartPath(series, maxValue, func(point dbstore.SiteAnalyticsDay) int64 { return point.Pageviews })
	if len(strings.Fields(wantPath)) != 2*len(series) {
		t.Fatalf("chart path does not contain every daily value")
	}
	if !strings.Contains(html, `d="`+wantPath+`"`) {
		t.Fatalf("rendered chart does not contain the full 180-day pageview path")
	}
}

func TestShortAnalyticsChartRetainsPointMarkers(t *testing.T) {
	series := normalizeAnalyticsSeries(nil, 7)
	var chart strings.Builder
	renderTrendChart(&chart, series, "alice/site", "site-chart", 7)
	if got := strings.Count(chart.String(), `<circle`); got != 2*len(series) {
		t.Fatalf("7-day chart rendered %d point markers, want %d", got, 2*len(series))
	}
}
