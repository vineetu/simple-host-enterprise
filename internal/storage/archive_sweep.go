package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ArchiveSweepResult reports what one bounded sweep did. Bytes are measured
// before and after so the reclaimed space is an observation rather than an
// estimate.
type ArchiveSweepResult struct {
	Scanned      int
	Archived     int
	Failed       int
	BytesBefore  int64
	BytesAfter   int64
	FirstFailure string
	ReachedLimit bool
}

// ArchiveIdleVersions compresses non-live version directories, up to limit.
//
// Deploying already compresses the version it supersedes, but that only reaches
// sites that deploy again. Sites that were last touched months ago keep their
// expanded history forever, and that history is most of the disk. This is the
// one-off pass for that backlog, run in bounded batches so it can be watched
// and stopped.
//
// It must run inside the server process: the per-site locks are plain mutexes
// and the lease table is an in-memory map, so a separate tool would archive
// versions out from under live deploys.
//
// A version that fails is counted and skipped; one bad site never stops the run.
func (s *DiskStorage) ArchiveIdleVersions(limit int) (ArchiveSweepResult, error) {
	var result ArchiveSweepResult
	if limit <= 0 {
		return result, nil
	}

	users, err := os.ReadDir(s.basePath)
	if err != nil {
		return result, fmt.Errorf("list site data: %w", err)
	}

	for _, userEntry := range users {
		if !userEntry.IsDir() {
			continue
		}
		user := userEntry.Name()
		sites, err := os.ReadDir(filepath.Join(s.basePath, user))
		if err != nil {
			continue
		}
		for _, siteEntry := range sites {
			if !siteEntry.IsDir() {
				continue
			}
			siteName := siteEntry.Name()
			if result.Archived >= limit {
				result.ReachedLimit = true
				return result, nil
			}
			s.sweepSite(user, siteName, limit, &result)
		}
	}
	return result, nil
}

func (s *DiskStorage) sweepSite(user, siteName string, limit int, result *ArchiveSweepResult) {
	sitePath := filepath.Join(s.basePath, user, siteName)

	// The live version is read here only to skip it cheaply. ArchiveVersion
	// re-checks it under the lock, including hidden rollback tokens, so a
	// version becoming live between this read and the archive is still refused.
	live := ""
	if target, err := os.Readlink(filepath.Join(sitePath, "current")); err == nil {
		live = target
	}

	entries, err := os.ReadDir(sitePath)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == live {
			continue
		}
		if _, ok := parseVersionName(name); !ok {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if result.Archived >= limit {
			result.ReachedLimit = true
			return
		}
		version, ok := parseVersionName(name)
		if !ok {
			continue
		}
		result.Scanned++

		before := directoryBytes(filepath.Join(sitePath, name))
		if err := s.ArchiveVersion(user, siteName, version); err != nil {
			result.Failed++
			if result.FirstFailure == "" {
				result.FirstFailure = fmt.Sprintf("%s/%s/%s: %v", user, siteName, name, err)
			}
			continue
		}
		after := int64(0)
		if info, err := os.Lstat(filepath.Join(sitePath, name+".tar.gz")); err == nil {
			after = info.Size()
		}
		result.Archived++
		result.BytesBefore += before
		result.BytesAfter += after
	}
}

// directoryBytes sums regular files beneath root, so the sweep can report what
// it actually reclaimed.
func directoryBytes(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}
