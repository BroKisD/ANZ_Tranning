// Package crypto encrypts/decrypts user_integrations.encrypted_credentials
// with AES-256-GCM using ENCRYPTION_MASTER_KEY.
package crypto

import "errors"

var ErrNotImplemented = errors.New("not implemented")

// Encrypt returns base64(nonce||ciphertext) for plaintext.
func Encrypt(masterKey []byte, plaintext []byte) (string, error) { return "", ErrNotImplemented }

// Decrypt reverses Encrypt.
func Decrypt(masterKey []byte, encoded string) ([]byte, error) { return nil, ErrNotImplemented }
