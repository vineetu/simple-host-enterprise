package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SessionCookieName is the one browser session cookie for this application
// (design.md 6.1): __Host- prefixed, so a browser refuses to set or send it
// unless the connection is Secure, the path is "/", and no Domain attribute
// is present. There is no insecure-mode variant — a cookie this shaped
// cannot have one.
const SessionCookieName = "__Host-sh_session"

// SigningKey is one named HMAC key. Mirrors config.SigningKey; this package
// does not import config; see the same comment on config.EnvelopeKey.
type SigningKey struct {
	ID  string
	Key []byte
}

// sessionPayload is what the cookie actually signs: enough to identify the
// session row and to let an expired cookie be rejected without a database
// round trip. UserID rides along so a forged session id alone is not enough
// to impersonate — the session row is still the source of truth (GetValidSession
// re-checks UserID against what the row says), but a mismatch here is
// rejected before any query runs.
//
// Host is empty for the base-host session cookie (SignSession) and set for
// a per-owner/restricted-site host cookie (SignHostSession, minted only at
// hand-off redemption): design.md 6.1 says "same signed payload" for every
// host a session mints a cookie on, but that let a cookie minted for
// alice.<base> be replayed on bob.<base> if it were ever captured or
// mis-delivered — the session row itself is shared on purpose (design.md
// 6.1's whole point), but each host's own cookie should only ever be
// accepted back on that same host. Recorded as a deviation from that
// wording in docs/security-review.md.
type sessionPayload struct {
	SessionID string `json:"sid"`
	UserID    string `json:"uid"`
	Exp       int64  `json:"exp"`
	Host      string `json:"host,omitempty"`
}

// SignSession produces a base-host session cookie value binding sessionID
// and userID, expiring at exp, signed with the first (signing) key in keys.
// Returns an error if keys is empty — a package with "no insecure mode"
// must not silently issue an unsigned cookie. Carries no Host: the base
// host is the only host this cookie is ever presented on
// (auth.Middleware's own callers never reach an owner or restricted-site
// host), so there is nothing to bind it against. See SignHostSession for
// the per-host cookie the session hand-off mints.
func SignSession(keys []SigningKey, sessionID, userID string, exp time.Time) (string, error) {
	return signSessionPayload(keys, sessionPayload{SessionID: sessionID, UserID: userID, Exp: exp.Unix()})
}

// SignHostSession is SignSession plus a host binding: the resulting cookie
// verifies only when VerifyHostedSession is given the same host back (case-
// insensitively — host should already be normalized/lowercased, as
// HostModel.Classify produces). Used only by the session hand-off
// (handoff.go's redeemHandoffSession) when minting an owner or
// restricted-site host's own cookie from the shared session row.
func SignHostSession(keys []SigningKey, sessionID, userID, host string, exp time.Time) (string, error) {
	if host == "" {
		return "", errors.New("auth: host session requires a non-empty host")
	}
	return signSessionPayload(keys, sessionPayload{SessionID: sessionID, UserID: userID, Exp: exp.Unix(), Host: strings.ToLower(host)})
}

func signSessionPayload(keys []SigningKey, payload sessionPayload) (string, error) {
	if len(keys) == 0 {
		return "", errors.New("auth: no session signing key configured")
	}
	signing := keys[0]
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadJSON)
	mac := hmac.New(sha256.New, signing.Key)
	mac.Write([]byte(encodedPayload))
	signature := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signing.ID + "." + encodedPayload + "." + signature, nil
}

// VerifiedSession is the outcome of a cookie that verified: it says nothing
// about whether the session row is still live (revoked, expired, idle) —
// that check is GetValidSession's job, against the SessionID this returns.
// Host is "" for a base-host cookie signed by SignSession.
type VerifiedSession struct {
	SessionID string
	UserID    string
	Host      string
}

// ErrSessionCookieInvalid covers every way a cookie value can fail to
// verify: wrong shape, unknown key id, bad signature, expired payload.
// Deliberately one error, for the reason ErrSessionInvalid in package db is
// one error too — the caller's response is the same regardless of which.
var ErrSessionCookieInvalid = errors.New("auth: session cookie is not valid")

// VerifySessionCookie checks a cookie value's signature against every key in
// keys (by id) and its embedded expiry, and returns the session and user id
// it was signed for. It does not consult the database.
func VerifySessionCookie(keys []SigningKey, value string) (VerifiedSession, error) {
	parts := strings.SplitN(value, ".", 3)
	if len(parts) != 3 {
		return VerifiedSession{}, ErrSessionCookieInvalid
	}
	keyID, encodedPayload, encodedSignature := parts[0], parts[1], parts[2]

	var verifyKey []byte
	for _, k := range keys {
		if k.ID == keyID {
			verifyKey = k.Key
			break
		}
	}
	if verifyKey == nil {
		return VerifiedSession{}, ErrSessionCookieInvalid
	}

	signature, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil {
		return VerifiedSession{}, ErrSessionCookieInvalid
	}
	mac := hmac.New(sha256.New, verifyKey)
	mac.Write([]byte(encodedPayload))
	expected := mac.Sum(nil)
	if subtle.ConstantTimeCompare(signature, expected) != 1 {
		return VerifiedSession{}, ErrSessionCookieInvalid
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return VerifiedSession{}, ErrSessionCookieInvalid
	}
	var payload sessionPayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return VerifiedSession{}, ErrSessionCookieInvalid
	}
	if payload.SessionID == "" || payload.UserID == "" {
		return VerifiedSession{}, ErrSessionCookieInvalid
	}
	if time.Now().After(time.Unix(payload.Exp, 0)) {
		return VerifiedSession{}, ErrSessionCookieInvalid
	}
	return VerifiedSession{SessionID: payload.SessionID, UserID: payload.UserID, Host: payload.Host}, nil
}

// DescribeSigningKeys is a startup-log helper: it never prints key material,
// only which ids are configured and which one signs, so an operator can
// confirm a rotation took effect without a secret ever reaching a log line.
func DescribeSigningKeys(keys []SigningKey) string {
	if len(keys) == 0 {
		return "none configured"
	}
	ids := make([]string, len(keys))
	for i, k := range keys {
		ids[i] = k.ID
	}
	return fmt.Sprintf("signing with %q, verifying %v", ids[0], ids)
}
