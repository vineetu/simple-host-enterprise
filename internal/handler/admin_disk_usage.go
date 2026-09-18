package handler

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// siteDiskUsage is the on-disk footprint of one site.
//
// Total counts every retained version, because that is what actually consumes
// the volume — the server keeps several versions per site, and a site whose
// live version is small can still be the biggest thing on the disk. Live is the
// version the `current` link points at, which is the number the owner would
// recognise as "my site".
type siteDiskUsage struct {
	totalBytes uint64
	liveBytes  uint64
}

// diskUsageCacheTTL bounds how stale the admin ranking can be. Measuring means
// walking the whole site tree, so it is not something to do on every page load,
// but it is also not data that changes minute to minute.
const diskUsageCacheTTL = 5 * time.Minute

var siteUsageCache struct {
	sync.Mutex
	measuredAt time.Time
	root       string
	usage      map[string]siteDiskUsage
}

// measureSiteDiskUsage returns per-site disk usage keyed by "<user>/<site>",
// recomputing at most once per diskUsageCacheTTL. Concurrent callers share one
// measurement rather than stampeding the filesystem.
//
// A partially unreadable tree yields partial numbers rather than an error: this
// feeds a ranking, and a missing site is better than a blank card.
func measureSiteDiskUsage(siteDir string) map[string]siteDiskUsage {
	siteUsageCache.Lock()
	defer siteUsageCache.Unlock()

	fresh := time.Since(siteUsageCache.measuredAt) < diskUsageCacheTTL
	if fresh && siteUsageCache.root == siteDir && siteUsageCache.usage != nil {
		return siteUsageCache.usage
	}

	usage := walkSiteDiskUsage(siteDir)
	siteUsageCache.measuredAt = time.Now()
	siteUsageCache.root = siteDir
	siteUsageCache.usage = usage
	return usage
}

func walkSiteDiskUsage(siteDir string) map[string]siteDiskUsage {
	usage := make(map[string]siteDiskUsage)
	if siteDir == "" {
		return usage
	}

	users, err := os.ReadDir(siteDir)
	if err != nil {
		return usage
	}

	for _, userEntry := range users {
		if !userEntry.IsDir() {
			continue
		}
		userPath := filepath.Join(siteDir, userEntry.Name())
		sites, err := os.ReadDir(userPath)
		if err != nil {
			continue
		}

		for _, siteEntry := range sites {
			if !siteEntry.IsDir() {
				continue
			}
			sitePath := filepath.Join(userPath, siteEntry.Name())
			usage[userEntry.Name()+"/"+siteEntry.Name()] = measureOneSite(sitePath)
		}
	}
	return usage
}

// measureOneSite sizes every version directory under a site and works out which
// one is live by reading the `current` link.
func measureOneSite(sitePath string) siteDiskUsage {
	var out siteDiskUsage

	entries, err := os.ReadDir(sitePath)
	if err != nil {
		return out
	}

	byVersionDir := make(map[string]uint64, len(entries))
	for _, entry := range entries {
		// os.ReadDir reports the link itself, not its target, so `current` is
		// not a directory here and its bytes are never counted twice.
		if !entry.IsDir() {
			continue
		}
		size := directorySize(filepath.Join(sitePath, entry.Name()))
		byVersionDir[entry.Name()] = size
		out.totalBytes += size
	}

	if target, err := os.Readlink(filepath.Join(sitePath, "current")); err == nil {
		out.liveBytes = byVersionDir[filepath.Base(target)]
	}
	return out
}

// directorySize sums regular files beneath root. Symlinks are counted as the
// link, not the target, so nothing is double counted and nothing outside the
// tree is followed.
func directorySize(root string) uint64 {
	var total uint64
	_ = filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip what we cannot read; keep the rest of the total
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		total += uint64(info.Size())
		return nil
	})
	return total
}
