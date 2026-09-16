package conformance

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/teochenglim/veda/internal/audit"
	"github.com/teochenglim/veda/internal/store"
	"github.com/teochenglim/veda/internal/syncengine"
	"golang.org/x/crypto/nacl/secretbox"
)

// Golden vectors (spec/vectors/) let implementations test offline, across
// versions, against bytes frozen at draft-02 time. The keys below are
// TEST-ONLY fixtures, deliberately committed — they guard nothing.
const (
	vectorExportFile = "export.v1.json"
	vectorAuditFile  = "audit-signed.json"
	vectorSyncFile   = "sync-envelope.json"

	vectorPassphrase = "uomp-golden-vector"
	vectorSalt       = "5f549156482c4b0ea456d64ca4a26932" // hex, 16 bytes
	vectorCreatedAt  = int64(1789440000)
	vectorInstallID  = "vector-install-id"
	// fixed Ed25519 test key (pub derives from priv; committed on purpose)
	vectorPrivHex = "9b933dcdaa43d61c86950c0cc68c1c9af72e8410a4139d33887c657264cefbc6299be5abf4325baff70cd026314b57f7a10e719ef042d8cb88708d43f2db1031"
)

var vectorsDir = func() string {
	dir, err := filepath.Abs(filepath.Join("..", "..", "spec", "vectors"))
	if err != nil {
		panic(err)
	}
	return dir
}()

