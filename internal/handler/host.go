package handler

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// The hostname model for per-owner subdomains. The server answers on
// "<label>.<base>", where base is the hostname of the configured public base
// URL and label is derived from a username by ownerLabel. It also answers on
// "<owner label>--<site label>.<base>" for a restricted site (design.md
// 5.2a): the double hyphen is reserved in owner labels precisely so it can
// never collide with this second shape.
//
// There is deliberately no label -> username resolution here, and no site
// label -> site name resolution either. The comparison only ever goes
// name -> label (OwnsLabel derives a label from a known username and
// compares strings; nothing in this file resolves a label back to a name).
// The host gate does that resolution against disk, the same way it already
// resolves an owner label to a username.

// ownerLabel maps a username to its DNS label: lowercase, every '.' becomes
// '-'. It is the only place the username -> label rule lives in Go; the
// unique index in internal/migrate/sql/0018_owner_label_unique.sql computes
// the same expression in SQL and must be rebuilt if this rule changes.
func ownerLabel(username string) string {
	return strings.ReplaceAll(strings.ToLower(username), ".", "-")
}

// siteLabel folds a site name into a DNS label candidate the same way
// ownerLabel folds a username: lowercase, every '.' becomes '-'. Unlike a
// username, a site name is not constrained to look like a label at all (it
// may contain spaces, underscores, uppercase letters unrelated to a dot);
// isValidLabel(siteLabel(name)) is what actually decides whether a site can
// be given a restricted-site hostname, and a site whose name does not fold
// to a valid label simply cannot be — see HostModel.RestrictedSiteAddressable.
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
	// hostUnknown is anything that is neither the base host, an owner host,
	// nor a restricted-site host: a pod IP, a port-forward, an alias, a
	// nested label, or a "--" label whose owner part is too short to form
	// (see restrictedSiteLabelMinOwnerLen).
	hostUnknown hostKind = iota
	// hostBase is exactly the configured base host.
	hostBase
	// hostOwner is exactly "<valid label>.<base>" containing no "--".
	hostOwner
	// hostRestrictedSite is "<owner label>--<site label>.<base>".
	hostRestrictedSite
)

// restrictedSiteLabelMinOwnerLen is design.md 10.6's RFC 5890 rule: a label
// whose third and fourth characters are both '-' is a reserved LDH label, so
// "<owner>--<site>" is only formed when the owner part is at least three
// characters (positions 0-2), keeping the "--" no earlier than position 3.
const restrictedSiteLabelMinOwnerLen = 3

// HostModel knows the base host and can classify request hosts, build owner
// and restricted-site hosts against it, and say which address a site should
// be handed out under.
//
// The zero value is usable and behaves as "no base host": it classifies
// everything as unknown, which is what handler tests that never set a base
// URL rely on.
type HostModel struct {
	base string
	// origin is the public base URL with any trailing slash removed, exactly
	// what the deploy response prefixed onto the long path before subdomains.
	origin string
	// scheme and port are those of the public base URL; an owner or
	// restricted-site host is reached over the same scheme and port as the
	// base host.
	scheme string
	port   string
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

// BaseHost returns the normalized base host, for example
// "simple-host.example.com".
func (m HostModel) BaseHost() string {
	return m.base
}

// Classify normalizes raw (a request Host header) and reports whether it is
// the base host, an owner host, a restricted-site host, or something else.
// For hostOwner the label is the owner's label; for hostRestrictedSite it is
// the composed "<owner label>--<site label>" (split it with
// SplitRestrictedSiteLabel). Owner and restricted-site hosts are exactly one
// label deep, so "a.b.<base>" is hostUnknown, and a label that fails
// isValidLabel (such as "-x" or "x-") is hostUnknown too.
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
	if strings.Contains(label, ".") || !isValidLabel(label) {
		return hostUnknown, ""
	}
	if idx := strings.Index(label, "--"); idx >= 0 {
		if idx < restrictedSiteLabelMinOwnerLen {
			// Too short to be a well-formed restricted-site label, and "--"
			// is reserved so this can never be an ordinary owner label
			// either: refuse it outright rather than guessing which one
			// was meant.
			return hostUnknown, ""
		}
		ownerPart, sitePart := label[:idx], label[idx+2:]
		if !isValidLabel(ownerPart) || !isValidLabel(sitePart) {
			return hostUnknown, ""
		}
		return hostRestrictedSite, label
	}
	return hostOwner, label
}

// SplitRestrictedSiteLabel splits a hostRestrictedSite label (as returned by
// Classify) into its owner and site parts. It assumes label was already
// produced by Classify and so does not re-validate; ok is false only if
// label does not contain "--" at all.
func SplitRestrictedSiteLabel(label string) (ownerLabelPart, siteLabelPart string, ok bool) {
	idx := strings.Index(label, "--")
	if idx < 0 {
		return "", "", false
	}
	return label[:idx], label[idx+2:], true
}

