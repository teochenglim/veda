// Package audit implements v0.7's compliance export: the full audit log as
// a portable JSON document, signed with Ed25519 so a compliance reviewer
// can prove nothing was altered after export.
//
// Key model: `veda audit keygen` writes the private key to
// ~/.veda/audit_signing_key (0600) and prints the public key. Exports embed
// the public key, so anyone holding the export can verify it — only the
// signer needed the private half.
package audit

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/teochenglim/veda/internal/store"
)

// ExportDoc is the on-disk shape. Payload is the exact signed bytes —
// verification uses the file's own bytes, never a re-marshal.
type ExportDoc struct {
	Type       string          `json:"type"` // "veda-audit-export"
	Version    int             `json:"version"`
	ExportedAt int64           `json:"exported_at"`
	Payload    json.RawMessage `json:"payload"`
	PublicKey  string          `json:"public_key"` // hex
	Signature  string          `json:"signature"`  // hex over Payload bytes
}

type payload struct {
	Type       string              `json:"type"`
	Version    int                 `json:"version"`
	ExportedAt int64               `json:"exported_at"`
	InstallID  string              `json:"install_id"`
	Entries    []*store.AuditEntry `json:"entries"`
}

// KeyPair is a generated signing identity.
type KeyPair struct {
	Public  []byte
	Private []byte
}

// Generate creates a new Ed25519 key pair.
func Generate() (KeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{Public: pub, Private: priv}, nil
}

// SavePrivate writes the private key hex, 0600.
func SavePrivate(path string, priv []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(hex.EncodeToString(priv)), 0o600)
}

// LoadPrivate reads a hex-encoded private key file.
func LoadPrivate(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("bad signing key: %w", err)
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("bad signing key length %d", len(key))
	}
	return key, nil
}

// BuildExport renders the full audit log as a signed document.
func BuildExport(s *store.Store, installID string, priv []byte, now time.Time) ([]byte, error) {
	entries, err := s.ListAudit(0)
	if err != nil {
		return nil, err
	}
	p := payload{
		Type: "veda-audit-export", Version: 1,
		ExportedAt: now.Unix(), InstallID: installID, Entries: entries,
	}
	payloadBytes, err := json.Marshal(p) // struct order ⇒ deterministic bytes
	if err != nil {
		return nil, err
	}
	sk := ed25519.PrivateKey(priv)
	pub := sk.Public().(ed25519.PublicKey)
	sig := ed25519.Sign(sk, payloadBytes)

	// Hand-assemble the file so the payload bytes on disk are exactly the
	// bytes that were signed — json.MarshalIndent would re-indent the
	// embedded RawMessage and break verification.
	meta := fmt.Sprintf(`{
  "type": "veda-audit-export",
  "version": 1,
  "exported_at": %d,
  "payload": %s,
  "public_key": %q,
  "signature": %q
}`, p.ExportedAt, payloadBytes, hex.EncodeToString(pub), hex.EncodeToString(sig))
	return []byte(meta), nil
}

// VerifyExport checks a signed export's signature over its own payload
// bytes and returns the decoded payload.
func VerifyExport(data []byte) (*payload, error) {
	var doc ExportDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("not a veda audit export: %w", err)
	}
	if doc.Type != "veda-audit-export" {
		return nil, fmt.Errorf("not a veda audit export (type %q)", doc.Type)
	}
	pub, err := hex.DecodeString(doc.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("bad public key")
	}
	sig, err := hex.DecodeString(doc.Signature)
	if err != nil {
		return nil, fmt.Errorf("bad signature encoding")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), doc.Payload, sig) {
		return nil, fmt.Errorf("SIGNATURE INVALID — this export has been altered")
	}
	var p payload
	if err := json.Unmarshal(doc.Payload, &p); err != nil {
		return nil, err
	}
	return &p, nil
}
