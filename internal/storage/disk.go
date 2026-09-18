package storage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/vsriram/simple-host/internal/safepath"
)

type DiskStorage struct {
	basePath         string
	root             *os.Root
	lifecycle        sync.RWMutex
	locks            [256]sync.RWMutex
	leaseMu          sync.Mutex
	leaseCond        *sync.Cond
	versionLeases    map[versionLeaseKey]int
	quarantineMu     sync.RWMutex
	quarantinedSites map[string]struct{}
}

type versionLeaseKey struct {
	user     string
	siteName string
	version  int
}

// VersionLease keeps an immutable version anchored beneath the storage root
// and pins that exact version against deletion. Close must be called when the
// caller is done streaming so cleanup cannot remove the version mid-read.
type VersionLease struct {
	root     *os.Root
	release  func()
	closeErr error
	once     sync.Once
}

func (l *VersionLease) FS() fs.FS {
	if l == nil || l.root == nil {
		return nil
	}
	return l.root.FS()
}

func (l *VersionLease) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if l.root != nil {
			l.closeErr = l.root.Close()
		}
		if l.release != nil {
			l.release()
		}
	})
	return l.closeErr
}

var ErrSiteCurrentQuarantined = errors.New("site current serving is quarantined pending operator repair")

func NewDiskStorage(basePath string) (*DiskStorage, error) {
	absolute, err := filepath.Abs(basePath)
	if err != nil {
		return nil, fmt.Errorf("resolve base path %q: %w", basePath, err)
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return nil, fmt.Errorf("create base path %q: %w", absolute, err)
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, fmt.Errorf("open base path %q: %w", absolute, err)
	}
	store := &DiskStorage{
		basePath:         absolute,
		root:             root,
		versionLeases:    make(map[versionLeaseKey]int),
		quarantinedSites: make(map[string]struct{}),
	}
	store.leaseCond = sync.NewCond(&store.leaseMu)
	return store, nil
}

func (s *DiskStorage) Close() error {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	return s.root.Close()
}

func (s *DiskStorage) WriteFiles(user, siteName string, version int, files map[string][]byte) error {
	versionName, err := validateStorageAddress(user, siteName, version)
	if err != nil {
		return err
	}
	fileNames := make([]string, 0, len(files))
	for name := range files {
		canonical, err := safepath.CanonicalRelativePath(name, false)
		if err != nil || canonical != name {
			return fmt.Errorf("invalid file path %q", name)
		}
		fileNames = append(fileNames, name)
	}
	sort.Strings(fileNames)

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.Lock()
	defer mutation.Unlock()
	if s.currentServingQuarantined(user, siteName) {
		return ErrSiteCurrentQuarantined
	}

	siteRoot, err := s.openSite(user, siteName, true)
	if err != nil {
		return err
	}
	defer siteRoot.Close()
	sitePath := filepath.Join(s.basePath, user, siteName)

	if _, err := siteRoot.Lstat(versionName); err == nil {
		return fmt.Errorf("version %s already exists", versionName)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect version %s: %w", versionName, err)
	}

	stagingName, err := createUniqueDir(siteRoot, ".stage-"+versionName+"-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = removeTree(siteRoot, stagingName)
		}
	}()

	stagingRoot, err := siteRoot.OpenRoot(stagingName)
	if err != nil {
		return fmt.Errorf("open staging directory: %w", err)
	}
	for _, name := range fileNames {
		if err := writeRootFile(stagingRoot, name, files[name]); err != nil {
			stagingRoot.Close()
			return err
		}
	}
	if err := stagingRoot.Close(); err != nil {
		return fmt.Errorf("close staging directory: %w", err)
	}

	// os.Root has no Rename operation. The validated absolute rename is safe
	// because the structural ancestors are real directories and all storage
	// mutations for this site are serialized. Recheck the destination immediately
	// before rename so an existing immutable version is never replaced.
	if _, err := siteRoot.Lstat(versionName); err == nil {
		return fmt.Errorf("version %s already exists", versionName)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect version %s before publish: %w", versionName, err)
	}
	if err := os.Rename(filepath.Join(sitePath, stagingName), filepath.Join(sitePath, versionName)); err != nil {
		return fmt.Errorf("publish version %s: %w", versionName, err)
	}
	published = true
	return nil
}

