package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// The hostname model. Every site has its own origin,
// "<site>.<owner label>.<base>" (siteHostPart), so no two sites share
// cookies, storage or scripts, whoever owns them. A TLS wildcard covers one
// label, so each owner needs its own "*.<owner>.<base>" certificate; until
// it is ready (OwnerReady) the owner's sites are served at
// "<owner>.<base>/<site>/", and once it is, those addresses redirect. The
// owner's own "<owner>.<base>" keeps the person's or team's index page.
// "<owner>--<site>.<base>", where v1.2 served a site shared with named
// people, redirects; the double hyphen is reserved in owner labels so that
// shape never collides with an owner.
//
// Addresses are only ever computed name -> label here. The host gate
// resolves a label back to a name against the site index.

// ownerLabel maps a username to its DNS label: lowercase, every '.' becomes
// '-'. It is the only place the username -> label rule lives in Go; the
// unique index in internal/migrate/sql/0018_owner_label_unique.sql computes
// the same expression in SQL and must be rebuilt if this rule changes.
func ownerLabel(username string) string {
	return strings.ReplaceAll(strings.ToLower(username), ".", "-")
}

// siteLabel folds a site name the same way ownerLabel folds a username:
// lowercase, every '.' becomes '-'. siteHostPart decides what a name that
// does not fold to a valid label gets instead.
func siteLabel(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), ".", "-")
}

