// Package secret encrypts credentials that are stored in the config file.
//
// A value written as "enc:v1:<base64>" is AES-256-GCM ciphertext. Plain values
// are left untouched, so a config can mix encrypted keys with keys read from the
// environment. The master key never enters the YAML file: it comes from
// LGLLM_MASTER_KEY or --master-key.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Prefix marks an encrypted value.
const Prefix = "enc:v1:"

// ErrNoKey means an encrypted value was found but no master key was configured.
var ErrNoKey = errors.New("encrypted secret found but no master key is configured")

// Box encrypts and decrypts secrets with one key.
type Box struct {
	aead cipher.AEAD
}

// New builds a box from a master key string. A 32-byte value in base64 or hex is
// used directly; any other string of at least 16 characters is hashed with
// SHA-256 so a passphrase works without a separate KDF.
func New(masterKey string) (*Box, error) {
	key, err := KeyBytes(masterKey)
	if err != nil {
		return nil, err
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

// KeyBytes normalizes a master key string into 32 bytes.
func KeyBytes(masterKey string) ([]byte, error) {
	masterKey = strings.TrimSpace(masterKey)
	if masterKey == "" {
		return nil, ErrNoKey
	}
	if b, err := base64.StdEncoding.DecodeString(masterKey); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := hex.DecodeString(masterKey); err == nil && len(b) == 32 {
		return b, nil
	}
	if len(masterKey) < 16 {
		return nil, fmt.Errorf("master key must be at least 16 characters (or 32 bytes base64/hex)")
	}
	sum := sha256.Sum256([]byte(masterKey))
	return sum[:], nil
}

// Encrypt returns the "enc:v1:" representation of plaintext.
func (b *Box) Encrypt(plaintext string) (string, error) {
	if b == nil {
		return "", ErrNoKey
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return Prefix + base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt opens an "enc:v1:" value. A plain value is returned unchanged.
func (b *Box) Decrypt(value string) (string, error) {
	if !strings.HasPrefix(value, Prefix) {
		return value, nil
	}
	if b == nil || b.aead == nil {
		return "", ErrNoKey
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, Prefix))
	if err != nil {
		return "", fmt.Errorf("encrypted secret is not valid base64: %w", err)
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("encrypted secret is truncated")
	}
	plain, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", errors.New("cannot decrypt secret: wrong master key or corrupt value")
	}
	return string(plain), nil
}

// IsEncrypted reports whether a value is ciphertext.
func IsEncrypted(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), Prefix)
}
