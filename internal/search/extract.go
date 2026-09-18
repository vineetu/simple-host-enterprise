package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const (
	maxHTMLPages          = 1_000
	maxHTMLBytesPerPage   = 2 << 20
	maxHTMLBytesPerSite   = 64 << 20
	maxNormalizedSiteText = 10 << 20
	maxTitleBytes         = 512
	maxDescriptionBytes   = 2 << 10
	maxHeadingsBytes      = 32 << 10
	maxBodyBytes          = 256 << 10
	maxDiagnostics        = 64
	extractionDeadline    = 30 * time.Second
)

// Document is the normalized, bounded search representation of one HTML page.
// It deliberately contains no original HTML.
type Document struct {
	PagePath    string
	URLPath     string
	Title       string
	Description string
	Headings    string
	BodyText    string
}

// TruncationKind identifies the limit which made extraction partial.
type TruncationKind string

const (
	TruncationPageCount     TruncationKind = "page_count"
	TruncationPageInput     TruncationKind = "page_input_bytes"
	TruncationSiteInput     TruncationKind = "site_input_bytes"
	TruncationSiteOutput    TruncationKind = "site_output_bytes"
	TruncationTitle         TruncationKind = "title_bytes"
	TruncationDescription   TruncationKind = "description_bytes"
	TruncationHeadings      TruncationKind = "headings_bytes"
	TruncationBody          TruncationKind = "body_bytes"
	TruncationDeadline      TruncationKind = "deadline"
	TruncationMalformedHTML TruncationKind = "malformed_html"
)

// Truncation records a bounded, non-fatal extraction diagnostic. PagePath is
// empty for a site-wide condition.
type Truncation struct {
	PagePath string
	Kind     TruncationKind
}

// Metadata describes the work retained in an extraction result.
type Metadata struct {
	Partial            bool
	InputBytes         int64
	OutputBytes        int64
	Truncations        []Truncation
	TruncationsOmitted int
}

// Result contains normalized page documents and non-fatal partial-coverage
// metadata.
type Result struct {
	Documents []Document
	Metadata  Metadata
}

type extractionLimits struct {
	pages            int
	pageInputBytes   int64
	siteInputBytes   int64
	siteOutputBytes  int64
	titleBytes       int
	descriptionBytes int
	headingsBytes    int
	bodyBytes        int
	diagnostics      int
	deadline         time.Duration
}

var defaultExtractionLimits = extractionLimits{
	pages:            maxHTMLPages,
	pageInputBytes:   maxHTMLBytesPerPage,
	siteInputBytes:   maxHTMLBytesPerSite,
	siteOutputBytes:  maxNormalizedSiteText,
	titleBytes:       maxTitleBytes,
	descriptionBytes: maxDescriptionBytes,
	headingsBytes:    maxHeadingsBytes,
	bodyBytes:        maxBodyBytes,
	diagnostics:      maxDiagnostics,
	deadline:         extractionDeadline,
}

var errExtractionStopped = errors.New("extraction stopped at a configured limit")

// SiteBase returns the address under which a site's pages are reachable,
// ending in "/": a page's URLPath is that address plus the page's escaped
// path. Nil means the relative long path on the base host, which is what the
// index held before per-owner subdomains; after the subdomain cutover the
// server passes a function that returns the absolute short address instead.
type SiteBase func(owner, site string) string

// Extract walks root deterministically and extracts bounded text from regular
// .html and .htm files, addressing pages under the relative long path. Limit
// exhaustion is returned as partial metadata, not as an error. Cancellation of
// the caller's context is returned to the caller.
func Extract(ctx context.Context, root *os.Root, owner, site string) (Result, error) {
	return extractWithLimits(ctx, root, owner, site, nil, defaultExtractionLimits)
}

// NewExtractor returns Extract with the page address prefix decided by base.
func NewExtractor(base SiteBase) func(context.Context, *os.Root, string, string) (Result, error) {
	return func(ctx context.Context, root *os.Root, owner, site string) (Result, error) {
		return extractWithLimits(ctx, root, owner, site, base, defaultExtractionLimits)
	}
}

