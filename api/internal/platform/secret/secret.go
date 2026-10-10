// Package secret encrypts values stored in the database (GSP credentials)
// with AES-256-GCM. The key comes only from the APP_ENCRYPTION_KEY env var.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

type Box struct{ aead cipher.AEAD }

// New takes a base64-encoded 32-byte key. An empty key gives a Box that
// refuses to encrypt, so credentials are never stored in clear.
func New(b64Key string) (*Box, error) {
	if b64Key == "" {
		return &Box{}, nil
	}
	key, err := base64.StdEncoding.DecodeString(b64Key)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("APP_ENCRYPTION_KEY must be 32 bytes, base64-encoded")
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

var ErrNoKey = errors.New("APP_ENCRYPTION_KEY is not configured")

func (b *Box) Seal(plain []byte) ([]byte, error) {
	if b.aead == nil {
		return nil, ErrNoKey
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plain, nil), nil
}

func (b *Box) Open(sealed []byte) ([]byte, error) {
	if b.aead == nil {
		return nil, ErrNoKey
	}
	n := b.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("secret: ciphertext too short")
	}
	return b.aead.Open(nil, sealed[:n], sealed[n:], nil)
}
