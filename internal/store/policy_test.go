package store

import (
	"strings"
	"testing"
	"time"
)

// AC3: retention policy retires only old memories — soft delete, tombstone,
// and a "retention" audit entry per record.
func TestAC3_RetentionEnforcement(t *testing.T) {
	s := openTest(t)
	oldNow := time.Now().Unix()
	// an old memory (created 40 days ago) and a fresh one
	if _, err := s.db.Exec(`INSERT INTO memories
		(id, type, content, source_turn_ids, agent_id, confidence, salience, created_at, updated_at, ttl, deleted, status, superseded_by)
		VALUES ('mem_old', 'fact', 'User lived in Berlin in 2019', '', 'cursor', 1, 0.5, ?, ?, 0, 0, 'active', ''),
		       ('mem_new', 'fact', 'User lives in Tokyo now', '', 'cursor', 1, 0.5, ?, ?, 0, 0, 'active', '')`,
		oldNow-40*86400, oldNow-40*86400, oldNow, oldNow); err != nil {
		t.Fatal(err)
	}

	cutoff := oldNow - 30*86400 // 30-day retention
	ids, err := s.EnforceRetention(cutoff, "policy")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "mem_old" {
		t.Fatalf("only the old memory must be retired, got %v", ids)
	}
	// soft deleted, hidden from list, tombstoned for sync
	if _, err := s.Get("mem_old"); err == nil {
		t.Fatal("retired memory must not be gettable")
	}
	tombs, _ := s.TombstonesSince(0)
	if len(tombs) != 1 || tombs[0].MemoryID != "mem_old" {
		t.Fatalf("retention must tombstone, got %+v", tombs)
	}
	// audited as policy action
	audit, _ := s.ListAudit(0)
	found := false
	for _, a := range audit {
		if a.Action == "retention" && a.Actor == "policy" && a.MemoryID == "mem_old" {
			found = true
		}
	}
	if !found {
		t.Fatal("retention must be audited per record")
	}
	// fresh memory untouched
	if m, err := s.Get("mem_new"); err != nil || m.Content != "User lives in Tokyo now" {
		t.Fatalf("fresh memory must survive: %+v (%v)", m, err)
	}
	// idempotent: second pass retires nothing
	if ids, _ := s.EnforceRetention(cutoff, "policy"); len(ids) != 0 {
		t.Fatalf("second enforce must be a no-op, got %v", ids)
	}
}

// AC4: redaction patterns scrub content on the write paths (remember, sync
// merge excluded by design; turns via InsertTurns).
func TestAC4_RedactionOnWrites(t *testing.T) {
	s := openTest(t)
	res, err := CompileRedactors([]string{
		`sk-[a-zA-Z0-9]{10,}`,
		`\b\d{3}-\d{2}-\d{4}\b`, // SSN-shaped
	})
	if err != nil {
		t.Fatal(err)
	}
	s.SetRedactors(res)

	id, err := s.Remember(&Memory{
		Content: "User's API key is sk-abcdefghij1234 and SSN 123-45-6789",
		Type:    "fact",
	}, "cursor")
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.Content, "sk-abcdefghij") || strings.Contains(m.Content, "123-45-6789") {
		t.Fatalf("redaction failed: %q", m.Content)
	}
	if !strings.Contains(m.Content, "[redacted]") {
		t.Fatalf("content should carry [redacted] markers: %q", m.Content)
	}
	if !strings.Contains(m.Type, "redacted") {
		t.Fatalf("type should note redaction for audit visibility: %q", m.Type)
	}

	// turns captured through the WAL drain path are scrubbed too
	s.InsertTurns([]Turn{{SessionID: "s", Role: "user", Content: "my key is sk-zzzzzzzzzz9999 keep it safe"}})
	st, _ := s.CollectStats()
	_ = st
	var content string
	if err := s.db.QueryRow(`SELECT content FROM turns WHERE role = 'user'`).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "sk-zzzzzzzzzz") {
		t.Fatalf("turn redaction failed: %q", content)
	}
	if !strings.Contains(content, "[redacted]") {
		t.Fatalf("turn should carry [redacted]: %q", content)
	}
}

// AC5: with no [policies] section, behavior is byte-identical to v0.6 —
// no redaction, no retention sweep.
func TestAC5_PoliciesDefaultOff(t *testing.T) {
	s := openTest(t)
	// no SetRedactors call at all
	id, _ := s.Remember(&Memory{Content: "key sk-abcdefghij1234 untouched"}, "mcp")
	m, _ := s.Get(id)
	if m.Content != "key sk-abcdefghij1234 untouched" {
		t.Fatalf("no-policy config must not scrub: %q", m.Content)
	}
	if ids, err := s.EnforceRetention(time.Now().Unix(), "policy"); err != nil || len(ids) != 0 {
		t.Fatalf("fresh memories must survive any cutoff at now: %v %v", ids, err)
	}
	// CompileRedactors rejects bad patterns by name
	if _, err := CompileRedactors([]string{"([bad"}); err == nil || !strings.Contains(err.Error(), "([bad") {
		t.Fatalf("bad pattern must name itself: %v", err)
	}
}
