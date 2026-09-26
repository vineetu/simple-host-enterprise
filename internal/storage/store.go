package storage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"sync"
	"time"

	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/safepath"
)

// Store is the site store: immutable objects in the bucket, the database
// deciding which of them are live, and a bounded pod-local cache to serve
// from. It holds no state a second replica would need to share, so any number
// of replicas can run against one bucket and one database.
//
// A deploy uploads the new version's archive (PutVersion) before the
// transaction that makes it active commits; serving resolves the active
// version from the database on every request (OpenCurrent) and reads the
// archive through the cache. A version that stops being referenced is queued
// for deletion in the same transaction (db.RetireObjects) and deleted by
// Sweep after a grace period.
type Store struct {
	objects Objects
	index   SiteIndex
	cache   *cache
}

// SiteIndex answers which sites exist and which version of each is live. The
// database is the only production implementation (NewDBIndex); tests supply
// a map.
type SiteIndex interface {
	// ServingSite returns the site's id and active version, or ok=false when
	// there is no such site.
	ServingSite(ctx context.Context, owner, site string) (siteID string, version int, ok bool, err error)
	// Owners lists every owner username that has at least one site.
	Owners(ctx context.Context) ([]string, error)
	// Sites lists owner's site names.
	Sites(ctx context.Context, owner string) ([]string, error)
}

// Options configures New.
type Options struct {
	Objects Objects
	Index   SiteIndex
	// CacheDir is the pod-local directory the cache lives in. It is emptied
	// on start.
	CacheDir string
	// CacheMaxBytes bounds the unpinned cache.
	CacheMaxBytes int64
}

// indexTimeout bounds a site lookup made on behalf of a caller that has no
// request context to pass (the host gate's resolution helpers).
const indexTimeout = 5 * time.Second

func New(options Options) (*Store, error) {
	if options.Objects == nil || options.Index == nil {
		return nil, errors.New("storage: objects and index are required")
	}
	cache, err := openCache(options.CacheDir, options.CacheMaxBytes)
	if err != nil {
		return nil, err
	}
	return &Store{objects: options.Objects, index: options.Index, cache: cache}, nil
}

func (s *Store) Close() error { return s.cache.close() }

// Objects exposes the bucket for the operator subcommands.
func (s *Store) Objects() Objects { return s.objects }

// Ping proves the bucket is reachable, for readiness.
func (s *Store) Ping(ctx context.Context) error { return s.objects.Ping(ctx) }

// PutVersionResult is what PutVersion stored: the total bytes of the files,
// and the size of the compressed archive that holds them, which is what the
// owner's storage quota counts (versions.size_bytes).
type PutVersionResult struct {
	FileBytes   int64
	StoredBytes int64
}

// PutVersion uploads a deploy's files as version's archive. Call it before
// committing the transaction that records the version, so a committed
// version always has its object. Uploading the same (site, version) again
// replaces an object no committed row refers to: version numbers are
// allocated under the site's advisory lock, so a number is only reused after
// the transaction that first took it rolled back.
func (s *Store) PutVersion(ctx context.Context, siteID string, version int, files map[string][]byte) (PutVersionResult, error) {
	key, err := VersionKey(siteID, version)
	if err != nil {
		return PutVersionResult{}, err
	}
	archive, totals, err := buildVersionArchive(files)
	if err != nil {
		return PutVersionResult{}, err
	}
	if err := s.objects.Put(ctx, key, archive, "application/gzip"); err != nil {
		return PutVersionResult{}, err
	}
	return PutVersionResult{FileBytes: totals.bytes, StoredBytes: int64(len(archive))}, nil
}

// DeleteVersionObject removes an uploaded version whose transaction is known
// not to have committed. Best effort: a leftover is unreferenced and harmless.
func (s *Store) DeleteVersionObject(ctx context.Context, siteID string, version int) error {
	key, err := VersionKey(siteID, version)
	if err != nil {
		return err
	}
	return s.objects.Delete(ctx, key)
}

// VersionLease is one version, unpacked in the local cache and pinned there
// until Close. Everything beneath it is read through an *os.Root, so a
// symlink or ".." can never reach outside the version.
type VersionLease struct {
	root    *os.Root
	release func()
	once    sync.Once
	err     error
}

func (l *VersionLease) FS() fs.FS      { return l.root.FS() }
func (l *VersionLease) Root() *os.Root { return l.root }

func (l *VersionLease) Close() error {
	l.once.Do(func() {
		l.err = l.root.Close()
		l.release()
	})
	return l.err
}

