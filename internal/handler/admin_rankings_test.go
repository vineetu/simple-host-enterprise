package handler

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/storage"
)

func TestParseRankMetricFallsBackToViews(t *testing.T) {
	tests := []struct {
		raw  string
		want rankMetric
	}{
		{"size", metricSize},
		{"downloads", metricViews}, // removed metric: falls back rather than 404s
		{"shared", metricViews},    // removed with editor grants
		{"editing", metricViews},   // removed with editor grants
		{"views", metricViews},
		{"", metricViews},
		{"Size", metricViews},    // case-sensitive on purpose
		{"editors", metricViews}, // the label, not the metric
		{"'; DROP TABLE", metricViews},
	}
	for _, tc := range tests {
		if got := parseRankMetric(tc.raw, userMetrics); got != tc.want {
			t.Errorf("parseRankMetric(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// "updated" is a site metric only; asking for it on the users card must not
// produce a column that card cannot render.
func TestParseRankMetricIsScopedToItsCard(t *testing.T) {
	if got := parseRankMetric("updated", userMetrics); got != metricViews {
		t.Errorf("parseRankMetric(updated, userMetrics) = %q, want views", got)
	}
	if got := parseRankMetric("updated", siteMetrics); got != metricUpdated {
		t.Errorf("parseRankMetric(updated, siteMetrics) = %q, want updated", got)
	}
}

func TestRankingURLPreservesTheOtherCard(t *testing.T) {
	raw := rankingURL(42, metricSize, metricUpdated)
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("rankingURL produced an unparseable URL %q: %v", raw, err)
	}
	q := parsed.Query()
	if q.Get("days") != "42" {
		t.Errorf("days = %q, want 42", q.Get("days"))
	}
	if q.Get("users") != "size" {
		t.Errorf("users = %q, want size", q.Get("users"))
	}
	if q.Get("sites") != "updated" {
		t.Errorf("sites = %q, want updated", q.Get("sites"))
	}
}

func TestSortUserRanksByEachMetric(t *testing.T) {
	rows := []userRank{
		{username: "carol", views: 10, totalBytes: 900},
		{username: "alice", views: 30, totalBytes: 100},
		{username: "bob", views: 20, totalBytes: 500},
	}

	for _, tc := range []struct {
		metric rankMetric
		want   string
	}{
		{metricViews, "alice,bob,carol"},
		{metricSize, "carol,bob,alice"},
	} {
		got := append([]userRank(nil), rows...)
		sortUserRanks(got, tc.metric)
		var names []string
		for _, r := range got {
			names = append(names, r.username)
		}
		if strings.Join(names, ",") != tc.want {
			t.Errorf("sort by %q = %v, want %s", tc.metric, names, tc.want)
		}
	}
}

// Ties must not shuffle between page loads, or the same data renders in a
// different order each refresh.
func TestSortUserRanksBreaksTiesDeterministically(t *testing.T) {
	rows := []userRank{
		{username: "zoe", views: 5, siteCount: 1},
		{username: "adam", views: 5, siteCount: 1},
		{username: "mary", views: 5, siteCount: 3},
	}
	sortUserRanks(rows, metricViews)
	// Equal views, so more sites wins; then username ascending.
	if rows[0].username != "mary" || rows[1].username != "adam" || rows[2].username != "zoe" {
		t.Fatalf("tie-break order = %s,%s,%s; want mary,adam,zoe",
			rows[0].username, rows[1].username, rows[2].username)
	}
}

func TestSortSiteRanksByEachMetric(t *testing.T) {
	rows := []siteRank{
		{name: "gamma", owner: "u", views: 1, totalBytes: 30},
		{name: "alpha", owner: "u", views: 3, totalBytes: 10},
		{name: "beta", owner: "u", views: 2, totalBytes: 20},
	}
	for _, tc := range []struct {
		metric rankMetric
		want   string
	}{
		{metricViews, "alpha,beta,gamma"},
		{metricSize, "gamma,beta,alpha"},
	} {
		got := append([]siteRank(nil), rows...)
		sortSiteRanks(got, tc.metric)
		var names []string
		for _, r := range got {
			names = append(names, r.name)
		}
		if strings.Join(names, ",") != tc.want {
			t.Errorf("sort by %q = %v, want %s", tc.metric, names, tc.want)
		}
	}
}

func TestRenderUserRankingCardCapsAtTen(t *testing.T) {
	rows := make([]userRank, 25)
	for i := range rows {
		rows[i] = userRank{username: string(rune('a'+i)) + "-user", views: int64(100 - i)}
	}
	var b strings.Builder
	renderUserRankingCard(&b, HostModel{}, rows, metricViews, metricViews, 7)

	if got := strings.Count(b.String(), `rank-row--ranked`); got != rankTop {
		t.Errorf("rendered %d rows, want %d", got, rankTop)
	}
}

// Each row carries exactly one value cell. The overflow this replaced came from
// packing several figures into that one right-aligned, non-shrinking cell.
func TestRankRowHasOneValueAndContextBeneathTheName(t *testing.T) {
	var b strings.Builder
	renderUserRankingCard(&b, HostModel{}, []userRank{
		{username: "george.phipps", siteCount: 12, totalBytes: 472400000, liveBytes: 130900000},
	}, metricSize, metricViews, 7)
	out := b.String()

	if got := strings.Count(out, `class="rank-value"`); got != 1 {
		t.Errorf("row has %d value cells, want 1", got)
	}
	// The secondary figures belong under the name, not in the value cell.
	value := between(t, out, `<span class="rank-value">`, `</span>`)
	if strings.Contains(value, "live") || strings.Contains(value, "sites") {
		t.Errorf("value cell carries secondary context: %q", value)
	}
	sub := between(t, out, `<span class="rank-sub">`, `</span>`)
	if !strings.Contains(sub, "live") || !strings.Contains(sub, "12 sites") {
		t.Errorf("sub-line is missing the context: %q", sub)
	}
}

// A long name must be ellipsizable and still readable in full on hover, rather
// than wrapping and pushing rows out of alignment.
func TestRankRowNameIsTruncatableWithFullNameAvailable(t *testing.T) {
	long := "auth-rate-sales-monthly-reduced-web-dashboard"
	var b strings.Builder
	renderSiteRankingCard(&b, HostModel{}, []siteRank{{name: long, owner: "luis.vargas"}}, metricViews, metricViews, 7)
	out := b.String()

	if !strings.Contains(out, `title="`+long+`"`) {
		t.Error("full name is not exposed via title")
	}
	if !strings.Contains(out, `class="rank-name"`) {
		t.Error("name cell is missing the class that ellipsizes it")
	}
	// The owner belongs on the sub-line; inline it made rows wrap unevenly.
	if !strings.Contains(out, `<span class="rank-sub">luis.vargas</span>`) {
		t.Errorf("owner is not on the sub-line: %s", out)
	}
}

// The reporting range is a property of the card, not of every row.
func TestReportingRangeAppearsOncePerCard(t *testing.T) {
	var b strings.Builder
	renderUserRankingCard(&b, HostModel{}, []userRank{
		{username: "a", views: 5}, {username: "b", views: 4}, {username: "c", views: 3},
	}, metricViews, metricViews, 7)

	if got := strings.Count(b.String(), analyticsRangeCompact(7)); got != 1 {
		t.Errorf("reporting range appears %d times, want 1", got)
	}
}

// Storage is not bounded by the reporting range, so showing one would be a lie.
func TestUnrangedMetricsOmitTheReportingRange(t *testing.T) {
	var b strings.Builder
	renderUserRankingCard(&b, HostModel{}, []userRank{{username: "a", totalBytes: 10}}, metricSize, metricViews, 7)

	if strings.Contains(b.String(), `class="card-count"`) {
		t.Error("storage ranking claims a reporting range it does not have")
	}
}

func between(t *testing.T, s, open, close string) string {
	t.Helper()
	_, rest, found := strings.Cut(s, open)
	if !found {
		t.Fatalf("%q not found", open)
	}
	inner, _, found := strings.Cut(rest, close)
	if !found {
		t.Fatalf("%q not closed", open)
	}
	return inner
}

func TestRenderRankingCardsEscapeUntrustedNames(t *testing.T) {
	var b strings.Builder
	renderUserRankingCard(&b, HostModel{}, []userRank{{username: `<script>x</script>`}}, metricViews, metricViews, 7)
	renderSiteRankingCard(&b, HostModel{}, []siteRank{{name: `"><img onerror=x>`, owner: "o"}}, metricViews, metricViews, 7)

	out := b.String()
	if strings.Contains(out, "<script>x</script>") {
		t.Error("username was not escaped")
	}
	if strings.Contains(out, "<img onerror=x>") {
		t.Error("site name was not escaped")
	}
}

func TestRenderRankingCardsHandleEmptyData(t *testing.T) {
	var b strings.Builder
	renderUserRankingCard(&b, HostModel{}, nil, metricSize, metricViews, 7)
	renderSiteRankingCard(&b, HostModel{}, nil, metricUpdated, metricViews, 7)

	out := b.String()
	if !strings.Contains(out, "No users yet.") || !strings.Contains(out, "No sites yet.") {
		t.Errorf("empty state missing from output: %s", out)
	}
}

// The active tab has to be marked for both sighted users and screen readers,
// and every tab must link somewhere.
func TestRankTabsMarkTheActiveMetric(t *testing.T) {
	var b strings.Builder
	renderUserRankingCard(&b, HostModel{}, nil, metricSize, metricViews, 7)
	out := b.String()

	if strings.Count(out, `class="rank-tab is-active"`) != 1 {
		t.Errorf("expected exactly one active tab, got: %s", out)
	}
	if !strings.Contains(out, `aria-selected="true"`) {
		t.Error("active tab is not marked with aria-selected")
	}
	// Count role="tab" rather than the class: `class="rank-tab` is also a
	// prefix of the container's `class="rank-tabs"`.
	if got := strings.Count(out, `role="tab"`); got != len(userMetrics) {
		t.Errorf("rendered %d tabs, want %d", got, len(userMetrics))
	}
	if got := strings.Count(out, `href="/admin?`); got != len(userMetrics) {
		t.Errorf("%d tabs link somewhere, want %d", got, len(userMetrics))
	}
}

func TestMeasureSiteStorageCountsAllObjectsAndTracksLive(t *testing.T) {
	siteUsageCache.Lock()
	siteUsageCache.measuredAt = time.Time{}
	siteUsageCache.Unlock()
	store := newTestStore(t)
	const siteID = "0a0a0a0a-0000-4000-8000-00000000000a"
	for version, size := range map[int]int{1: 100, 2: 300} {
		key, _ := storage.VersionKey(siteID, version)
		if err := store.objects.Put(context.Background(), key, make([]byte, size), ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.objects.Put(context.Background(), "sites/"+siteID+"/assets/0a0a0a0a-0000-4000-8000-0000000000aa", make([]byte, 50), ""); err != nil {
		t.Fatal(err)
	}

	measured := measureSiteStorage(context.Background(), store.Store)
	usage := measured.site(siteID, 2)
	// 100 + 300 + 50: every retained version and every asset, as stored.
	if usage.totalBytes != 450 {
		t.Errorf("totalBytes = %d, want 450", usage.totalBytes)
	}
	if usage.liveBytes != 300 {
		t.Errorf("liveBytes = %d, want 300", usage.liveBytes)
	}
	if measured.totalBytes != 450 {
		t.Errorf("bucket total = %d, want 450", measured.totalBytes)
	}
	if got := measured.site(siteID, 9).liveBytes; got != 0 {
		t.Errorf("liveBytes for a version with no object = %d, want 0", got)
	}
}

func TestMeasureSiteStorageWithoutStoreShowsNothing(t *testing.T) {
	if got := measureSiteStorage(context.Background(), nil); got.bySite != nil || storageStatsHTML(got) != "" {
		t.Fatalf("nil store measured %+v", got)
	}
}

func TestSiteRanksSortByUpdated(t *testing.T) {
	if got := parseRankMetric("updated", siteMetrics); got != metricUpdated {
		t.Fatalf("parseRankMetric(updated, siteMetrics) = %q, want updated", got)
	}
	if got := parseRankMetric("updated", userMetrics); got != metricViews {
		t.Errorf("parseRankMetric(updated, userMetrics) = %q, want views", got)
	}

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	rows := []siteRank{
		{name: "stale", owner: "ann", views: 9000, updatedAt: base},
		{name: "fresh", owner: "bob", views: 1, updatedAt: base.Add(48 * time.Hour)},
		{name: "middle", owner: "cid", views: 500, updatedAt: base.Add(24 * time.Hour)},
	}
	sortSiteRanks(rows, metricUpdated)
	got := []string{rows[0].name, rows[1].name, rows[2].name}
	want := []string{"fresh", "middle", "stale"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}

	// Views must still win on its own tab, so the new metric cannot leak into
	// the default ordering.
	sortSiteRanks(rows, metricViews)
	if rows[0].name != "stale" {
		t.Errorf("views ordering leader = %q, want stale", rows[0].name)
	}
}

// A timestamp has to arrive as markup, not as escaped text, or the browser
// shows the reader a raw <time> tag instead of their local time.
func TestUpdatedRowRendersRealTimeMarkup(t *testing.T) {
	var b strings.Builder
	row := siteRank{name: "portfolio", owner: "ann", updatedAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)}
	writeRankRow(&b, 1, "https://ann.foo.example/portfolio/", row.name, siteMetricCell(row, metricUpdated))
	out := b.String()
	if !strings.Contains(out, `<time data-local-time="datetime" datetime="2026-08-01T12:00:00Z">`) {
		t.Errorf("row lost its time element:\n%s", out)
	}
	if strings.Contains(out, "&lt;time") {
		t.Errorf("time element was escaped into visible text:\n%s", out)
	}
	if !strings.Contains(out, `>ann<`) {
		t.Errorf("row dropped the owner:\n%s", out)
	}
}

// Escaping still applies to every metric that is a plain value.
func TestPlainRankValuesStayEscaped(t *testing.T) {
	var b strings.Builder
	writeRankRow(&b, 1, "https://ann.foo.example/", "ann", rankCell{value: `<b>1</b>`, unit: "views", sub: "1 site"})
	if !strings.Contains(b.String(), "&lt;b&gt;1&lt;/b&gt;") {
		t.Errorf("plain value was not escaped:\n%s", b.String())
	}
}

func TestNewUsersCardOrdersNewestFirst(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	var b strings.Builder
	renderNewUsersCard(&b, HostModel{}, []newUser{
		{username: "carol", joined: base.Add(72 * time.Hour), siteCount: 0, disabled: true},
		{username: "bob", joined: base.Add(24 * time.Hour), siteCount: 3},
	}, 41)
	out := b.String()
	if strings.Index(out, "carol") > strings.Index(out, "bob") {
		t.Errorf("card is not newest-first:\n%s", out)
	}
	if !strings.Contains(out, "41 total") {
		t.Errorf("card does not report the total user count:\n%s", out)
	}
	if !strings.Contains(out, "0 sites · disabled") {
		t.Errorf("card does not flag a disabled account:\n%s", out)
	}
	if !strings.Contains(out, "3 sites") {
		t.Errorf("card does not show site counts:\n%s", out)
	}
}

func TestAdminListScriptBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the admin list DOM behavior test")
	}
	script := strings.TrimSuffix(strings.TrimPrefix(adminListScript, "<script>"), "</script>")
	command := exec.Command(node, "testdata/admin_list_test.js")
	command.Env = append(os.Environ(), "ADMIN_LIST_SCRIPT="+script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run admin list behavior test: %v\n%s", err, output)
	}
}

// The "disabled" chip is how an admin spots an offboarded person. A team has
// no sign-in and is never disabled, so it must not wear the chip — every
// team would otherwise be indistinguishable from an offboarded person.
func TestNewUsersCardMarksTeamsInsteadOfDisabled(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	var b strings.Builder
	renderNewUsersCard(&b, HostModel{}, []newUser{
		{username: "platform", joined: base.Add(48 * time.Hour), siteCount: 0, team: true},
		{username: "dana", joined: base.Add(24 * time.Hour), siteCount: 0, disabled: true},
	}, 2)
	out := b.String()
	if !strings.Contains(out, "0 sites · team") {
		t.Errorf("team is not marked as one:\n%s", out)
	}
	if strings.Contains(out, "team · disabled") {
		t.Errorf("team is flagged as disabled:\n%s", out)
	}
	if !strings.Contains(out, "0 sites · disabled") {
		t.Errorf("disabled person lost the chip:\n%s", out)
	}
}

// A person's block is unchanged by teams existing: a Disable control for an
// active account, an Enable control (and the "disabled" chip) for one an
// admin has offboarded.
func TestUserBlockHeaderKeepsPersonControls(t *testing.T) {
	joined := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	var active strings.Builder
	writeUserBlockHeader(&active, HostModel{}, db.User{Username: "dana", CreatedAt: joined}, 0, "dana", 0, 0)
	if strings.Contains(active.String(), "disabled<") {
		t.Errorf("active person is flagged as disabled:\n%s", active.String())
	}
	if !strings.Contains(active.String(), `/api/admin/users/dana/disable`) {
		t.Errorf("active person lost the disable control:\n%s", active.String())
	}

	// Kind is only loaded by the queries that select it; a row without it has
	// to render exactly as it did before teams existed.
	disabledAt := joined.Add(time.Hour)
	var disabled strings.Builder
	writeUserBlockHeader(&disabled, HostModel{}, db.User{Username: "dana", CreatedAt: joined, DisabledAt: &disabledAt}, 2, "dana demo", 0, 0)
	out := disabled.String()
	if !strings.Contains(out, `chip chip-warn">disabled<`) {
		t.Errorf("disabled person is not flagged:\n%s", out)
	}
	if !strings.Contains(out, `/api/admin/users/dana/enable`) {
		t.Errorf("disabled person lost the enable control:\n%s", out)
	}
	if !strings.Contains(out, "2 sites") {
		t.Errorf("header lost the site count:\n%s", out)
	}
}

// A team block says what it is, says how many people are in it, and offers
// nothing about keys: it has none to be pending and none to reset.
func TestUserBlockHeaderMarksTeamAndDropsKeyControls(t *testing.T) {
	var b strings.Builder
	writeUserBlockHeader(&b, testHostModel(t), db.User{
		Username:    "platform",
		Kind:        "team",
		MemberCount: 3,
		CreatedAt:   time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	}, 1, "platform docs", 0, 0)
	out := b.String()
	if !strings.Contains(out, `<span class="chip">team</span>`) {
		t.Errorf("team is not marked as one:\n%s", out)
	}
	if !strings.Contains(out, "3 members") {
		t.Errorf("team does not show its member count:\n%s", out)
	}
	if strings.Contains(out, "disabled") {
		t.Errorf("team wears the disabled chip:\n%s", out)
	}
	if strings.Contains(out, "/disable") || strings.Contains(out, "/enable") {
		t.Errorf("team offers an offboarding control it has no sign-in to need:\n%s", out)
	}
	if !strings.Contains(out, `href="https://platform.foo.example/"`) {
		t.Errorf("team lost the details link:\n%s", out)
	}
	if !strings.Contains(out, "1 site") {
		t.Errorf("team lost its site count:\n%s", out)
	}
}

// One member is one member, not "1 members".
func TestUserBlockHeaderSingularMemberCount(t *testing.T) {
	var b strings.Builder
	writeUserBlockHeader(&b, HostModel{}, db.User{Username: "solo", Kind: "team", MemberCount: 1}, 0, "solo", 0, 0)
	if !strings.Contains(b.String(), "1 member<") {
		t.Errorf("member count is not singular:\n%s", b.String())
	}
}
