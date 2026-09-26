package handler

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"

	"github.com/vsriram/simple-host/internal/audit"
	db "github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/scan"
	"github.com/vsriram/simple-host/internal/storage"
)

// UploadQuota is the per-owner limits (QUOTA_MAX_*). An owner is a person's
// namespace or a team's; both are users rows. Zero MaxSites or MaxBytes is
// unlimited. A handler nobody configures enforces nothing and keeps
// defaultMaxVersions.
type UploadQuota struct {
	MaxSites    int64
	MaxBytes    int64
	MaxVersions int
}

// defaultMaxVersions is how many versions of a site are kept when
// QUOTA_MAX_VERSIONS is not wired in (tests); the config default is the same.
const defaultMaxVersions = 5

func (q UploadQuota) versionsToKeep() int {
	if q.MaxVersions < 1 {
		return defaultMaxVersions
	}
	return q.MaxVersions
}

func (q UploadQuota) enforced() bool { return q.MaxSites > 0 || q.MaxBytes > 0 }

// quotaRefusal is a deploy or upload the owner's quota does not admit.
type quotaRefusal struct {
	status int
	body   errorResponse
}

func (r *quotaRefusal) write(w http.ResponseWriter) { writeJSON(w, r.status, r.body) }

// checkOwnerQuota runs at the end of a deploy or upload transaction, after
// its own rows are written: it takes the owner's quota lock and counts what
// the owner holds, this transaction included. newSite says the transaction
// created a site; added and freed are the stored bytes it added and released
// (versions pruned). A change that frees at least as much as it adds is
// always allowed, so an owner who is over (after the limits were lowered, or
// with sizes still being filled in) can still replace a site with a smaller
// one.
func checkOwnerQuota(ctx context.Context, tx *sql.Tx, quota UploadQuota, ownerID string, newSite bool, added, freed int64) (*quotaRefusal, error) {
	if !quota.enforced() {
		return nil, nil
	}
	if err := db.LockOwnerQuota(ctx, tx, ownerID); err != nil {
		return nil, err
	}
	usage, err := db.OwnerUsageOf(ctx, tx, ownerID)
	if err != nil {
		return nil, err
	}
	if newSite && quota.MaxSites > 0 && usage.Sites > quota.MaxSites {
		return &quotaRefusal{status: http.StatusConflict, body: errorResponse{
			Error: fmt.Sprintf("too many sites (%s of %s): delete a site to create another", formatCount(usage.Sites-1), formatCount(quota.MaxSites)),
			Code:  "site_limit",
		}}, nil
	}
	if quota.MaxBytes > 0 && usage.Bytes > quota.MaxBytes && added > freed {
		return storageQuotaRefusal(usage.Bytes-added+freed, added, quota.MaxBytes), nil
	}
	return nil, nil
}

func storageQuotaRefusal(used, adding, limit int64) *quotaRefusal {
	return &quotaRefusal{status: http.StatusRequestEntityTooLarge, body: errorResponse{
		Error: fmt.Sprintf("storage quota exceeded (used %s of %s; this upload needs %s)",
			formatBytes(uint64(max(used, 0))), formatBytes(uint64(limit)), formatBytes(uint64(adding))),
		Code: "storage_quota",
	}}
}

// pruneVersions removes the site's versions beyond the quota's retention,
// oldest first and never the active one, in tx, and queues their objects for
// the retire sweep. It returns the stored bytes that frees.
func pruneVersions(ctx context.Context, tx *sql.Tx, quota UploadQuota, siteID string, activeVersion int) (int64, error) {
	pruned, err := db.PruneVersions(ctx, tx, siteID, activeVersion, quota.versionsToKeep())
	if err != nil {
		return 0, err
	}
	var freed int64
	for _, version := range pruned {
		key, err := storage.VersionKey(siteID, version.VersionNumber)
		if err != nil {
			return 0, err
		}
		if err := db.RetireObjects(ctx, tx, key, storage.RetireGrace); err != nil {
			return 0, err
		}
		freed += version.SizeBytes
	}
	return freed, nil
}

// scanRefusal is an upload the malware scan refused or could not clear.
type scanRefusal struct {
	status    int
	body      errorResponse
	file      string
	signature string
}

func (r *scanRefusal) write(w http.ResponseWriter) { writeJSON(w, r.status, r.body) }

func infectedRefusal(file, signature string) *scanRefusal {
	return &scanRefusal{
		status: http.StatusUnprocessableEntity,
		body: errorResponse{
			Error: fmt.Sprintf("upload rejected: %s is infected (%s); nothing was stored", file, signature),
			Code:  "malware_found",
		},
		file: file, signature: signature,
	}
}

func scannerDownRefusal() *scanRefusal {
	return &scanRefusal{
		status: http.StatusServiceUnavailable,
		body: errorResponse{
			Error: "the malware scanner is unavailable, so the upload was refused; nothing was stored",
			Code:  "scanner_unavailable",
		},
	}
}

