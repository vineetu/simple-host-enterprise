package oplimits

import (
	"strings"
	"testing"
	"time"
)

func TestTextForPeople(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{Days(1), "1 day"},
		{Days(30), "30 days"},
		{Days(1000), "1,000 days"},
		{Duration(time.Hour), "1 hour"},
		{Duration(10 * time.Minute), "10 minutes"},
		{Duration(180 * 24 * time.Hour), "180 days"},
		{Duration(90 * time.Second), "90 seconds"},
		{Duration(50 * time.Millisecond), "50ms"},
		{Duration(90 * time.Minute), "90 minutes"},
		{Count(50_000), "50,000"},
		{Count(999), "999"},
		{Count(1_234_567), "1,234,567"},
		{Bytes(100 << 20), "100 MiB"},
		{Bytes(3 << 29), "1.5 GiB"},
		{Bytes(512), "512 B"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

func TestExpandFollowsTheValuesInForce(t *testing.T) {
	before := Get()
	t.Cleanup(func() { Set(before) })

	text := "{{DELETED_RETENTION}} / {{PREVIEW_LINK_TTL}} / {{MAX_ARCHIVE}} / {{MAX_FILES_PER_SITE}}"
	if got := Expand(text); got != "30 days / 1 hour / 100 MiB / 50,000" {
		t.Fatalf("defaults expand to %q", got)
	}
	v := Defaults()
	v.DeletedRetentionDays = 7
	v.PreviewLinkTTL = 15 * time.Minute
	v.MaxArchiveBytes = 250 << 20
	v.MaxFilesPerSite = 80_000
	Set(v)
	if got := Expand(text); got != "7 days / 15 minutes / 250 MiB / 80,000" {
		t.Fatalf("overrides expand to %q", got)
	}
	// Every placeholder is filled: none survives into served text.
	pairs := v.Placeholders()
	var all strings.Builder
	for i := 0; i < len(pairs); i += 2 {
		all.WriteString(pairs[i])
	}
	if got := Expand(all.String()); strings.Contains(got, "{{") {
		t.Fatalf("unexpanded placeholder in %q", got)
	}
}
