package handler

import (
	"fmt"
	"strings"
	"testing"
	"time"

	dbstore "github.com/vsriram/simple-host/internal/db"
)

func showcaseEntryFor(id, owner, name string, views int64, updated, created time.Time) showcaseEntry {
	return showcaseEntry{
		site:    dbstore.Site{ID: id, Name: name, UpdatedAt: updated, CreatedAt: created},
		owner:   owner,
		views7d: views,
	}
}

func orderOf(entries []showcaseEntry) string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.site.Name)
	}
	return strings.Join(names, ",")
}

func TestSortShowcaseEntriesByEachMetric(t *testing.T) {
	base := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	rows := []showcaseEntry{
		showcaseEntryFor("1", "carol", "gamma", 5, base.Add(3*time.Hour), base),
		showcaseEntryFor("2", "alice", "beta", 9, base.Add(time.Hour), base.Add(2*time.Hour)),
		showcaseEntryFor("3", "bob", "alpha", 7, base.Add(2*time.Hour), base.Add(time.Hour)),
	}

	for _, tc := range []struct {
		key  showcaseSortKey
		want string
	}{
		{showcaseSortViews, "beta,alpha,gamma"},
		{showcaseSortUpdated, "gamma,alpha,beta"},
		{showcaseSortNewest, "beta,alpha,gamma"},
		{showcaseSortOwner, "beta,alpha,gamma"},
		{showcaseSortName, "alpha,beta,gamma"},
	} {
		got := append([]showcaseEntry(nil), rows...)
		sortShowcaseEntries(got, tc.key)
		if orderOf(got) != tc.want {
			t.Errorf("sort %q = %s, want %s", tc.key, orderOf(got), tc.want)
		}
	}
}

// Sub-second differences must survive. An earlier version emitted whole-second
// timestamps for the browser to sort on, which silently collapsed these.
func TestSortShowcaseEntriesKeepsSubSecondOrdering(t *testing.T) {
	base := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	rows := []showcaseEntry{
		showcaseEntryFor("1", "alice", "earlier", 0, base.Add(100*time.Millisecond), base),
		showcaseEntryFor("2", "alice", "later", 0, base.Add(500*time.Millisecond), base),
	}
	sortShowcaseEntries(rows, showcaseSortUpdated)
	if rows[0].site.Name != "later" {
		t.Errorf("order = %s, want later first", orderOf(rows))
	}
}

// The comparison must be a strict weak ordering, or sort.Slice may order a pair
// arbitrarily. Names that differ under one case-folding rule but not another
// are the case that previously slipped through the tiebreak.
func TestSortShowcaseEntriesIsTotal(t *testing.T) {
	rows := []showcaseEntry{
		showcaseEntryFor("b", "alice", "İ", 0, time.Time{}, time.Time{}), // capital I with dot
		showcaseEntryFor("a", "alice", "i", 0, time.Time{}, time.Time{}),
	}
	first := append([]showcaseEntry(nil), rows...)
	sortShowcaseEntries(first, showcaseSortName)

	// Sorting the reversed input must reach the same answer.
	second := []showcaseEntry{rows[1], rows[0]}
	sortShowcaseEntries(second, showcaseSortName)

	if orderOf(first) != orderOf(second) {
		t.Errorf("unstable ordering: %s vs %s from reversed input", orderOf(first), orderOf(second))
	}
}

// The ranks handed to the browser must reproduce the server's own order for
// every sort — that is the whole reason the browser is given ranks instead of
// values to compare.
func TestShowcaseRanksMatchServerOrder(t *testing.T) {
	base := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	entries := []showcaseEntry{
		showcaseEntryFor("1", "carol", "Zebra", 5, base.Add(3*time.Hour), base),
		showcaseEntryFor("2", "alice", "café", 9, base.Add(time.Hour), base.Add(2*time.Hour)),
		showcaseEntryFor("3", "bob", "my_site", 9, base.Add(2*time.Hour), base.Add(time.Hour)),
		showcaseEntryFor("4", "bob", "my-site", 0, base, base.Add(3*time.Hour)),
	}
	ranks := showcaseRanks(entries)

	for _, option := range showcaseSorts {
		ordered := append([]showcaseEntry(nil), entries...)
		sortShowcaseEntries(ordered, option.key)
		for position, e := range ordered {
			want := fmt.Sprintf(` data-rank-%s="%d"`, option.key, position)
			if !strings.Contains(ranks[e.site.ID], want) {
				t.Errorf("sort %q: %s missing %s (got %q)", option.key, e.site.Name, want, ranks[e.site.ID])
			}
		}
	}

	// Every site carries a rank for every ordering.
	for _, e := range entries {
		for _, option := range showcaseSorts {
			if !strings.Contains(ranks[e.site.ID], fmt.Sprintf(`data-rank-%s=`, option.key)) {
				t.Errorf("%s has no rank for %q", e.site.Name, option.key)
			}
		}
	}
}

// Raw timestamps must not reach the page: sites.updated_at moves on writes to a
// site's unauthenticated public state, so publishing it here would expose when
// the last submission to any listed site arrived.
func TestShowcaseRanksCarryNoTimestamps(t *testing.T) {
	base := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	entries := []showcaseEntry{showcaseEntryFor("1", "alice", "demo", 3, base, base)}
	attributes := showcaseRanks(entries)["1"]

	for _, forbidden := range []string{
		fmt.Sprintf("%d", base.Unix()),
		"data-updated",
		"data-created",
	} {
		if strings.Contains(attributes, forbidden) {
			t.Errorf("row attributes contain %q: %s", forbidden, attributes)
		}
	}
}

func TestSortShowcaseEntriesHandlesEmptyAndSingle(t *testing.T) {
	sortShowcaseEntries(nil, showcaseSortViews)
	sortShowcaseEntries([]showcaseEntry{}, showcaseSortName)

	one := []showcaseEntry{showcaseEntryFor("1", "alice", "only", 0, time.Time{}, time.Time{})}
	sortShowcaseEntries(one, showcaseSortUpdated)
	if len(one) != 1 || one[0].site.Name != "only" {
		t.Errorf("single-entry sort altered the slice: %v", orderOf(one))
	}
	if got := showcaseRanks(nil); len(got) != 0 {
		t.Errorf("ranks for no entries = %v, want empty", got)
	}
}