// OpenVersion returns exactly one immutable version, fetching and unpacking
// it into the cache on a miss. A version with no object reports
// fs.ErrNotExist.
func (s *Store) OpenVersion(ctx context.Context, siteID string, version int) (*VersionLease, error) {
	key, err := VersionKey(siteID, version)
	if err != nil {
		return nil, err
	}
	name := siteID + ".v" + strconv.Itoa(version)
	entry, err := s.cache.acquire(ctx, name, func(ctx context.Context, temporary string) (int64, error) {
		body, err := s.objects.Get(ctx, key, maxVersionObjectBytes)
		if errors.Is(err, ErrObjectNotFound) {
			return 0, fmt.Errorf("%w: %w", fs.ErrNotExist, err)
		}
		if err != nil {
			return 0, err
		}
		if err := s.cache.root.Mkdir(temporary, 0o755); err != nil {
			return 0, err
		}
		destination, err := s.cache.root.OpenRoot(temporary)
		if err != nil {
			return 0, err
		}
		totals, err := scanVersionArchive(bytes.NewReader(body), destination)
		closeErr := destination.Close()
		if err != nil {
			return 0, fmt.Errorf("unpack %s: %w", key, err)
		}
		if closeErr != nil {
			return 0, closeErr
		}
		return totals.bytes + (totals.count+1)*cacheBlockOverhead, nil
	})
	if err != nil {
		return nil, err
	}
	root, err := s.cache.root.OpenRoot(name)
	if err != nil {
		s.cache.release(entry)
		return nil, err
	}
	return &VersionLease{root: root, release: func() { s.cache.release(entry) }}, nil
}

// OpenCurrent returns the version the database says owner's site is serving.
// A missing site reports fs.ErrNotExist.
func (s *Store) OpenCurrent(ctx context.Context, owner, site string) (*VersionLease, error) {
	if err := validateIdentity(owner, site); err != nil {
		return nil, err
	}
	siteID, version, ok, err := s.index.ServingSite(ctx, owner, site)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fs.ErrNotExist
	}
	return s.OpenVersion(ctx, siteID, version)
}

// CurrentVersion reports the version owner's site is serving, if the site exists.
func (s *Store) CurrentVersion(owner, site string) (int, bool, error) {
	if err := validateIdentity(owner, site); err != nil {
		return 0, false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), indexTimeout)
	defer cancel()
	_, version, ok, err := s.index.ServingSite(ctx, owner, site)
	return version, ok, err
}

// ListUsers lists every owner username with at least one site.
func (s *Store) ListUsers() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), indexTimeout)
	defer cancel()
	return s.index.Owners(ctx)
}

// ListSites lists owner's site names.
func (s *Store) ListSites(owner string) ([]string, error) {
	if err := safepath.ValidateSegment(owner); err != nil {
		return nil, fmt.Errorf("invalid user: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), indexTimeout)
	defer cancel()
	return s.index.Sites(ctx, owner)
}

// SiteUsage is what one site holds in the bucket: every retained version's
// archive and every asset (compressed, as stored), plus each version's own
// archive size so the caller can pick out the live one.
type SiteUsage struct {
	TotalBytes   int64
	VersionBytes map[int]int64
}

// Usage lists the bucket and totals it per site id.
func (s *Store) Usage(ctx context.Context) (map[string]SiteUsage, error) {
	objects, err := s.objects.List(ctx, "sites/")
	if err != nil {
		return nil, err
	}
	usage := map[string]SiteUsage{}
	for _, object := range objects {
		rest := object.Key[len("sites/"):]
		if len(rest) < 37 || !isUUID(rest[:36]) || rest[36] != '/' {
			continue
		}
		siteID, name := rest[:36], rest[37:]
		entry := usage[siteID]
		entry.TotalBytes += object.Size
		if digits, ok := cutVersionArchiveName(name); ok {
			if entry.VersionBytes == nil {
				entry.VersionBytes = map[int]int64{}
			}
			entry.VersionBytes[digits] = object.Size
		}
		usage[siteID] = entry
	}
	return usage, nil
}

func cutVersionArchiveName(name string) (int, bool) {
	if len(name) < len("v1.tar.gz") || name[0] != 'v' || name[len(name)-len(".tar.gz"):] != ".tar.gz" {
		return 0, false
	}
	version, err := strconv.Atoi(name[1 : len(name)-len(".tar.gz")])
	if err != nil || version < 1 {
		return 0, false
	}
	return version, true
}

func validateIdentity(user, siteName string) error {
	if err := safepath.ValidateSegment(user); err != nil {
		return fmt.Errorf("invalid user: %w", err)
	}
	if err := safepath.ValidateSegment(siteName); err != nil {
		return fmt.Errorf("invalid site: %w", err)
	}
	return nil
}

// dbIndex is SiteIndex on the database.
type dbIndex struct{ database *sql.DB }

// NewDBIndex returns the production SiteIndex.
func NewDBIndex(database *sql.DB) SiteIndex { return dbIndex{database: database} }

func (d dbIndex) ServingSite(ctx context.Context, owner, site string) (string, int, bool, error) {
	siteID, version, err := db.ServingSite(ctx, d.database, owner, site)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return siteID, version, true, nil
}

func (d dbIndex) Owners(ctx context.Context) ([]string, error) {
	return db.ListSiteOwnerUsernames(ctx, d.database)
}

func (d dbIndex) Sites(ctx context.Context, owner string) ([]string, error) {
	return db.ListSiteNamesByOwnerUsername(ctx, d.database, owner)
}
