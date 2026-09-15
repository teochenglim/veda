package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teochenglim/veda/internal/store"
)

func extStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/veda.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// AC1: a consented install's payload carries the ext block with the new
// fields; with no collector data the payload stays v0.4-shaped (no ext).
func TestAC1_ExtBlockPresentOnlyWithData(t *testing.T) {
	s := extStore(t)
	s.Remember(&store.Memory{Content: "User prefers aisle seats"}, "cursor")

	// no ext: v0.4-shaped payload (ext omitted)
	payload, err := Preview(s, "install-1", "v0.5.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, `"ext"`) {
		t.Fatalf("nil ext must omit the block: %s", payload)
	}

	// collector data: ext block present with the fields
	c := NewCollector()
	c.AddClient("claude")
	c.AddClient("cursor")
	c.AddGateRejected(3)
	c.AddUIOpen()
	c.RecordError("embed_fail")
	dbFile := filepath.Join(t.TempDir(), "veda.db")
	os.WriteFile(dbFile, make([]byte, 4096), 0o600)
	ext := c.Snapshot(dbFile, true, false, ProviderFromURL("http://localhost:11434/v1"))
	payload, err = Preview(s, "install-1", "v0.5.0", ext)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		t.Fatal(err)
	}
	extDoc, ok := doc["ext"].(map[string]any)
	if !ok {
		t.Fatalf("ext block missing: %s", payload)
	}
	for _, key := range []string{"agent", "clients", "semantic_enabled", "embed_provider", "gate_rejected", "ui_opens", "db_size_kb", "errors"} {
		if _, ok := extDoc[key]; !ok {
			t.Errorf("ext missing %q: %v", key, extDoc)
		}
	}
	if extDoc["agent"] != "claude" {
		t.Fatalf("agent = %v, want claude (first client seen)", extDoc["agent"])
	}
	if extDoc["embed_provider"] != "ollama" {
		t.Fatalf("embed_provider = %v, want the ollama enum", extDoc["embed_provider"])
	}
}

// AC2: the serialized payload contains no URL/path/locale patterns; the
// embedding endpoint never appears — even with a full collector.
func TestAC2_ExtNeverContainsUrlsOrPaths(t *testing.T) {
	s := extStore(t)
	s.Remember(&store.Memory{Content: "User's notes mention /Users/secret/path and https://private.example.com"}, "cursor")

	c := NewCollector()
	c.AddClient("cursor")
	c.RecordError("llm_fail")
	ext := c.Snapshot("/Users/nobody/.veda/veda.db", true, true, ProviderFromURL("http://127.0.0.1:5432/v1"))

	for _, leak := range []string{"/Users", "http", "://", ".example.com", "5432", "secret", "TZ="} {
		if strings.Contains(ext.EmbedProvider, leak) || containsAny(serialize(t, ext), leak) {
			t.Fatalf("payload leaks %q: %s", leak, serialize(t, ext))
		}
	}
	// full payload through Preview too (install id is pseudonymous, allowed)
	payload, err := Preview(s, "install-2", "v0.5.0", ext)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"http", "://", "/Users", "private.example.com"} {
		if strings.Contains(payload, leak) {
			t.Fatalf("payload leaks %q: %s", leak, payload)
		}
	}
}

// AC3: sanitization — names lowercased to [a-z0-9._-] and ≤ 32, unknown →
// "other", clients capped at 8, error codes capped at 16 and allowlisted.
func TestAC3_ExtSanitizedAndCapped(t *testing.T) {
	if got := sanitizeName("  CuRsOr.EXE-9 "); got != "cursor.exe-9" {
		t.Fatalf("sanitizeName: %q", got)
	}
	if got := sanitizeName(""); got != "other" {
		t.Fatalf("empty must be other, got %q", got)
	}
	if got := sanitizeName("unknown"); got != "other" {
		t.Fatalf("unknown must be other, got %q", got)
	}
	if got := sanitizeName("Weird Name With Spaces!"); got != "weirdnamewithspaces" {
		t.Fatalf("spaces/symbols stripped: %q", got)
	}
	if got := sanitizeName(strings.Repeat("x", 50)); len(got) != maxNameLen {
		t.Fatalf("must cap at %d, got %d", maxNameLen, len(got))
	}
	// embed provider never carries a URL
	if got := ProviderFromURL("https://api.openai.com/v1"); got != "openai" {
		t.Fatalf("openai enum: %q", got)
	}
	if got := ProviderFromURL("https://weird.host.internal:9999/v1"); got != "other" {
		t.Fatalf("unknown host must be other, got %q", got)
	}

	c := NewCollector()
	for i := 0; i < 20; i++ {
		c.AddClient(fmt.Sprintf("client-%02d", i)) // 20 > cap of 8
	}
	for _, code := range []string{"embed_fail", "llm_fail", "not_allowlisted"} {
		c.RecordError(code)
	}
	ext := c.Snapshot("", false, false, "")
	if len(ext.Clients) != maxClients {
		t.Fatalf("clients must cap at %d, got %d", maxClients, len(ext.Clients))
	}
	if len(ext.Errors) != 2 {
		t.Fatalf("only allowlisted codes survive, got %v", ext.Errors)
	}
	if _, ok := ext.Errors["not_allowlisted"]; ok {
		t.Fatal("non-allowlisted error code must be dropped")
	}
}

// AC4: erasure is unchanged — preview/export keep working with ext, and the
// DELETE contract is untouched (covered by the transport test); here we pin
// that ext rides the same payload/install id and forget semantics hold.
func TestAC4_ForgetUnchangedWithExt(t *testing.T) {
	s := extStore(t)
	id, _ := s.Remember(&store.Memory{Content: "User prefers window seats"}, "mcp")
	s.Forget(id, "mcp", "test")

	c := NewCollector()
	c.AddClient("codex")
	payload, err := Preview(s, "install-3", "v0.5.0", c.Snapshot("", false, false, ""))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	json.Unmarshal([]byte(payload), &doc)
	if doc["install_id"] != "install-3" {
		t.Fatalf("install id must ride the same payload: %v", doc["install_id"])
	}
	if doc["memories"].(float64) != 0 {
		t.Fatal("forgotten memory must not be counted")
	}
}

func serialize(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
