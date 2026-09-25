package handler

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/vsriram/simple-host/internal/storage"
)

// siteDiskUsage is what one site holds in the bucket.
//
// Total counts every retained version and every asset, because that is what
// the bucket actually stores — the server keeps several versions per site,
// and a site whose live version is small can still be the biggest thing in
// it. Live is the active version's own archive, the number the owner would
// recognise as "my site". Sizes are as stored (compressed).
type siteDiskUsage struct {
	totalBytes uint64
	liveBytes  uint64
}

// siteStorage is one measurement of the whole bucket, keyed by site id.
type siteStorage struct {
	bySite     map[string]storage.SiteUsage
	totalBytes uint64
}

func (s siteStorage) site(siteID string, activeVersion int) siteDiskUsage {
	usage := s.bySite[siteID]
	return siteDiskUsage{
		totalBytes: uint64(max(usage.TotalBytes, 0)),
		liveBytes:  uint64(max(usage.VersionBytes[activeVersion], 0)),
	}
}

// diskUsageCacheTTL bounds how stale the admin ranking can be. Measuring means
// listing the whole bucket, so it is not something to do on every page load,
// but it is also not data that changes minute to minute.
const diskUsageCacheTTL = 5 * time.Minute

var siteUsageCache struct {
	sync.Mutex
	measuredAt time.Time
	usage      siteStorage
}

// measureSiteStorage returns per-site bucket usage, recomputing at most once
// per diskUsageCacheTTL. Concurrent callers share one measurement.
//
// A failed listing yields no numbers rather than an error: this feeds a
// ranking, and a blank size is better than a broken dashboard.
func measureSiteStorage(ctx context.Context, store *storage.Store) siteStorage {
	if store == nil {
		return siteStorage{}
	}
	siteUsageCache.Lock()
	defer siteUsageCache.Unlock()
	if time.Since(siteUsageCache.measuredAt) < diskUsageCacheTTL && siteUsageCache.usage.bySite != nil {
		return siteUsageCache.usage
	}
	listCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	bySite, err := store.Usage(listCtx)
	if err != nil {
		log.Printf("admin storage usage: %v", err)
		return siteStorage{}
	}
	measured := siteStorage{bySite: bySite}
	for _, usage := range bySite {
		measured.totalBytes += uint64(max(usage.TotalBytes, 0))
	}
	siteUsageCache.measuredAt = time.Now()
	siteUsageCache.usage = measured
	return measured
}