// isValidLabel reports whether label is a DNS label this server will treat as
// an owner label: non-empty, at most 63 bytes, only [a-z0-9-], no leading or
// trailing '-'. It validates and never repairs, so "Alice" is invalid.
func isValidLabel(label string) bool {
	if label == "" || len(label) > 63 {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// normalizeHost turns a raw request Host header into a comparable bare host:
// lowercased, port stripped (including the bracketed IPv6 form "[::1]:8080",
// which becomes "::1"), and one trailing dot removed. Empty input stays empty.
// It is for bare hosts, not origins; origins go through parseRequestOrigin.
//
// Non-ASCII input normalizes to "" (and so classifies as unknown) rather than
// being lowercased: Unicode folding maps characters such as the Kelvin sign
// to ASCII letters, which would let a foreign spelling become a valid label.
// Brackets are unwrapped only around an IP literal, so "[alice.example]" is
// not an alias of "alice.example".
func normalizeHost(raw string) string {
	h := strings.TrimSpace(raw)
	if h == "" {
		return ""
	}
	for i := 0; i < len(h); i++ {
		if h[i] >= 0x80 {
			return ""
		}
	}
	// SplitHostPort allocates an AddrError on failure, and the common case (a
	// base-domain request with no port) would fail it on every request, so
	// only ask it when there is a ':' to split on.
	if strings.Contains(h, ":") {
		host, _, err := net.SplitHostPort(h)
		if err == nil && strings.HasPrefix(h, "[") && net.ParseIP(host) == nil {
			// "[alice.example]:443" is a second spelling of a hostname,
			// and two spellings must never become one host.
			return ""
		}
		if err == nil {
			h = host
		}
	}
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") && net.ParseIP(h[1:len(h)-1]) != nil {
		// Bracketed IPv6 without a port.
		h = h[1 : len(h)-1]
	}
	h = strings.ToLower(h)
	return strings.TrimSuffix(h, ".")
}

// hostKind classifies a request host relative to the configured base host.
type hostKind int

const (
	// hostUnknown is anything that is none of the kinds below: a pod IP, a
	// port-forward, an alias, a host three or more labels deep, or a label
	// that would read as punycode ("xn--...").
	hostUnknown hostKind = iota
	// hostBase is exactly the configured base host.
	hostBase
	// hostOwner is exactly "<valid label>.<base>" containing no "--": the
	// person's or team's index page, and the pre-v1.3
	// "<owner>.<base>/<site>/" addresses.
	hostOwner
	// hostSite is "<site part>.<owner label>.<base>", one site's own
	// origin. Every site is served here, whatever its access level, once its
	// owner's certificate is ready (HostModel.OwnerReady).
	hostSite
	// hostLegacySite is "<owner label>--<site label>.<base>", where a site
	// shared with named people was served before v1.3. It only redirects.
	hostLegacySite
)

// maxLabelLen is the DNS limit on one label.
const maxLabelLen = 63

// maxHostLen is the DNS limit on a whole name.
const maxHostLen = 253

// siteHashLen is how many hex digits of sha256(site name) end the host part
// of a site whose name is not already a usable label (see siteHostPart).
const siteHashLen = 6

// HostModel knows the base host and can classify request hosts, build owner
// and site hosts against it, and say which address a site is handed out
// under.
//
// The zero value is usable and behaves as "no base host": it classifies
// everything as unknown, which is what handler tests that never set a base
// URL rely on.
type HostModel struct {
	base string
	// origin is the public base URL with any trailing slash removed.
	origin string
	// scheme and port are those of the public base URL; an owner or site
	// host is reached over the same scheme and port as the base host.
	scheme string
	port   string
	// ready reports whether an owner label's "*.<owner>.<base>" certificate
	// is in place (OwnerHostReadiness). nil means every owner's is: an
	// install that manages those certificates itself (OWNER_CERTS=manual),
	// and tests.
	ready func(ownerLabel string) bool
}

// NewHostModel derives the base host from the public base URL's hostname (no
// port), lowercased and with a trailing dot stripped. It fails if the URL
// does not parse or has no hostname.
func NewHostModel(publicBaseURL string) (HostModel, error) {
	trimmed := strings.TrimSpace(publicBaseURL)
	u, err := url.Parse(trimmed)
	if err != nil {
		return HostModel{}, fmt.Errorf("host model: parse public base URL: %w", err)
	}
	base := normalizeHost(u.Hostname())
	if base == "" {
		return HostModel{}, errors.New("host model: public base URL has no hostname")
	}
	return HostModel{
		base:   base,
		origin: strings.TrimRight(trimmed, "/"),
		scheme: strings.ToLower(u.Scheme),
		port:   u.Port(),
	}, nil
}

// WithOwnerReadiness returns m answering OwnerReady from ready. Set it before
// the model is handed to anything, since HostModel is copied by value.
func (m HostModel) WithOwnerReadiness(ready func(ownerLabel string) bool) HostModel {
	m.ready = ready
	return m
}

// OwnerReady reports whether sites of the owner with this label are handed
// out at, and redirected to, their own "<site>.<owner>.<base>" host. Until
// then they are served at "<owner>.<base>/<site>/", so nobody is ever sent
// to a host whose certificate does not exist yet.
func (m HostModel) OwnerReady(ownerLbl string) bool {
	return m.ready == nil || m.ready(ownerLbl)
}

// BaseHost returns the normalized base host, for example
// "simple-host.example.com".
func (m HostModel) BaseHost() string {
	return m.base
}

// Classify normalizes raw (a request Host header) and reports what kind of
// host it is. The label is everything before ".<base>": the owner label for
// hostOwner, "<site part>.<owner label>" for hostSite (split it with
// SplitSiteLabel), "<owner>--<site>" for hostLegacySite. Every label must
// pass isValidLabel, so "-x" or "x-" is hostUnknown.
func (m HostModel) Classify(raw string) (hostKind, string) {
	h := normalizeHost(raw)
	if h == "" || m.base == "" {
		return hostUnknown, ""
	}
	if h == m.base {
		return hostBase, ""
	}
	suffix := "." + m.base
	if !strings.HasSuffix(h, suffix) {
		return hostUnknown, ""
	}
	label := strings.TrimSuffix(h, suffix)
	if strings.HasPrefix(label, "xn--") {
		// Browsers render a punycode label as Unicode; never ours.
		return hostUnknown, ""
	}
	if sitePart, ownerPart, two := strings.Cut(label, "."); two {
		if strings.Contains(ownerPart, ".") || strings.Contains(ownerPart, "--") ||
			!isValidLabel(ownerPart) || !isValidLabel(sitePart) {
			return hostUnknown, ""
		}
		return hostSite, label
	}
	if !isValidLabel(label) {
		return hostUnknown, ""
	}
	if idx := strings.Index(label, "--"); idx >= 0 {
		if !isValidLabel(label[:idx]) || !isValidLabel(label[idx+2:]) {
			return hostUnknown, ""
		}
		return hostLegacySite, label
	}
	return hostOwner, label
}

// SplitSiteLabel splits a hostSite label (as returned by Classify) into its
// site part and owner label. ok is false if label is not two labels.
func SplitSiteLabel(label string) (sitePart, ownerLabelPart string, ok bool) {
	sitePart, ownerLabelPart, ok = strings.Cut(label, ".")
	return sitePart, ownerLabelPart, ok
}

// splitLegacySiteLabel splits a hostLegacySite label at its first "--";
// owner labels never contain "--", so the split is unambiguous.
func splitLegacySiteLabel(label string) (ownerLabelPart, sitePart string, ok bool) {
	idx := strings.Index(label, "--")
	if idx < 0 {
		return "", "", false
	}
	return label[:idx], label[idx+2:], true
}

// OwnerHost returns the host of a person's or team's index page:
// ownerLabel(username) + "." + BaseHost().
func (m HostModel) OwnerHost(username string) string {
	return ownerLabel(username) + "." + m.base
}

// siteHostPart is a site's own label in "<part>.<owner>.<base>", a pure
// function of the site name so every address can be computed without a
// lookup. A name that already folds (siteLabel) to a valid label keeps it,
// which is every name a new site may have and every pre-v1.3 "specific"
// site's address. Any other name (spaces, underscores, longer than a label)
// becomes its folded name with every other character replaced by '-', cut
// to fit, plus "-" and the first six hex digits of sha256(name), so two
// such names never share a part.
func siteHostPart(siteName string) string {
	if folded := siteLabel(siteName); isValidLabel(folded) && !strings.HasPrefix(folded, "xn--") {
		return folded
	}
	var b strings.Builder
	for _, c := range []byte(siteLabel(siteName)) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteByte(c)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	stem := strings.TrimPrefix(strings.TrimRight(b.String(), "-"), "xn-")
	if limit := maxLabelLen - siteHashLen - 1; len(stem) > limit {
		stem = strings.TrimRight(stem[:limit], "-")
	}
	sum := sha256.Sum256([]byte(siteName))
	digest := hex.EncodeToString(sum[:])[:siteHashLen]
	if stem == "" {
		return "s-" + digest
	}
	return stem + "-" + digest
}

// SiteHost returns a site's own host, "<site part>.<owner label>.<base>",
// or "" when the owner cannot be a label or the name would be too long.
func (m HostModel) SiteHost(owner, siteName string) string {
	if m.base == "" || !isAddressableOwner(owner) {
		return ""
	}
	host := siteHostPart(siteName) + "." + ownerLabel(owner) + "." + m.base
	if len(host) > maxHostLen {
		return ""
	}
	return host
}

// ValidNewSiteName reports whether name may be given to a new site under
// owner: it must already be its own host label, so the address reads
// exactly as the name was typed — lowercase letters, digits and hyphens,
// not starting or ending with a hyphen, at most 63 characters. Sites created
// before v1.3 keep whatever name they had.
func (m HostModel) ValidNewSiteName(owner, name string) bool {
	return isValidLabel(name) && !strings.HasPrefix(name, "xn--") &&
		(m.base == "" || m.SiteHost(owner, name) != "")
}

// MaxSiteNameLen is the longest new site name.
func MaxSiteNameLen(string) int {
	return maxLabelLen
}

// isAddressableOwner reports whether ownerLabel(username) is a valid DNS
// label. Registration guarantees this for new accounts (validateOwnerName);
// older rows and hostile path segments are not guaranteed, and a name that
// fails must never be spliced into an authority.
func isAddressableOwner(username string) bool {
	return isValidLabel(ownerLabel(username))
}

// SiteURL returns the absolute address a person or agent should be given for
// a site: the root of its own host once its owner's certificate is ready,
// "<owner>.<base>/<site>/" until then. "" when it has none, so a caller can
// decide how to degrade.
func (m HostModel) SiteURL(username, siteName string) string {
	host := m.SiteHost(username, siteName)
	if host == "" {
		return ""
	}
	if !m.OwnerReady(ownerLabel(username)) {
		return m.OwnerOrigin(ownerLabel(username)) + "/" + url.PathEscape(siteName) + "/"
	}
	return m.OwnerOrigin(strings.TrimSuffix(host, "."+m.base)) + "/"
}

// OwnerOrigin returns the origin an owner or site host is reached at,
// "<scheme>://<label>.<base>[:port]", built from the configured public base
// URL's scheme and port. label is a label this model produced (from Classify
// or ownerLabel); the request's own Host header is never echoed into it, so a
// forged Host cannot become an expected origin.
func (m HostModel) OwnerOrigin(label string) string {
	authority := label + "." + m.base
	if m.port != "" {
		authority += ":" + m.port
	}
	return m.scheme + "://" + authority
}

// OwnerPageURL is the root of a person's own host: the index of everything
// they have published (owner_index.go). It is what a name links to anywhere
// that person is credited.
func (m HostModel) OwnerPageURL(username string) string {
	return m.OwnerOrigin(ownerLabel(username)) + "/"
}

// RedirectHost returns the canonical host a plain-HTTP request addressed to
// raw should be redirected to: the base host for the base host and for any
// unrecognised host, and the rebuilt owner or site label for the
// other two kinds. It never echoes raw, so a client-controlled Host header
// cannot become a Location target.
func (m HostModel) RedirectHost(raw string) string {
	switch kind, label := m.Classify(raw); kind {
	case hostOwner, hostSite, hostLegacySite:
		return label + "." + m.base
	default:
		return m.base
	}
}

// OwnsLabel reports whether label is the label derived from username. It is a
// pure string comparison in the username -> label direction; it never resolves
// a label back to a user.
func (m HostModel) OwnsLabel(label, username string) bool {
	return ownerLabel(username) == label
}

// SiteHostResolver returns the host that serves a site's own API: the
// site's own host once its owner is ready, the owner host (which serves the
// named API shape until then) before. It needs no lookup; a site that does
// not exist gets that host's ordinary 404.
func (m HostModel) SiteHostResolver() func(ctx context.Context, owner, site string) (string, error) {
	return func(_ context.Context, owner, site string) (string, error) {
		host := m.SiteHost(owner, site)
		if host == "" {
			return "", fmt.Errorf("site %s/%s has no address", owner, site)
		}
		if !m.OwnerReady(ownerLabel(owner)) {
			return m.OwnerHost(owner), nil
		}
		return host, nil
	}
}
