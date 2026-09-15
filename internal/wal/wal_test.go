package wal

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// AC3: the WAL is an append-only NDJSON file, and Append never blocks.
func TestAC3_WALAppendOnlyNDJSON(t *testing.T) {
	path := t.TempDir() + "/wal.ndjson"
	w, err := Open(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	w.Append(Entry{SessionID: "s1", Role: "user", Content: "I prefer window seats"})
	w.Append(Entry{SessionID: "s1", Role: "assistant", Content: "Noted, window seats it is."})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// every line is valid NDJSON with the written fields
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	lines := 0
	for sc.Scan() {
		lines++
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("line %d is not NDJSON: %v", lines, err)
		}
		if e.SessionID != "s1" || e.Content == "" {
			t.Fatalf("line %d lost fields: %+v", lines, e)
		}
	}
	if lines != 2 {
		t.Fatalf("expected 2 WAL lines, got %d", lines)
	}

	// append-only: reopening appends rather than truncates
	w2, err := Open(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	w2.Append(Entry{SessionID: "s2", Role: "user", Content: "Another fact for the log"})
	w2.Close()
	data, _ := os.ReadFile(path)
	if got := strings.Count(string(data), "\n"); got != 3 {
		t.Fatalf("reopen must append, got %d lines", got)
	}
}

// AC3: Append never blocks even when the buffer is full.
func TestAC3_AppendNeverBlocks(t *testing.T) {
	w, err := Open(t.TempDir()+"/wal.ndjson", 1)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 4096; i++ {
			w.Append(Entry{Role: "user", Content: "filler"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Append blocked on a full buffer")
	}
	w.Close()
}

// Drain atomically empties the file and returns the turns.
func TestDrainRotatesFile(t *testing.T) {
	path := t.TempDir() + "/wal.ndjson"
	w, _ := Open(path, 8)
	w.Append(Entry{SessionID: "s1", Role: "user", Content: "turn one"})
	w.Append(Entry{SessionID: "s1", Role: "user", Content: "turn two"})
	w.Close()

	turns, err := w.Drain()
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Content != "turn one" {
		t.Fatalf("drain got %+v", turns)
	}
	if _, err := os.Stat(path + ".draining"); !os.IsNotExist(err) {
		t.Fatal("drain must clean up its rotate file")
	}
	if more, _ := w.Drain(); len(more) != 0 {
		t.Fatalf("drain must empty the file, got %d", len(more))
	}
	// the file still exists for future appends
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("drain must recreate the wal: %v", err)
	}
	// Drain on a missing file is a no-op
	w2, _ := Open(t.TempDir()+"/missing.ndjson", 8)
	if turns, err := w2.Drain(); err != nil || turns != nil {
		t.Fatalf("drain on missing file: %v %v", turns, err)
	}
}