func (s *DiskStorage) SetCurrentVersion(user, siteName string, version int) error {
	versionName, err := validateStorageAddress(user, siteName, version)
	if err != nil {
		return err
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.Lock()
	defer mutation.Unlock()
	if s.currentServingQuarantined(user, siteName) {
		return ErrSiteCurrentQuarantined
	}

	siteRoot, err := s.openSite(user, siteName, false)
	if err != nil {
		return err
	}
	defer siteRoot.Close()
	if err := s.materializeVersionLocked(siteRoot, versionName); err != nil {
		return err
	}
	sitePath := filepath.Join(s.basePath, user, siteName)
	if err := requireRealDir(siteRoot, versionName); err != nil {
		return fmt.Errorf("authorize current target %s: %w", versionName, err)
	}

	if _, err := siteRoot.Lstat("current"); err == nil {
		currentRoot, err := openAuthorizedCurrent(siteRoot, sitePath)
		if err != nil {
			return fmt.Errorf("refuse to replace unsafe current link: %w", err)
		}
		if err := currentRoot.Close(); err != nil {
			return fmt.Errorf("close current target: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect current link: %w", err)
	}

	// Remember what is being replaced, so it can be compressed once it is no
	// longer the version being served.
	outgoing, hasOutgoing := currentLinkTarget(siteRoot)

	temporary, err := uniqueName(".current-")
	if err != nil {
		return err
	}
	temporaryPath := filepath.Join(sitePath, temporary)
	if err := os.Symlink(versionName, temporaryPath); err != nil {
		return fmt.Errorf("create temporary current link: %w", err)
	}
	defer siteRoot.Remove(temporary)
	if err := os.Rename(temporaryPath, filepath.Join(sitePath, "current")); err != nil {
		return fmt.Errorf("replace current link: %w", err)
	}

	// The previous version has just stopped being served, so store it
	// compressed. Old versions exist only so a user can roll back, and expanded
	// they account for most of the disk.
	//
	// Deliberately best-effort: a failure to compress leaves a perfectly good
	// raw version directory, and must never fail the deploy or rollback that
	// has already succeeded above. The next attempt picks it up.
	if hasOutgoing && outgoing != versionName {
		s.archiveSupersededVersion(siteRoot, user, siteName, outgoing)
	}
	return nil
}

// archiveSupersededVersion compresses a version that has just stopped being
// served. It is best effort by design: the deploy or rollback has already
// succeeded, and failing to compress only leaves a working raw directory that a
// later attempt will pick up.
//
// A version being streamed right now is skipped rather than waited for. Draining
// the lease here would hold the site's write lock — which the serving path also
// needs — for as long as somebody's download takes.
func (s *DiskStorage) archiveSupersededVersion(siteRoot *os.Root, user, siteName, versionName string) {
	version, ok := parseVersionName(versionName)
	if !ok {
		return
	}
	if s.versionIsLeased(versionLeaseKey{user: user, siteName: siteName, version: version}) {
		return
	}
	if err := s.archiveVersionLocked(siteRoot, versionName); err != nil {
		log.Printf("archive superseded version %s/%s/%s: %v", user, siteName, versionName, err)
	}
}

// versionIsLeased reports whether a reader is currently streaming this version,
// without blocking on it.
func (s *DiskStorage) versionIsLeased(key versionLeaseKey) bool {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	return s.versionLeases[key] > 0
}

// parseVersionName turns a "vN" directory name back into its version number.
func parseVersionName(name string) (int, bool) {
	digits, ok := strings.CutPrefix(name, "v")
	if !ok || digits == "" {
		return 0, false
	}
	version, err := strconv.Atoi(digits)
	if err != nil || version < 1 {
		return 0, false
	}
	return version, true
}

// currentLinkTarget reports which version the serving link points at, if it is
// a symlink to a plain version name.
func currentLinkTarget(siteRoot *os.Root) (string, bool) {
	info, err := siteRoot.Lstat("current")
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return "", false
	}
	target, err := siteRoot.Readlink("current")
	if err != nil || target == "" || strings.Contains(target, "/") {
		return "", false
	}
	return target, true
}

// RemoveCurrentVersion removes current only when it is an authorized link to
// the expected version. It is used to compensate a newly-created site without
// deleting unrelated orphaned versions that may already be present on disk.
func (s *DiskStorage) RemoveCurrentVersion(user, siteName string, version int) error {
	versionName, err := validateStorageAddress(user, siteName, version)
	if err != nil {
		return err
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.Lock()
	defer mutation.Unlock()
	if s.currentServingQuarantined(user, siteName) {
		return ErrSiteCurrentQuarantined
	}

	siteRoot, err := s.openSite(user, siteName, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer siteRoot.Close()
	sitePath := filepath.Join(s.basePath, user, siteName)
	target, err := authorizedLinkTarget(siteRoot, sitePath, "current")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if target != versionName {
		return fmt.Errorf("current targets %q, not expected %q", target, versionName)
	}
	if err := siteRoot.Remove("current"); err != nil {
		return fmt.Errorf("remove current link: %w", err)
	}
	return nil
}

// CurrentVersion returns the exactly authorized current version, when present.
// A process-lifetime repair latch takes precedence over the on-disk link.
func (s *DiskStorage) CurrentVersion(user, siteName string) (int, bool, error) {
	if err := validateIdentity(user, siteName); err != nil {
		return 0, false, err
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.RLock()
	defer mutation.RUnlock()
	if s.currentServingQuarantined(user, siteName) {
		return 0, false, ErrSiteCurrentQuarantined
	}

	siteRoot, err := s.openSite(user, siteName, false)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer siteRoot.Close()
	target, err := authorizedLinkTarget(siteRoot, filepath.Join(s.basePath, user, siteName), "current")
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	version, err := strconv.Atoi(strings.TrimPrefix(target, "v"))
	if err != nil {
		return 0, false, err
	}
	return version, true, nil
}

const hiddenCurrentPrefix = ".hidden-current-"

// HideCurrent atomically removes a validated current link from the public
// serving path while retaining an opaque same-directory name for rollback.
// A missing site/current is already hidden and returns an empty token.
func (s *DiskStorage) HideCurrent(user, siteName string) (string, error) {
	if err := validateIdentity(user, siteName); err != nil {
		return "", err
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.Lock()
	defer mutation.Unlock()
	if s.currentServingQuarantined(user, siteName) {
		return "", ErrSiteCurrentQuarantined
	}

	siteRoot, err := s.openSite(user, siteName, false)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer siteRoot.Close()
	sitePath := filepath.Join(s.basePath, user, siteName)
	if _, err := authorizedLinkTarget(siteRoot, sitePath, "current"); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}

	for attempt := 0; attempt < 8; attempt++ {
		token, err := uniqueName(hiddenCurrentPrefix)
		if err != nil {
			return "", err
		}
		if _, err := siteRoot.Lstat(token); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect hidden current name: %w", err)
		}
		if err := os.Rename(filepath.Join(sitePath, "current"), filepath.Join(sitePath, token)); err != nil {
			return "", fmt.Errorf("hide current link: %w", err)
		}
		return token, nil
	}
	return "", fmt.Errorf("could not allocate hidden current name")
}

// RestoreCurrent restores a token returned by HideCurrent, but never replaces
// a current link created by a later operation.
func (s *DiskStorage) RestoreCurrent(user, siteName, token string) error {
	if err := validateIdentity(user, siteName); err != nil {
		return err
	}
	if err := validateHiddenCurrentToken(token); err != nil {
		return err
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.Lock()
	defer mutation.Unlock()
	if s.currentServingQuarantined(user, siteName) {
		return ErrSiteCurrentQuarantined
	}

	siteRoot, err := s.openSite(user, siteName, false)
	if err != nil {
		return err
	}
	defer siteRoot.Close()
	sitePath := filepath.Join(s.basePath, user, siteName)
	if _, err := siteRoot.Lstat("current"); err == nil {
		return fmt.Errorf("refuse to replace existing current link")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect current link: %w", err)
	}
	if _, err := authorizedLinkTarget(siteRoot, sitePath, token); err != nil {
		return fmt.Errorf("authorize hidden current: %w", err)
	}
	if err := os.Rename(filepath.Join(sitePath, token), filepath.Join(sitePath, "current")); err != nil {
		return fmt.Errorf("restore current link: %w", err)
	}
	return nil
}

func (s *DiskStorage) DeleteSite(user, siteName string) error {
	if err := validateIdentity(user, siteName); err != nil {
		return err
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.Lock()
	defer mutation.Unlock()
	if s.currentServingQuarantined(user, siteName) {
		return ErrSiteCurrentQuarantined
	}
	s.waitForSiteLeases(user, siteName)

	userRoot, err := s.openUser(user, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer userRoot.Close()
	if err := requireRealDir(userRoot, siteName); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect site directory: %w", err)
	}
	if err := removeTree(userRoot, siteName); err != nil {
		return fmt.Errorf("delete site %s/%s: %w", user, siteName, err)
	}
	return nil
}

func (s *DiskStorage) DeleteVersion(user, siteName string, version int) error {
	versionName, err := validateStorageAddress(user, siteName, version)
	if err != nil {
		return err
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.Lock()
	defer mutation.Unlock()
	if s.currentServingQuarantined(user, siteName) {
		return ErrSiteCurrentQuarantined
	}
	s.waitForVersionLease(versionLeaseKey{user: user, siteName: siteName, version: version})

	siteRoot, err := s.openSite(user, siteName, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer siteRoot.Close()
	rawExists, err := realDirExists(siteRoot, versionName)
	if err != nil {
		return fmt.Errorf("inspect version directory: %w", err)
	}
	archiveName := versionName + ".tar.gz"
	archiveExists, err := realFileExists(siteRoot, archiveName)
	if err != nil {
		return fmt.Errorf("inspect version archive: %w", err)
	}
	if !rawExists && !archiveExists {
		return nil
	}
	if archiveExists {
		if err := siteRoot.Remove(archiveName); err != nil {
			return fmt.Errorf("delete version archive %s: %w", archiveName, err)
		}
	}
	if rawExists {
		if err := removeTree(siteRoot, versionName); err != nil {
			return fmt.Errorf("delete version %s: %w", versionName, err)
		}
	}
	return nil
}

// ArchiveVersion replaces an inactive raw version with a verified archive.
func (s *DiskStorage) ArchiveVersion(user, siteName string, version int) error {
	versionName, err := validateStorageAddress(user, siteName, version)
	if err != nil {
		return err
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.Lock()
	defer mutation.Unlock()
	if s.currentServingQuarantined(user, siteName) {
		return ErrSiteCurrentQuarantined
	}
	s.waitForVersionLease(versionLeaseKey{user: user, siteName: siteName, version: version})

	siteRoot, err := s.openSite(user, siteName, false)
	if err != nil {
		return err
	}
	defer siteRoot.Close()
	return s.archiveVersionLocked(siteRoot, versionName)
}

// archiveVersionLocked contains the directory-to-archive transition for callers
// that already hold lifecycle.RLock and the per-site write lock. Keep this split
// so locked public methods do not recursively acquire those locks through
// ArchiveVersion and deadlock — the same reason materializeVersionLocked exists.
func (s *DiskStorage) archiveVersionLocked(siteRoot *os.Root, versionName string) error {
	archiveName := versionName + ".tar.gz"

	rawExists, err := realDirExists(siteRoot, versionName)
	if err != nil {
		return fmt.Errorf("inspect version directory: %w", err)
	}
	if rawExists {
		live, err := versionHasServingLink(siteRoot, versionName)
		if err != nil {
			return err
		}
		if live {
			return fmt.Errorf("refuse to archive live version %s", versionName)
		}
	}
	archiveExists, err := realFileExists(siteRoot, archiveName)
	if err != nil {
		if rawExists {
			// Raw wins after an interrupted transition. Remove only the rooted
			// archive-shaped entry; removeTree does not follow symlinks.
			if removeErr := removeTree(siteRoot, archiveName); removeErr != nil {
				return fmt.Errorf("remove unsafe version archive: %w", removeErr)
			}
			archiveExists = false
		} else {
			return fmt.Errorf("inspect version archive: %w", err)
		}
	}
	if !rawExists {
		if archiveExists {
			return nil
		}
		return fmt.Errorf("version %s does not exist", versionName)
	}

	rawRoot, err := openRealDir(siteRoot, versionName, false)
	if err != nil {
		return fmt.Errorf("open version directory: %w", err)
	}
	rawStats, err := directoryRegularStats(rawRoot)
	closeRawErr := rawRoot.Close()
	if err != nil {
		return fmt.Errorf("inspect version contents: %w", err)
	}
	if closeRawErr != nil {
		return fmt.Errorf("close version directory: %w", closeRawErr)
	}

	if archiveExists {
		archivedStats, verifyErr := readArchiveStats(siteRoot, archiveName)
		if verifyErr == nil && archivedStats == rawStats {
			if err := syncRootFile(siteRoot, archiveName); err != nil {
				return fmt.Errorf("sync verified version archive: %w", err)
			}
			if err := syncRootDirectory(siteRoot); err != nil {
				return fmt.Errorf("sync verified version archive name: %w", err)
			}
			return retireRawVersion(siteRoot, versionName)
		}
		// A crash may leave both forms. The raw directory is authoritative, so
		// discard an unverifiable archive and rebuild it before removing raw.
		if err := siteRoot.Remove(archiveName); err != nil {
			return fmt.Errorf("remove suspect version archive: %w", err)
		}
	}

	temporary, err := uniqueName(".tmp-" + archiveName + "-")
	if err != nil {
		return err
	}
	defer func() {
		_ = siteRoot.Remove(temporary)
	}()

	rawRoot, err = openRealDir(siteRoot, versionName, false)
	if err != nil {
		return fmt.Errorf("reopen version directory: %w", err)
	}
	archiveFile, err := siteRoot.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		rawRoot.Close()
		return fmt.Errorf("create temporary version archive: %w", err)
	}
	writeStats, writeErr := writeVersionArchive(archiveFile, rawRoot)
	if writeErr == nil && writeStats != rawStats {
		writeErr = fmt.Errorf("source changed while archiving")
	}
	closeRawErr = rawRoot.Close()
	if writeErr != nil {
		_ = archiveFile.Close()
		return fmt.Errorf("write temporary version archive: %w", writeErr)
	}
	if closeRawErr != nil {
		_ = archiveFile.Close()
		return fmt.Errorf("close version directory: %w", closeRawErr)
	}

	return publishVerifiedArchive(siteRoot, temporary, archiveName, versionName, rawStats, archiveFile)
}

// MaterializeVersion replaces an archived version with a verified raw tree.
func (s *DiskStorage) MaterializeVersion(user, siteName string, version int) error {
	versionName, err := validateStorageAddress(user, siteName, version)
	if err != nil {
		return err
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.Lock()
	defer mutation.Unlock()
	if s.currentServingQuarantined(user, siteName) {
		return ErrSiteCurrentQuarantined
	}
	s.waitForVersionLease(versionLeaseKey{user: user, siteName: siteName, version: version})

	siteRoot, err := s.openSite(user, siteName, false)
	if err != nil {
		return err
	}
	defer siteRoot.Close()
	return s.materializeVersionLocked(siteRoot, versionName)
}

// materializeVersionLocked contains the archive-to-directory transition for
// callers that already hold lifecycle.RLock and the per-site write lock. Keep
// this split so locked public methods do not recursively acquire those locks
// through MaterializeVersion and deadlock.
func (s *DiskStorage) materializeVersionLocked(siteRoot *os.Root, versionName string) error {
	archiveName := versionName + ".tar.gz"
	rawExists, err := realDirExists(siteRoot, versionName)
	if err != nil {
		return fmt.Errorf("inspect version directory: %w", err)
	}
	if rawExists {
		// Raw is authoritative in the recoverable both-forms crash state.
		return nil
	}
	archiveExists, err := realFileExists(siteRoot, archiveName)
	if err != nil {
		return fmt.Errorf("inspect version archive: %w", err)
	}
	if !archiveExists {
		return fmt.Errorf("version %s does not exist", versionName)
	}

	temporary, err := createUniqueDir(siteRoot, ".tmp-"+versionName+"-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = removeTree(siteRoot, temporary)
		}
	}()
	temporaryRoot, err := openRealDir(siteRoot, temporary, false)
	if err != nil {
		return fmt.Errorf("open temporary version directory: %w", err)
	}
	archiveStats, extractErr := extractVersionArchive(siteRoot, archiveName, temporaryRoot)
	if extractErr == nil {
		extractedStats, statErr := directoryRegularStats(temporaryRoot)
		if statErr != nil {
			extractErr = statErr
		} else if extractedStats != archiveStats {
			extractErr = fmt.Errorf("extracted file totals do not match archive")
		}
	}
	if extractErr == nil {
		extractErr = syncRootTreeDirectories(temporaryRoot)
	}
	closeTemporaryErr := temporaryRoot.Close()
	if extractErr != nil {
		return fmt.Errorf("extract version archive: %w", extractErr)
	}
	if closeTemporaryErr != nil {
		return fmt.Errorf("close temporary version directory: %w", closeTemporaryErr)
	}
	if err := siteRoot.Rename(temporary, versionName); err != nil {
		return fmt.Errorf("publish materialized version: %w", err)
	}
	published = true
	if err := syncRootDirectory(siteRoot); err != nil {
		return fmt.Errorf("sync materialized version: %w", err)
	}
	if err := siteRoot.Remove(archiveName); err != nil {
		return fmt.Errorf("remove materialized version archive: %w", err)
	}
	return syncRootDirectory(siteRoot)
}

// VersionExists reports whether either durable representation is present.
func (s *DiskStorage) VersionExists(user, siteName string, version int) bool {
	versionName, err := validateStorageAddress(user, siteName, version)
	if err != nil {
		return false
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.RLock()
	defer mutation.RUnlock()
	siteRoot, err := s.openSite(user, siteName, false)
	if err != nil {
		return false
	}
	defer siteRoot.Close()
	return requireRealDir(siteRoot, versionName) == nil || requireRealFile(siteRoot, versionName+".tar.gz") == nil
}

func (s *DiskStorage) VersionDirExists(user, siteName string, version int) bool {
	versionName, err := validateStorageAddress(user, siteName, version)
	if err != nil {
		return false
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.RLock()
	defer mutation.RUnlock()
	siteRoot, err := s.openSite(user, siteName, false)
	if err != nil {
		return false
	}
	defer siteRoot.Close()
	return requireRealDir(siteRoot, versionName) == nil
}

// OpenVersion returns an os.Root anchored to exactly the requested immutable
// version. It never consults the site's current link. A process-lifetime repair
// latch denies opening, and the caller must close a successful result.
func (s *DiskStorage) OpenVersion(user, siteName string, version int) (*os.Root, error) {
	versionName, err := validateStorageAddress(user, siteName, version)
	if err != nil {
		return nil, err
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.Lock()
	defer mutation.Unlock()
	if s.currentServingQuarantined(user, siteName) {
		return nil, ErrSiteCurrentQuarantined
	}

	siteRoot, err := s.openSite(user, siteName, false)
	if err != nil {
		return nil, err
	}
	defer siteRoot.Close()
	if err := s.materializeVersionLocked(siteRoot, versionName); err != nil {
		return nil, err
	}
	return openRealDir(siteRoot, versionName, false)
}

// LeaseVersion opens exactly one immutable version and registers an exact
// process-local deletion lease until Close. The broad site lock is released
// before returning, so a slow archive reader cannot block updates or an
// unrelated site that happens to share the same lock stripe. DeleteVersion and
// DeleteSite wait for matching leases before removing filesystem content.
func (s *DiskStorage) LeaseVersion(user, siteName string, version int) (*VersionLease, error) {
	versionName, err := validateStorageAddress(user, siteName, version)
	if err != nil {
		return nil, err
	}

	s.lifecycle.RLock()
	mutation := s.siteLock(user, siteName)
	mutation.Lock()
	releaseOpenLocks := func() {
		mutation.Unlock()
		s.lifecycle.RUnlock()
	}
	if s.currentServingQuarantined(user, siteName) {
		releaseOpenLocks()
		return nil, ErrSiteCurrentQuarantined
	}

	siteRoot, err := s.openSite(user, siteName, false)
	if err != nil {
		releaseOpenLocks()
		return nil, err
	}
	if err := s.materializeVersionLocked(siteRoot, versionName); err != nil {
		_ = siteRoot.Close()
		releaseOpenLocks()
		return nil, err
	}
	versionRoot, err := openRealDir(siteRoot, versionName, false)
	closeErr := siteRoot.Close()
	if err != nil {
		releaseOpenLocks()
		return nil, err
	}
	if closeErr != nil {
		_ = versionRoot.Close()
		releaseOpenLocks()
		return nil, closeErr
	}
	key := versionLeaseKey{user: user, siteName: siteName, version: version}
	s.addVersionLease(key)
	mutation.Unlock()
	return &VersionLease{
		root: versionRoot,
		release: func() {
			s.releaseVersionLease(key)
			s.lifecycle.RUnlock()
		},
	}, nil
}

func (s *DiskStorage) addVersionLease(key versionLeaseKey) {
	s.leaseMu.Lock()
	s.versionLeases[key]++
	s.leaseMu.Unlock()
}

func (s *DiskStorage) releaseVersionLease(key versionLeaseKey) {
	s.leaseMu.Lock()
	if count := s.versionLeases[key]; count > 1 {
		s.versionLeases[key] = count - 1
	} else {
		delete(s.versionLeases, key)
	}
	s.leaseCond.Broadcast()
	s.leaseMu.Unlock()
}

func (s *DiskStorage) waitForVersionLease(key versionLeaseKey) {
	s.leaseMu.Lock()
	for s.versionLeases[key] > 0 {
		s.leaseCond.Wait()
	}
	s.leaseMu.Unlock()
}

func (s *DiskStorage) waitForSiteLeases(user, siteName string) {
	s.leaseMu.Lock()
	for s.siteHasVersionLease(user, siteName) {
		s.leaseCond.Wait()
	}
	s.leaseMu.Unlock()
}

func (s *DiskStorage) siteHasVersionLease(user, siteName string) bool {
	for key, count := range s.versionLeases {
		if count > 0 && key.user == user && key.siteName == siteName {
			return true
		}
	}
	return false
}

// OpenCurrent returns an os.Root anchored to the authorized immutable version.
// A process-lifetime repair latch denies opening even if current remains on
// disk. The caller must close a successful result. Symlinks encountered inside
// that version are still constrained by os.Root and cannot escape this root.
func (s *DiskStorage) OpenCurrent(user, siteName string) (*os.Root, error) {
	if err := validateIdentity(user, siteName); err != nil {
		return nil, err
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.RLock()
	defer mutation.RUnlock()
	if s.currentServingQuarantined(user, siteName) {
		return nil, ErrSiteCurrentQuarantined
	}
	siteRoot, err := s.openSite(user, siteName, false)
	if err != nil {
		return nil, err
	}
	defer siteRoot.Close()
	return openAuthorizedCurrent(siteRoot, filepath.Join(s.basePath, user, siteName))
}

// QuarantineCurrentServing installs a process-lifetime fail-closed latch for a
// site whose public current link could not be proven absent after an
// indeterminate database commit. There is deliberately no automatic clear:
// an operator must repair database/disk state before restarting this process.
func (s *DiskStorage) QuarantineCurrentServing(user, siteName string) error {
	if err := validateIdentity(user, siteName); err != nil {
		return err
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	mutation := s.siteLock(user, siteName)
	mutation.Lock()
	defer mutation.Unlock()
	s.quarantineMu.Lock()
	s.quarantinedSites[siteQuarantineKey(user, siteName)] = struct{}{}
	s.quarantineMu.Unlock()
	return nil
}

func (s *DiskStorage) currentServingQuarantined(user, siteName string) bool {
	s.quarantineMu.RLock()
	_, quarantined := s.quarantinedSites[siteQuarantineKey(user, siteName)]
	s.quarantineMu.RUnlock()
	return quarantined
}

func siteQuarantineKey(user, siteName string) string {
	return user + "\x00" + siteName
}

func (s *DiskStorage) UserDirExists(user string) (bool, error) {
	if err := safepath.ValidateSegment(user); err != nil {
		return false, fmt.Errorf("invalid user: %w", err)
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	userRoot, err := s.openUser(user, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := userRoot.Close(); err != nil {
		return false, err
	}
	return true, nil
}

// ListUsers returns the names of the real (non-symlink) directories directly
// under the base path that are valid path segments, in directory order. It
// exists for callers that must find an owner without the database, such as
// the per-owner host gate, and it deliberately reads nothing below the user
// level.
func (s *DiskStorage) ListUsers() ([]string, error) {
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	entries, err := fs.ReadDir(s.root.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("read base path: %w", err)
	}
	users := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		// DirEntry types come from lstat, so a symlink to a directory reports
		// ModeSymlink rather than IsDir, which is the exclusion wanted here.
		if !entry.IsDir() || !safepath.IsSegment(name) {
			continue
		}
		users = append(users, name)
	}
	return users, nil
}

// ListSites returns the names of the site directories under one user's own
// directory, unsorted. It exists for label resolution (handler.HostModel's
// restricted-site addressing, design.md 5.2a): a site's DNS label folds its
// name the same way an owner's label folds a username, and resolving a
// received label back to the site it names means listing candidates and
// comparing, the same pattern ListUsers already serves for owner labels. A
// user directory that does not exist yet reports zero sites, not an error.
func (s *DiskStorage) ListSites(user string) ([]string, error) {
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if err := safepath.ValidateSegment(user); err != nil {
		return nil, fmt.Errorf("invalid user: %w", err)
	}
	userRoot, err := s.openUser(user, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open user directory: %w", err)
	}
	defer userRoot.Close()
	entries, err := fs.ReadDir(userRoot.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("read user directory: %w", err)
	}
	sites := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !safepath.IsSegment(name) {
			continue
		}
		sites = append(sites, name)
	}
	return sites, nil
}

func (s *DiskStorage) BasePath() string {
	return s.basePath
}

func (s *DiskStorage) siteLock(user, siteName string) *sync.RWMutex {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(user))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(siteName))
	return &s.locks[hash.Sum32()%uint32(len(s.locks))]
}

func validateStorageAddress(user, siteName string, version int) (string, error) {
	if err := validateIdentity(user, siteName); err != nil {
		return "", err
	}
	if version < 1 {
		return "", fmt.Errorf("version must be at least 1")
	}
	return fmt.Sprintf("v%d", version), nil
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

func (s *DiskStorage) openUser(user string, create bool) (*os.Root, error) {
	return openRealDir(s.root, user, create)
}

func (s *DiskStorage) openSite(user, siteName string, create bool) (*os.Root, error) {
	userRoot, err := s.openUser(user, create)
	if err != nil {
		return nil, err
	}
	defer userRoot.Close()
	siteRoot, err := openRealDir(userRoot, siteName, create)
	if err != nil {
		return nil, err
	}
	return siteRoot, nil
}

func openRealDir(parent *os.Root, name string, create bool) (*os.Root, error) {
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && create {
		if mkdirErr := parent.Mkdir(name, 0o755); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
			return nil, fmt.Errorf("create directory %q: %w", name, mkdirErr)
		}
		info, err = parent.Lstat(name)
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%q is not a real directory", name)
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("open directory %q: %w", name, err)
	}
	return root, nil
}

func requireRealDir(parent *os.Root, name string) error {
	info, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%q is not a real directory", name)
	}
	return nil
}

func requireRealFile(parent *os.Root, name string) error {
	info, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%q is not a real regular file", name)
	}
	return nil
}

func realDirExists(parent *os.Root, name string) (bool, error) {
	if err := requireRealDir(parent, name); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func realFileExists(parent *os.Root, name string) (bool, error) {
	if err := requireRealFile(parent, name); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// Hidden current links survive after HideCurrent releases the site lock and
// before RestoreCurrent reacquires it. That failed-deploy window still makes
// the target live for rollback purposes, even though no public current exists.
func versionHasServingLink(siteRoot *os.Root, versionName string) (bool, error) {
	directory, err := siteRoot.Open(".")
	if err != nil {
		return false, fmt.Errorf("open site directory: %w", err)
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil {
		return false, fmt.Errorf("list site directory: %w", readErr)
	}
	if closeErr != nil {
		return false, fmt.Errorf("close site directory: %w", closeErr)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != "current" && !strings.HasPrefix(name, hiddenCurrentPrefix) {
			continue
		}
		info, err := siteRoot.Lstat(name)
		if err != nil {
			return false, fmt.Errorf("inspect serving link %q: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := siteRoot.Readlink(name)
		if err != nil {
			return false, fmt.Errorf("read serving link %q: %w", name, err)
		}
		if target == versionName {
			return true, nil
		}
	}
	return false, nil
}

type regularFileStats struct {
	count int64
	bytes int64
}

func (s *regularFileStats) add(size int64) error {
	if size < 0 || size > int64(^uint64(0)>>1)-s.bytes {
		return fmt.Errorf("regular file byte total overflows")
	}
	s.count++
	s.bytes += size
	return nil
}

func directoryRegularStats(root *os.Root) (regularFileStats, error) {
	var totals regularFileStats
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("version entry %q is not a real regular file or directory", name)
		}
		return totals.add(info.Size())
	})
	return totals, err
}

func writeVersionArchive(destination *os.File, root *os.Root) (regularFileStats, error) {
	var totals regularFileStats
	gzipWriter := gzip.NewWriter(destination)
	tarWriter := tar.NewWriter(gzipWriter)
	walkErr := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("version entry %q is not a real regular file or directory", name)
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("build tar header for %q: %w", name, err)
		}
		header.Name = filepath.ToSlash(name)
		if err := tarWriter.WriteHeader(header); err != nil {
			return fmt.Errorf("write tar header for %q: %w", name, err)
		}
		if info.IsDir() {
			return nil
		}
		file, err := root.Open(name)
		if err != nil {
			return fmt.Errorf("open version file %q: %w", name, err)
		}
		written, copyErr := io.Copy(tarWriter, file)
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("archive version file %q: %w", name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close version file %q: %w", name, closeErr)
		}
		if written != info.Size() {
			return fmt.Errorf("version file %q changed size while archiving", name)
		}
		return totals.add(written)
	})
	if err := tarWriter.Close(); walkErr == nil && err != nil {
		walkErr = err
	}
	if err := gzipWriter.Close(); walkErr == nil && err != nil {
		walkErr = err
	}
	return totals, walkErr
}

type storedArchivePathKind uint8

const (
	storedArchiveImplicitDir storedArchivePathKind = iota + 1
	storedArchiveExplicitDir
	storedArchiveRegularFile
)

type storedArchivePathTracker map[string]storedArchivePathKind

func (t storedArchivePathTracker) add(name string, directory bool) error {
	components := strings.Split(name, "/")
	for index := 1; index < len(components); index++ {
		ancestor := strings.Join(components[:index], "/")
		switch t[ancestor] {
		case storedArchiveRegularFile:
			return fmt.Errorf("archive path %q has file ancestor %q", name, ancestor)
		case 0:
			t[ancestor] = storedArchiveImplicitDir
		}
	}
	existing := t[name]
	if directory {
		switch existing {
		case 0:
			t[name] = storedArchiveExplicitDir
			return nil
		case storedArchiveImplicitDir:
			t[name] = storedArchiveExplicitDir
			return nil
		case storedArchiveExplicitDir:
			return fmt.Errorf("duplicate archive path %q", name)
		default:
			return fmt.Errorf("archive path %q is both a file and directory", name)
		}
	}
	if existing == 0 {
		t[name] = storedArchiveRegularFile
		return nil
	}
	if existing == storedArchiveRegularFile {
		return fmt.Errorf("duplicate archive path %q", name)
	}
	return fmt.Errorf("archive path %q is both a file and directory", name)
}

func readArchiveStats(siteRoot *os.Root, archiveName string) (regularFileStats, error) {
	if err := requireRealFile(siteRoot, archiveName); err != nil {
		return regularFileStats{}, err
	}
	file, err := siteRoot.Open(archiveName)
	if err != nil {
		return regularFileStats{}, err
	}
	totals, readErr := scanVersionArchive(file, nil)
	closeErr := file.Close()
	if readErr != nil {
		return regularFileStats{}, readErr
	}
	if closeErr != nil {
		return regularFileStats{}, closeErr
	}
	return totals, nil
}

func scanVersionArchive(source io.Reader, destination *os.Root) (regularFileStats, error) {
	gzipReader, err := gzip.NewReader(source)
	if err != nil {
		return regularFileStats{}, fmt.Errorf("open gzip stream: %w", err)
	}
	tarReader := tar.NewReader(gzipReader)
	paths := make(storedArchivePathTracker)
	var totals regularFileStats
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			if _, err := io.Copy(io.Discard, gzipReader); err != nil {
				gzipReader.Close()
				return regularFileStats{}, fmt.Errorf("finish gzip stream: %w", err)
			}
			if err := gzipReader.Close(); err != nil {
				return regularFileStats{}, fmt.Errorf("close gzip stream: %w", err)
			}
			return totals, nil
		}
		if err != nil {
			gzipReader.Close()
			return regularFileStats{}, fmt.Errorf("read tar entry: %w", err)
		}
		isDirectory := header.Typeflag == tar.TypeDir
		isRegular := header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA
		if !isDirectory && !isRegular {
			gzipReader.Close()
			return regularFileStats{}, fmt.Errorf("unsupported tar entry type for %q", header.Name)
		}
		if header.Size < 0 || (isDirectory && header.Size != 0) {
			gzipReader.Close()
			return regularFileStats{}, fmt.Errorf("invalid tar entry size for %q", header.Name)
		}
		name, err := safepath.CanonicalRelativePath(header.Name, isDirectory)
		if err != nil {
			gzipReader.Close()
			return regularFileStats{}, fmt.Errorf("invalid tar entry path %q: %w", header.Name, err)
		}
		if name == "." {
			continue
		}
		if err := paths.add(name, isDirectory); err != nil {
			gzipReader.Close()
			return regularFileStats{}, err
		}
		if isDirectory {
			if destination != nil {
				mode := os.FileMode(header.Mode).Perm()
				if mode == 0 {
					mode = 0o755
				}
				if err := ensureRootDirectory(destination, name, mode); err != nil {
					gzipReader.Close()
					return regularFileStats{}, err
				}
			}
			continue
		}
		if err := totals.add(header.Size); err != nil {
			gzipReader.Close()
			return regularFileStats{}, err
		}
		if destination == nil {
			if _, err := io.CopyN(io.Discard, tarReader, header.Size); err != nil {
				gzipReader.Close()
				return regularFileStats{}, fmt.Errorf("read tar entry %q: %w", name, err)
			}
			continue
		}
		mode := os.FileMode(header.Mode).Perm()
		if mode == 0 {
			mode = 0o644
		}
		if err := writeArchiveFile(destination, name, mode, tarReader, header.Size); err != nil {
			gzipReader.Close()
			return regularFileStats{}, err
		}
	}
}

func extractVersionArchive(siteRoot *os.Root, archiveName string, destination *os.Root) (regularFileStats, error) {
	if err := requireRealFile(siteRoot, archiveName); err != nil {
		return regularFileStats{}, err
	}
	file, err := siteRoot.Open(archiveName)
	if err != nil {
		return regularFileStats{}, err
	}
	totals, extractErr := scanVersionArchive(file, destination)
	closeErr := file.Close()
	if extractErr != nil {
		return regularFileStats{}, extractErr
	}
	if closeErr != nil {
		return regularFileStats{}, closeErr
	}
	return totals, nil
}

func ensureRootDirectory(root *os.Root, name string, finalMode os.FileMode) error {
	parts := strings.Split(name, "/")
	for index := range parts {
		current := strings.Join(parts[:index+1], "/")
		mode := os.FileMode(0o755)
		if index == len(parts)-1 {
			mode = finalMode
		}
		if err := root.Mkdir(current, mode); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create archive directory %q: %w", current, err)
		}
		if err := requireRealDir(root, current); err != nil {
			return fmt.Errorf("inspect archive directory %q: %w", current, err)
		}
	}
	return nil
}

func writeArchiveFile(root *os.Root, name string, mode os.FileMode, source io.Reader, size int64) error {
	parent := path.Dir(name)
	if parent != "." {
		if err := ensureRootDirectory(root, parent, 0o755); err != nil {
			return err
		}
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create archive file %q: %w", name, err)
	}
	_, copyErr := io.CopyN(file, source, size)
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("write archive file %q: %w", name, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close archive file %q: %w", name, closeErr)
	}
	return nil
}

// publishVerifiedArchive owns archiveFile. Raw is not touched until a second
// read has validated the complete gzip stream and its regular-file totals.
func publishVerifiedArchive(siteRoot *os.Root, temporary, archiveName, versionName string, expected regularFileStats, archiveFile *os.File) error {
	defer siteRoot.Remove(temporary)
	verified, verifyErr := readArchiveStats(siteRoot, temporary)
	if verifyErr == nil && verified != expected {
		verifyErr = fmt.Errorf("regular-file totals changed")
	}
	if verifyErr != nil {
		_ = archiveFile.Close()
		return fmt.Errorf("verify temporary version archive: %w", verifyErr)
	}
	if err := archiveFile.Sync(); err != nil {
		_ = archiveFile.Close()
		return fmt.Errorf("sync temporary version archive: %w", err)
	}
	if err := archiveFile.Close(); err != nil {
		return fmt.Errorf("close temporary version archive: %w", err)
	}
	if err := siteRoot.Rename(temporary, archiveName); err != nil {
		return fmt.Errorf("publish version archive: %w", err)
	}
	if err := syncRootDirectory(siteRoot); err != nil {
		return fmt.Errorf("sync published version archive: %w", err)
	}
	return retireRawVersion(siteRoot, versionName)
}

func retireRawVersion(siteRoot *os.Root, versionName string) error {
	return retireRawVersionWithCleanup(siteRoot, versionName, syncRootDirectory, removeTree)
}

func retireRawVersionWithCleanup(
	siteRoot *os.Root,
	versionName string,
	syncDirectory func(*os.Root) error,
	remove func(*os.Root, string) error,
) error {
	var temporary string
	for attempt := 0; attempt < 8; attempt++ {
		candidate, err := uniqueName(".tmp-remove-" + versionName + "-")
		if err != nil {
			return err
		}
		if _, err := siteRoot.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			temporary = candidate
			break
		} else if err != nil {
			return fmt.Errorf("inspect raw-removal staging name: %w", err)
		}
	}
	if temporary == "" {
		return fmt.Errorf("could not allocate raw-removal staging name")
	}
	// Removing a large tree in place could leave a partial canonical vN after
	// a kill, which raw-wins recovery would mistake for authoritative. Retire
	// the canonical name atomically, then remove only the transient tree.
	if err := siteRoot.Rename(versionName, temporary); err != nil {
		return fmt.Errorf("retire archived version directory: %w", err)
	}
	if err := syncDirectory(siteRoot); err != nil {
		// The archive is already durable and authoritative. Preserve the complete
		// transient tree when rename durability is uncertain; its contents may be
		// needed after a crash.
		return nil
	}
	if err := remove(siteRoot, temporary); err != nil {
		// Cleanup cannot be reported as an archive failure after the canonical raw
		// name is gone, and restoring a partially removed tree would be unsafe.
		return nil
	}
	// The archive remains authoritative if persisting transient cleanup fails.
	_ = syncDirectory(siteRoot)
	return nil
}

func syncRootFile(root *os.Root, name string) error {
	if err := requireRealFile(root, name); err != nil {
		return err
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func syncRootTreeDirectories(root *os.Root) error {
	directories := make([]string, 0)
	if err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, name)
		}
		return nil
	}); err != nil {
		return err
	}
	// Child directory entries must be durable before their parent is published.
	for index := len(directories) - 1; index >= 0; index-- {
		directory, err := root.Open(directories[index])
		if err != nil {
			return err
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func syncRootDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func writeRootFile(root *os.Root, name string, content []byte) error {
	parent := path.Dir(name)
	if parent != "." {
		parts := strings.Split(parent, "/")
		for index := range parts {
			directory := strings.Join(parts[:index+1], "/")
			info, err := root.Lstat(directory)
			if errors.Is(err, os.ErrNotExist) {
				if err := root.Mkdir(directory, 0o755); err != nil {
					return fmt.Errorf("create directory %q: %w", directory, err)
				}
				info, err = root.Lstat(directory)
			}
			if err != nil {
				return fmt.Errorf("inspect directory %q: %w", directory, err)
			}
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("parent %q is not a real directory", directory)
			}
		}
	}

	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create file %q: %w", name, err)
	}
	_, copyErr := io.Copy(file, bytes.NewReader(content))
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("write file %q: %w", name, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close file %q: %w", name, closeErr)
	}
	return nil
}

func createUniqueDir(root *os.Root, prefix string) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		name, err := uniqueName(prefix)
		if err != nil {
			return "", err
		}
		if err := root.Mkdir(name, 0o755); err == nil {
			return name, nil
		} else if !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("create staging directory: %w", err)
		}
	}
	return "", fmt.Errorf("could not allocate unique staging directory")
}

