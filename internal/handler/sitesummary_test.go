package handler

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vsriram/simple-host/internal/db"
)

func TestExtractSiteSummary(t *testing.T) {
	for _, tt := range []struct {
		name, page string
		want       db.SiteSummary
	}{
		{"normal", `<html><head><title>  Real   estate &amp; homes </title><META NAME="DESCRIPTION" CONTENT=" Find  your home "></head><body><h1>Welcome <em>home</em></h1><h2> For sale </h2><h3>Today</h3><p>See <b>our</b> listings.</p></body></html>`, db.SiteSummary{Title: "Real estate & homes", Description: "Find your home", Headings: "Welcome home | For sale | Today", Text: "Welcome home For sale Today See our listings."}},
		{"no title", `<meta name="description" content="Survey"><body><h1>Questions</h1>`, db.SiteSummary{Description: "Survey", Headings: "Questions", Text: "Questions"}},
		{"no description", `<title>Survey</title><h1>Questions</h1>`, db.SiteSummary{Title: "Survey", Headings: "Questions", Text: "Questions"}},
		{"no headings", `<title>Survey</title><p>Answers</p>`, db.SiteSummary{Title: "Survey", Text: "Answers"}},
		{"hidden content", `<head><title>Page</title><style>head css</style></head><body>Before<script>secret code</script><style>body css</style><noscript>hidden fallback</noscript><!-- secret comment --><p>After</p></body>`, db.SiteSummary{Title: "Page", Text: "Before After"}},
		// <title> is a raw-text element (like <textarea>): "<script>" inside it
		// is literal title text, never a parsed child element, so only the
		// heading case below exercises textContent's script/style exclusion.
		{"hidden content in a heading", `<h1>Welcome<script>secret_js()</script><style>.secret_css{}</style><noscript>fallback</noscript></h1>`, db.SiteSummary{Headings: "Welcome", Text: "Welcome"}},
		{"first six", `<h1>1</h1><h2>2</h2><h3>3</h3><h4>not a heading match</h4><h1>4</h1><h2>5</h2><h3>6</h3><h1>7</h1>`, db.SiteSummary{Headings: "1 | 2 | 3 | 4 | 5 | 6", Text: "1 2 3 not a heading match 4 5 6 7"}},
		{"malformed", `<title>Broken</title><body><h1>Still here<p>Unclosed`, db.SiteSummary{Title: "Broken", Headings: "Still hereUnclosed", Text: "Still here Unclosed"}},
		{"unclosed title", `<title> Still here`, db.SiteSummary{Title: "Still here"}},
		{"first metadata", `<title>First</title><title>Second</title><meta name="description" content="One"><meta name="description" content="Two">`, db.SiteSummary{Title: "First", Description: "One"}},
		{"empty", ``, db.SiteSummary{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractSiteSummary(map[string][]byte{"index.html": []byte(tt.page)})
			if got != tt.want {
				t.Errorf("got %#v, want %#v", got, tt.want)
			}
		})
	}
	if got := ExtractSiteSummary(map[string][]byte{"other.html": []byte("<title>Ignore</title>")}); got != (db.SiteSummary{}) {
		t.Errorf("missing index: %#v", got)
	}
}

func TestExtractSiteSummaryByteCaps(t *testing.T) {
	for _, prefix := range []int{0, 1, 2} {
		t.Run(strings.Repeat("x", prefix)+"boundary", func(t *testing.T) {
			title := strings.Repeat("x", 200-prefix) + "界" + strings.Repeat("y", 400)
			other := strings.Repeat("x", 300-prefix) + "界" + strings.Repeat("y", 400)
			got := ExtractSiteSummary(map[string][]byte{"index.html": []byte("<title>" + title + "</title><meta name=description content='" + other + "'><body><h1>" + other + "</h1><p>" + other + "</p>")})
			for _, field := range []struct {
				name, value string
				cap         int
			}{{"title", got.Title, 200}, {"description", got.Description, 300}, {"headings", got.Headings, 300}, {"text", got.Text, 300}} {
				if !utf8.ValidString(field.value) || len(field.value) != field.cap-prefix {
					t.Errorf("%s: invalid cap or UTF-8: %q (%d)", field.name, field.value, len(field.value))
				}
			}
		})
	}
	got := ExtractSiteSummary(map[string][]byte{"index.html": []byte("<title>" + strings.Repeat("界", 100) + "</title><p>" + strings.Repeat("界", 150))})
	if len(got.Title) != 198 || len(got.Text) != 300 || !utf8.ValidString(got.Title+got.Text) {
		t.Fatalf("multi-byte content: %#v", got)
	}
}
