package storage

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/safepath"
)

// assetsDirName is the fixed subdirectory, sibling to every vN/, that holds
// a site's uploaded assets (design.md 7.3): <SITE_DIR>/<owner>/<site>/assets/<id>.
// It sits outside every version, so it survives deploys and rollbacks, and
// Phase 5's backup/restore already expects exactly this layout
// (internal/storage/backup.go's assetsObjectPrefix, restore.go's
// RestoreAssets) — this file must not change it.
const assetsDirName = "assets"

// assetIDHeadPeek is how many leading bytes CreateAsset samples to sniff a
// content type. It never affects site content, so 512 (net/http's own
// sniffing window, https://mimesniff.spec.whatwg.org/) is plenty.
const assetIDHeadPeek = 512

var (
	// ErrAssetTooLarge is returned when an upload exceeds limits.MaxFileBytes,
	// whether the client's declared size said so up front or the actual
	// stream did.
	ErrAssetTooLarge = errors.New("asset exceeds the per-file size limit")
	// ErrAssetQuotaExceeded is returned when accepting the upload would put
	// the site's live asset count or total bytes over limits.MaxSiteCount or
	// limits.MaxSiteBytes.
	ErrAssetQuotaExceeded = errors.New("site asset quota exceeded")
	// ErrAssetTypeNotAllowed is returned when the sniffed content does not
	// classify into the allowlist: design.md 7.3's image/*, video/*,
	// audio/*, application/pdf, application/json, text/csv, text/plain,
	// application/octet-stream, plus application/zip and application/gzip
	// (or its sniffed alias application/x-gzip) — extended beyond design
	// 7.3's literal table so a site can offer a plain archive download; see
	// classifyContentType and docs/security-review.md. This is
	// deliberately about the bytes, not the client's declared Content-Type:
	// an HTML file relabeled as text/plain is still refused, because it
	// still sniffs as text/html. An executable and everything else Go's
	// sniffer names specifically (and does not appear above) is refused.
	ErrAssetTypeNotAllowed = errors.New("asset content type is not allowed")
	// ErrAssetNotFound is returned by OpenAsset and DeleteAsset when the id
	// does not name a regular file under the site's assets directory.
	ErrAssetNotFound = errors.New("asset not found")
)

// AssetLimits are the quota and size ceilings CreateAsset enforces. They
// come from configuration (internal/config, outside this package's remit)
// and are passed in by the caller on every call rather than read from
// package state, so a test — or a future per-plan override — never has to
// fight a package-level default.
type AssetLimits struct {
	// MaxFileBytes bounds one upload. Design.md 7.3's default is 25 MiB.
	MaxFileBytes int64
	// MaxSiteBytes bounds a site's total live-asset bytes. Design.md 7.3's
	// default is 500 MiB.
	MaxSiteBytes int64
	// MaxSiteCount bounds a site's total live-asset count. Design.md 7.3's
	// default is 5,000.
	MaxSiteCount int64
}

func (l AssetLimits) validate() error {
	if l.MaxFileBytes <= 0 {
		return fmt.Errorf("asset limits: MaxFileBytes must be positive")
	}
	if l.MaxSiteBytes <= 0 {
		return fmt.Errorf("asset limits: MaxSiteBytes must be positive")
	}
	if l.MaxSiteCount <= 0 {
		return fmt.Errorf("asset limits: MaxSiteCount must be positive")
	}
	return nil
}

// StoredAsset describes what CreateAsset actually wrote: the id it minted,
// the content type it classified the bytes as (not necessarily the
// client's declared type), the byte count it verified while streaming, and
// its SHA-256. Inline reports whether design.md 7.3 wants this type served
// with no Content-Disposition (image/*, video/*, audio/*, application/pdf)
// or with "attachment" (everything else in the allowlist).
type StoredAsset struct {
	ID          string
	ContentType string
	Size        int64
	SHA256      [sha256.Size]byte
	Inline      bool
}

// AssetFileInfo is one entry from a raw directory walk of a site's assets,
// independent of whatever the database's site_assets table believes. It
// carries only what the filesystem itself knows: the id (the filename),
// its size, and its modification time.
type AssetFileInfo struct {
	ID      string
	Size    int64
	ModTime time.Time
}

