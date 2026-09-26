package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"strings"
	"sync"

	db "github.com/vsriram/simple-host/internal/db"
)

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
	// the site's live asset count or total bytes over its limits. The check
	// is the database's (db.CreateAssetWithinQuota), so this is that error.
	ErrAssetQuotaExceeded = db.ErrAssetQuotaExceeded
	// ErrAssetTypeNotAllowed is returned when the sniffed content does not
	// classify into the allowlist: image/*, video/*, audio/*,
	// application/pdf, application/json, text/csv, text/plain,
	// application/octet-stream, plus application/zip and application/gzip
	// (or its sniffed alias application/x-gzip) — extended to also allow a
	// site to offer a plain archive download; see classifyContentType and
	// docs/security-review.md. This is
	// deliberately about the bytes, not the client's declared Content-Type:
	// an HTML file relabeled as text/plain is still refused, because it
	// still sniffs as text/html. An executable and everything else Go's
	// sniffer names specifically (and does not appear above) is refused.
	ErrAssetTypeNotAllowed = errors.New("asset content type is not allowed")
	// ErrAssetNotFound is returned by OpenAsset when the id is malformed or
	// names no object.
	ErrAssetNotFound = errors.New("asset not found")
)

// AssetLimits are the quota and size ceilings CreateAsset enforces. They
// come from configuration (internal/config, outside this package's remit)
// and are passed in by the caller on every call rather than read from
// package state, so a test — or a future per-plan override — never has to
// fight a package-level default.
type AssetLimits struct {
	// MaxFileBytes bounds one upload. The default is 25 MiB.
	MaxFileBytes int64
	// MaxSiteBytes bounds a site's total live-asset bytes. The default is
	// 500 MiB.
	MaxSiteBytes int64
	// MaxSiteCount bounds a site's total live-asset count. The default is
	// 5,000.
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
// its SHA-256. Inline reports whether this type is served with no
// Content-Disposition (image/*, video/*, audio/*, application/pdf) or with
// "attachment" (everything else in the allowlist).
type StoredAsset struct {
	ID          string
	ContentType string
	Size        int64
	SHA256      [sha256.Size]byte
	Inline      bool
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
// rather than executed. An executable, and everything else
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

// isInlineContentType reports whether this stored type is served with no
// Content-Disposition at all (true) or "attachment" (false). Only
// classifyContentType's output should ever reach this.
func isInlineContentType(contentType string) bool {
	base := mediaTypeBase(contentType)
	return strings.HasPrefix(base, "image/") || strings.HasPrefix(base, "video/") || strings.HasPrefix(base, "audio/") || base == "application/pdf"
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

// CreateAsset validates, sniffs and uploads one asset under a newly minted
// id, and returns what it stored. It enforces the per-file cap and the type
// allowlist against the bytes actually read. The per-site quota is the
// caller's to enforce, in the transaction that records the asset row
// (db.CreateAssetWithinQuota), because only the database sees every replica's
// uploads; on a quota refusal the caller deletes the object again.
func (s *Store) CreateAsset(ctx context.Context, siteID, declaredContentType string, source io.Reader, limits AssetLimits) (StoredAsset, error) {
	if err := limits.validate(); err != nil {
		return StoredAsset{}, err
	}
	if source == nil {
		return StoredAsset{}, fmt.Errorf("asset source is nil")
	}
	if _, err := SitePrefix(siteID); err != nil {
		return StoredAsset{}, err
	}
	var buffer bytes.Buffer
	written, storedType, sum, err := writeAssetContent(&buffer, source, declaredContentType, limits.MaxFileBytes)
	if err != nil {
		return StoredAsset{}, err
	}
	id, err := newAssetID()
	if err != nil {
		return StoredAsset{}, err
	}
	key, err := AssetKey(siteID, id)
	if err != nil {
		return StoredAsset{}, err
	}
	if err := s.objects.Put(ctx, key, buffer.Bytes(), storedType); err != nil {
		return StoredAsset{}, err
	}
	return StoredAsset{
		ID:          id,
		ContentType: storedType,
		Size:        written,
		SHA256:      sum,
		Inline:      isInlineContentType(storedType),
	}, nil
}

// AssetLease is one asset's bytes, pinned in the local cache until Close.
// File is seekable, so the caller can answer Range requests from it.
type AssetLease struct {
	File    *os.File
	release func()
	once    sync.Once
	err     error
}

func (l *AssetLease) Close() error {
	l.once.Do(func() {
		l.err = l.File.Close()
		l.release()
	})
	return l.err
}

// OpenAsset returns one asset's bytes, fetching it into the cache on a miss.
// maxBytes bounds the fetch (the configured per-file cap). wantSHA256 is the
// digest the asset's row recorded; a fetched object that does not match it is
// refused rather than served. The caller must already have found the live
// asset row: a deleted asset's object may still be cached here.
func (s *Store) OpenAsset(ctx context.Context, siteID, id string, maxBytes int64, wantSHA256 []byte) (*AssetLease, error) {
	key, err := AssetKey(siteID, id)
	if err != nil {
		return nil, err
	}
	name := siteID + ".a." + id
	entry, err := s.cache.acquire(ctx, name, func(ctx context.Context, temporary string) (int64, error) {
		body, err := s.objects.Get(ctx, key, maxBytes)
		if errors.Is(err, ErrObjectNotFound) {
			return 0, ErrAssetNotFound
		}
		if err != nil {
			return 0, err
		}
		if sum := sha256.Sum256(body); !bytes.Equal(sum[:], wantSHA256) {
			return 0, fmt.Errorf("asset %s does not match its recorded sha256", key)
		}
		file, err := s.cache.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return 0, err
		}
		_, writeErr := file.Write(body)
		closeErr := file.Close()
		if writeErr != nil {
			return 0, writeErr
		}
		if closeErr != nil {
			return 0, closeErr
		}
		return int64(len(body)) + cacheBlockOverhead, nil
	})
	if err != nil {
		return nil, err
	}
	file, err := s.cache.root.Open(name)
	if err != nil {
		s.cache.release(entry)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrAssetNotFound
		}
		return nil, err
	}
	return &AssetLease{File: file, release: func() { s.cache.release(entry) }}, nil
}

// DeleteAsset removes one asset's object. A missing object is not an error.
// A copy still in some replica's cache is unreachable once the row is
// soft-deleted, because serving looks the row up first.
func (s *Store) DeleteAsset(ctx context.Context, siteID, id string) error {
	key, err := AssetKey(siteID, id)
	if err != nil {
		return err
	}
	return s.objects.Delete(ctx, key)
}
