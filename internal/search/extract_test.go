package search

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestExtractFieldsNormalizationAndSkippedSubtrees(t *testing.T) {
	root, _ := newExtractionRoot(t, map[string]string{
		"index.html": `<!doctype html>
<html>
  <head>
    <title>  Demo&#x2003;Site  </title>
    <meta NAME="description" content=" First&#9;description&#10;here ">
    <meta name="description" content="second description">
    <style>.css-secret { display: block }</style>
  </head>
  <body>
    <!-- comment secret -->
    <h1>Visible <span>Heading</span></h1>
    <h2 hidden>hidden heading</h2>
    <p>Hello
       world <span hidden>hidden text</span>
       <span aria-hidden=" TRUE ">aria secret</span> visible.</p>
    <script>script secret</script>
    <style>style secret</style>
    <template>template secret</template>
  </body>
</html>`,
	})

	result, err := Extract(context.Background(), root, "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Documents) != 1 {
		t.Fatalf("documents = %d, want 1", len(result.Documents))
	}
	document := result.Documents[0]
	if document.PagePath != "index.html" || document.URLPath != "/sites/alice/demo/" {
		t.Fatalf("page identity = (%q, %q)", document.PagePath, document.URLPath)
	}
	if document.Title != "Demo Site" {
		t.Fatalf("title = %q", document.Title)
	}
	if document.Description != "First description here" {
		t.Fatalf("description = %q", document.Description)
	}
	if document.Headings != "Visible Heading" {
		t.Fatalf("headings = %q", document.Headings)
	}
	if document.BodyText != "Visible Heading Hello world visible." {
		t.Fatalf("body = %q", document.BodyText)
	}
	for _, forbidden := range []string{"second description", "secret", ".css-secret"} {
		if strings.Contains(document.Title+document.Description+document.Headings+document.BodyText, forbidden) {
			t.Errorf("extracted skipped content %q", forbidden)
		}
	}
	if result.Metadata.Partial {
		t.Fatalf("ordinary extraction was partial: %+v", result.Metadata)
	}
	wantOutput := int64(len(document.Title) + len(document.Description) + len(document.Headings) + len(document.BodyText))
	if result.Metadata.OutputBytes != wantOutput {
		t.Fatalf("output bytes = %d, want %d", result.Metadata.OutputBytes, wantOutput)
	}
}

func TestExtractDeterministicRegularHTMLWalkAndURLMapping(t *testing.T) {
	root, base := newExtractionRoot(t, map[string]string{
		"B.HTM":           `<body>B</body>`,
		"a.HTML":          `<body>A</body>`,
		"docs/INDEX.HTML": `<body>index</body>`,
		"docs/index.htm":  `<body>short index</body>`,
		"docs/page.htm":   `<head><base href="https://evil.example/"><link rel="canonical" href="/wrong"></head><body><a href="https://evil.example/page">page</a></body>`,
		"notes.txt":       `<body>not HTML</body>`,
	})
	if err := os.Symlink("a.HTML", filepath.Join(base, "alias.HTML")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "directory.html"), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := Extract(context.Background(), root, "alice smith", "demo#1")
	if err != nil {
		t.Fatal(err)
	}
	wantPaths := []string{"B.HTM", "a.HTML", "docs/INDEX.HTML", "docs/index.htm", "docs/page.htm"}
	wantURLs := []string{
		"/sites/alice%20smith/demo%231/B.HTM",
		"/sites/alice%20smith/demo%231/a.HTML",
		"/sites/alice%20smith/demo%231/docs/INDEX.HTML",
		"/sites/alice%20smith/demo%231/docs/index.htm",
		"/sites/alice%20smith/demo%231/docs/page.htm",
	}
	var gotPaths, gotURLs []string
	for _, document := range result.Documents {
		gotPaths = append(gotPaths, document.PagePath)
		gotURLs = append(gotURLs, document.URLPath)
	}
	if !reflect.DeepEqual(gotPaths, wantPaths) {
		t.Fatalf("page order = %#v, want %#v", gotPaths, wantPaths)
	}
	if !reflect.DeepEqual(gotURLs, wantURLs) {
		t.Fatalf("URLs = %#v, want %#v", gotURLs, wantURLs)
	}
}

func TestExtractToleratesMalformedHTML(t *testing.T) {
	t.Run("recoverable tree", func(t *testing.T) {
		root, _ := newExtractionRoot(t, map[string]string{
			"broken.html": `<title>Broken<title><body><h1>Still useful<p>body text`,
		})
		result, err := Extract(context.Background(), root, "alice", "demo")
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Documents) != 1 {
			t.Fatalf("documents = %d, want 1", len(result.Documents))
		}
		combined := result.Documents[0].Title + " " + result.Documents[0].Headings + " " + result.Documents[0].BodyText
		if !strings.Contains(combined, "Broken") {
			t.Fatalf("malformed page yielded no useful content: %+v", result.Documents[0])
		}
	})

	t.Run("parser rejection is partial", func(t *testing.T) {
		root, _ := newExtractionRoot(t, map[string]string{
			"too-deep.html": strings.Repeat("<div>", 600) + "content",
		})
		result, err := Extract(context.Background(), root, "alice", "demo")
		if err != nil {
			t.Fatalf("malformed page failed extraction: %v", err)
		}
		assertTruncation(t, result, TruncationMalformedHTML)
	})
}

