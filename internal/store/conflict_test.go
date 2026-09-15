package store

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// AC1: writing a contradicting memory supersedes the old one instead of
// keeping both.
func TestAC1_SupersedeOnContradiction(t *testing.T) {
	s := openTest(t)
	old, err := s.Remember(&Memory{Content: "User lives in Singapore with their family"}, "cursor")
	if err != nil {
		t.Fatal(err)
	}
	new, err := s.Remember(&Memory{Content: "User moved to Tokyo last month for work"}, "claude")
	if err != nil {
		t.Fatal(err)
	}

	// old memory is superseded, not deleted and not active
	var status, by string
	if err := s.db.QueryRow(`SELECT status, superseded_by FROM memories WHERE id = ?`, old).Scan(&status, &by); err != nil {
		t.Fatal(err)
	}
	if status != "superseded" || by != new {
		t.Fatalf("old memory: status=%q by=%q (want superseded by %q)", status, by, new)
	}
	// exactly one conflict pair, auto-resolved to "new"
	cs, err := s.ListConflicts(0)
	if err != nil || len(cs) != 1 {
		t.Fatalf("conflicts: %v %d", err, len(cs))
	}
	if cs[0].OldID != old || cs[0].NewID != new || cs[0].Resolution != "new" {
		t.Fatalf("conflict pair wrong: %+v", cs[0])
	}
	// supersession is audited on the old memory
	audit, _ := s.ListAudit(0)
	sawSupersede := false
	for _, a := range audit {
		if a.Action == "supersede" && a.MemoryID == old {
			sawSupersede = true
		}
	}
	if !sawSupersede {
		t.Fatal("supersede must be audited")
	}
	// list defaults exclude the superseded memory
	mems, _ := s.List("", "", time.Time{}, time.Time{}, 0, false)
	if len(mems) != 1 || mems[0].ID != new {
		t.Fatalf("default list must show only the new memory, got %+v", mems)
	}
	// include_superseded shows both
	mems, _ = s.List("", "", time.Time{}, time.Time{}, 0, true)
	if len(mems) != 2 {
		t.Fatalf("include_superseded must show both, got %d", len(mems))
	}
}

// AC2: recall never returns both sides of a conflict by default.
func TestAC2_RecallExcludesSuperseded(t *testing.T) {
	s := openTest(t)
	s.Remember(&Memory{Content: "User works at Acme Corp on payments"}, "cursor")
	s.Remember(&Memory{Content: "User joined Globex as a staff engineer"}, "claude")

	for _, q := range []string{"Acme", "Globex", "works", "engineer"} {
		mems, err := s.Recall(q, 10, 0.1)
		if err != nil {
			t.Fatal(err)
		}
		if len(mems) > 1 {
			t.Fatalf("recall %q returned both sides: %d", q, len(mems))
		}
	}
	// the surviving side is the new one
	mems, _ := s.Recall("engineer", 10, 0.1)
	if len(mems) != 1 || !strings.Contains(mems[0].Content, "Globex") {
		t.Fatalf("recall must return the winning side, got %+v", mems)
	}
}

// AC3: a keep-both resolution is honored and audited — both memories become
// active again.
func TestAC3_ResolveKeepBoth(t *testing.T) {
	s := openTest(t)
	old, _ := s.Remember(&Memory{Content: "User uses Cursor for their coding work"}, "cursor")
	new, _ := s.Remember(&Memory{Content: "User switched to Zed for their coding work"}, "codex")

	cs, _ := s.ListConflicts(0)
	if len(cs) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(cs))
	}
	if err := s.ResolveConflict(cs[0].ID, "both", "ui"); err != nil {
		t.Fatal(err)
	}
	// both active again
	mems, _ := s.List("", "", time.Time{}, time.Time{}, 0, false)
	if len(mems) != 2 {
		t.Fatalf("keep-both must reinstate the old memory, %d active", len(mems))
	}
	// resolution recorded
	cs, _ = s.ListConflicts(0)
	if cs[0].Resolution != "both" || cs[0].OldStatus != "active" {
		t.Fatalf("resolution not recorded: %+v", cs[0])
	}
	// audited on both sides
	audit, _ := s.ListAudit(0)
	resolves := 0
	for _, a := range audit {
		if a.Action == "resolve" && a.Actor == "ui" {
			resolves++
		}
	}
	if resolves != 2 {
		t.Fatalf("keep-both must be audited on both memories, got %d", resolves)
	}
	_ = old
	_ = new
}

