// Package vault does the end-to-end encryption: a key derived from the sync
// passphrase seals every item before it leaves the PC.
package vault

import (
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/Simplyph0/claudesync/internal/proto"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

const checkText = "claudesync-vault-v1"

var ErrWrongPassphrase = errors.New("wrong sync passphrase")

// DeriveKey turns the passphrase into a 256-bit key with Argon2id.
func DeriveKey(passphrase string, salt []byte) []byte {
	return argon2.IDKey([]byte(passphrase), salt, 3, 64*1024, 4, chacha20poly1305.KeySize)
}

// Seal encrypts plaintext with XChaCha20-Poly1305; the output is nonce||ciphertext.
func Seal(key, plaintext, ad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	if _, err := rand.Read(out); err != nil {
		return nil, err
	}
	return aead.Seal(out, out, plaintext, ad), nil
}

// Open reverses Seal.
func Open(key, blob, ad []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	if len(blob) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("encrypted blob too short")
	}
	pt, err := aead.Open(nil, blob[:aead.NonceSize()], blob[aead.NonceSize():], ad)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return pt, nil
}

// New creates a vault record for a new passphrase and returns it with the key.
func New(passphrase string) (proto.Vault, []byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return proto.Vault{}, nil, err
	}
	key := DeriveKey(passphrase, salt)
	check, err := Seal(key, []byte(checkText), []byte("vault"))
	if err != nil {
		return proto.Vault{}, nil, err
	}
	return proto.Vault{Salt: salt, Check: check}, key, nil
}

// Unlock checks the passphrase against a vault record and returns the key.
func Unlock(v proto.Vault, passphrase string) ([]byte, error) {
	key := DeriveKey(passphrase, v.Salt)
	pt, err := Open(key, v.Check, []byte("vault"))
	if err != nil || string(pt) != checkText {
		return nil, ErrWrongPassphrase
	}
	return key, nil
}