// scanFiles scans every file of a deploy, in name order, and stops at the
// first infected one. nil scanner means scanning is off.
func scanFiles(ctx context.Context, scanner scan.Scanner, files map[string][]byte) *scanRefusal {
	if scanner == nil {
		return nil
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		signature, err := scanner.Scan(ctx, bytes.NewReader(files[name]))
		if err != nil {
			log.Printf("malware scan of %q: %v", name, err)
			return scannerDownRefusal()
		}
		if signature != "" {
			return infectedRefusal(name, signature)
		}
	}
	return nil
}

// recordInfected audits a refused infected upload. Best effort, like any
// Record: nothing was stored, so there is no transaction to join.
func recordInfected(ctx context.Context, recorder audit.Recorder, event audit.Event, refusal *scanRefusal, extra map[string]any) {
	if refusal.signature == "" || recorder == nil {
		return
	}
	event.Action = "upload_infected"
	event.Extra = map[string]any{"file": refusal.file, "signature": refusal.signature}
	for k, v := range extra {
		event.Extra[k] = v
	}
	recorder.Record(ctx, event)
}

// scanDeploy scans a deploy's files before anything is stored, writing the
// refusal and auditing an infected upload. siteID is empty for a new site.
func (h *SiteHandler) scanDeploy(w http.ResponseWriter, r *http.Request, target mutationTarget, siteName, siteID string, files map[string][]byte) bool {
	refusal := scanFiles(r.Context(), h.scanner, files)
	if refusal == nil {
		return true
	}
	actorKind, keyID := auditActorKind(r.Context())
	recordInfected(r.Context(), h.audit, audit.Event{
		ActorID: target.ActorID, ActorKind: actorKind, KeyID: keyID,
		OwnerID: target.OwnerID, SiteID: siteID, RequestID: auditRequestID(r.Context()),
	}, refusal, map[string]any{"site": siteName, "kind": "deploy"})
	refusal.write(w)
	return false
}

// enforceQuota records the new version's stored size and checks the owner's
// quota in the deploy transaction, writing the refusal when it fails.
func (h *SiteHandler) enforceQuota(w http.ResponseWriter, r *http.Request, tx *sql.Tx, ownerID, versionID string, added, freed int64, newSite bool) bool {
	if err := db.SetVersionSize(r.Context(), tx, versionID, added); err != nil {
		log.Printf("record version size %s: %v", versionID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return false
	}
	refusal, err := checkOwnerQuota(r.Context(), tx, h.quota, ownerID, newSite, added, freed)
	if err != nil {
		log.Printf("check quota for owner %s: %v", ownerID, err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "internal server error"})
		return false
	}
	if refusal != nil {
		refusal.write(w)
		return false
	}
	return true
}

// scanAsset scans an uploaded asset before it is stored and rewinds it for
// the store. It audits an infected upload.
func (h *SiteAPIHandler) scanAsset(r *http.Request, call siteAPICall, name string, file io.ReadSeeker) *scanRefusal {
	signature, err := h.scanner.Scan(r.Context(), file)
	if err != nil {
		log.Printf("malware scan of asset for %s/%s: %v", call.Owner, call.SiteName, err)
		return scannerDownRefusal()
	}
	if signature != "" {
		refusal := infectedRefusal(name, signature)
		recordInfected(r.Context(), h.audit, h.auditEvent(r.Context(), h.database, call, "", nil), refusal, map[string]any{"site": call.SiteName, "kind": "asset"})
		return refusal
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		log.Printf("rewind scanned asset for %s/%s: %v", call.Owner, call.SiteName, err)
		return scannerDownRefusal()
	}
	return nil
}

// checkAssetQuota checks, under the owner's quota lock, that the owner's
// stored bytes leave room for an asset of size.
func (h *SiteAPIHandler) checkAssetQuota(ctx context.Context, tx *sql.Tx, ownerUsername string, size int64) (*quotaRefusal, error) {
	owner, err := db.GetUserByUsername(ctx, tx, ownerUsername)
	if err != nil {
		return nil, err
	}
	if err := db.LockOwnerQuota(ctx, tx, owner.ID); err != nil {
		return nil, err
	}
	usage, err := db.OwnerUsageOf(ctx, tx, owner.ID)
	if err != nil {
		return nil, err
	}
	if usage.Bytes+size > h.quota.MaxBytes {
		return storageQuotaRefusal(usage.Bytes, size, h.quota.MaxBytes), nil
	}
	return nil, nil
}

type namespaceRef struct{ ID, Name string }

// ownerUsage is one namespace's usage against its quota, as GET /api/me
// reports it and the dashboard shows it. A zero maximum is unlimited.
type ownerUsage struct {
	Owner    string `json:"owner"`
	Sites    int64  `json:"sites"`
	MaxSites int64  `json:"max_sites"`
	Bytes    int64  `json:"bytes"`
	MaxBytes int64  `json:"max_bytes"`
}

func ownerUsages(ctx context.Context, q db.Querier, quota UploadQuota, owners []namespaceRef) ([]ownerUsage, error) {
	out := make([]ownerUsage, 0, len(owners))
	for _, owner := range owners {
		usage, err := db.OwnerUsageOf(ctx, q, owner.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ownerUsage{Owner: owner.Name, Sites: usage.Sites, MaxSites: quota.MaxSites, Bytes: usage.Bytes, MaxBytes: quota.MaxBytes})
	}
	return out, nil
}
