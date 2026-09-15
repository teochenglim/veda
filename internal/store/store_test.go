package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/veda.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// AC1: init creates the PRD schema (all six tables) in ~/.veda/veda.db.
func TestAC1_SchemaCreatedOnOpen(t *testing.T) {
	s := openTest(t)
	want := []string{"sessions", "turns", "memories", "memories_fts", "audit", "telemetry_queue"}
	for _, table := range want {
		var name string
		err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type IN ('table','virtual table') AND lower(name) = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing after init: %v", table, err)
		}
	}
}

// AC2 (part): remember → {id} creates a memory with sane defaults.
func TestAC2_RememberReturnsID(t *testing.T) {
	s := openTest(t)
	id, err := s.Remember(&Memory{Content: "User prefers tea over coffee", Type: "preference"}, "mcp")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "mem_") {
		t.Fatalf("expected mem_ id, got %q", id)
	}
	m, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if m.Content != "User prefers tea over coffee" || m.Type != "preference" {
		t.Errorf("round-trip mismatch: %+v", m)
	}
	if m.CreatedAt == 0 || m.UpdatedAt < m.CreatedAt {
		t.Errorf("timestamps not stamped: %+v", m)
	}
}

// AC6: recall honors FTS5 matching, limit and min_confidence.
func TestAC6_RecallFTS5LimitConfidence(t *testing.T) {
	s := openTest(t)
	fixtures := []struct {
		content string
		conf    float64
	}{
		{"User prefers tea over coffee in the afternoon", 0.9},
		{"User is allergic to peanuts", 0.9},
		{"User lives in Singapore", 0.2}, // below default min_confidence
	}
	for _, f := range fixtures {
		if _, err := s.Remember(&Memory{Content: f.content, Confidence: f.conf}, "mcp"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Recall("tea", 5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Content, "tea") {
		t.Fatalf("expected the tea memory, got %+v", got)
	}
	// limit is enforced
	got, err = s.Recall("user", 2, 0.0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("limit=2 not enforced, got %d", len(got))
	}
	// min_confidence filters low-quality memories
	got, err = s.Recall("Singapore", 5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("min_confidence=0.5 should hide the 0.2-confidence memory, got %+v", got)
	}
	// punctuation-heavy query falls back to LIKE without an FTS syntax error
	got, err = s.Recall("allergic to peanuts!", 5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("LIKE fallback failed, got %+v", got)
	}
}

// AC2/AC5 helper behavior: forget is soft + audited; list hides deleted and expired.
func TestAC2b_ForgetListAuditTTL(t *testing.T) {
	s := openTest(t)
	id, _ := s.Remember(&Memory{Content: "User is learning Go", Type: "goal"}, "mcp")
	// TTL in the past => expired, hidden and not gettable
	expired, _ := s.Remember(&Memory{Content: "stale fact", TTL: time.Now().Add(-time.Hour).Unix()}, "mcp")
	if _, err := s.Get(expired); err == nil {
		t.Fatal("expired memory should not be gettable")
	}
	ok, err := s.Forget(id, "mcp", "test forget")
	if err != nil || !ok {
		t.Fatalf("forget ok=%v err=%v", ok, err)
	}
	if ok2, _ := s.Forget(id, "mcp", "again"); ok2 {
		t.Fatal("double forget should report false")
	}
	if _, err := s.Get(id); err == nil {
		t.Fatal("forgotten memory should not be gettable")
	}
	audit, err := s.ListAudit(0)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]int{}
	for _, a := range audit {
		actions[a.Action]++
	}
	if actions["create"] != 2 || actions["forget"] != 1 {
		t.Fatalf("audit log wrong: %v", actions)
	}
	// List filters
	mems, _ := s.List("goal", "", time.Time{}, time.Time{}, 0)
	if len(mems) != 0 {
		t.Fatalf("deleted memory must not be listed, got %+v", mems)
	}
}

// AC10: export → wipe → import round-trips memories.
func TestAC10_ExportImportRoundTrip(t *testing.T) {
	src := openTest(t)
	ids := map[string]bool{}
	for _, c := range []string{"fact one about the user", "fact two about the user", "fact three"} {
		id, err := src.Remember(&Memory{Content: c, Type: "fact"}, "mcp")
		if err != nil {
			t.Fatal(err)
		}
		ids[id] = true
	}
	data, err := src.Export()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"memories"`) {
		t.Fatal("export must be a JSON document with a memories key")
	}
	dst := openTest(t)
	n, err := dst.Import(data)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("expected 3 imported, got %d", n)
	}
	// importing again is idempotent (existing ids skipped)
	if n, _ := dst.Import(data); n != 0 {
		t.Fatalf("re-import should skip existing ids, got %d", n)
	}
	got, _ := dst.List("", "", time.Time{}, time.Time{}, 0)
	if len(got) != 3 {
		t.Fatalf("round-trip lost memories: %d", len(got))
	}
	for _, m := range got {
		if !ids[m.ID] {
			t.Fatalf("imported id %q not from source export", m.ID)
		}
	}
}

// Pending queue behavior backing the UI's Pending tab and the worker.
func TestPendingQueue(t *testing.T) {
	s := openTest(t)
	s.InsertPending([]PendingTurn{
		{SessionID: "s1", Role: "user", Content: "I prefer window seats on flights", TS: 1, GateScore: 0.5},
		{SessionID: "s1", Role: "user", Content: "My daughter is called Ada", TS: 2, GateScore: 0.5},
	})
	pending, err := s.ListPending(0)
	if err != nil || len(pending) != 2 {
		t.Fatalf("ListPending: %v %d", err, len(pending))
	}
	taken, err := s.TakePending(1)
	if err != nil || len(taken) != 1 {
		t.Fatalf("TakePending: %v %d", err, len(taken))
	}
	if taken[0].Content != "I prefer window seats on flights" {
		t.Fatalf("TakePending must be oldest-first, got %q", taken[0].Content)
	}
	if left, _ := s.ListPending(0); len(left) != 1 {
		t.Fatalf("TakePending must remove taken rows, %d left", len(left))
	}
	if err := s.DropPending(left0ID(t, s)); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.ListPending(0); len(left) != 0 {
		t.Fatal("DropPending should empty the queue")
	}
}

func left0ID(t *testing.T, s *Store) int64 {
	t.Helper()
	p, _ := s.ListPending(0)
	return p[0].ID
}

// Stats: counts only, no content ever leaves the store layer.
func TestCollectStatsNoContent(t *testing.T) {
	s := openTest(t)
	s.Remember(&Memory{Content: "secret preference content"}, "mcp")
	s.RecordRecall(true)
	st, err := s.CollectStats()
	if err != nil {
		t.Fatal(err)
	}
	if st.Memories != 1 || st.RecallCalls != 1 || st.RecallHits != 1 {
		t.Fatalf("stats wrong: %+v", st)
	}
	b, _ := jsonMarshal(st)
	if strings.Contains(string(b), "secret") {
		t.Fatal("telemetry stats must never contain content")
	}
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
