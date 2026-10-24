package handler

import (
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/db"
)

func TestSiteListFilters(t *testing.T) {
	dates := []time.Time{}
	for i := 20; i <= 22; i++ {
		dates = append(dates, time.Date(2026, 10, i, 23, 59, 59, 999999000, time.UTC))
	}
	sites := []siteResponse{
		{Name: "homes", URL: "https://homes.alice.example.test", Summary: db.SiteSummary{Title: "Real estate", Description: "Coastal homes", Headings: "For sale", Text: "Sunny apartments"}, CreatedAt: dates[0], UpdatedAt: dates[0], DeployedAt: &dates[0]},
		{Name: "survey", Summary: db.SiteSummary{Title: "Tuesday survey", Headings: "Real feedback"}, CreatedAt: dates[1], UpdatedAt: dates[1], DeployedAt: &dates[1]},
		{Name: "empty", CreatedAt: dates[2], UpdatedAt: dates[2]},
	}
	for _, tc := range []struct {
		query string
		want  []string
		code  string
	}{
		{"", []string{"homes", "survey", "empty"}, ""},
		{"ignored=1", []string{"homes", "survey", "empty"}, ""},
		{"query=", []string{"empty", "survey", "homes"}, ""},
		{"query=REAL+coastal", []string{"homes"}, ""},
		{"query=real+tuesday+apartments", []string{}, ""},
		{"query=alice.example.test+sale", []string{"homes"}, ""},
		{"query=apart", []string{"homes"}, ""},
		{"created_after=2026-10-21&created_before=2026-10-21", []string{"survey"}, ""},
		{"created_after=2026-10-21T23:59:59.999999Z&created_before=2026-10-21T23:59:59.999999Z", []string{"survey"}, ""},
		{"updated_after=2026-10-22T01:00:00%2B02:00&updated_before=2026-10-22T02:00:00%2B02:00", []string{"survey"}, ""},
		{"query=real&limit=1&offset=1", []string{"homes"}, ""},
		{"offset=1", []string{"survey", "homes"}, ""},
		{"limit=0", []string{}, ""},
		{"offset=999", []string{}, ""},
		{"limit=-1", nil, "bad_pagination"},
		{"offset=1.5", nil, "bad_pagination"},
		{"limit=word", nil, "bad_pagination"},
		{"created_after=Tuesday", nil, "bad_date"},
		{"updated_before=", nil, "bad_date"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			params, _ := url.ParseQuery(tc.query)
			got, err := filterSiteList(sites, params)
			if tc.code != "" {
				if err == nil || err.Code != tc.code {
					t.Fatalf("error: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			names := []string{}
			for _, s := range got {
				names = append(names, s.Name)
			}
			if !reflect.DeepEqual(names, tc.want) {
				t.Fatalf("%v want %v", names, tc.want)
			}
		})
	}
	many := make([]siteResponse, 600)
	for _, tc := range []struct {
		query string
		count int
	}{{"query=", 200}, {"limit=1000", 500}, {"offset=599", 1}} {
		params, _ := url.ParseQuery(tc.query)
		got, err := filterSiteList(many, params)
		if err != nil || len(got) != tc.count {
			t.Fatalf("%s count %d error %v", tc.query, len(got), err)
		}
	}
}