func TestExtractPageCountLimit(t *testing.T) {
	root, _ := newExtractionRoot(t, map[string]string{
		"c.html": `<body>c</body>`,
		"a.html": `<body>a</body>`,
		"b.html": `<body>b</body>`,
	})
	configured := generousTestLimits()
	configured.pages = 2
	result, err := extractWithLimits(context.Background(), root, "alice", "demo", nil, configured)
	if err != nil {
		t.Fatal(err)
	}
	if got := documentPaths(result.Documents); !reflect.DeepEqual(got, []string{"a.html", "b.html"}) {
		t.Fatalf("limited paths = %#v", got)
	}
	assertTruncation(t, result, TruncationPageCount)
}

func TestExtractInputLimits(t *testing.T) {
	t.Run("page", func(t *testing.T) {
		root, _ := newExtractionRoot(t, map[string]string{
			"index.html": `<title>long title</title><body>long body</body>`,
		})
		configured := generousTestLimits()
		configured.pageInputBytes = 16
		result, err := extractWithLimits(context.Background(), root, "alice", "demo", nil, configured)
		if err != nil {
			t.Fatal(err)
		}
		if result.Metadata.InputBytes != 16 {
			t.Fatalf("input bytes = %d, want 16", result.Metadata.InputBytes)
		}
		assertTruncation(t, result, TruncationPageInput)
	})

	t.Run("site", func(t *testing.T) {
		root, _ := newExtractionRoot(t, map[string]string{
			"a.html": "a",
			"b.html": "b",
		})
		configured := generousTestLimits()
		configured.siteInputBytes = 1
		result, err := extractWithLimits(context.Background(), root, "alice", "demo", nil, configured)
		if err != nil {
			t.Fatal(err)
		}
		if result.Metadata.InputBytes != 1 {
			t.Fatalf("input bytes = %d, want 1", result.Metadata.InputBytes)
		}
		if got := documentPaths(result.Documents); !reflect.DeepEqual(got, []string{"a.html"}) {
			t.Fatalf("site-input-limited paths = %#v", got)
		}
		assertTruncation(t, result, TruncationSiteInput)
	})
}

func TestExtractFieldAndAggregateOutputLimits(t *testing.T) {
	tests := []struct {
		name  string
		kind  TruncationKind
		limit func(*extractionLimits)
		field func(Document) string
	}{
		{
			name: "title", kind: TruncationTitle,
			limit: func(configured *extractionLimits) { configured.titleBytes = 3 },
			field: func(document Document) string { return document.Title },
		},
		{
			name: "description", kind: TruncationDescription,
			limit: func(configured *extractionLimits) { configured.descriptionBytes = 3 },
			field: func(document Document) string { return document.Description },
		},
		{
			name: "headings", kind: TruncationHeadings,
			limit: func(configured *extractionLimits) { configured.headingsBytes = 3 },
			field: func(document Document) string { return document.Headings },
		},
		{
			name: "body", kind: TruncationBody,
			limit: func(configured *extractionLimits) { configured.bodyBytes = 3 },
			field: func(document Document) string { return document.BodyText },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, _ := newExtractionRoot(t, map[string]string{
				"index.html": `<title>abcdef</title><meta name="description" content="abcdef"><body><h1>abcdef</h1><p>ghijkl</p></body>`,
			})
			configured := generousTestLimits()
			test.limit(&configured)
			result, err := extractWithLimits(context.Background(), root, "alice", "demo", nil, configured)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Documents) != 1 {
				t.Fatalf("documents = %d", len(result.Documents))
			}
			if field := test.field(result.Documents[0]); len(field) > 3 {
				t.Fatalf("bounded field = %q (%d bytes)", field, len(field))
			}
			assertTruncation(t, result, test.kind)
		})
	}

	t.Run("site output", func(t *testing.T) {
		root, _ := newExtractionRoot(t, map[string]string{
			"index.html": `<title>ééé</title><body>ghijkl</body>`,
		})
		configured := generousTestLimits()
		configured.siteOutputBytes = 4
		result, err := extractWithLimits(context.Background(), root, "alice", "demo", nil, configured)
		if err != nil {
			t.Fatal(err)
		}
		if result.Metadata.OutputBytes > 4 {
			t.Fatalf("output bytes = %d", result.Metadata.OutputBytes)
		}
		if title := result.Documents[0].Title; title != "éé" || !utf8.ValidString(title) {
			t.Fatalf("aggregate UTF-8 title = %q", title)
		}
		assertTruncation(t, result, TruncationSiteOutput)
	})
}