// SiteHost returns the host a username's sites are served on:
// ownerLabel(username) + "." + BaseHost().
func (m HostModel) SiteHost(username string) string {
	return ownerLabel(username) + "." + m.base
}

// RestrictedSiteHost returns the host a restricted site is served on:
// "<owner label>--<site label>.<base>". Only meaningful when
// RestrictedSiteAddressable reports true; callers that skip that check may
// get a host nothing will ever classify as hostRestrictedSite (for example,
// a two-character owner) or, if the site name folds to something invalid,
// a nonsensical label.
func (m HostModel) RestrictedSiteHost(owner, siteName string) string {
	return ownerLabel(owner) + "--" + siteLabel(siteName) + "." + m.base
}

// RestrictedSiteAddressable reports whether a restricted site's own hostname
// can be formed at all: both the owner and site names must fold to valid DNS
// labels, and the owner label must be at least restrictedSiteLabelMinOwnerLen
// characters so the composed label does not fall foul of the RFC 5890 "--"
// reservation (design.md 10.6). A site that fails this cannot be restricted
// to specific viewers until it (or, for the owner, the account) is renamed;
// the dashboard is expected to say so rather than silently forming a broken
// address.
func (m HostModel) RestrictedSiteAddressable(owner, siteName string) bool {
	return isAddressableOwner(owner) &&
		len(ownerLabel(owner)) >= restrictedSiteLabelMinOwnerLen &&
		isValidLabel(siteLabel(siteName))
}

// isAddressableOwner reports whether ownerLabel(username) is a valid DNS
// label. Registration guarantees this for new accounts (validateOwnerName);
// older rows and hostile path segments are not guaranteed, and a name that
// fails must never be spliced into an authority.
func isAddressableOwner(username string) bool {
	return isValidLabel(ownerLabel(username))
}

// SiteURL returns the absolute address a person or agent should be given for
// a site: the short address on the owner's own host, or on the restricted
// site's own host when restricted is true. Subdomain addressing is the only
// addressing this server has (design.md 7.1); an owner whose name cannot be
// a hostname label has no address at all, which should not happen for any
// account created since validateOwnerName started enforcing it, but is
// reported as "" rather than panicking so a caller can decide how to degrade.
func (m HostModel) SiteURL(username, siteName string, restricted bool) string {
	if m.base == "" || !isAddressableOwner(username) {
		return ""
	}
	if restricted {
		if !m.RestrictedSiteAddressable(username, siteName) {
			return ""
		}
		return m.restrictedSiteURL(username, siteName)
	}
	return m.shortSiteURL(username, siteName)
}

// SiteLink is a narrow compatibility shim for the admin dashboard's ranking
// cards (admin_rankings.go), which rank sites by a struct with no site id
// and so cannot look up whether a given site is restricted the way every
// other caller of SiteURL now does. It always reports the unrestricted
// address; for a site that has since been restricted, that link 404s on the
// rank card while the same site's row elsewhere on the same dashboard (which
// does carry a site id) links correctly. Accepted as a cosmetic gap rather
// than threading a site id through the ranking pipeline for this one
// internal, admin-only view — see docs/security-review.md.
func (m HostModel) SiteLink(username, siteName string) string {
	return m.SiteURL(username, siteName, false)
}

// OwnerOrigin returns the origin an owner host is reached at,
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

// shortSiteURL builds "<scheme>://<label>.<base>[:port]/<site>/" from the
// public base URL's scheme and port, so a base URL with a port keeps it.
func (m HostModel) shortSiteURL(username, siteName string) string {
	return m.OwnerOrigin(ownerLabel(username)) + "/" + url.PathEscape(siteName) + "/"
}

// restrictedSiteURL builds "<scheme>://<owner>--<site>.<base>[:port]/", the
// root of a restricted site's own host; there is no "/<site>/" segment
// because the whole host is dedicated to this one site.
func (m HostModel) restrictedSiteURL(username, siteName string) string {
	authority := m.RestrictedSiteHost(username, siteName)
	if m.port != "" {
		authority += ":" + m.port
	}
	return m.scheme + "://" + authority + "/"
}

// RedirectHost returns the canonical host a plain-HTTP request addressed to
// raw should be redirected to: the base host for the base host and for any
// unrecognised host, and the rebuilt owner or restricted-site label for the
// other two kinds. It never echoes raw, so a client-controlled Host header
// cannot become a Location target.
func (m HostModel) RedirectHost(raw string) string {
	switch kind, label := m.Classify(raw); kind {
	case hostOwner, hostRestrictedSite:
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
