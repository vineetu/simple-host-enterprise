package handler

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"

	"github.com/vsriram/simple-host/internal/safepath"
	"github.com/vsriram/simple-host/internal/tarball"
)

// errMergedTooLarge: the files sent plus the live files kept exceed the
// site limits.
var errMergedTooLarge = errors.New("the files sent plus the live files kept are larger than a site may be")

// errMergedNoIndex: the merged version would have no index.html.
var errMergedNoIndex = errors.New("the new version would have no index.html at the site root, so the site would not serve")

// deployKeep reads ?keep=1 (keep every live file the archive does not
// replace) and ?remove=<path>, repeatable, only with keep. The default is
// unchanged: the archive is the whole new version.
func deployKeep(w http.ResponseWriter, r *http.Request) (keep bool, remove map[string]bool, ok bool) {
	q := r.URL.Query()
	switch q.Get("keep") {
	case "", "0", "false":
	case "1", "true":
		keep = true
	default:
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "keep must be 1 or 0"})
		return false, nil, false
	}
	raw := q["remove"]
	if len(raw) > 0 && !keep {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "remove requires keep=1"})
		return false, nil, false
	}
	remove = map[string]bool{}
	for _, p := range raw {
		clean, err := safepath.CanonicalRelativePath(p, false)
		if err != nil || clean == "." {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "unsafe path in remove"})
			return false, nil, false
		}
		remove[clean] = true
	}
	return keep, remove, true
}

// keepLiveFiles adds to files every file of the site's live version that the
// upload does not replace and remove does not name, then drops the removed
// paths. Run under the site's mutation lock, after the If-Match check, so the
// version merged is the one the caller saw.
func (h *SiteHandler) keepLiveFiles(ctx context.Context, siteID string, active int, files map[string][]byte, remove map[string]bool) error {
	totalLimit, fileLimit, entryLimit := tarball.MergeLimits()
	var total int64
	for _, b := range files {
		total += int64(len(b))
	}
	if active > 0 {
		lease, err := h.store.OpenVersion(ctx, siteID, active)
		if err != nil {
			return fmt.Errorf("open live version: %w", err)
		}
		defer lease.Close()
		root := lease.FS()
		err = fs.WalkDir(root, ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return errors.New("live version contains a non-regular file")
			}
			if remove[name] {
				return nil
			}
			if _, sent := files[name]; sent {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
			if total > totalLimit || info.Size() > fileLimit || len(files)+1 > entryLimit {
				return errMergedTooLarge
			}
			body, err := fs.ReadFile(root, name)
			if err != nil {
				return err
			}
			files[name] = body
			return nil
		})
		if err != nil {
			return err
		}
	}
	for p := range remove {
		delete(files, p)
	}
	if _, ok := files["index.html"]; !ok {
		return errMergedNoIndex
	}
	return nil
}

// writeKeepError answers a failed merge.
func writeKeepError(w http.ResponseWriter, err error) {
	if errors.Is(err, errMergedNoIndex) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: errMergedNoIndex.Error()})
		return
	}
	if errors.Is(err, errMergedTooLarge) {
		writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: errMergedTooLarge.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
}
