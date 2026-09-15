// Package syncengine implements v0.4 hosted sync: end-to-end encrypted
// bundle exchange with a remote endpoint (the Supabase-based backend ships
// in a separate repository; Veda only speaks its small REST contract).
//
// Privacy model: the server stores opaque ciphertext envelopes — memory
// content, types, and ids are all inside the encrypted payload, sealed with
// a key derived (scrypt) from the user's sync passphrase, which never
// leaves the device. Sync is off by default; when disabled, the engine makes
// zero network calls. HTTP 402 from the endpoint is the paid-tier gate.
package syncengine

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/scrypt"
)

// KDF parameters. Deliberately fixed (not configurable): one parameter set
// keeps every device's derived key interchangeable and the envelope honest
// about what it carries.
const (
	scryptN = 32768
	scryptR = 8
	scryptP = 1
	keyLen  = 32
)

// CryptoParams describes how the envelope was sealed; sent in the clear
// because none of it is secret.
type CryptoParams struct {
	Algo string `json:"algo"` // "xsalsa20poly1305"
	KDF  string `json:"kdf"`  // "scrypt"
	Salt string `json:"salt"` // hex
	N    int    `json:"N"`
	R    int    `json:"r"`
	P    int    `json:"p"`
}

func newSalt() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// DeriveKey turns the sync passphrase into the 32-byte bundle key.
func DeriveKey(passphrase string, saltHex string) ([]byte, error) {
	salt, err := hex.DecodeString(saltHex)
	if err != nil || len(salt) < 8 {
		return nil, fmt.Errorf("bad kdf salt")
	}
	return scrypt.Key([]byte(passphrase), salt, scryptN, scryptR, scryptP, keyLen)
}

func asKey(key []byte) *[32]byte {
	var k [32]byte
	copy(k[:], key)
	return &k
}

// Seal encrypts plaintext under key. Returns nonce and ciphertext.
func Seal(key, plaintext []byte) (nonce []byte, ciphertext []byte, err error) {
	var n [24]byte
	if _, err := rand.Read(n[:]); err != nil {
		return nil, nil, err
	}
	return n[:], secretbox.Seal(nil, plaintext, &n, asKey(key)), nil
}

// Open decrypts a sealed envelope. A wrong passphrase (or tampered payload)
// fails here — the only failure mode the server can ever trigger.
func Open(key, nonce, ciphertext []byte) ([]byte, error) {
	var n [24]byte
	if len(nonce) != len(n) {
		return nil, fmt.Errorf("bad nonce length")
	}
	copy(n[:], nonce)
	out, ok := secretbox.Open(nil, ciphertext, &n, asKey(key))
	if !ok {
		return nil, fmt.Errorf("decryption failed — wrong passphrase or corrupted bundle")
	}
	return out, nil
}
