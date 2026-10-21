// Package secrets encrypts small secrets stored in the database (vendor bind
// passwords) with AES-256-GCM. The key comes from SECRETS_KEY (64 hex chars);
// in production it is supplied by the KMS / secret manager.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
)

// Box encrypts and decrypts with one key.
type Box struct {
	aead cipher.AEAD
}

// DevKey is used when SECRETS_KEY is unset in APP_ENV=dev. Never in production.
const DevKey = "0000000000000000000000000000000000000000000000000000000000000000"

// New parses a 32-byte hex key.
func New(hexKey string) (*Box, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("secrets: key must be 64 hex characters")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// FromEnv builds a Box from SECRETS_KEY, falling back to DevKey only when
// appEnv is "dev".
func FromEnv(hexKey, appEnv string) (*Box, error) {
	if hexKey == "" {
		if appEnv != "dev" {
			return nil, errors.New("secrets: SECRETS_KEY is required outside APP_ENV=dev")
		}
		hexKey = DevKey
	}
	return New(hexKey)
}

// Seal encrypts plaintext; the nonce is prepended.
func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Open decrypts what Seal produced.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("secrets: ciphertext too short")
	}
	out, err := b.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return nil, fmt.Errorf("secrets: decrypt: %w", err)
	}
	return out, nil
}