// CreateAsset validates, sniffs, and streams one upload to
// <site>/assets/<newly-minted-id>, atomically: nothing under assetsDirName
// is visible under a half-written name, and nothing is written at all if
// any check fails. The site must already exist (a site is created by its
// first WriteFiles call); CreateAsset does not create one.
//
// declaredSize is the caller's best guess at the upload's length (e.g. a
// multipart part's Content-Length) and is used only for an early, cheap
// quota rejection before any bytes are read; it is never trusted for the
// actual size or the per-file cap, both of which are enforced against the
// bytes actually read. Pass 0 if unknown.
//
// The quota check (MaxSiteCount, MaxSiteBytes) is computed by walking the
// site's assets directory under the same site lock CreateAsset writes
// under, not by asking a caller-supplied count. This makes it authoritative
// against whatever is actually on disk regardless of what any database
// row believes, at the cost of an O(files) directory read per upload —
// acceptable at the counts design.md 7.3 allows per site (5,000).
func (s *DiskStorage) CreateAsset(user, siteName, declaredContentType string, source io.Reader, declaredSize int64, limits AssetLimits) (StoredAsset, error) {
	if err := validateIdentity(user, siteName); err != nil {
		return StoredAsset{}, err
	}
	if err := limits.validate(); err != nil {
		return StoredAsset{}, err
	}
	if source == nil {
		return StoredAsset{}, fmt.Errorf("asset source is nil")
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.Lock()
	defer mutation.Unlock()

	siteRoot, err := s.openSite(user, siteName, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return StoredAsset{}, fmt.Errorf("site does not exist")
		}
		return StoredAsset{}, fmt.Errorf("open site: %w", err)
	}
	defer siteRoot.Close()

	assetsRoot, err := openRealDir(siteRoot, assetsDirName, true)
	if err != nil {
		return StoredAsset{}, fmt.Errorf("open assets directory: %w", err)
	}
	defer assetsRoot.Close()

	usage, err := assetUsageOnDisk(assetsRoot)
	if err != nil {
		return StoredAsset{}, err
	}
	if usage.count+1 > limits.MaxSiteCount {
		return StoredAsset{}, ErrAssetQuotaExceeded
	}
	if declaredSize > 0 {
		if declaredSize > limits.MaxFileBytes {
			return StoredAsset{}, ErrAssetTooLarge
		}
		if usage.bytes+declaredSize > limits.MaxSiteBytes {
			return StoredAsset{}, ErrAssetQuotaExceeded
		}
	}

	tempName, err := uniqueName(".tmp-asset-")
	if err != nil {
		return StoredAsset{}, err
	}
	file, err := assetsRoot.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return StoredAsset{}, fmt.Errorf("create asset temp file: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = assetsRoot.Remove(tempName)
		}
	}()

	written, storedType, sum, err := writeAssetContent(file, source, declaredContentType, limits.MaxFileBytes)
	closeErr := file.Close()
	if err != nil {
		return StoredAsset{}, err
	}
	if closeErr != nil {
		return StoredAsset{}, fmt.Errorf("close asset temp file: %w", closeErr)
	}

	// Authoritative post-write checks: declaredSize above was only ever an
	// optimistic early exit, and the site-bytes check has to happen again
	// here regardless of whether that branch ran, against what was actually
	// written.
	if usage.bytes+written > limits.MaxSiteBytes {
		return StoredAsset{}, ErrAssetQuotaExceeded
	}

	id, err := allocateAssetID(assetsRoot)
	if err != nil {
		return StoredAsset{}, err
	}
	if err := assetsRoot.Rename(tempName, id); err != nil {
		return StoredAsset{}, fmt.Errorf("publish asset %q: %w", id, err)
	}
	published = true

	return StoredAsset{
		ID:          id,
		ContentType: storedType,
		Size:        written,
		SHA256:      sum,
		Inline:      isInlineContentType(storedType),
	}, nil
}

// writeAssetContent sniffs the first assetIDHeadPeek bytes of source,
// classifies them (refusing anything outside the allowlist —
// classifyContentType), and streams the rest to destination while hashing
// everything written. It
// never writes more than maxFileBytes to destination, returning
// ErrAssetTooLarge the moment source would exceed it.
func writeAssetContent(destination io.Writer, source io.Reader, declaredContentType string, maxFileBytes int64) (int64, string, [sha256.Size]byte, error) {
	headLimit := int64(assetIDHeadPeek)
	if maxFileBytes < headLimit {
		headLimit = maxFileBytes
	}
	head := make([]byte, headLimit)
	n, readErr := io.ReadFull(source, head)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		return 0, "", [sha256.Size]byte{}, fmt.Errorf("read asset: %w", readErr)
	}
	head = head[:n]

	storedType, ok := classifyContentType(http.DetectContentType(head), declaredContentType)
	if !ok {
		return 0, "", [sha256.Size]byte{}, ErrAssetTypeNotAllowed
	}

	hasher := sha256.New()
	multi := io.MultiWriter(destination, hasher)
	if len(head) > 0 {
		if _, err := multi.Write(head); err != nil {
			return 0, "", [sha256.Size]byte{}, fmt.Errorf("write asset: %w", err)
		}
	}

	remaining := maxFileBytes - int64(len(head))
	if remaining < 0 {
		remaining = 0
	}
	limited := &io.LimitedReader{R: source, N: remaining + 1}
	rest, copyErr := io.Copy(multi, limited)
	if copyErr != nil {
		return 0, "", [sha256.Size]byte{}, fmt.Errorf("write asset: %w", copyErr)
	}
	total := int64(len(head)) + rest
	if rest > remaining {
		return 0, "", [sha256.Size]byte{}, ErrAssetTooLarge
	}

	var sum [sha256.Size]byte
	copy(sum[:], hasher.Sum(nil))
	return total, storedType, sum, nil
}

