package handler

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"net/netip"
	"strings"

	"github.com/vsriram/simple-host/internal/db"
)

type notificationCipher struct{ aead cipher.AEAD }

func (h *SiteHandler) WithNotifications(key []byte, allowed []netip.Prefix) *SiteHandler {
	if len(key) != 0 {
		block, err := aes.NewCipher(key)
		if err != nil {
			panic("invalid notification key")
		}
		h.notifyCipher.aead, err = cipher.NewGCM(block)
		if err != nil {
			panic("invalid notification cipher")
		}
	}
	h.notifyAllowed = allowed
	return h
}
func (h *SiteHandler) NotificationsReady() bool { return h.notifyCipher.aead != nil }
func (p *notificationCipher) sealNotification(id, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if p.aead == nil {
		return "", notifyFailure("notifications_not_enabled")
	}
	nonce := make([]byte, p.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := p.aead.Seal(nonce, nonce, []byte(plaintext), []byte("notification:"+id))
	return "enc1:" + base64.StdEncoding.EncodeToString(sealed), nil
}
func (p *notificationCipher) openNotification(id, stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	// Plaintext is used only for an unsaved verification challenge. Database
	// writers always seal before insertion.
	encoded, sealed := strings.CutPrefix(stored, "enc1:")
	if !sealed {
		return stored, nil
	}
	if p.aead == nil {
		return "", notifyFailure("secret_unreadable")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) < p.aead.NonceSize() {
		return "", notifyFailure("secret_unreadable")
	}
	n := p.aead.NonceSize()
	plain, err := p.aead.Open(nil, raw[:n], raw[n:], []byte("notification:"+id))
	if err != nil {
		return "", notifyFailure("secret_unreadable")
	}
	return string(plain), nil
}
func (h *SiteHandler) notificationSecrets() db.NotificationSecrets {
	return db.NotificationSecrets{Seal: h.notifyCipher.sealNotification, Open: h.notifyCipher.openNotification}
}