func uniqueName(prefix string) (string, error) {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate temporary name: %w", err)
	}
	return prefix + hex.EncodeToString(random), nil
}

func validateHiddenCurrentToken(token string) error {
	if err := safepath.ValidateSegment(token); err != nil {
		return fmt.Errorf("invalid hidden current token: %w", err)
	}
	encoded := strings.TrimPrefix(token, hiddenCurrentPrefix)
	if encoded == token || len(encoded) != 24 {
		return fmt.Errorf("invalid hidden current token")
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return fmt.Errorf("invalid hidden current token")
	}
	return nil
}

func openAuthorizedCurrent(siteRoot *os.Root, sitePath string) (*os.Root, error) {
	target, err := authorizedLinkTarget(siteRoot, sitePath, "current")
	if err != nil {
		return nil, err
	}
	versionRoot, err := siteRoot.OpenRoot(target)
	if err != nil {
		return nil, fmt.Errorf("open current target %q: %w", target, err)
	}
	return versionRoot, nil
}

func authorizedLinkTarget(siteRoot *os.Root, sitePath, linkName string) (string, error) {
	info, err := siteRoot.Lstat(linkName)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("%s is not a symbolic link", linkName)
	}
	target, err := os.Readlink(filepath.Join(sitePath, linkName))
	if err != nil {
		return "", fmt.Errorf("read %s link: %w", linkName, err)
	}
	if !strings.HasPrefix(target, "v") {
		return "", fmt.Errorf("link target %q is not a version", target)
	}
	version, err := strconv.Atoi(strings.TrimPrefix(target, "v"))
	if err != nil || version < 1 || fmt.Sprintf("v%d", version) != target {
		return "", fmt.Errorf("link target %q is not canonical", target)
	}
	if err := requireRealDir(siteRoot, target); err != nil {
		return "", fmt.Errorf("link target %q is unsafe: %w", target, err)
	}
	return target, nil
}