// classifyContentType decides what to store and serve an upload as. The
// sniffed type (net/http.DetectContentType against the actual bytes)
// always wins for anything it recognizes as media or PDF; the client's
// declared type is only ever consulted to disambiguate JSON or CSV from
// generic sniffed plain text, and only after the sniff has already ruled
// out anything script-capable. application/zip and application/gzip (or
// its sniffed alias application/x-gzip) are allowed and served as
// attachments — a site may legitimately offer a plain archive download,
// and an archive is inert once served with Content-Disposition: attachment
// (design.md 7.4) rather than executed. An executable, and everything else
// the sniffer names specifically that is not on this list, is refused.
// Anything the sniffer calls text/html, text/xml, or any other type
// outside the allowlist is refused outright — there is no path through
// this function for a file to be stored as, or later served as, text/html.
func classifyContentType(sniffed, declared string) (string, bool) {
	base := mediaTypeBase(sniffed)
	switch {
	case strings.HasPrefix(base, "image/"), strings.HasPrefix(base, "video/"), strings.HasPrefix(base, "audio/"):
		return base, true
	case base == "application/pdf":
		return base, true
	case base == "text/plain":
		switch mediaTypeBase(declared) {
		case "application/json":
			return "application/json", true
		case "text/csv":
			return "text/csv", true
		default:
			return "text/plain", true
		}
	case base == "application/octet-stream":
		return "application/octet-stream", true
	case base == "application/zip":
		return base, true
	case base == "application/x-gzip", base == "application/gzip":
		// net/http.DetectContentType only ever names the gzip signature
		// "application/x-gzip" (never the IANA-registered "application/gzip"),
		// but both name the same bytes; either sniffed value is accepted and
		// stored as itself rather than forced to one canonical spelling.
		return base, true
	default:
		return "", false
	}
}

// mediaTypeBase strips parameters (";charset=..." and the like) and
// lower-cases a content type. mime.ParseMediaType rejects a handful of
// inputs http.DetectContentType and multipart clients still produce (an
// empty string, a bare type with no parameters in some Go versions' strict
// mode); the manual fallback covers those instead of misclassifying them
// as no type at all.
func mediaTypeBase(contentType string) string {
	base, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		base = strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
	}
	return strings.ToLower(base)
}

// isInlineContentType reports whether design.md 7.3 wants this stored type
// served with no Content-Disposition at all (true) or "attachment"
// (false). Only classifyContentType's output should ever reach this.
func isInlineContentType(contentType string) bool {
	base := mediaTypeBase(contentType)
	return strings.HasPrefix(base, "image/") || strings.HasPrefix(base, "video/") || strings.HasPrefix(base, "audio/") || base == "application/pdf"
}

// allocateAssetID mints a fresh, collision-checked id: a random,
// RFC 4122-shaped v4 UUID string. It is generated here (not left to the
// database's own DEFAULT gen_random_uuid()) so the same string names the
// on-disk file and the site_assets row the caller inserts afterward — the
// canonical hyphenated form round-trips unchanged through Postgres's uuid
// type, which a plain hex string would not (Postgres would return it
// re-hyphenated on the next read, no longer matching the filename).
func allocateAssetID(assetsRoot *os.Root) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		candidate, err := newAssetID()
		if err != nil {
			return "", err
		}
		if _, statErr := assetsRoot.Lstat(candidate); errors.Is(statErr, os.ErrNotExist) {
			return candidate, nil
		} else if statErr != nil {
			return "", fmt.Errorf("inspect asset id %q: %w", candidate, statErr)
		}
	}
	return "", fmt.Errorf("could not allocate a unique asset id")
}