func extractWithLimits(ctx context.Context, root *os.Root, owner, site string, base SiteBase, configured extractionLimits) (Result, error) {
	var result Result
	if root == nil {
		return result, fmt.Errorf("extract HTML: nil root")
	}
	if err := configured.validate(); err != nil {
		return result, err
	}

	workCtx, cancel := context.WithTimeout(ctx, configured.deadline)
	defer cancel()
	state := extractionState{
		parent: ctx,
		work:   workCtx,
		limits: configured,
		result: &result,
	}

	pagePaths, err := state.htmlPaths(root)
	if err != nil {
		if errors.Is(err, errExtractionStopped) {
			return result, nil
		}
		return result, err
	}

	for _, pagePath := range pagePaths {
		if err := state.checkContext(); err != nil {
			if errors.Is(err, errExtractionStopped) {
				break
			}
			return result, err
		}

		input, stopAfterPage, includePage, err := state.readPage(root, pagePath)
		if err != nil {
			if errors.Is(err, errExtractionStopped) {
				break
			}
			return result, err
		}
		if !includePage {
			if stopAfterPage {
				break
			}
			continue
		}
		if err := state.checkContext(); err != nil {
			if errors.Is(err, errExtractionStopped) {
				break
			}
			return result, err
		}

		node, err := html.Parse(bytes.NewReader(input))
		input = nil
		if err != nil {
			state.addTruncation(pagePath, TruncationMalformedHTML)
			if stopAfterPage {
				break
			}
			continue
		}
		if err := state.checkContext(); err != nil {
			if errors.Is(err, errExtractionStopped) {
				break
			}
			return result, err
		}

		fields := newPageFields(configured)
		if err := state.walkNode(node, fields, false, false, false); err != nil {
			if errors.Is(err, errExtractionStopped) {
				break
			}
			return result, err
		}
		if err := state.checkContext(); err != nil {
			if errors.Is(err, errExtractionStopped) {
				break
			}
			return result, err
		}
		state.recordFieldTruncations(pagePath, fields)

		document := Document{
			PagePath:    pagePath,
			URLPath:     pageURL(base, owner, site, pagePath),
			Title:       fields.title.String(),
			Description: fields.description.String(),
			Headings:    fields.headings.String(),
			BodyText:    fields.body.String(),
		}
		outputStopped := state.fitOutput(pagePath, &document)
		result.Documents = append(result.Documents, document)
		if stopAfterPage || outputStopped {
			break
		}
	}

	return result, nil
}

func (l extractionLimits) validate() error {
	if l.pages < 0 || l.pageInputBytes < 0 || l.siteInputBytes < 0 || l.siteOutputBytes < 0 ||
		l.titleBytes < 0 || l.descriptionBytes < 0 || l.headingsBytes < 0 || l.bodyBytes < 0 ||
		l.diagnostics < 0 || l.deadline <= 0 {
		return fmt.Errorf("extract HTML: limits must be non-negative and deadline must be positive")
	}
	return nil
}

type extractionState struct {
	parent context.Context
	work   context.Context
	limits extractionLimits
	result *Result
}

func (s *extractionState) checkContext() error {
	if err := s.work.Err(); err != nil {
		if parentErr := s.parent.Err(); parentErr != nil {
			return parentErr
		}
		s.addTruncation("", TruncationDeadline)
		return errExtractionStopped
	}
	return nil
}

func (s *extractionState) addTruncation(pagePath string, kind TruncationKind) {
	s.result.Metadata.Partial = true
	if len(s.result.Metadata.Truncations) < s.limits.diagnostics {
		s.result.Metadata.Truncations = append(s.result.Metadata.Truncations, Truncation{
			PagePath: pagePath,
			Kind:     kind,
		})
		return
	}
	s.result.Metadata.TruncationsOmitted++
}

