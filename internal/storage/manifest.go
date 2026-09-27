package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/vsriram/simple-host/internal/db"
	"github.com/vsriram/simple-host/internal/identityhash"
)

// A site's manifest, sites/<site-id>/manifest.json, is the one object that
// says whose a site is: its owner (by name, and by a hash of their sign-in
// identity or their team's id), its name, the version that was live, whether
// it was deleted or restricted by an admin, and its uploaded files' names
// and types, as of the last deploy, rollback, rename, hand-over, delete,
// restore, restriction or file change. Never an email address. The database is the source of truth and
// nothing serving reads the manifest; it exists so that a lost database can
// be rebuilt from the bucket alone (`simple-host rebuild-index`). It is
// written through the same Objects as everything else, so it is encrypted
// the same way, and it goes with the site's prefix when the site is purged.
//
// Saved data, its history, access levels and viewers live only in the
// database and are not in it.

// manifestName is the manifest's name within a site's prefix.
const manifestName = "manifest.json"

// maxManifestBytes bounds a manifest read; a site's asset list is capped
// well below this (ASSET_MAX_SITE_COUNT).
const maxManifestBytes = 16 << 20

// SiteManifest is the manifest's JSON.
type SiteManifest struct {
	SiteID string `json:"site_id"`
	// Seq is one more than the manifest it replaced: writes are made one at
	// a time per site (handler.WriteSiteManifest), so a larger Seq is newer.
	Seq   int64  `json:"seq"`
	Owner string `json:"owner"`
	// OwnerKind is "person" or "team".
	OwnerKind string `json:"owner_kind"`
	// OwnerIdentity is a person's sign-in identity as a keyed hash of issuer
	// and subject (db.ErasedSubjectHash: "<key id>:<hex>", or a plain hex
	// SHA-256 in a manifest from before v1.8), "" for an account with none.
	OwnerIdentity string `json:"owner_identity,omitempty"`
	// TeamID is a team owner's id.
	TeamID      string     `json:"team_id,omitempty"`
	Site        string     `json:"site"`
	LiveVersion int        `json:"live_version"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
	// PurgeAt is when a deleted site stops being recoverable, as promised
	// when it was deleted (sites.purge_at); absent in older manifests.
	PurgeAt *time.Time `json:"purge_at,omitempty"`
	// Restricted is an admin's take-down (access_decision 'restricted'),
	// with the admin's reason.
	Restricted       bool            `json:"restricted,omitempty"`
	RestrictedReason string          `json:"restricted_reason,omitempty"`
	WrittenAt        time.Time       `json:"written_at"`
	Assets           []ManifestAsset `json:"assets"`
	// Sig is an HMAC of the manifest with Sig empty, under a key derived
	// from the session signing key (identityhash.SignManifest):
	// "<key id>:<hex>". rebuild-index refuses a manifest whose signature does
	// not verify, and one without (written before v1.8) unless told to
	// accept it.
	Sig string `json:"sig,omitempty"`
}

// canonical is the manifest's bytes as signed: its JSON with Sig empty and
// every time in UTC.
func (m SiteManifest) canonical() ([]byte, error) {
	m.Sig = ""
	if m.Assets == nil {
		m.Assets = []ManifestAsset{}
	}
	m.WrittenAt = m.WrittenAt.UTC()
	for _, t := range []**time.Time{&m.DeletedAt, &m.PurgeAt} {
		if *t != nil {
			u := (*t).UTC()
			*t = &u
		}
	}
	return json.Marshal(m)
}

// Signature states of a manifest (VerifySignature).
const (
	ManifestSigned     = "signed"
	ManifestUnsigned   = "unsigned"
	ManifestUnknownKey = "unknown_key"
	ManifestBadSig     = "bad_signature"
)

// VerifySignature checks Sig against the configured keys.
func (m SiteManifest) VerifySignature() string {
	if m.Sig == "" {
		return ManifestUnsigned
	}
	body, err := m.canonical()
	if err != nil {
		return ManifestBadSig
	}
	ok, known := identityhash.VerifyManifest(body, m.Sig)
	switch {
	case !known:
		return ManifestUnknownKey
	case !ok:
		return ManifestBadSig
	}
	return ManifestSigned
}

// Owner kinds a manifest records.
const (
	ManifestOwnerPerson = "person"
	ManifestOwnerTeam   = "team"
)

// maxManifestReasonLen bounds a restriction's reason as the manifest keeps it.
const maxManifestReasonLen = 4000

// maxManifestAssetName bounds an uploaded file's name as the manifest keeps it.
const maxManifestAssetName = 1024

// Validate checks every field a rebuild would write into the database, so a
// manifest edited in the bucket (or written by a bug) is refused rather
// than recreated. Names of the owner and the site are checked by the caller
// against the rules a create applies (handler.ValidateRebuiltNames).
func (m SiteManifest) Validate() error {
	if !isUUID(m.SiteID) {
		return errors.New("site_id is not an id")
	}
	if m.Seq < 0 || m.LiveVersion < 1 {
		return errors.New("seq or live_version out of range")
	}
	switch m.OwnerKind {
	case ManifestOwnerPerson:
		if m.TeamID != "" || (m.OwnerIdentity != "" && !identityhash.Valid(m.OwnerIdentity)) {
			return errors.New("a person's owner_identity is not an identity hash, or a team_id is set")
		}
	case ManifestOwnerTeam:
		if !isUUID(m.TeamID) || m.OwnerIdentity != "" {
			return errors.New("a team's team_id is not an id, or an owner_identity is set")
		}
	default:
		return fmt.Errorf("owner_kind %q is neither person nor team", m.OwnerKind)
	}
	if m.PurgeAt != nil && m.DeletedAt == nil {
		return errors.New("purge_at without deleted_at")
	}
	if !m.Restricted && m.RestrictedReason != "" {
		return errors.New("restricted_reason without restricted")
	}
	if len(m.RestrictedReason) > maxManifestReasonLen || !plainText(m.RestrictedReason) {
		return errors.New("restricted_reason is too long or has control characters")
	}
	seen := map[string]bool{}
	for _, a := range m.Assets {
		switch {
		case !isUUID(a.ID) || seen[a.ID]:
			return fmt.Errorf("asset id %q is not an id or is listed twice", a.ID)
		case a.Name == "" || len(a.Name) > maxManifestAssetName || !plainText(a.Name):
			return fmt.Errorf("asset %s: name is empty, too long or has control characters", a.ID)
		case !StoredAssetType(a.ContentType):
			return fmt.Errorf("asset %s: content type %q is not one an upload stores", a.ID, a.ContentType)
		case a.Size < 0 || a.Size > maxVersionObjectBytes:
			return fmt.Errorf("asset %s: size %d out of range", a.ID, a.Size)
		case !isHex(a.SHA256, 64):
			return fmt.Errorf("asset %s: sha256 is not a SHA-256", a.ID)
		}
		seen[a.ID] = true
	}
	return nil
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// plainText is valid UTF-8 with no control or format characters.
func plainText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

// ManifestAsset is one uploaded file as the manifest records it: enough to
// recreate its row (SHA256 is hex).
type ManifestAsset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// ManifestKey names a site's manifest.
func ManifestKey(siteID string) (string, error) {
	prefix, err := SitePrefix(siteID)
	if err != nil {
		return "", err
	}
	return prefix + manifestName, nil
}

// PutSiteManifest writes m under its site's prefix, replacing the last one.
func PutSiteManifest(ctx context.Context, objects Objects, m SiteManifest) error {
	key, err := ManifestKey(m.SiteID)
	if err != nil {
		return err
	}
	canonical, err := m.canonical()
	if err != nil {
		return err
	}
	// Re-read from the canonical bytes, so what is written is exactly what
	// was signed.
	var signed SiteManifest
	if err := json.Unmarshal(canonical, &signed); err != nil {
		return err
	}
	signed.Sig = identityhash.SignManifest(canonical)
	body, err := json.Marshal(signed)
	if err != nil {
		return err
	}
	return objects.Put(ctx, key, body, "application/json")
}

// DeleteSiteManifestInTurn deletes a site's manifest in turn with every
// write of it (db.LockSiteManifest, held until the object is gone), after
// the site's removal has committed: a write that read the site before it
// went finishes its PUT first, and every later write finds the site gone
// and writes nothing, so a purged or erased site's manifest never comes
// back.
func DeleteSiteManifestInTurn(ctx context.Context, database *sql.DB, objects Objects, siteID string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := db.LockSiteManifest(ctx, tx, siteID); err != nil {
		return err
	}
	if err := DeleteSiteManifest(ctx, objects, siteID); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteSiteManifest deletes a site's manifest, for a site gone for good
// (purged, erased), so a rebuild from the bucket never brings it back.
func DeleteSiteManifest(ctx context.Context, objects Objects, siteID string) error {
	key, err := ManifestKey(siteID)
	if err != nil {
		return err
	}
	err = objects.Delete(ctx, key)
	if errors.Is(err, ErrObjectNotFound) {
		return nil
	}
	return err
}

// PutSiteManifest is PutSiteManifest on the store's bucket.
func (s *Store) PutSiteManifest(ctx context.Context, m SiteManifest) error {
	return PutSiteManifest(ctx, s.objects, m)
}

// RecoverableSite is one site found in the bucket: what its manifest says
// (nil when it has none, for a site not deployed since manifests were
// written) and which versions and uploaded files are actually there.
type RecoverableSite struct {
	SiteID      string
	Manifest    *SiteManifest
	ManifestErr error
	Versions    []int
	AssetIDs    map[string]bool
}

// NewestVersion is the newest version archive there, 0 for none: what an
// operator restores by hand for a site with no manifest.
func (r RecoverableSite) NewestVersion() int {
	newest := 0
	for _, v := range r.Versions {
		newest = max(newest, v)
	}
	return newest
}

// LiveArchive reports whether the manifest's live version's archive is
// there. A rebuild makes only that version live, never another: a newer
// archive may be a version stored without being made live, which nobody
// approved.
func (r RecoverableSite) LiveArchive() bool {
	if r.Manifest == nil {
		return false
	}
	for _, v := range r.Versions {
		if v == r.Manifest.LiveVersion {
			return true
		}
	}
	return false
}

// GetSiteManifest reads one site's manifest. ErrObjectNotFound when it has
// none.
func GetSiteManifest(ctx context.Context, objects Objects, siteID string) (*SiteManifest, error) {
	key, err := ManifestKey(siteID)
	if err != nil {
		return nil, err
	}
	body, err := objects.Get(ctx, key, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	var m SiteManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("manifest unreadable: %w", err)
	}
	if m.SiteID != siteID {
		return nil, fmt.Errorf("manifest names site %q, not %q", m.SiteID, siteID)
	}
	return &m, nil
}

// ListRecoverableSites walks everything under sites/ and groups it by site,
// reading each manifest. Sorted by owner, then site, then id; sites
// without a manifest last.
func ListRecoverableSites(ctx context.Context, objects Objects) ([]RecoverableSite, error) {
	listed, err := objects.List(ctx, "sites/")
	if err != nil {
		return nil, err
	}
	byID := map[string]*RecoverableSite{}
	site := func(id string) *RecoverableSite {
		if byID[id] == nil {
			byID[id] = &RecoverableSite{SiteID: id, AssetIDs: map[string]bool{}}
		}
		return byID[id]
	}
	for _, o := range listed {
		rest, found := strings.CutPrefix(o.Key, "sites/")
		if !found || len(rest) < 37 || !isUUID(rest[:36]) || rest[36] != '/' {
			continue
		}
		id, name := rest[:36], rest[37:]
		switch {
		case name == manifestName:
			site(id)
		case strings.HasPrefix(name, "assets/") && isUUID(strings.TrimPrefix(name, "assets/")):
			site(id).AssetIDs[strings.TrimPrefix(name, "assets/")] = true
		default:
			if n, ok := cutVersionArchiveName(name); ok && "v"+fmt.Sprint(n)+".tar.gz" == name {
				s := site(id)
				s.Versions = append(s.Versions, n)
			}
		}
	}
	out := make([]RecoverableSite, 0, len(byID))
	for id, s := range byID {
		sort.Ints(s.Versions)
		m, err := GetSiteManifest(ctx, objects, id)
		switch {
		case err == nil:
			s.Manifest = m
		case errors.Is(err, ErrObjectNotFound):
		default:
			s.ManifestErr = err
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Manifest == nil) != (b.Manifest == nil) {
			return a.Manifest != nil
		}
		if a.Manifest != nil && a.Manifest.Owner != b.Manifest.Owner {
			return a.Manifest.Owner < b.Manifest.Owner
		}
		if a.Manifest != nil && a.Manifest.Site != b.Manifest.Site {
			return a.Manifest.Site < b.Manifest.Site
		}
		return a.SiteID < b.SiteID
	})
	return out, nil
}
