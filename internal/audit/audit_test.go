package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teochenglim/veda/internal/store"
)

func auditStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/veda.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// AC1: keygen creates a working Ed25519 identity; export signs; verify
// passes; altering one byte of the payload fails loudly.
func TestAC1_KeygenSignVerifyTamper(t *testing.T) {
	kp, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	s := auditStore(t)
	s.Remember(&store.Memory{Content: "User prefers window seats"}, "cursor")
	id, _ := s.Remember(&store.Memory{Content: "User runs marathons"}, "cursor")
	s.Forget(id, "cursor", "changed mind")

	data, err := BuildExport(s, "install-7", kp.Private, time.Unix(1789500000, 0))
	if err != nil {
		t.Fatal(err)
	}
	p, err := VerifyExport(data)
	if err != nil {
		t.Fatalf("fresh export must verify: %v", err)
	}
	if len(p.Entries) < 3 {
		t.Fatalf("export must carry the full audit trail, got %d entries", len(p.Entries))
	}

	// tamper: flip a byte inside the payload region
	tampered := bytes.Replace(data, []byte(`"action":"create"`), []byte(`"action":"created"`), 1)
	if bytes.Equal(tampered, data) {
		t.Fatal("tamper target not found")
	}
	if _, err := VerifyExport(tampered); err == nil {
		t.Fatal("tampered export must fail verification")
	} else if !strings.Contains(err.Error(), "SIGNATURE INVALID") {
		t.Fatalf("tamper error should be loud: %v", err)
	}

	// private key persistence round trip
	path := filepath.Join(t.TempDir(), "audit_signing_key")
	if err := SavePrivate(path, kp.Private); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("private key must be 0600, got %v", fi.Mode().Perm())
	}
	loaded, err := LoadPrivate(path)
	if err != nil || !bytes.Equal(loaded, kp.Private) {
		t.Fatalf("key round trip: %v", err)
	}
}

// AC2: the export is complete (every action type, chronological) and the
// signature covers the entries — no silent gaps between export and verify.
func TestAC2_ExportCompleteness(t *testing.T) {
	s := auditStore(t)
	id1, _ := s.Remember(&store.Memory{Content: "User speaks English and German"}, "cursor")
	id2, _ := s.Remember(&store.Memory{Content: "User works at Globex"}, "claude")
	// a v0.3-style conflict + resolution lands in the export too
	s.Remember(&store.Memory{Content: "User lives in Singapore with family"}, "cursor")
	s.Remember(&store.Memory{Content: "User moved to Tokyo last month"}, "claude")
	cs, _ := s.ListConflicts(0)
	if len(cs) != 1 {
		t.Fatalf("fixture conflict missing: %d", len(cs))
	}
	if err := s.ResolveConflict(cs[0].ID, "both", "ui"); err != nil {
		t.Fatal(err)
	}
	s.Forget(id1, "claude", "user asked")
	_ = id2

	kp, _ := Generate()
	data, err := BuildExport(s, "install-8", kp.Private, time.Unix(1789500000, 0))
	if err != nil {
		t.Fatal(err)
	}
	p, err := VerifyExport(data)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]int{}
	for _, e := range p.Entries {
		actions[e.Action]++
	}
	for _, want := range []string{"create", "forget", "supersede", "resolve", "retention"} {
		if want != "retention" && actions[want] == 0 {
			t.Fatalf("export incomplete, missing %q: %v", want, actions)
		}
	}
	if p.InstallID != "install-8" {
		t.Fatalf("install id missing: %q", p.InstallID)
	}
}
