// Package worker is Veda's async pipeline: drain the WAL into SQLite, run
// the cheap gate over new turns, and every interval distill pending
// candidates into memories using the user's own LLM key.
package worker

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/teochenglim/veda/internal/gate"
	"github.com/teochenglim/veda/internal/llm"
	"github.com/teochenglim/veda/internal/store"
	"github.com/teochenglim/veda/internal/wal"
)

// Worker owns the background pipeline.
type Worker struct {
	Store    *store.Store
	WAL      *wal.WAL
	LLM      *llm.Client    // nil => drain+gate only, no summarization
	Embedder store.Embedder // nil => keyword-only recall; vectors backfilled when set
	Interval time.Duration
	Batch    int

	// v0.5 telemetry hooks (nil-safe): gate rejections and allowlisted errors.
	OnGateRejected func(n int64)
	OnError        func(code string)

	mu      sync.Mutex
	lastRun time.Time
}

func (w *Worker) err(code string) {
	if w.OnError != nil {
		w.OnError(code)
	}
}

func (w *Worker) gateRejected(n int64) {
	if w.OnGateRejected != nil {
		w.OnGateRejected(n)
	}
}

// RunOnce performs one full pass. It is safe to call concurrently with the
// MCP server: SQLite is opened with a single connection and busy timeout.
func (w *Worker) RunOnce(ctx context.Context) error {
	turns, err := w.WAL.Drain()
	if err != nil {
		w.err("wal_fail")
		return err
	}
	if err := w.Store.InsertTurns(turns); err != nil {
		return err
	}
	var pending []store.PendingTurn
	gated := int64(0)
	for _, t := range turns {
		if r := gate.Check(t.Role, t.Content); r.Candidate {
			pending = append(pending, store.PendingTurn{
				SessionID: t.SessionID, Role: t.Role, Content: t.Content,
				TS: t.TS, GateScore: r.Score,
			})
		} else {
			gated++
		}
	}
	w.gateRejected(gated)
	if err := w.Store.InsertPending(pending); err != nil {
		return err
	}
	if err := w.summarize(ctx); err != nil {
		return err
	}
	if err := w.backfillVectors(ctx); err != nil {
		log.Printf("veda worker: embed backfill: %v", err)
	}
	w.mu.Lock()
	w.lastRun = time.Now()
	w.mu.Unlock()
	return nil
}

// backfillVectors embeds live memories that have no vector yet (v0.1→v0.2
// migration and anything written since the last pass). Best-effort: an
// embedder failure leaves the memories keyword-only until the next pass.
func (w *Worker) backfillVectors(ctx context.Context) error {
	if w.Embedder == nil {
		return nil
	}
	const chunk = 32
	for {
		memories, err := w.Store.MissingEmbeddings(chunk)
		if err != nil || len(memories) == 0 {
			return err
		}
		texts := make([]string, len(memories))
		for i, m := range memories {
			texts[i] = m.Type + ": " + m.Content
		}
		vecs, err := w.Embedder.Embed(ctx, texts)
		if err != nil {
			w.err("embed_fail")
			return err
		}
		for i, m := range memories {
			if i >= len(vecs) || len(vecs[i]) == 0 {
				continue
			}
			if err := w.Store.SetEmbedding(m.ID, vecs[i]); err != nil {
				return err
			}
		}
		if len(memories) < chunk {
			return nil
		}
	}
}

// summarize takes one batch of pending turns and promotes distilled facts to
// memories. Without an LLM client it is a no-op: candidates stay pending for
// human review in the UI.
func (w *Worker) summarize(ctx context.Context) error {
	if w.LLM == nil {
		return nil
	}
	batch := w.Batch
	if batch <= 0 {
		batch = 20
	}
	turns, err := w.Store.TakePending(batch)
	if err != nil {
		return err
	}
	if len(turns) == 0 {
		return nil
	}
	texts := make([]string, len(turns))
	for i, t := range turns {
		texts[i] = t.Role + ": " + t.Content
	}
	facts, err := w.LLM.Distill(ctx, texts)
	if err != nil {
		w.err("llm_fail")
		// Put the batch back so nothing is lost; worker retries next tick.
		var retry []store.PendingTurn
		for _, t := range turns {
			retry = append(retry, store.PendingTurn{
				SessionID: t.SessionID, Role: t.Role, Content: t.Content,
				TS: t.TS, GateScore: t.GateScore,
			})
		}
		if ierr := w.Store.InsertPending(retry); ierr != nil {
			log.Printf("veda: lost pending batch after llm error: %v", ierr)
		}
		return err
	}
	for _, f := range facts {
		_, err := w.Store.Remember(&store.Memory{
			Type: f.Type, Content: f.Content,
			Confidence: f.Confidence, Salience: f.Salience,
			AgentID: "worker",
		}, "worker")
		if err != nil {
			return err
		}
	}
	return nil
}

// LastRun reports when RunOnce last completed.
func (w *Worker) LastRun() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastRun
}

// Run loops until ctx is cancelled, executing a pass every Interval.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.RunOnce(ctx); err != nil {
				log.Printf("veda worker: %v", err)
			}
		}
	}
}
