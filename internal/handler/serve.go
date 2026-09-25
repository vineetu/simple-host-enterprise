package handler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/audit"
	"github.com/vsriram/simple-host/internal/auth"
	dbstore "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/safepath"
	"github.com/vsriram/simple-host/internal/storage"
)

const (
	siteVisitCookieName        = "simple_host_visit"
	siteVisitDuration          = 30 * time.Minute
	analyticsWriteLimit        = 500 * time.Millisecond
	defaultAnalyticsDays       = 7
	maxAnalyticsChartXLabels   = 8
	maxAnalyticsChartPointDays = 14
	maxDownloadPathLen         = 512
)

type analyticsRangeOption struct {
	days  int
	label string
}

type userListingSite struct {
	Site          dbstore.Site
	OwnerUsername string
	AccessRole    dbstore.CollaborationRole
}

var analyticsRangeOptions = [...]analyticsRangeOption{
	{days: 7, label: "7 days"},
	{days: 30, label: "30 days"},
	{days: 42, label: "6 weeks"},
	{days: 180, label: "6 months"},
}

func redirectWithTrailingSlash(w http.ResponseWriter, r *http.Request) {
	target := r.URL.EscapedPath() + "/"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

func renderShareDialog(builder *strings.Builder) {
	builder.WriteString(`<dialog id="shareDialog" class="share-dialog" aria-labelledby="shareDialogTitle" aria-describedby="shareDialogNote"><div class="share-dialog-shell"><header class="dialog-header"><div><div class="dialog-kicker">Site access</div><h2 id="shareDialogTitle">Share site</h2><p class="dialog-subtitle" id="shareDialogSubtitle"></p></div><button type="button" class="dialog-close" data-dialog-close aria-label="Close share dialog">&times;</button></header><div class="dialog-body"><section class="share-block" aria-labelledby="currentEditorsTitle"><h3 id="currentEditorsTitle">Current editors</h3><p class="share-help">Editors can download, deploy, and roll back this site's static files with their own API key.</p><div id="editorList" class="editor-list" aria-live="polite"></div></section><section class="share-block" aria-labelledby="addEditorsTitle"><h3 id="addEditorsTitle">Add editors</h3><p class="share-help">Choose from registered Simple Host users. You can add several people at once.</p><label class="search-label" for="editorSearch">Search usernames</label><input id="editorSearch" class="editor-search" type="search" maxlength="100" autocomplete="off" placeholder="Start typing a username"><div id="candidateList" class="candidate-list" aria-label="Registered users" aria-live="polite"></div><div id="selectedEditors" class="selected-editors" aria-label="Selected editors"></div><div class="share-footer"><p class="share-note" id="shareDialogNote">Revoking access blocks future changes and downloads. It does not undo content an editor already deployed or erase files they downloaded.</p><button type="button" id="addEditorsButton" class="btn btn-primary add-editors" disabled>Add selected</button></div></section></div></div></dialog>`)
}

// siteOpenTimeout bounds how long one request waits for its site's version
// to be fetched into the cache.
const siteOpenTimeout = 20 * time.Second

// SiteFiles serves a site's current version under a URL prefix: /{sitename}
// on the owner's own host, or the root of a restricted site's own host. The
// visit cookie, prefix stripping, and download-path trimming all follow the
// prefix.
//
// Build one per process and hand it to both RegisterServeRoutes and
// NewHostGate: it owns the download recorder's worker pool, and two of them
// would double the write concurrency against the database.
type SiteFiles struct {
	store     *storage.Store
	database  *sql.DB
	cookies   CookiePolicy
	downloads *fileDownloadRecorder
	// signingKeys and sessionIdle let the owner-detection paths (the
	// per-user listing page, self-traffic exclusion) verify the session
	// cookie the same way internal/auth.Middleware does, without importing
	// the whole middleware for what is otherwise a read-only "who is
	// looking" check.
	signingKeys []auth.SigningKey
	sessionIdle time.Duration
	// access batches access_log inserts (design.md 8.2) for every hosted-
	// content response serveSite produces, including the owner's and
	// editors' own — the isSelfTraffic exclusion above applies only to the
	// analytics counters, never to this log. Defaults to nil, in which case
	// Enqueue is skipped entirely (audit.(*AccessWriter).Enqueue is also
	// nil-safe, but skipping avoids building an AccessEvent nobody reads
	// when Phase 4 has not wired a real writer in yet).
	access *audit.AccessWriter
}

// NewSiteFiles builds the shared site file server.
func NewSiteFiles(store *storage.Store, database *sql.DB, cookies CookiePolicy, signingKeys []auth.SigningKey, sessionIdle time.Duration) *SiteFiles {
	return &SiteFiles{
		store:       store,
		database:    database,
		cookies:     cookies,
		downloads:   newFileDownloadRecorder(database),
		signingKeys: signingKeys,
		sessionIdle: sessionIdle,
	}
}

// WithAccessWriter attaches the access-log batching writer and returns s for
// chaining, the same shape SiteHandler.WithAudit and TeamHandler.WithAudit
// use. Called once from main.go.
func (s *SiteFiles) WithAccessWriter(access *audit.AccessWriter) *SiteFiles {
	s.access = access
	return s
}

// serveSite serves the current version of user's site beneath prefix, which is
// the decoded URL path the site is mounted at with no trailing slash, for
// example "/my-site". The request path must begin
// with prefix followed by "/". userID and sessionID are the host session the
// gate already authenticated the request against (design.md 8.1's
// requireHostSession) — recorded on the access_log row this method writes
// for every response, including the owner's and editors' own. Every mounted
// route requires a host session before reaching here.
func (s *SiteFiles) serveSite(w http.ResponseWriter, r *http.Request, user, siteName, prefix, userID, sessionID string) {
	if !safepath.IsSegment(user) || !safepath.IsSegment(siteName) {
		http.NotFound(w, r)
		return
	}
	// The database says which version is live; the cache serves its files.
	// A request waits a bounded time for a cold version; the shared fetch
	// behind it carries on for whoever asks next.
	openCtx, cancel := context.WithTimeout(r.Context(), siteOpenTimeout)
	root, err := s.store.OpenCurrent(openCtx, user, siteName)
	cancel()
	if err != nil {
		// Only "no such site" is a 404. Anything else — the database or the
		// bucket unreachable, a live version missing from the bucket — is
		// the server's failure, not the visitor's.
		if err == fs.ErrNotExist {
			http.NotFound(w, r)
			return
		}
		log.Printf("open current %s/%s: %v", user, siteName, err)
		http.Error(w, "site temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	defer root.Close()
	fileServer := http.StripPrefix(
		prefix,
		http.FileServer(noDirListingFS{fs: http.FS(root.FS())}),
	)
	database := s.database

	recorder := &statusRecorder{
		ResponseWriter: w,
		status:         http.StatusOK,
		beforeWriteHeader: func(status int, header http.Header) {
			if shouldRecordPageview(r, status, header) {
				// The owner and their editors checking their own work are
				// not an audience. Skip the visit cookie too, so their
				// browser is not marked as having visited.
				if isSelfTraffic(r, database, s.signingKeys, s.sessionIdle, user, siteName) {
					return
				}
				kind := classifyClient(r)
				isNewVisit := !hasVisitCookie(r)
				if recordSitePageview(r, database, user, siteName, isNewVisit, kind.isBot()) && isNewVisit && !kind.isBot() {
					// Deliberately no visit cookie for a bot. It would not
					// store one anyway, which is exactly why every bare
					// fetch used to register as a brand new visitor.
					// The cookie is scoped to the prefix actually served:
					// one scoped to the long path would never come back on
					// the short one, and every view there would be a new
					// visit.
					http.SetCookie(w, visitCookie(escapePathSegments(prefix)+"/", s.cookies))
				}
				return
			}
			if shouldRecordDownload(r, status, header) {
				if isSelfTraffic(r, database, s.signingKeys, s.sessionIdle, user, siteName) {
					return
				}
				file := strings.TrimPrefix(r.URL.Path, prefix+"/")
				if file == r.URL.Path {
					file = path.Base(r.URL.Path)
				}
				if file == "" || file == "." || file == "/" {
					return
				}
				if len(file) > maxDownloadPathLen {
					file = path.Base(file)
					if len(file) > maxDownloadPathLen {
						return
					}
				}
				s.downloads.Enqueue(fileDownloadEvent{username: user, siteName: siteName, path: file})
			}
		},
	}
	fileServer.ServeHTTP(recorder, r)

	// Every hosted-content response is logged here, including the owner's
	// and editors' own (design.md 8.2): the isSelfTraffic exclusion above
	// applies only to the pageview/download analytics counters, never to
	// this log. Enqueue is best-effort and non-blocking (audit.AccessWriter's
	// own contract); a nil s.access (no writer configured, or a test using
	// SiteFiles directly) is a safe no-op.
	s.access.Enqueue(audit.AccessEvent{
		At:         time.Now().UTC(),
		UserID:     userID,
		SessionID:  sessionID,
		OwnerLabel: ownerLabel(user),
		SiteName:   siteName,
		Path:       r.URL.Path,
		Method:     r.Method,
		Status:     recorder.status,
		Bytes:      recorder.bytes,
		IP:         remoteClientKey(r),
		UserAgent:  r.UserAgent(),
		ClientKind: classifyClient(r).String(),
	})
}

// escapePathSegments percent-encodes each segment of a decoded URL path so it
// can be used where an encoded path is required, such as a cookie Path.
func escapePathSegments(decoded string) string {
	segments := strings.Split(decoded, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

type statusRecorder struct {
	http.ResponseWriter
	status            int
	bytes             int64
	wroteHeader       bool
	beforeWriteHeader func(status int, header http.Header)
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	if r.beforeWriteHeader != nil {
		r.beforeWriteHeader(status, r.Header())
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(body)
	r.bytes += int64(n)
	return n, err
}

func shouldRecordPageview(r *http.Request, status int, header http.Header) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return false
	}
	return strings.HasPrefix(header.Get("Content-Type"), "text/html")
}

func recordSitePageview(r *http.Request, database *sql.DB, username, siteName string, countVisit, isBot bool) bool {
	if database == nil {
		return false
	}

	ctx, cancel := context.WithTimeout(r.Context(), analyticsWriteLimit)
	defer cancel()

	err := dbstore.RecordSiteDailyAnalytics(ctx, database, username, siteName, countVisit, isBot)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("record analytics for %s/%s: %v", username, siteName, err)
		}
		return false
	}
	return true
}

func shouldRecordDownload(r *http.Request, status int, header http.Header) bool {
	if r.Method != http.MethodGet || status != http.StatusOK || r.Header.Get("Range") != "" {
		return false
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(header.Get("Content-Disposition"))), "attachment") {
		return true
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(header.Get("Content-Type"), ";", 2)[0]))
	if mediaType == "" || strings.HasPrefix(mediaType, "text/") ||
		strings.HasPrefix(mediaType, "image/") ||
		strings.HasPrefix(mediaType, "audio/") ||
		strings.HasPrefix(mediaType, "video/") ||
		strings.HasPrefix(mediaType, "font/") {
		return false
	}
	switch mediaType {
	case "application/javascript", "application/json", "application/manifest+json",
		"application/wasm", "application/xml", "application/xhtml+xml",
		"application/font-woff", "application/vnd.ms-fontobject",
		"application/x-font-opentype", "application/x-font-ttf":
		return false
	}
	return !strings.HasSuffix(mediaType, "+json") && !strings.HasSuffix(mediaType, "+xml")
}

