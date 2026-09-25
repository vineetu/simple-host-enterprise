package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// EnvelopeKey is one named 32-byte AES-256 key for the optional client-side
// envelope. The first configured key wraps every new object; every
// configured key is tried, by id, to unwrap an existing one.
//
// Objects are immutable and kept for as long as the version or asset they
// hold is retained, so an object wrapped under a key needs that key for as
// long as the object exists. Rotation is therefore: add the new key second,
// deploy, swap the order, deploy — and keep the old key configured. Removing
// a key makes every object still wrapped under it unreadable.
type EnvelopeKey struct {
	ID  string
	Key []byte
}

// Object metadata keys for the client-side envelope. Stored without the
// "x-amz-meta-" prefix the SDK adds and strips automatically.
const (
	metaEnvelopeKeyID   = "sh-envelope-key-id"
	metaEnvelopeWrapNC  = "sh-envelope-wrap-nonce"
	metaEnvelopeWrapKey = "sh-envelope-wrapped-key"
	metaEnvelopeDataNC  = "sh-envelope-data-nonce"

	dataKeyLength = 32 // AES-256
)

// wrapObject encrypts plaintext under a fresh per-object data key with
// AES-256-GCM, then wraps that data key with the given envelope key (also
// AES-256-GCM, its own nonce). The wrapped key and both nonces travel as
// object metadata; the ciphertext is the object body. Unwrapping needs only
// the envelope key and this metadata, never the plaintext data key at rest
// anywhere.
func wrapObject(plaintext []byte, envelopeKey EnvelopeKey) ([]byte, map[string]string, error) {
	dataKey := make([]byte, dataKeyLength)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, nil, fmt.Errorf("generate data key: %w", err)
	}

	dataGCM, err := newGCM(dataKey)
	if err != nil {
		return nil, nil, err
	}
	dataNonce := make([]byte, dataGCM.NonceSize())
	if _, err := rand.Read(dataNonce); err != nil {
		return nil, nil, fmt.Errorf("generate data nonce: %w", err)
	}
	ciphertext := dataGCM.Seal(nil, dataNonce, plaintext, nil)

	wrapGCM, err := newGCM(envelopeKey.Key)
	if err != nil {
		return nil, nil, err
	}
	wrapNonce := make([]byte, wrapGCM.NonceSize())
	if _, err := rand.Read(wrapNonce); err != nil {
		return nil, nil, fmt.Errorf("generate wrap nonce: %w", err)
	}
	// The envelope key id is bound in as GCM associated data so a wrapped key
	// only authenticates under the id it was actually wrapped with. Without
	// this, swapping the sh-envelope-key-id metadata field to name a
	// different configured key that happens to share the same raw key bytes
	// (e.g. a botched rotation entry) would unwrap silently under the wrong
	// id instead of failing.
	wrappedKey := wrapGCM.Seal(nil, wrapNonce, dataKey, []byte(envelopeKey.ID))

	metadata := map[string]string{
		metaEnvelopeKeyID:   envelopeKey.ID,
		metaEnvelopeWrapNC:  base64.StdEncoding.EncodeToString(wrapNonce),
		metaEnvelopeWrapKey: base64.StdEncoding.EncodeToString(wrappedKey),
		metaEnvelopeDataNC:  base64.StdEncoding.EncodeToString(dataNonce),
	}
	return ciphertext, metadata, nil
}

// unwrapObject reverses wrapObject. It looks up the envelope key named in the
// object's metadata among the configured keys (by id, so a rotation in
// progress unwraps objects wrapped under either the old or the new key), then
// decrypts the data key and the body in turn. AES-GCM's tag makes both steps
// fail closed on any corruption or tampering.
func unwrapObject(ciphertext []byte, metadata map[string]string, keys []EnvelopeKey) ([]byte, error) {
	keyID := metadata[metaEnvelopeKeyID]
	var envelopeKey *EnvelopeKey
	for i := range keys {
		if keys[i].ID == keyID {
			envelopeKey = &keys[i]
			break
		}
	}
	if envelopeKey == nil {
		return nil, fmt.Errorf("no configured envelope key matches object key id %q", keyID)
	}

	wrapNonce, err := base64.StdEncoding.DecodeString(metadata[metaEnvelopeWrapNC])
	if err != nil {
		return nil, fmt.Errorf("decode wrap nonce: %w", err)
	}
	wrappedKey, err := base64.StdEncoding.DecodeString(metadata[metaEnvelopeWrapKey])
	if err != nil {
		return nil, fmt.Errorf("decode wrapped key: %w", err)
	}
	dataNonce, err := base64.StdEncoding.DecodeString(metadata[metaEnvelopeDataNC])
	if err != nil {
		return nil, fmt.Errorf("decode data nonce: %w", err)
	}

	wrapGCM, err := newGCM(envelopeKey.Key)
	if err != nil {
		return nil, err
	}
	dataKey, err := wrapGCM.Open(nil, wrapNonce, wrappedKey, []byte(keyID))
	if err != nil {
		return nil, fmt.Errorf("unwrap data key (key id %q): %w", keyID, err)
	}

	dataGCM, err := newGCM(dataKey)
	if err != nil {
		return nil, err
	}
	plaintext, err := dataGCM.Open(nil, dataNonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt object body (key id %q): %w", keyID, err)
	}
	return plaintext, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm mode: %w", err)
	}
	return gcm, nil
}

// isEnveloped reports whether metadata carries the client-side envelope
// this package writes, as opposed to a plain object (no envelope configured
// at write time, or an object this package never wrote).
func isEnveloped(metadata map[string]string) bool {
	return metadata[metaEnvelopeKeyID] != ""
}