func TestExtractUTF8SafeTruncationAndUnicodeNormalization(t *testing.T) {
	root, _ := newExtractionRoot(t, map[string]string{
		"index.html": `<title>ééé</title><body>body</body>`,
	})
	configured := generousTestLimits()
	configured.titleBytes = 5
	result, err := extractWithLimits(context.Background(), root, "alice", "demo", nil, configured)
	if err != nil {
		t.Fatal(err)
	}
	title := result.Documents[0].Title
	if title != "éé" || !utf8.ValidString(title) {
		t.Fatalf("UTF-8 title = %q (%d bytes)", title, len(title))
	}
	assertTruncation(t, result, TruncationTitle)

	accumulator := newTextAccumulator(100)
	accumulator.Add("  a\u2003\tb\u0001c\n d  ")
	if got := accumulator.String(); got != "a bc d" {
		t.Fatalf("normalized text = %q", got)
	}
}

func TestExtractDeadlineAndCancellation(t *testing.T) {
	root, _ := newExtractionRoot(t, map[string]string{"index.html": `<body>content</body>`})

	configured := generousTestLimits()
	configured.deadline = time.Nanosecond
	result, err := extractWithLimits(context.Background(), root, "alice", "demo", nil, configured)
	if err != nil {
		t.Fatalf("internal deadline returned error: %v", err)
	}
	assertTruncation(t, result, TruncationDeadline)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Extract(ctx, root, "alice", "demo")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled extraction error = %v", err)
	}
}

func TestExtractBoundsDiagnostics(t *testing.T) {
	root, _ := newExtractionRoot(t, map[string]string{
		"index.html": `<title>abcdef</title><meta name="description" content="abcdef"><body><h1>abcdef</h1>ghijkl</body>`,
	})
	configured := generousTestLimits()
	configured.titleBytes = 1
	configured.descriptionBytes = 1
	configured.headingsBytes = 1
	configured.bodyBytes = 1
	configured.diagnostics = 1
	result, err := extractWithLimits(context.Background(), root, "alice", "demo", nil, configured)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Metadata.Truncations) != 1 || result.Metadata.TruncationsOmitted != 3 {
		t.Fatalf("diagnostic bounds = %+v", result.Metadata)
	}
}

func TestExtractZeroHTML(t *testing.T) {
	root, base := newExtractionRoot(t, map[string]string{
		"notes.txt": "plain text",
		"app.js":    "document.body.textContent = 'not indexed'",
	})
	if err := os.Mkdir(filepath.Join(base, "fake.html"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := Extract(context.Background(), root, "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Documents) != 0 || result.Metadata.Partial || result.Metadata.InputBytes != 0 || result.Metadata.OutputBytes != 0 {
		t.Fatalf("zero-HTML result = %+v", result)
	}
}

func generousTestLimits() extractionLimits {
	configured := defaultExtractionLimits
	configured.pages = 100
	configured.pageInputBytes = 1 << 20
	configured.siteInputBytes = 2 << 20
	configured.siteOutputBytes = 1 << 20
	configured.titleBytes = 1 << 10
	configured.descriptionBytes = 1 << 10
	configured.headingsBytes = 1 << 10
	configured.bodyBytes = 1 << 10
	configured.diagnostics = 64
	configured.deadline = time.Second
	return configured
}

func newExtractionRoot(t *testing.T, files map[string]string) (*os.Root, string) {
	t.Helper()
	base := t.TempDir()
	for name, content := range files {
		fullPath := filepath.Join(base, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Errorf("close extraction root: %v", err)
		}
	})
	return root, base
}

func documentPaths(documents []Document) []string {
	paths := make([]string, len(documents))
	for index, document := range documents {
		paths[index] = document.PagePath
	}
	return paths
}

func assertTruncation(t *testing.T, result Result, want TruncationKind) {
	t.Helper()
	if !result.Metadata.Partial {
		t.Fatalf("result not marked partial; want %q", want)
	}
	for _, truncation := range result.Metadata.Truncations {
		if truncation.Kind == want {
			return
		}
	}
	t.Fatalf("missing truncation %q in %+v", want, result.Metadata)
}

func TestNewExtractorUsesTheGivenSiteBase(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"index.html":      "<title>Home</title>",
		"docs/index.html": "<title>Docs</title>",
		"docs/a b.html":   "<title>Spaced</title>",
	} {
		if err := os.WriteFile(filepath.Join(base, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	absolute := func(owner, site string) string {
		return "https://" + owner + ".foo.example/" + site + "/"
	}
	result, err := NewExtractor(absolute)(context.Background(), root, "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, document := range result.Documents {
		got = append(got, document.URLPath)
	}
	want := []string{
		"https://alice.foo.example/demo/docs/a%20b.html",
		"https://alice.foo.example/demo/docs/",
		"https://alice.foo.example/demo/",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("URLPaths = %q, want %q", got, want)
	}

	// A nil base is the pre-cutover relative long path, exactly what Extract emits.
	viaNil, err := NewExtractor(nil)(context.Background(), root, "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	viaExtract, err := Extract(context.Background(), root, "alice", "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viaNil.Documents, viaExtract.Documents) {
		t.Fatalf("NewExtractor(nil) = %+v, want Extract's %+v", viaNil.Documents, viaExtract.Documents)
	}
	if got := viaNil.Documents[len(viaNil.Documents)-1].URLPath; got != "/sites/alice/demo/" {
		t.Fatalf("nil base URLPath = %q, want /sites/alice/demo/", got)
	}
}