func hasVisitCookie(r *http.Request) bool {
	_, err := r.Cookie(siteVisitCookieName)
	return err == nil
}

// visitCookie marks a browser as having visited the site mounted at path,
// which is the encoded prefix the site is served under, with its trailing
// slash ("/my-site/").
func visitCookie(path string, cookies CookiePolicy) *http.Cookie {
	return &http.Cookie{
		Name:     siteVisitCookieName,
		Value:    "1",
		Path:     path,
		Expires:  time.Now().Add(siteVisitDuration),
		MaxAge:   int(siteVisitDuration.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   cookies.Secure,
	}
}

func analyticsDaysForRequest(r *http.Request, allowSelection bool) int {
	if !allowSelection {
		return defaultAnalyticsDays
	}
	switch r.URL.Query().Get("days") {
	case "30":
		return 30
	case "42":
		return 42
	case "180":
		return 180
	default:
		return defaultAnalyticsDays
	}
}

func analyticsRangeLabel(days int) string {
	for _, option := range analyticsRangeOptions {
		if option.days == days {
			return option.label
		}
	}
	return analyticsRangeOptions[0].label
}

func analyticsRangeAdjective(days int) string {
	switch days {
	case 30:
		return "30-day"
	case 42:
		return "6-week"
	case 180:
		return "6-month"
	default:
		return "7-day"
	}
}

func analyticsRangeCompact(days int) string {
	switch days {
	case 30:
		return "30D"
	case 42:
		return "6W"
	case 180:
		return "6M"
	default:
		return "7D"
	}
}

func renderAnalyticsRangeSelector(builder *strings.Builder, action string, days int) {
	renderAnalyticsRangeSelectorWith(builder, action, days, nil)
}

// renderAnalyticsRangeSelectorWith renders the range form, carrying extra query
// parameters through as hidden fields. Without this a page that keeps other
// state in the URL — the admin ranking metrics, for instance — silently resets
// it every time someone changes the reporting range.
func renderAnalyticsRangeSelectorWith(builder *strings.Builder, action string, days int, extra map[string]string) {
	builder.WriteString(`<form method="get" action="`)
	builder.WriteString(html.EscapeString(action))
	builder.WriteString(`" class="analytics-range" aria-label="Analytics reporting range: `)
	builder.WriteString(html.EscapeString(analyticsRangeLabel(days)))
	builder.WriteString(`"><label for="analytics-days">Reporting range</label><select id="analytics-days" name="days">`)
	for _, option := range analyticsRangeOptions {
		fmt.Fprintf(builder, `<option value="%d"`, option.days)
		if option.days == days {
			builder.WriteString(` selected`)
		}
		builder.WriteString(`>`)
		builder.WriteString(html.EscapeString(option.label))
		builder.WriteString(`</option>`)
	}
	builder.WriteString(`</select>`)
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(builder, `<input type="hidden" name="%s" value="%s">`,
			html.EscapeString(name), html.EscapeString(extra[name]))
	}
	builder.WriteString(`<button type="submit">Apply</button></form>`)
}

