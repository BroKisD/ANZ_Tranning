// Package crypto encrypts/decrypts user_integrations.encrypted_credentials
// with AES-256-GCM using ENCRYPTION_MASTER_KEY.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

const keySize = 32

var ErrInvalidCiphertext = errors.New("invalid ciphertext")

// LoadMasterKey parses ENCRYPTION_MASTER_KEY: 32 raw bytes encoded as base64 or
// 64 hex characters.
func LoadMasterKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s) // a secret pasted into Secret Manager often ends with a newline
	if s == "" {
		return nil, errors.New("ENCRYPTION_MASTER_KEY is empty")
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == keySize {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == keySize {
		return b, nil
	}
	return nil, errors.New("ENCRYPTION_MASTER_KEY must be 32 bytes, base64- or hex-encoded")
}

func newGCM(masterKey []byte) (cipher.AEAD, error) {
	if len(masterKey) != keySize {
		return nil, fmt.Errorf("master key must be %d bytes, got %d", keySize, len(masterKey))
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Encrypt returns base64(nonce || ciphertext || tag) for plaintext. A fresh
// random nonce is used on every call.
func Encrypt(masterKey, plaintext []byte) (string, error) {
	gcm, err := newGCM(masterKey)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plaintext, nil)), nil
}

// Decrypt reverses Encrypt. It fails on a wrong key or tampered data.
func Decrypt(masterKey []byte, encoded string) ([]byte, error) {
	gcm, err := newGCM(masterKey)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) < gcm.NonceSize()+gcm.Overhead() {
		return nil, ErrInvalidCiphertext
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, ErrInvalidCiphertext
	}
	return pt, nil
}