// TestGenerateVectors regenerates every vector deterministically. It only
// writes when VEDA_UPDATE_VECTORS=1; either way it then runs the same
// assertions as TestAC4_GoldenVectors.
func TestGenerateVectors(t *testing.T) {
	if os.Getenv("VEDA_UPDATE_VECTORS") == "" {
		t.Skip("set VEDA_UPDATE_VECTORS=1 to regenerate spec/vectors/")
	}
	if err := os.MkdirAll(vectorsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// export.v1.json — the canonical draft-01 export document
	exportDoc := map[string]any{
		"version":     "1",
		"exported_at": vectorCreatedAt,
		"memories":    goldenMemories(),
		"turns":       []map[string]any{{"id": 1, "session_id": "s1", "role": "user", "content": "vector turn one", "ts": vectorCreatedAt}},
		"audit":       []map[string]any{{"id": 1, "memory_id": "mem_00000000000000000000000000000001", "action": "create", "actor": "vector", "reason": "golden vector", "ts": vectorCreatedAt}},
	}
	writeVector(t, vectorExportFile, marshalPretty(t, exportDoc))

	// audit-signed.json — signed with the fixed test key, assembled exactly
	// the way audit.BuildExport does (payload bytes are what get signed)
	priv := mustKey(t)
	payload := fmt.Sprintf(
		`{"type":"veda-audit-export","version":1,"exported_at":%d,"install_id":%q,"entries":[{"id":1,"memory_id":"mem_00000000000000000000000000000001","action":"create","actor":"vector","reason":"golden vector","ts":%d}]}`,
		vectorCreatedAt, vectorInstallID, vectorCreatedAt)
	sig := ed25519.Sign(ed25519.PrivateKey(mustBytes(t, vectorPrivHex)), []byte(payload))
	signed := fmt.Sprintf(`{
  "type": "veda-audit-export",
  "version": 1,
  "exported_at": %d,
  "payload": %s,
  "public_key": %q,
  "signature": %q
}`, vectorCreatedAt, payload, hex.EncodeToString(priv.Public().(ed25519.PublicKey)), hex.EncodeToString(sig))
	writeVector(t, vectorAuditFile, []byte(signed))

	// sync-envelope.json — fixed key + fixed nonce ⇒ byte-deterministic
	bundle := syncengine.Bundle{
		DeviceID: "dev_vector",
		Memories: goldenMemories()[:1],
	}
	key, err := syncengine.DeriveKey(vectorPassphrase, vectorSalt)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := mustJSON(t, bundle)
	nonce := mustBytes(t, "000102030405060708090a0b0c0d0e0f1011121314151617")
	sealed := mustSeal(t, key, nonce, plaintext)
	env := syncengine.Envelope{
		DeviceID:   "dev_vector",
		CreatedAt:  vectorCreatedAt,
		Crypto:     syncengine.CryptoParams{Algo: "xsalsa20poly1305", KDF: "scrypt", Salt: vectorSalt, N: 32768, R: 8, P: 1},
		Nonce:      hex.EncodeToString(nonce),
		Ciphertext: base64Std(sealed),
	}
	writeVector(t, vectorSyncFile, marshalPretty(t, env))

	verifyVectors(t)
}

// TestAC4_GoldenVectors verifies the committed vectors: a fixed-key envelope
// round-trips, the signed audit vector verifies, and the export vector is a
// valid draft-01 document.
func TestAC4_GoldenVectors(t *testing.T) {
	verifyVectors(t)
}

func verifyVectors(t *testing.T) {
	t.Helper()

	// export vector: valid draft-01 shape, and importable by the real store
	data := readVector(t, vectorExportFile)
	if viols := ValidateExport(data); len(viols) > 0 {
		t.Fatalf("export vector violates draft-01: %v", viols)
	}
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "veda.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n, err := s.Import(data)
	if err != nil || n != 2 {
		t.Fatalf("export vector failed to import: %d imported, err %v", n, err)
	}

	// audit vector: signature verifies over its own payload bytes
	doc, err := audit.VerifyExport(readVector(t, vectorAuditFile))
	if err != nil {
		t.Fatalf("signed audit vector does not verify: %v", err)
	}
	if doc.InstallID != vectorInstallID || len(doc.Entries) != 1 || doc.Entries[0].Action != "create" {
		t.Fatalf("audit vector payload drifted: %+v", doc)
	}

	// sync vector: opens under the documented passphrase and matches
	var env syncengine.Envelope
	if err := json.Unmarshal(readVector(t, vectorSyncFile), &env); err != nil {
		t.Fatal(err)
	}
	bundle, err := syncengine.OpenEnvelope(env, vectorPassphrase)
	if err != nil {
		t.Fatalf("fixed-key envelope does not open: %v", err)
	}
	if bundle.DeviceID != "dev_vector" || len(bundle.Memories) != 1 ||
		bundle.Memories[0].Content != "Vector: user prefers window seats" {
		t.Fatalf("sync vector bundle drifted: %+v", bundle)
	}
}

// --- deterministic fixture bytes -------------------------------------------

func goldenMemories() []*store.Memory {
	return []*store.Memory{
		{ID: "mem_00000000000000000000000000000001", Type: "preference", Content: "Vector: user prefers window seats", Confidence: 0.9, Salience: 0.8, CreatedAt: vectorCreatedAt, UpdatedAt: vectorCreatedAt},
		{ID: "mem_00000000000000000000000000000002", Type: "fact", Content: "Vector: user lives in Singapore", SourceTurnIDs: "t1,t2", AgentID: "cursor", Confidence: 1, Salience: 0.5, CreatedAt: vectorCreatedAt + 1, UpdatedAt: vectorCreatedAt + 1},
	}
}

func writeVector(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(vectorsDir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readVector(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(vectorsDir, name))
	if err != nil {
		t.Fatalf("missing golden vector %s (regenerate with VEDA_UPDATE_VECTORS=1): %v", name, err)
	}
	return data
}

func marshalPretty(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	b := mustBytes(t, vectorPrivHex)
	if len(b) != ed25519.PrivateKeySize {
		t.Fatalf("vector private key has wrong size %d", len(b))
	}
	return ed25519.PrivateKey(b)
}

// mustSeal is secretbox.Seal with a caller-supplied nonce — the fixed nonce
// is what makes the sync vector byte-deterministic.
func mustSeal(t *testing.T, key, nonce, plaintext []byte) []byte {
	t.Helper()
	var n [24]byte
	copy(n[:], nonce)
	var k [32]byte
	copy(k[:], key)
	return secretbox.Seal(nil, plaintext, &n, &k)
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