func (s *extractionState) htmlPaths(root *os.Root) ([]string, error) {
	pagePaths := make([]string, 0, min(s.limits.pages, 32))
	err := fs.WalkDir(root.FS(), ".", func(pagePath string, entry fs.DirEntry, walkErr error) error {
		if err := s.checkContext(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if pagePath == "." || entry.IsDir() || !isHTMLPath(pagePath) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if len(pagePaths) >= s.limits.pages {
			s.addTruncation("", TruncationPageCount)
			return fs.SkipAll
		}
		pagePaths = append(pagePaths, pagePath)
		return s.checkContext()
	})
	if err != nil {
		return pagePaths, fmt.Errorf("walk HTML files: %w", err)
	}
	return pagePaths, nil
}

func isHTMLPath(name string) bool {
	extension := path.Ext(name)
	return strings.EqualFold(extension, ".html") || strings.EqualFold(extension, ".htm")
}

func (s *extractionState) readPage(root *os.Root, pagePath string) ([]byte, bool, bool, error) {
	file, err := root.Open(pagePath)
	if err != nil {
		return nil, false, false, fmt.Errorf("open HTML page %q: %w", pagePath, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, false, fmt.Errorf("inspect HTML page %q: %w", pagePath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, false, fmt.Errorf("HTML page %q is not a regular file", pagePath)
	}
	if err := s.checkContext(); err != nil {
		return nil, false, false, err
	}

	remainingSite := s.limits.siteInputBytes - s.result.Metadata.InputBytes
	if remainingSite == 0 && info.Size() > 0 {
		s.addTruncation("", TruncationSiteInput)
		return nil, true, false, nil
	}
	readLimit := min(s.limits.pageInputBytes, remainingSite)
	stopAfterPage := false
	if info.Size() > readLimit {
		if s.limits.pageInputBytes <= remainingSite {
			s.addTruncation(pagePath, TruncationPageInput)
		}
		if remainingSite <= s.limits.pageInputBytes {
			s.addTruncation("", TruncationSiteInput)
			stopAfterPage = true
		}
	}

	content, err := io.ReadAll(io.LimitReader(file, readLimit))
	if err != nil {
		return nil, false, false, fmt.Errorf("read HTML page %q: %w", pagePath, err)
	}
	s.result.Metadata.InputBytes += int64(len(content))
	if err := s.checkContext(); err != nil {
		return nil, false, false, err
	}
	return content, stopAfterPage, true, nil
}

type pageFields struct {
	titleFound       bool
	descriptionFound bool
	title            *textAccumulator
	description      *textAccumulator
	headings         *textAccumulator
	body             *textAccumulator
}

func newPageFields(configured extractionLimits) *pageFields {
	return &pageFields{
		title:       newTextAccumulator(configured.titleBytes),
		description: newTextAccumulator(configured.descriptionBytes),
		headings:    newTextAccumulator(configured.headingsBytes),
		body:        newTextAccumulator(configured.bodyBytes),
	}
}

func (s *extractionState) walkNode(node *html.Node, fields *pageFields, inBody, inHeading, inTitle bool) error {
	if err := s.checkContext(); err != nil {
		return err
	}

	switch node.Type {
	case html.ElementNode:
		tag := strings.ToLower(node.Data)
		if skippedElement(tag) || hiddenElement(node) {
			return nil
		}

		body := inBody || tag == "body"
		heading := inHeading || isHeading(tag)
		title := inTitle
		if tag == "title" && !fields.titleFound {
			fields.titleFound = true
			title = true
		} else if tag == "title" {
			title = false
		}
		if tag == "meta" && !fields.descriptionFound {
			if description, ok := metaDescription(node); ok {
				fields.descriptionFound = true
				fields.description.Add(description)
			}
		}

		block := body && isBlockElement(tag)
		if block {
			fields.body.Separate()
		}
		if tag != "title" && isHeading(tag) {
			fields.headings.Separate()
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if err := s.walkNode(child, fields, body, heading, title); err != nil {
				return err
			}
		}
		if block {
			fields.body.Separate()
		}
		if tag != "title" && isHeading(tag) {
			fields.headings.Separate()
		}
		return nil

	case html.TextNode:
		if inTitle {
			fields.title.Add(node.Data)
		}
		if inHeading {
			fields.headings.Add(node.Data)
		}
		if inBody {
			fields.body.Add(node.Data)
		}
	}

	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if err := s.walkNode(child, fields, inBody, inHeading, inTitle); err != nil {
			return err
		}
	}
	return nil
}

func skippedElement(tag string) bool {
	switch tag {
	case "script", "style", "template":
		return true
	default:
		return false
	}
}

func hiddenElement(node *html.Node) bool {
	for _, attribute := range node.Attr {
		if strings.EqualFold(attribute.Key, "hidden") {
			return true
		}
		if strings.EqualFold(attribute.Key, "aria-hidden") && strings.EqualFold(strings.TrimSpace(attribute.Val), "true") {
			return true
		}
	}
	return false
}

func metaDescription(node *html.Node) (string, bool) {
	var name, content string
	for _, attribute := range node.Attr {
		switch {
		case strings.EqualFold(attribute.Key, "name"):
			name = strings.TrimSpace(attribute.Val)
		case strings.EqualFold(attribute.Key, "content"):
			content = attribute.Val
		}
	}
	return content, strings.EqualFold(name, "description")
}

func isHeading(tag string) bool {
	return len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6'
}

func isBlockElement(tag string) bool {
	switch tag {
	case "address", "article", "aside", "blockquote", "br", "dd", "div", "dl", "dt", "fieldset",
		"figcaption", "figure", "footer", "form", "h1", "h2", "h3", "h4", "h5", "h6", "header",
		"hr", "li", "main", "nav", "ol", "p", "pre", "section", "table", "tbody", "td", "tfoot",
		"th", "thead", "tr", "ul":
		return true
	default:
		return false
	}
}

func (s *extractionState) recordFieldTruncations(pagePath string, fields *pageFields) {
	if fields.title.Truncated() {
		s.addTruncation(pagePath, TruncationTitle)
	}
	if fields.description.Truncated() {
		s.addTruncation(pagePath, TruncationDescription)
	}
	if fields.headings.Truncated() {
		s.addTruncation(pagePath, TruncationHeadings)
	}
	if fields.body.Truncated() {
		s.addTruncation(pagePath, TruncationBody)
	}
}

func (s *extractionState) fitOutput(pagePath string, document *Document) bool {
	remaining := s.limits.siteOutputBytes - s.result.Metadata.OutputBytes
	fields := []*string{&document.Title, &document.Description, &document.Headings, &document.BodyText}
	for index, field := range fields {
		if int64(len(*field)) <= remaining {
			s.result.Metadata.OutputBytes += int64(len(*field))
			remaining -= int64(len(*field))
			continue
		}

		*field = truncateUTF8(*field, remaining)
		s.result.Metadata.OutputBytes += int64(len(*field))
		for _, remainder := range fields[index+1:] {
			*remainder = ""
		}
		s.addTruncation(pagePath, TruncationSiteOutput)
		return true
	}
	return false
}

type textAccumulator struct {
	maximum      int
	builder      strings.Builder
	pendingSpace bool
	truncated    bool
}

func newTextAccumulator(maximum int) *textAccumulator {
	return &textAccumulator{maximum: maximum}
}

func (a *textAccumulator) Add(value string) {
	if a.truncated {
		return
	}
	for _, character := range value {
		if unicode.IsSpace(character) {
			if a.builder.Len() > 0 {
				a.pendingSpace = true
			}
			continue
		}
		if unicode.IsControl(character) {
			continue
		}

		characterBytes := utf8.RuneLen(character)
		needed := characterBytes
		if a.pendingSpace {
			needed++
		}
		if a.builder.Len()+needed > a.maximum {
			a.truncated = true
			return
		}
		if a.pendingSpace {
			a.builder.WriteByte(' ')
			a.pendingSpace = false
		}
		a.builder.WriteRune(character)
	}
}

func (a *textAccumulator) Separate() {
	if a.builder.Len() > 0 {
		a.pendingSpace = true
	}
}

func (a *textAccumulator) String() string {
	return a.builder.String()
}

func (a *textAccumulator) Truncated() bool {
	return a.truncated
}

func truncateUTF8(value string, maximum int64) string {
	if maximum <= 0 {
		return ""
	}
	if int64(len(value)) <= maximum {
		return value
	}
	end := int(maximum)
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func pageURL(siteBase SiteBase, owner, site, pagePath string) string {
	if siteBase == nil {
		siteBase = longSitePath
	}
	base := siteBase(owner, site)
	if path.Base(pagePath) == "index.html" {
		directory := path.Dir(pagePath)
		if directory == "." {
			return base
		}
		return base + escapeArchivePath(directory) + "/"
	}
	return base + escapeArchivePath(pagePath)
}

// longSitePath is the pre-cutover site address: the relative long path on the
// base host, one escaped segment per name.
func longSitePath(owner, site string) string {
	return "/sites/" + url.PathEscape(owner) + "/" + url.PathEscape(site) + "/"
}

func escapeArchivePath(archivePath string) string {
	segments := strings.Split(archivePath, "/")
	for index := range segments {
		segments[index] = url.PathEscape(segments[index])
	}
	return strings.Join(segments, "/")
}
