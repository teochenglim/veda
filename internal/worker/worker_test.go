package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teochenglim/veda/internal/llm"
	"github.com/teochenglim/veda/internal/store"
	"github.com/teochenglim/veda/internal/wal"
)

func setup(t *testing.T) (*store.Store, *wal.WAL) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(dir + "/veda.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	w, err := wal.Open(dir+"/wal.ndjson", 16)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return s, w
}

// AC5 (no-key path): the worker drains the WAL into turns, gates candidates
// into the pending queue, and never calls an LLM when no key is configured —
// candidates stay pending for human review.
func TestAC5_WorkerWithoutLLMKeepsPending(t *testing.T) {
	s, w := setup(t)
	w.Append(wal.Entry{SessionID: "s1", Role: "user", Content: "I prefer window seats on every flight"})
	w.Append(wal.Entry{SessionID: "s1", Role: "user", Content: "ok cool"}) // gated out: too short / no signal
	w.Close()                                                              // flush pending appends before draining (Append is async)
	wk := &Worker{Store: s, WAL: w, Batch: 20}
	if err := wk.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending, err := s.ListPending(0)
	if err != nil || len(pending) != 1 {
		t.Fatalf("gated candidate must be pending, got %d (%v)", len(pending), err)
	}
	if pending[0].Content != "I prefer window seats on every flight" {
		t.Fatalf("wrong pending content: %q", pending[0].Content)
	}
	mems, _ := s.List("", "", time.Time{}, time.Time{}, 0, false)
	if len(mems) != 0 {
		t.Fatalf("no memories may be created without an LLM key, got %d", len(mems))
	}
	// the drained turn is persisted
	st, _ := s.CollectStats()
	if st.Turns != 2 {
		t.Fatalf("both turns must be persisted, got %d", st.Turns)
	}
}

// AC5 (LLM path): pending candidates are batched and distilled into memories.
func TestAC5_WorkerWithLLMCreatesMemories(t *testing.T) {
	s, w := setup(t)
	w.Append(wal.Entry{SessionID: "s1", Role: "user", Content: "I prefer window seats on every flight I take"})
	w.Append(wal.Entry{SessionID: "s1", Role: "user", Content: "Remember that my daughter Ada is six years old"})
	w.Close() // flush pending appends before draining (Append is async)

	var gotPrompt string
	llmSrv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		b, _ := json.Marshal(req["messages"])
		gotPrompt = string(b)
		json.NewEncoder(rw).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]string{
					"role": "assistant",
					// includes markdown fences on purpose: the parser must tolerate them
					"content": "```json\n[{\"type\":\"preference\",\"content\":\"The user prefers window seats on flights\",\"confidence\":0.9,\"salience\":0.7},{\"type\":\"identity\",\"content\":\"The user has a six-year-old daughter named Ada\",\"confidence\":0.95,\"salience\":0.9}]\n```",
				},
			}},
		})
	}))
	defer llmSrv.Close()

	lc := llm.New(llmSrv.URL, "test-key", "test-model")
	wk := &Worker{Store: s, WAL: w, LLM: lc, Batch: 20}
	if err := wk.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	mems, err := s.List("", "", time.Time{}, time.Time{}, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 2 {
		t.Fatalf("expected 2 distilled memories, got %d", len(mems))
	}
	if !strings.Contains(gotPrompt, "window seats") {
		t.Fatal("batched prompt must contain the pending turns")
	}
	if left, _ := s.ListPending(0); len(left) != 0 {
		t.Fatalf("summarized candidates must leave the pending queue, %d left", len(left))
	}
	// audit trail: worker-created memories say so
	audit, _ := s.ListAudit(0)
	sawWorkerCreate := false
	for _, a := range audit {
		if a.Action == "create" && a.Actor == "worker" {
			sawWorkerCreate = true
		}
	}
	if !sawWorkerCreate {
		t.Fatal("worker memory creation must be audited")
	}
}

// AC5 (resilience): an LLM failure puts the batch back in the queue.
func TestAC5_WorkerRetriesAfterLLMFailure(t *testing.T) {
	s, w := setup(t)
	w.Append(wal.Entry{SessionID: "s1", Role: "user", Content: "I prefer aisle seats on night flights"})
	w.Close() // flush pending appends before draining (Append is async)
	llmSrv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Error(rw, "boom", http.StatusInternalServerError)
	}))
	defer llmSrv.Close()

	wk := &Worker{Store: s, WAL: w, LLM: llm.New(llmSrv.URL, "k", "m"), Batch: 20}
	if err := wk.RunOnce(context.Background()); err == nil {
		t.Fatal("expected the llm error to propagate")
	}
	pending, _ := s.ListPending(0)
	if len(pending) != 1 {
		t.Fatalf("failed batch must be requeued, %d pending", len(pending))
	}
}

// fake embedder for backfill tests: deterministic vector per text.
type backfillEmbedder struct{ err error }

func (b backfillEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if b.err != nil {
		return nil, b.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(i + 1), 0.5}
	}
	return out, nil
}

// v0.2: the worker backfills embeddings for memories missing vectors.
func TestWorkerBackfillsEmbeddings(t *testing.T) {
	s, w := setup(t)
	s.Remember(&store.Memory{Content: "User prefers window seats on every flight"}, "mcp")
	s.Remember(&store.Memory{Content: "User's daughter Ada is six years old"}, "mcp")

	wk := &Worker{Store: s, WAL: w, Embedder: backfillEmbedder{}, Batch: 20}
	if err := wk.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountEmbedded(); n != 2 {
		t.Fatalf("expected both memories embedded, got %d", n)
	}
	// a second pass is a no-op (nothing left to backfill)
	if err := wk.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountEmbedded(); n != 2 {
		t.Fatalf("backfill must be idempotent, got %d", n)
	}
}

// v0.2: an embedder failure during backfill must not fail the whole pass.
func TestWorkerBackfillEmbedderFailureIsBestEffort(t *testing.T) {
	s, w := setup(t)
	s.Remember(&store.Memory{Content: "User prefers window seats on every flight"}, "mcp")

	wk := &Worker{Store: s, WAL: w, Embedder: backfillEmbedder{err: errors.New("down")}, Batch: 20}
	if err := wk.RunOnce(context.Background()); err != nil {
		t.Fatalf("backfill failure must be swallowed: %v", err)
	}
	if n, _ := s.CountEmbedded(); n != 0 {
		t.Fatalf("no vectors should exist, got %d", n)
	}
}
