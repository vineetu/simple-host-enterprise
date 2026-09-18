package storage

import (
	"bytes"
	"strings"
	"testing"
)

func testKey(id string, fill byte) EnvelopeKey {
	key := make([]byte, dataKeyLength)
	for i := range key {
		key[i] = fill
	}
	return EnvelopeKey{ID: id, Key: key}
}

func TestWrapUnwrapObjectRoundTrip(t *testing.T) {
	key := testKey("k1", 0x42)
	plaintext := []byte("the quick brown fox jumps over the lazy dog, repeated for bulk\n")

	ciphertext, metadata, err := wrapObject(plaintext, key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("wrapObject did not change the plaintext")
	}
	if metadata["sh-envelope-key-id"] != "k1" {
		t.Fatalf("metadata key id = %q, want k1", metadata["sh-envelope-key-id"])
	}

	got, err := unwrapObject(ciphertext, metadata, []EnvelopeKey{key})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("unwrapObject = %q, want %q", got, plaintext)
	}
}

func TestUnwrapObjectRotation(t *testing.T) {
	oldKey := testKey("old", 0x01)
	newKey := testKey("new", 0x02)
	plaintext := []byte("rotate me")

	// An object wrapped under the key that is about to be retired must still
	// unwrap as long as that key is still configured, even though it is no
	// longer first (design 6.1's rotation shape, reused for the envelope).
	ciphertext, metadata, err := wrapObject(plaintext, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := unwrapObject(ciphertext, metadata, []EnvelopeKey{newKey, oldKey})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("unwrapObject after rotation = %q, want %q", got, plaintext)
	}
}

func TestUnwrapObjectUnknownKeyID(t *testing.T) {
	key := testKey("k1", 0x03)
	ciphertext, metadata, err := wrapObject([]byte("secret"), key)
	if err != nil {
		t.Fatal(err)
	}
	other := testKey("k2", 0x04)
	if _, err := unwrapObject(ciphertext, metadata, []EnvelopeKey{other}); err == nil {
		t.Fatal("unwrapObject accepted a key id with no configured match")
	} else if !strings.Contains(err.Error(), "k1") {
		t.Fatalf("error %v does not name the missing key id", err)
	}
}

func TestUnwrapObjectTamperedCiphertextFailsClosed(t *testing.T) {
	key := testKey("k1", 0x05)
	ciphertext, metadata, err := wrapObject([]byte("do not tamper"), key)
	if err != nil {
		t.Fatal(err)
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[0] ^= 0xFF
	if _, err := unwrapObject(tampered, metadata, []EnvelopeKey{key}); err == nil {
		t.Fatal("unwrapObject accepted a tampered ciphertext")
	}
}

func TestUnwrapObjectSwappedKeyIDFailsClosed(t *testing.T) {
	// Two configured keys that happen to share the same raw key bytes (a
	// botched rotation entry, or an operator error) must still not let an
	// object wrapped under one id be accepted under the other: the key id is
	// bound into the wrap GCM's associated data, so relabeling the metadata's
	// sh-envelope-key-id field to a different (even validly configured) id
	// must fail authentication rather than unwrap "successfully" under the
	// wrong id.
	sharedBytes := testKey("k1", 0x07)
	aliasKey := EnvelopeKey{ID: "k2", Key: sharedBytes.Key}

	ciphertext, metadata, err := wrapObject([]byte("do not relabel"), sharedBytes)
	if err != nil {
		t.Fatal(err)
	}
	metadata["sh-envelope-key-id"] = aliasKey.ID

	if _, err := unwrapObject(ciphertext, metadata, []EnvelopeKey{aliasKey}); err == nil {
		t.Fatal("unwrapObject accepted a wrapped key relabeled with a different key id")
	}
}

func TestUnwrapObjectTamperedWrappedKeyFailsClosed(t *testing.T) {
	key := testKey("k1", 0x06)
	ciphertext, metadata, err := wrapObject([]byte("do not tamper"), key)
	if err != nil {
		t.Fatal(err)
	}
	metadata["sh-envelope-wrapped-key"] = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	if _, err := unwrapObject(ciphertext, metadata, []EnvelopeKey{key}); err == nil {
		t.Fatal("unwrapObject accepted a tampered wrapped key")
	}
}
