package handler

import (
	"errors"
	"strings"

	"github.com/lib/pq"

	"github.com/vsriram/simple-host/internal/safepath"
)

// Name rules for accounts and sites live here so that the two places that
// read them (registration, and the host gate's short-path routing) agree.

// reservedLabels are hostnames under the base domain that must never belong
// to an account: infrastructure names, and names that would shadow a route or
// pass for the service itself. An account named one of these would be
// addressed as "<name>.<base>", which is either already something else or a
// name a browser, mail client, or human would trust for the wrong reason.
var reservedLabels = map[string]bool{
	// The service and its routes on the base host.
	"www":         true,
	"api":         true,
	"admin":       true,
	"sites":       true,
	"static":      true,
	"docs":        true,
	"mcp":         true,
	"healthz":     true,
	"readyz":      true,
	"homepage":    true,
	"simple-host": true,
	"simplehost":  true,
	"staging":     true,
	"dev":         true,
	"test":        true,
	"localhost":   true,
	"setup":       true,
	"skills":      true,
	"fonts":       true,

	// DNS and mail infrastructure a resolver or client probes by convention.
	"ns":           true,
	"ns1":          true,
	"ns2":          true,
	"mail":         true,
	"smtp":         true,
	"imap":         true,
	"pop":          true,
	"pop3":         true,
	"mx":           true,
	"ftp":          true,
	"autoconfig":   true,
	"autodiscover": true,
	"wpad":         true,
	"isatap":       true,

	// Names that would let a page under them pass for an operator surface.
	"login":    true,
	"auth":     true,
	"sso":      true,
	"account":  true,
	"accounts": true,
	"secure":   true,
	"support":  true,
	"status":   true,
	"cdn":      true,
	"assets":   true,
	"app":      true,
}

// extraReservedLabels is populated once at startup from config.ReservedLabels
// (the RESERVED_LABELS setting) via SetExtraReservedLabels. It is checked
// alongside the built-in reservedLabels map everywhere that map is; kept
// separate rather than merged into it so the built-in set stays a compile-time
// constant an installer's configuration cannot accidentally shrink.
var extraReservedLabels = map[string]bool{}

// SetExtraReservedLabels installs the installation-specific reserved-label
// list. Call it once, at startup, before any request is served; it is not
// safe to call concurrently with a request in flight.
func SetExtraReservedLabels(labels []string) {
	next := make(map[string]bool, len(labels))
	for _, l := range labels {
		next[strings.ToLower(strings.TrimSpace(l))] = true
	}
	extraReservedLabels = next
}

// isExplicitlyReservedLabel is the named-list reservation only: the built-in
// infrastructure and control-plane names, plus any RESERVED_LABELS entries.
// It excludes the "--" rule below on purpose, so validateOwnerName can check
// the more specific punycode reason first for a label like "xn--alice",
// which would otherwise always match the "--" rule instead.
func isExplicitlyReservedLabel(label string) bool {
	return reservedLabels[label] || extraReservedLabels[label]
}

// reservedSiteNames are site names that cannot have a short URL on an owner
// host because the gate serves those prefixes itself: on "<label>.<base>",
// /sites/... and /api/... begin the long site path and the state routes, so a
// site by either name could never be reached at /<sitename>/.
var reservedSiteNames = map[string]bool{
	"sites": true,
	"api":   true,
}

// The reasons validateOwnerName can refuse a name. Callers map each to a
// message in plain words; the sentinels keep the wording out of this file.
var (
	errOwnerNameUnsafe   = errors.New("username is not a safe path segment")
	errOwnerNameReserved = errors.New("username label is reserved")
	errOwnerNameNotLabel = errors.New("username does not map to a valid DNS label")

	// errTypedNameDotted is validateTypedName's only extra reason.
	errTypedNameDotted = errors.New("typed name contains a dot")
)

// validateOwnerName reports why a username may not be registered: it must be
// a safe path segment, its label must be a valid DNS label (so the account can
// be addressed as <label>.<base>), and the label must not be reserved.
//
// The username must also already be lowercase. ownerLabel lowercases, so
// "Alice" and "alice" would share a label; registration derives usernames
// from a lowercased email, and refusing the uppercase spelling here keeps that
// invariant visible rather than relying on the caller. Existing accounts are
// not re-validated; this runs only when a name is being created.
func validateOwnerName(username string) error {
	if safepath.ValidateSegment(username) != nil {
		return errOwnerNameUnsafe
	}
	label := ownerLabel(username)
	if isExplicitlyReservedLabel(label) {
		return errOwnerNameReserved
	}
	if !isValidLabel(label) || username != strings.ToLower(username) {
		return errOwnerNameNotLabel
	}
	// "xn--" marks a punycode label: browsers render it as Unicode or flag
	// it as suspicious, and neither is a name an owner chose. Checked before
	// the general "--" reservation below, since every punycode label
	// contains "--" and would otherwise report the less specific reason.
	if strings.HasPrefix(label, "xn--") {
		return errOwnerNameNotLabel
	}
	if strings.Contains(label, "--") {
		return errOwnerNameReserved
	}
	return nil
}

// ownerLabelIndexName is the unique index created by migration
// 0018_owner_label_unique.sql over the expression ownerLabel computes. A
// unique violation naming it means another account already owns the same
// hostname under a different spelling ("alice.b" and "alice-b").
const ownerLabelIndexName = "users_owner_label_idx"

// isOwnerLabelViolation reports whether err is a Postgres unique violation on
// the owner-label index, as opposed to the username column's own uniqueness.
func isOwnerLabelViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505" && pqErr.Constraint == ownerLabelIndexName
}

// validateTypedName reports why a name somebody typed may not be used. It
// refuses any name containing a dot, and otherwise applies every rule
// validateOwnerName applies.
//
// The asymmetry is deliberate, not a bug. Usernames are *derived* from an
// email address, where a dot is ordinary, so ownerLabel folds it to a hyphen
// and "alice.smith" becomes the host label "alice-smith". A typed name has
// no such excuse: "first.last" and "first-last" would fold to one hostname, so
// one of the two spellings would silently become the other, and the person who
// typed it would be handed a name they did not choose. Refusing the dot tells
// them instead. validateOwnerName keeps folding, because existing accounts
// were registered under that rule and must keep working.
//
// The dot is checked before the shared rules so that every dotted name gets
// the same answer. Otherwise a name like "acme." would fold to "acme-"
// and be refused as an invalid DNS label, pointing at a hyphen the person
// never typed.
func validateTypedName(name string) error {
	if strings.Contains(name, ".") {
		return errTypedNameDotted
	}
	return validateOwnerName(name)
}
