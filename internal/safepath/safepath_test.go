package safepath

import (
	"strings"
	"testing"
)

func TestValidateSegment(t *testing.T) {
	t.Parallel()

	valid := []string{"alice", "site.name", "a-b_c", "two words"}
	for _, value := range valid {
		value := value
		t.Run("valid_"+value, func(t *testing.T) {
			t.Parallel()
			if err := ValidateSegment(value); err != nil {
				t.Fatalf("ValidateSegment(%q): %v", value, err)
			}
		})
	}

	invalid := []string{"", ".", "..", "a/b", `a\b`, "a\x00b", "a\nb", string([]byte{0xff}), strings.Repeat("a", MaxSegmentBytes+1)}
	for _, value := range invalid {
		value := value
		t.Run("invalid", func(t *testing.T) {
			t.Parallel()
			if err := ValidateSegment(value); err == nil {
				t.Fatalf("ValidateSegment(%q) unexpectedly succeeded", value)
			}
		})
	}
}

func TestCanonicalRelativePath(t *testing.T) {
	t.Parallel()

	valid := []struct {
		raw       string
		directory bool
		want      string
	}{
		{raw: "index.html", want: "index.html"},
		{raw: "./assets/app.js", want: "assets/app.js"},
		{raw: "././assets/", directory: true, want: "assets"},
		{raw: "./", directory: true, want: "."},
		{raw: "downloads/app.dmg", want: "downloads/app.dmg"},
	}
	for _, test := range valid {
		test := test
		t.Run(test.raw, func(t *testing.T) {
			t.Parallel()
			got, err := CanonicalRelativePath(test.raw, test.directory)
			if err != nil {
				t.Fatalf("CanonicalRelativePath(%q): %v", test.raw, err)
			}
			if got != test.want {
				t.Fatalf("CanonicalRelativePath(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}

	invalid := []struct {
		raw       string
		directory bool
	}{
		{raw: ""},
		{raw: "."},
		{raw: ".."},
		{raw: "../escape"},
		{raw: "/absolute"},
		{raw: `windows\path`},
		{raw: "a//b"},
		{raw: "a/./b"},
		{raw: "a/../b"},
		{raw: "file/"},
		{raw: "a\x00b"},
		{raw: string([]byte{0xff})},
		{raw: strings.Repeat("a", MaxRelativeComponentBytes+1)},
		{raw: strings.Repeat("a/", MaxRelativePathDepth) + "a"},
		{raw: strings.Repeat("a", MaxRelativePathBytes+1)},
	}
	for _, test := range invalid {
		test := test
		t.Run("invalid", func(t *testing.T) {
			t.Parallel()
			if _, err := CanonicalRelativePath(test.raw, test.directory); err == nil {
				t.Fatalf("CanonicalRelativePath(%q) unexpectedly succeeded", test.raw)
			}
		})
	}
}