// prefer-old swaps the supersession direction.
func TestResolvePreferOld(t *testing.T) {
	s := openTest(t)
	s.Remember(&Memory{Content: "User lives in Singapore with their family"}, "cursor")
	newID, _ := s.Remember(&Memory{Content: "User moved to Tokyo last month for work"}, "claude")

	cs, _ := s.ListConflicts(0)
	if err := s.ResolveConflict(cs[0].ID, "old", "ui"); err != nil {
		t.Fatal(err)
	}
	mems, _ := s.List("", "", time.Time{}, time.Time{}, 0, false)
	if len(mems) != 1 || !strings.Contains(mems[0].Content, "Singapore") {
		t.Fatalf("prefer-old must reinstate Singapore, got %+v", mems)
	}
	// the Tokyo memory is now the superseded one
	row := s.db.QueryRow(`SELECT status, superseded_by FROM memories WHERE id = ?`, newID)
	var status, by string
	row.Scan(&status, &by)
	if status != "superseded" || by == "" {
		t.Fatalf("prefer-old must supersede the new memory: %q %q", status, by)
	}
	// invalid resolutions are rejected
	if err := s.ResolveConflict(cs[0].ID, "nuke", "ui"); err == nil {
		t.Fatal("invalid resolution must be rejected")
	}
	// non-conflicting writes never create conflicts
	s.Remember(&Memory{Content: "User prefers window seats on flights"}, "cursor")
	if cs, _ := s.ListConflicts(0); len(cs) != 1 {
		t.Fatalf("compatible writes must not create conflicts, got %d", len(cs))
	}
}

// AC5: a pre-v0.3 database upgrades in place on Open — the migration ALTERs
// status/superseded_by onto the existing memories table (all rows default to
// active), creates the conflicts table, and conflict detection works on the
// upgraded rows. Built here exactly the way a real v0.2 install looks:
// v0.2-era DDL written by hand, then opened with the current binary.
func TestAC5_MigrationFromV02Schema(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", dir+"/veda.db")
	if err != nil {
		t.Fatal(err)
	}
	// the v0.2 schema, verbatim: no status/superseded_by, no conflicts table
	_, err = db.Exec(`
CREATE TABLE memories (
  id TEXT PRIMARY KEY, type TEXT, content TEXT,
  source_turn_ids TEXT, agent_id TEXT,
  confidence REAL, salience REAL,
  created_at INTEGER, updated_at INTEGER,
  ttl INTEGER, deleted INTEGER DEFAULT 0
);
CREATE VIRTUAL TABLE memories_fts USING fts5(
  content, type, content='memories', content_rowid='rowid'
);
CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
  INSERT INTO memories_fts(rowid, content, type) VALUES (new.rowid, new.content, new.type);
END;
CREATE TRIGGER memories_ad AFTER DELETE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, content, type) VALUES('delete', old.rowid, old.content, old.type);
END;
CREATE TRIGGER memories_au AFTER UPDATE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, content, type) VALUES('delete', old.rowid, old.content, old.type);
  INSERT INTO memories_fts(rowid, content, type) VALUES (new.rowid, new.content, new.type);
END;
INSERT INTO memories (id, type, content, source_turn_ids, agent_id, confidence, salience, created_at, updated_at, ttl, deleted)
VALUES ('mem_old', 'fact', 'User lives in Singapore', '', 'cursor', 1.0, 0.5, 1, 1, 0, 0);
`)
	if err != nil {
		t.Fatalf("build v0.2 db: %v", err)
	}
	db.Close()

	// open with the current binary: triggers + migration apply
	s2, err := Open(dir + "/veda.db")
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	defer s2.Close()
	mems, err := s2.List("", "", time.Time{}, time.Time{}, 0, false)
	if err != nil || len(mems) != 1 {
		t.Fatalf("migrated memory must be active: %d (%v)", len(mems), err)
	}
	// and the conflict machinery works on the migrated row
	if _, err := s2.Remember(&Memory{Content: "User moved to Tokyo for work"}, "claude"); err != nil {
		t.Fatal(err)
	}
	if cs, _ := s2.ListConflicts(0); len(cs) != 1 {
		t.Fatalf("conflict detection must work after migration, got %d", len(cs))
	}
	if mems, _ := s2.List("", "", time.Time{}, time.Time{}, 0, false); len(mems) != 1 ||
		!strings.Contains(mems[0].Content, "Tokyo") {
		t.Fatalf("post-migration supersession must hold, got %+v", mems)
	}
}
