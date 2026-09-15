package syncengine

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"

	"github.com/teochenglim/veda/internal/store"
)

// Bundle is the decrypted payload: everything two devices need to converge.
type Bundle struct {
	DeviceID   string            `json:"device_id"`
	Memories   []*store.Memory   `json:"memories"`
	Tombstones []store.Tombstone `json:"tombstones"`
}

// Envelope is what the server stores: routing metadata in the clear, the
// bundle itself only as ciphertext. Nothing inside Ciphertext is ever
// visible to the endpoint.
type Envelope struct {
	DeviceID   string       `json:"device_id"`
	CreatedAt  int64        `json:"created_at"`
	Crypto     CryptoParams `json:"crypto"`
	Nonce      string       `json:"nonce"`      // hex
	Ciphertext string       `json:"ciphertext"` // base64
}

// BuildEnvelope seals a bundle for the wire.
func BuildEnvelope(bundle Bundle, passphrase, saltHex string, createdAt int64) (Envelope, error) {
	key, err := DeriveKey(passphrase, saltHex)
	if err != nil {
		return Envelope{}, err
	}
	plaintext, err := json.Marshal(bundle)
	if err != nil {
		return Envelope{}, err
	}
	nonce, ciphertext, err := Seal(key, plaintext)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		DeviceID:   bundle.DeviceID,
		CreatedAt:  createdAt,
		Crypto:     CryptoParams{Algo: "xsalsa20poly1305", KDF: "scrypt", Salt: saltHex, N: scryptN, R: scryptR, P: scryptP},
		Nonce:      hex.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ciphertext),
	}, nil
}

// OpenEnvelope unseals a received envelope with the sync passphrase. The
// KDF salt rides in the (non-secret) envelope, so any device holding the
// same passphrase can decrypt any device's bundle.
func OpenEnvelope(env Envelope, passphrase string) (Bundle, error) {
	var b Bundle
	key, err := DeriveKey(passphrase, env.Crypto.Salt)
	if err != nil {
		return b, err
	}
	nonce, err := hex.DecodeString(env.Nonce)
	if err != nil {
		return b, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return b, err
	}
	plaintext, err := Open(key, nonce, ciphertext)
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(plaintext, &b); err != nil {
		return b, err
	}
	return b, nil
}