func normalizeAnalyticsSeries(rows []dbstore.SiteAnalyticsDay, days int) []dbstore.SiteAnalyticsDay {
	if days < 1 {
		days = 1
	}

	byDay := make(map[string]dbstore.SiteAnalyticsDay, len(rows))
	for _, row := range rows {
		byDay[row.Day.UTC().Format("2006-01-02")] = row
	}

	today := time.Now().UTC().Truncate(24 * time.Hour)
	start := today.AddDate(0, 0, -(days - 1))
	series := make([]dbstore.SiteAnalyticsDay, 0, days)
	for i := 0; i < days; i++ {
		day := start.AddDate(0, 0, i)
		point := byDay[day.Format("2006-01-02")]
		point.Day = day
		series = append(series, point)
	}

	return series
}

func combineAnalyticsSeries(seriesBySiteID map[string][]dbstore.SiteAnalyticsDay, siteIDs []string, days int) []dbstore.SiteAnalyticsDay {
	combined := normalizeAnalyticsSeries(nil, days)
	indexByDay := make(map[string]int, len(combined))
	for i, point := range combined {
		indexByDay[point.Day.Format("2006-01-02")] = i
	}

	for _, siteID := range siteIDs {
		for _, point := range normalizeAnalyticsSeries(seriesBySiteID[siteID], days) {
			i, ok := indexByDay[point.Day.Format("2006-01-02")]
			if !ok {
				continue
			}
			combined[i].Pageviews += point.Pageviews
			combined[i].Visits += point.Visits
		}
	}

	return combined
}