func newAssetID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate asset id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// OpenAsset opens one asset's raw bytes for reading. It does not hold any
// lock for the lifetime of the returned ReadCloser — matching how the rest
// of this package serves file content — so a concurrent DeleteAsset can
// unlink the file while a read is in flight. On the POSIX filesystem this
// package targets, that leaves the open file descriptor perfectly
// readable until Close; the caller only ever sees the delete on its next,
// separate call.
func (s *DiskStorage) OpenAsset(user, siteName, id string) (io.ReadCloser, os.FileInfo, error) {
	if err := validateIdentity(user, siteName); err != nil {
		return nil, nil, err
	}
	if err := safepath.ValidateSegment(id); err != nil {
		return nil, nil, ErrAssetNotFound
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	lock := s.siteLock(user, siteName)
	lock.RLock()
	defer lock.RUnlock()

	siteRoot, err := s.openSite(user, siteName, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, ErrAssetNotFound
		}
		return nil, nil, fmt.Errorf("open site: %w", err)
	}
	defer siteRoot.Close()

	assetsRoot, err := openRealDir(siteRoot, assetsDirName, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, ErrAssetNotFound
		}
		return nil, nil, fmt.Errorf("open assets directory: %w", err)
	}
	defer assetsRoot.Close()

	if err := requireRealFile(assetsRoot, id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, ErrAssetNotFound
		}
		return nil, nil, fmt.Errorf("inspect asset %q: %w", id, err)
	}
	file, err := assetsRoot.Open(id)
	if err != nil {
		return nil, nil, fmt.Errorf("open asset %q: %w", id, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("stat asset %q: %w", id, err)
	}
	return file, info, nil
}

// DeleteAsset removes one asset's on-disk bytes. It does not touch the
// database's site_assets row — the caller is expected to pair this with
// db.SoftDeleteAsset (or call it first: freeing disk space before the
// audit-trail row disappears from listings is the safer order on a
// partial failure, since a listed asset whose file is already gone is
// simply a broken link, while a file that outlives its row leaks quota
// forever).
func (s *DiskStorage) DeleteAsset(user, siteName, id string) error {
	if err := validateIdentity(user, siteName); err != nil {
		return err
	}
	if err := safepath.ValidateSegment(id); err != nil {
		return ErrAssetNotFound
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	lock := s.siteLock(user, siteName)
	lock.Lock()
	defer lock.Unlock()

	siteRoot, err := s.openSite(user, siteName, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrAssetNotFound
		}
		return fmt.Errorf("open site: %w", err)
	}
	defer siteRoot.Close()

	assetsRoot, err := openRealDir(siteRoot, assetsDirName, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrAssetNotFound
		}
		return fmt.Errorf("open assets directory: %w", err)
	}
	defer assetsRoot.Close()

	if err := requireRealFile(assetsRoot, id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrAssetNotFound
		}
		return fmt.Errorf("inspect asset %q: %w", id, err)
	}
	if err := assetsRoot.Remove(id); err != nil {
		return fmt.Errorf("delete asset %q: %w", id, err)
	}
	return nil
}

// ListAssets walks a site's assets directory directly, independent of
// whatever the database's site_assets table believes. A site with no
// assets directory yet (nothing uploaded) returns an empty list, not an
// error.
func (s *DiskStorage) ListAssets(user, siteName string) ([]AssetFileInfo, error) {
	if err := validateIdentity(user, siteName); err != nil {
		return nil, err
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	lock := s.siteLock(user, siteName)
	lock.RLock()
	defer lock.RUnlock()

	siteRoot, err := s.openSite(user, siteName, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open site: %w", err)
	}
	defer siteRoot.Close()

	assetsRoot, err := openRealDir(siteRoot, assetsDirName, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open assets directory: %w", err)
	}
	defer assetsRoot.Close()

	return listAssetFiles(assetsRoot)
}

type assetUsage struct {
	count int64
	bytes int64
}

// assetUsageOnDisk sums assetUsage from a directory listing already open
// under the site lock. Callers already holding assetsRoot use this
// directly instead of ListAssets, which would re-derive and re-open it.
func assetUsageOnDisk(assetsRoot *os.Root) (assetUsage, error) {
	files, err := listAssetFiles(assetsRoot)
	if err != nil {
		return assetUsage{}, err
	}
	var usage assetUsage
	for _, f := range files {
		usage.count++
		usage.bytes += f.Size
	}
	return usage, nil
}

// listAssetFiles lists the regular, non-hidden files directly inside
// assetsRoot. Uploads-in-progress stage under a "." prefixed temp name
// (writeAssetContent's tempName) and are skipped, the same way any other
// leftover dotfile would be.
func listAssetFiles(assetsRoot *os.Root) ([]AssetFileInfo, error) {
	directory, err := assetsRoot.Open(".")
	if err != nil {
		return nil, fmt.Errorf("open assets directory: %w", err)
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return nil, fmt.Errorf("list assets directory: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close assets directory: %w", closeErr)
	}

	var files []AssetFileInfo
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat asset %q: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, AssetFileInfo{ID: name, Size: info.Size(), ModTime: info.ModTime()})
	}
	return files, nil
}
