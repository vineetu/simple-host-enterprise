package handler

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/vsriram/simple-host/internal/auth"
)

const photoCountLimit = 30
const photoMaxBytes = 1000000
const photoMaxDimension = 1600

var photoName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,59}\.(webp|jpg)$`)

type photoResult struct {
	Path   string `json:"path"`
	URL    string `json:"url"`
	Bytes  int    `json:"bytes"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// addPhotos validates client-encoded photos, then uses the ordinary versioned
// update, including its owner/team checks, ETag, bucket write, quota, and audit.
func (h *SiteHandler) addPhotos(w http.ResponseWriter, r *http.Request) {
	user := auth.GetUser(r.Context())
	if user == nil {
		writeJSON(w, 401, errorResponse{Error: "unauthorized"})
		return
	}
	owner := r.URL.Query().Get("owner")
	if owner == "" {
		owner = user.Username
	}
	r.SetPathValue("owner", owner)
	r.SetPathValue("sitename", r.PathValue("site"))
	owner, name, ok := validatedCollaborationPath(w, r)
	if !ok {
		return
	}
	access, ok := h.resolveCollaborationAccess(w, r, owner, name)
	if !ok || !requireOwnerRole(w, access) {
		return
	}
	if !requireSitePrecondition(w, r, access.Site, false) {
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(60 * time.Second))
	defer rc.SetReadDeadline(time.Time{})
	replayCreate(w, r, h.database, user.ID, access.Site.ID, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const bodyCap = photoCountLimit*(photoMaxBytes+4096) + 4096
		if r.ContentLength > bodyCap {
			writeJSON(w, 413, errorResponse{Error: "The photo upload is too large."})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, bodyCap)
		reader, err := r.MultipartReader()
		if err != nil {
			writeJSON(w, 400, errorResponse{Error: "Choose photos to upload."})
			return
		}
		files := map[string][]byte{}
		lease, err := h.store.OpenVersion(r.Context(), access.Site.ID, access.Site.ActiveVersion)
		if err != nil {
			writeJSON(w, 500, errorResponse{Error: "Could not read this site."})
			return
		}
		err = fs.WalkDir(lease.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				files[path] = nil
			}
			return r.Context().Err()
		})
		lease.Close()
		if err != nil {
			writeJSON(w, 500, errorResponse{Error: "Could not read this site."})
			return
		}
		occupied, err := photoOccupiedPaths(r.Context(), files)
		if err != nil {
			return
		}
		files = map[string][]byte{}
		results := []photoResult{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				photoBodyError(w, err)
				return
			}
			if len(results) == photoCountLimit {
				writeJSON(w, 400, errorResponse{Error: "Choose up to 30 photos at a time."})
				return
			}
			_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			filename := params["filename"]
			if err != nil || part.FormName() != "photos" || !photoName.MatchString(filename) {
				writeJSON(w, 400, errorResponse{Error: "Use lowercase letters, numbers, and hyphens, followed by .webp or .jpg."})
				return
			}
			data, err := io.ReadAll(io.LimitReader(part, photoMaxBytes+1))
			if len(data) > photoMaxBytes {
				writeJSON(w, 413, errorResponse{Error: "A photo is too large. Choose it again to make a smaller copy."})
				return
			}
			if err != nil {
				photoBodyError(w, err)
				return
			}
			cfg, ext, err := validatePhoto(data, photoMaxDimension)
			if err != nil {
				writeJSON(w, 400, errorResponse{Error: err.Error()})
				return
			}
			if !strings.HasSuffix(filename, "."+ext) {
				writeJSON(w, 400, errorResponse{Error: "The photo extension does not match its contents."})
				return
			}
			path, err := nextPhotoPath(r.Context(), occupied, filename)
			if err != nil {
				writeJSON(w, 409, errorResponse{Error: err.Error()})
				return
			}
			occupied[path] = false
			files[path] = data
			results = append(results, photoResult{Path: path, Bytes: len(data), Width: cfg.Width, Height: cfg.Height})
		}
		if len(results) == 0 {
			writeJSON(w, 400, errorResponse{Error: "Choose at least one photo."})
			return
		}
		var archive bytes.Buffer
		gz := gzip.NewWriter(&archive)
		tw := tar.NewWriter(gz)
		for path, data := range files {
			if err := tw.WriteHeader(&tar.Header{Name: path, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
				writeJSON(w, 500, errorResponse{Error: "Could not prepare photos."})
				return
			}
			if _, err := tw.Write(data); err != nil {
				writeJSON(w, 500, errorResponse{Error: "Could not prepare photos."})
				return
			}
		}
		if err := tw.Close(); err != nil {
			writeJSON(w, 500, errorResponse{Error: "Could not prepare photos."})
			return
		}
		if err := gz.Close(); err != nil {
			writeJSON(w, 500, errorResponse{Error: "Could not prepare photos."})
			return
		}
		update := r.Clone(r.Context())
		update.Method = "PUT"
		update.URL.RawQuery = "keep=1"
		update.Header.Set("If-Match", formatSiteETag(access.Site.ID, access.Site.ActiveVersion))
		update.Header.Set("Content-Type", "application/gzip")
		update.Body = io.NopCloser(bytes.NewReader(archive.Bytes()))
		update.ContentLength = int64(archive.Len())
		reply := &photoResponse{header: w.Header(), status: 200}
		h.updateCollaborationSite(reply, update)
		if reply.status >= 400 {
			w.WriteHeader(reply.status)
			w.Write(reply.body.Bytes())
			return
		}
		var site collaborationSiteResponse
		if json.Unmarshal(reply.body.Bytes(), &site) != nil {
			writeJSON(w, 500, errorResponse{Error: "Could not confirm the save. Check the site before retrying."})
			return
		}
		for i := range results {
			results[i].URL = strings.TrimRight(site.URL, "/") + "/" + results[i].Path
		}
		writeJSON(w, 200, struct {
			Site    string        `json:"site"`
			Version int           `json:"version"`
			Photos  []photoResult `json:"photos"`
		}{owner + "/" + name, site.ActiveVersion, results})
	}))
}
func photoBodyError(w http.ResponseWriter, err error) {
	status := 400
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		status = 413
	}
	writeJSON(w, status, errorResponse{Error: "The photo upload is incomplete or too large."})
}

type photoResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (p *photoResponse) Header() http.Header         { return p.header }
func (p *photoResponse) WriteHeader(status int)      { p.status = status }
func (p *photoResponse) Write(b []byte) (int, error) { return p.body.Write(b) }
