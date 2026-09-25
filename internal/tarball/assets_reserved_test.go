package tarball

import (
	"os"
	"testing"
)

// TestExtractRefusesReservedAssetsPath covers the rule that an archive
// must not be able to shadow the /{site}/_assets/{id} route with content
// of its own, at the top level or nested under it, as a file or a
// directory, in either archive format.
func TestExtractRefusesReservedAssetsPath(t *testing.T) {
	cases := []struct {
		name    string
		entries []testEntry
	}{
		{
			name:    "top-level directory",
			entries: []testEntry{{name: "_assets/", mode: os.ModeDir | 0o755}},
		},
		{
			name:    "top-level file",
			entries: []testEntry{{name: "_assets", data: []byte("shadow")}},
		},
		{
			name:    "nested file",
			entries: []testEntry{{name: "_assets/logo.png", data: []byte("shadow")}},
		},
		{
			name: "alongside legitimate content",
			entries: []testEntry{
				{name: "index.html", data: []byte("hello")},
				{name: "_assets/logo.png", data: []byte("shadow")},
			},
		},
	}

	for _, format := range []string{"tar.gz", "zip"} {
		format := format
		for _, tc := range cases {
			tc := tc
			t.Run(format+"_"+tc.name, func(t *testing.T) {
				archive := buildArchive(t, format, tc.entries)
				if _, err := ExtractBytes(archive, "site."+format); err == nil {
					t.Fatalf("expected _assets entry to be refused, got no error")
				}
			})
		}
	}
}

// TestExtractAllowsNonReservedNamesResemblingAssets makes sure the reserved
// check is exact, not a prefix match on "_assets" as a substring: a
// sibling file or directory that merely starts with the same characters is
// ordinary content and must extract normally.
func TestExtractAllowsNonReservedNamesResemblingAssets(t *testing.T) {
	entries := []testEntry{
		{name: "_assets-legacy/logo.png", data: []byte("fine")},
		{name: "_assets2.txt", data: []byte("fine")},
	}
	for _, format := range []string{"tar.gz", "zip"} {
		format := format
		t.Run(format, func(t *testing.T) {
			archive := buildArchive(t, format, entries)
			files, err := ExtractBytes(archive, "site."+format)
			if err != nil {
				t.Fatalf("ExtractBytes: %v", err)
			}
			if _, ok := files["_assets-legacy/logo.png"]; !ok {
				t.Fatal("expected _assets-legacy/logo.png to extract")
			}
			if _, ok := files["_assets2.txt"]; !ok {
				t.Fatal("expected _assets2.txt to extract")
			}
		})
	}
}
