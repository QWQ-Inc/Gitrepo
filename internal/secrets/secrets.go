// Package secrets encrypts platform credentials at rest with AES-256-GCM.
package secrets

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

const version byte = 1

// Box seals and opens credential blobs with a single master key.
type Box struct {
	aead cipher.AEAD
}

// New creates a Box from a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secret key must be 32 bytes, got %d", len(key))
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

// ParseKey accepts a 32-byte key encoded as hex (64 chars) or base64.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("secret key is empty")
	}
	if len(s) == 64 {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("secret key must be 32 bytes encoded as hex or base64 (generate one with `gitrepo keygen`)")
}

// GenerateKey returns a new random key, base64 encoded.
func GenerateKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// Seal encrypts plaintext. aad binds the ciphertext to its owner (e.g. the
// target name) so blobs cannot be swapped between rows.
func (b *Box) Seal(plaintext []byte, aad string) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+b.aead.Overhead())
	out = append(out, version)
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, plaintext, []byte(aad)), nil
}

// Open decrypts a blob produced by Seal.
func (b *Box) Open(blob []byte, aad string) ([]byte, error) {
	if len(blob) == 0 {
		return nil, nil
	}
	ns := b.aead.NonceSize()
	if len(blob) < 1+ns+b.aead.Overhead() || blob[0] != version {
		return nil, errors.New("malformed credential ciphertext")
	}
	pt, err := b.aead.Open(nil, blob[1:1+ns], blob[1+ns:], []byte(aad))
	if err != nil {
		return nil, errors.New("cannot decrypt credential (wrong secret key?)")
	}
	return pt, nil
}