func removeTree(parent *os.Root, name string) error {
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return parent.Remove(name)
	}

	child, err := parent.OpenRoot(name)
	if err != nil {
		return err
	}
	directory, err := child.Open(".")
	if err != nil {
		child.Close()
		return err
	}
	entries, readErr := directory.ReadDir(-1)
	closeDirErr := directory.Close()
	if readErr != nil {
		child.Close()
		return readErr
	}
	if closeDirErr != nil {
		child.Close()
		return closeDirErr
	}
	for _, entry := range entries {
		if err := removeTree(child, entry.Name()); err != nil {
			child.Close()
			return err
		}
	}
	if err := child.Close(); err != nil {
		return err
	}
	return parent.Remove(name)
}

// RemoveEmptyUserDir removes a namespace's own directory, and only when it is
// already empty. It exists for one caller: deleting a team, which frees the
// name in the database while leaving <SITE_DIR>/<name>/ behind.
//
// That leftover is not cosmetic. The host gate resolves a hostname label by
// listing these directories and fails closed when two of them claim the same
// label, so a stale directory would permanently 404 the hostname of anybody
// who later registers a name folding to the same label — with nothing but a
// log line to explain it. DeleteSite removes <user>/<site> and never the
// containing <user>/, so without this nothing ever clears it.
//
// Refusing to remove a non-empty directory is the safety property: this must
// never be a way to delete somebody's sites. A directory that still holds
// anything is left alone and reported, because that means the caller's own
// precondition — no sites — was not actually true.
func (s *DiskStorage) RemoveEmptyUserDir(user string) error {
	if err := safepath.ValidateSegment(user); err != nil {
		return fmt.Errorf("invalid user: %w", err)
	}

	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()

	entries, err := fs.ReadDir(s.root.FS(), user)
	if errors.Is(err, os.ErrNotExist) {
		// Never published anything, so there is nothing to clear.
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect user directory %s: %w", user, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return fmt.Errorf("user directory %s is not empty: %s", user, strings.Join(names, ", "))
	}
	if err := s.root.Remove(user); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove user directory %s: %w", user, err)
	}
	return nil
}