func renderTrendChart(builder *strings.Builder, series []dbstore.SiteAnalyticsDay, title, id string, days int) {
	if len(series) == 0 {
		series = normalizeAnalyticsSeries(nil, days)
	}

	maxValue := int64(0)
	for _, point := range series {
		if point.Pageviews > maxValue {
			maxValue = point.Pageviews
		}
		if point.Visits > maxValue {
			maxValue = point.Visits
		}
	}
	maxValue = chartMax(maxValue)

	builder.WriteString(`<div class="chart-wrap"><svg class="chart" viewBox="0 0 860 220" role="img" aria-labelledby="`)
	builder.WriteString(id)
	builder.WriteString(`-title `)
	builder.WriteString(id)
	builder.WriteString(`-desc"><title id="`)
	builder.WriteString(id)
	builder.WriteString(`-title">`)
	builder.WriteString(html.EscapeString(title))
	builder.WriteString(` `)
	builder.WriteString(analyticsRangeAdjective(days))
	builder.WriteString(` traffic trend</title><desc id="`)
	builder.WriteString(id)
	builder.WriteString(`-desc">Line chart comparing daily pageviews and visits.</desc>`)
	for _, y := range []int{36, 72, 108, 144} {
		fmt.Fprintf(builder, `<line class="grid" x1="64" y1="%d" x2="812" y2="%d"></line>`, y, y)
	}
	builder.WriteString(`<line class="axis" x1="64" y1="176" x2="812" y2="176"></line><line class="axis" x1="64" y1="36" x2="64" y2="176"></line>`)
	for i, value := range []int64{maxValue, maxValue * 3 / 4, maxValue / 2, maxValue / 4, 0} {
		y := []int{40, 76, 112, 148, 180}[i]
		fmt.Fprintf(builder, `<text x="24" y="%d">%s</text>`, y, html.EscapeString(formatCompactCount(value)))
	}
	fmt.Fprintf(builder, `<path class="views-line" d="%s"></path>`, chartPath(series, maxValue, func(point dbstore.SiteAnalyticsDay) int64 { return point.Pageviews }))
	fmt.Fprintf(builder, `<path class="visits-line" d="%s"></path>`, chartPath(series, maxValue, func(point dbstore.SiteAnalyticsDay) int64 { return point.Visits }))
	if len(series) <= maxAnalyticsChartPointDays {
		writeChartPoints(builder, "point-views", series, maxValue, func(point dbstore.SiteAnalyticsDay) int64 { return point.Pageviews })
		writeChartPoints(builder, "point-visits", series, maxValue, func(point dbstore.SiteAnalyticsDay) int64 { return point.Visits })
	}
	for i, point := range series {
		if !shouldRenderChartXLabel(i, len(series)) {
			continue
		}
		x := chartX(i, len(series))
		label := point.Day.Format("Jan 02")
		if i == len(series)-1 {
			label = "Today"
		}
		fmt.Fprintf(builder, `<text class="x-label" x="%.0f" y="206">%s</text>`, x-14, html.EscapeString(label))
	}
	builder.WriteString(`</svg></div><div class="chart-legend"><span class="legend-item"><span class="legend-swatch"></span>Pageviews</span><span class="legend-item"><span class="legend-swatch visits"></span>Visits</span></div>`)
}

