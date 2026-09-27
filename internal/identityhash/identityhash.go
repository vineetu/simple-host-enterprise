// Package identityhash keys the hashes Simple Host keeps of a person's
// sign-in identity (erased_identities, a site manifest's owner_identity) and
// signs site manifests. A plain SHA-256 of an email or an identity-provider
// subject is reversible by hashing a company directory, and a bucket writer
// could forge one; these are HMACs under keys derived (HKDF-SHA256, each use
// with its own label) from the session signing keys (SESSION_SIGNING_KEY).
//
// A keyed value is "<key id>:<hex>", written under the first signing key.
// Every configured key is tried when matching, and so is the plain SHA-256
// written before this, so rows and manifests from before the change keep
// matching. A value written under a key that is later removed from
// SESSION_SIGNING_KEY no longer matches.
package identityhash

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
	"sync"
)

const (
	identityLabel = "simple-host identity hash v1"
	manifestLabel = "simple-host manifest signature v1"
)

// Key is one session signing key: its id and its 32 bytes.
type Key struct {
	ID  string
	Key []byte
}

type derived struct {
	id                 string
	identity, manifest []byte
}

var (
	mu   sync.RWMutex
	keys []derived
)

// Configure derives the identity and manifest keys from the session signing
// keys, the first of which writes. Called once at startup (the server and
// every subcommand); until it is, values are the plain SHA-256 and
// manifests are unsigned.
func Configure(signing []Key) error {
	out := make([]derived, 0, len(signing))
	for _, k := range signing {
		identity, err := hkdf.Key(sha256.New, k.Key, nil, identityLabel, 32)
		if err != nil {
			return err
		}
		manifest, err := hkdf.Key(sha256.New, k.Key, nil, manifestLabel, 32)
		if err != nil {
			return err
		}
		out = append(out, derived{id: k.ID, identity: identity, manifest: manifest})
	}
	mu.Lock()
	keys = out
	mu.Unlock()
	return nil
}

func current() []derived {
	mu.RLock()
	defer mu.RUnlock()
	return keys
}

func mac(key []byte, parts ...string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(h.Sum(nil))
}

func plain(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

func subjectInput(issuer, subject string) string {
	return strings.TrimRight(issuer, "/") + "\n" + subject
}

func emailInput(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func keyed(domain, input string) string {
	k := current()
	if len(k) == 0 {
		return plain(input)
	}
	return k[0].id + ":" + mac(k[0].identity, domain, input)
}

func candidates(domain, input string) []string {
	var out []string
	for _, k := range current() {
		out = append(out, k.id+":"+mac(k.identity, domain, input))
	}
	return append(out, plain(input))
}

// Subject is the value kept for a sign-in identity (issuer and subject).
func Subject(issuer, subject string) string { return keyed("subject", subjectInput(issuer, subject)) }

// SubjectCandidates is every value Subject may have been written as: under
// each configured key, and the plain SHA-256 of before.
func SubjectCandidates(issuer, subject string) []string {
	return candidates("subject", subjectInput(issuer, subject))
}

// Email is the value kept for an email address.
func Email(email string) string { return keyed("email", emailInput(email)) }

// EmailCandidates is every value Email may have been written as.
func EmailCandidates(email string) []string { return candidates("email", emailInput(email)) }

// Valid reports whether s has the shape of a value this package writes: a
// plain SHA-256, or "<key id>:<HMAC-SHA256>".
func Valid(s string) bool {
	if i := strings.LastIndex(s, ":"); i >= 0 {
		if i == 0 {
			return false
		}
		s = s[i+1:]
	}
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// Signed reports whether manifests are signed (a key is configured).
func Signed() bool { return len(current()) > 0 }

// SignManifest returns the signature for a manifest's canonical bytes (its
// JSON with the signature empty), "" when no key is configured.
func SignManifest(body []byte) string {
	k := current()
	if len(k) == 0 {
		return ""
	}
	return k[0].id + ":" + mac(k[0].manifest, "manifest", string(body))
}

// VerifyManifest checks a manifest signature against every configured key.
// knownKey is false when the signature names a key that is not configured
// (one retired from SESSION_SIGNING_KEY), so the caller can say so.
func VerifyManifest(body []byte, sig string) (ok, knownKey bool) {
	id, value, found := strings.Cut(sig, ":")
	if !found {
		return false, false
	}
	for _, k := range current() {
		if k.id != id {
			continue
		}
		want := mac(k.manifest, "manifest", string(body))
		return subtle.ConstantTimeCompare([]byte(want), []byte(value)) == 1, true
	}
	return false, false
}