func shouldRenderChartXLabel(index, total int) bool {
	if total <= maxAnalyticsChartXLabels {
		return true
	}
	if index == total-1 {
		return true
	}
	interval := (total - 1 + (maxAnalyticsChartXLabels - 2)) / (maxAnalyticsChartXLabels - 1)
	return index%interval == 0
}

func chartMax(value int64) int64 {
	if value < 4 {
		return 4
	}
	return value + ((value + 4) / 5)
}

func chartPath(series []dbstore.SiteAnalyticsDay, maxValue int64, value func(dbstore.SiteAnalyticsDay) int64) string {
	var path strings.Builder
	for i, point := range series {
		command := "L"
		if i == 0 {
			command = "M"
		}
		fmt.Fprintf(&path, `%s%.0f %.0f `, command, chartX(i, len(series)), chartY(value(point), maxValue))
	}
	return strings.TrimSpace(path.String())
}

func writeChartPoints(builder *strings.Builder, className string, series []dbstore.SiteAnalyticsDay, maxValue int64, value func(dbstore.SiteAnalyticsDay) int64) {
	for i, point := range series {
		fmt.Fprintf(builder, `<circle class="%s" cx="%.0f" cy="%.0f" r="4"></circle>`, className, chartX(i, len(series)), chartY(value(point), maxValue))
	}
}

func chartX(index, total int) float64 {
	if total <= 1 {
		return 64
	}
	return 80 + (float64(index) * (720 / float64(total-1)))
}

func chartY(value, maxValue int64) float64 {
	if maxValue < 1 {
		maxValue = 1
	}
	return 176 - ((float64(value) / float64(maxValue)) * 140)
}

func formatCompactCount(value int64) string {
	if value < 1000 {
		return formatCount(value)
	}

	whole := value / 1000
	tenth := (value % 1000) / 100
	if tenth == 0 {
		return fmt.Sprintf("%dk", whole)
	}
	return fmt.Sprintf("%d.%dk", whole, tenth)
}

type noDirListingFS struct {
	fs http.FileSystem
}

func (n noDirListingFS) Open(name string) (http.File, error) {
	file, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}

	if !info.IsDir() {
		return file, nil
	}

	indexPath := filepath.ToSlash(filepath.Join(name, "index.html"))
	indexFile, err := n.fs.Open(indexPath)
	if err != nil {
		file.Close()
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return nil, fs.ErrNotExist
		}
		return nil, err
	}
	indexFile.Close()

	return file, nil
}
