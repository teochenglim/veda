package store

import (
	"context"
	"errors"
	"testing"
)

// fakeEmbedder maps texts to hand-built vectors via a keyword → dimension
// mapping — enough to simulate a real model's paraphrase behavior in tests.
type fakeEmbedder struct {
	err error
}

func clusterVec(text string) []float32 {
	// one dimension per topic cluster, summed keyword hits — just enough
	// geometry to simulate a real embedding model's paraphrase behavior.
	vec := make([]float32, 3)
	keywords := [][]string{
		{"sleep", "insomnia", "caffeine", "jittery", "night"},
		{"coffee", "morning", "espresso"},
		{"run", "gym", "marathon", "exercise"},
	}
	for i, ks := range keywords {
		for _, k := range ks {
			if containsFold(text, k) {
				vec[i] += 1
			}
		}
	}
	return vec
}

func containsFold(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if eqFold(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

func eqFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 32
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func (f fakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = clusterVec(t)
	}
	return out, nil
}

func hybridSetup(t *testing.T) *Store {
	t.Helper()
	s := openTest(t)
	fixtures := []string{
		"User avoids caffeine after 2pm because it ruins their sleep", // sleep cluster
		"User drinks coffee every morning before work",                // coffee cluster
		"User trains for marathons and runs every weekend",            // exercise cluster
	}
	for _, c := range fixtures {
		if _, err := s.Remember(&Memory{Content: c, Type: "fact", Confidence: 0.9}, "mcp"); err != nil {
			t.Fatal(err)
		}
	}
	// embed all memories (as the worker backfill would)
	memories, err := s.MissingEmbeddings(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range memories {
		if err := s.SetEmbedding(m.ID, clusterVec(m.Content)); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// AC1: a semantic query that shares no keywords with a memory still recalls
// it via embeddings.
func TestAC1_SemanticParaphraseRecall(t *testing.T) {
	s := hybridSetup(t)
	// "things that keep me up at night" shares zero tokens with the
	// caffeine/sleep memory, but clusters with it semantically.
	query := "what keeps them awake at night"
	mems, err := s.RecallHybrid(context.Background(), query, 3, 0.5, fakeEmbedder{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) == 0 {
		t.Fatal("semantic recall returned nothing")
	}
	if !containsFold(mems[0].Content, "caffeine") {
		t.Fatalf("expected the sleep-cluster memory first, got %q", mems[0].Content)
	}
	// hybrid must also find it (FTS finds nothing for this query)
	mems, err = s.RecallHybrid(context.Background(), query, 3, 0.5, fakeEmbedder{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) == 0 || !containsFold(mems[0].Content, "caffeine") {
		t.Fatalf("hybrid recall missed the paraphrase: %v", mems)
	}
}

// AC2: the exact-keyword FTS winner keeps rank 1 in hybrid mode — semantic
// neighbors can be appended, never promoted above it.
func TestAC2_ExactKeywordStaysTop(t *testing.T) {
	s := hybridSetup(t)
	ftsOnly, err := s.Recall("caffeine", 5, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(ftsOnly) == 0 {
		t.Fatal("FTS should match the caffeine memory")
	}
	// weighted-RRF fusion (FTS weight 0.6 > semantic 0.4) must keep the
	// exact-keyword winner in rank 1 even though semantic re-ranks the rest.
	hybrid, err := s.RecallHybrid(context.Background(), "caffeine", 5, 0.5, fakeEmbedder{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if hybrid[0].ID != ftsOnly[0].ID {
		t.Fatalf("hybrid must keep the FTS winner on top: fts=%q hybrid=%q",
			ftsOnly[0].Content, hybrid[0].Content)
	}
}

// AC3: an embedding-provider failure degrades to keyword-only recall — the
// call succeeds, never errors.
func TestAC3_EmbedderFailureDegradesToFTS(t *testing.T) {
	s := hybridSetup(t)
	broken := fakeEmbedder{err: errors.New("provider down")}
	mems, err := s.RecallHybrid(context.Background(), "caffeine", 5, 0.5, broken, false)
	if err != nil {
		t.Fatalf("embedder failure must not surface as an error: %v", err)
	}
	if len(mems) == 0 || !containsFold(mems[0].Content, "caffeine") {
		t.Fatalf("degraded recall must equal FTS results, got %v", mems)
	}
	// and with no embedder configured at all
	mems, err = s.RecallHybrid(context.Background(), "caffeine", 5, 0.5, nil, false)
	if err != nil || len(mems) == 0 {
		t.Fatalf("nil embedder: %v %v", mems, err)
	}
}

func TestVectorBookkeeping(t *testing.T) {
	s := openTest(t)
	id1, _ := s.Remember(&Memory{Content: "memory without a vector yet"}, "mcp")
	id2, _ := s.Remember(&Memory{Content: "another memory without a vector"}, "mcp")

	missing, err := s.MissingEmbeddings(0)
	if err != nil || len(missing) != 2 {
		t.Fatalf("both memories should lack vectors: %d (%v)", len(missing), err)
	}
	if err := s.SetEmbedding(id1, []float32{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	// upsert overwrites, never duplicates
	if err := s.SetEmbedding(id1, []float32{4, 5, 6}); err != nil {
		t.Fatal(err)
	}
	missing, _ = s.MissingEmbeddings(0)
	if len(missing) != 1 || missing[0].ID != id2 {
		t.Fatalf("expected only id2 missing, got %d", len(missing))
	}
	n, err := s.CountEmbedded()
	if err != nil || n != 1 {
		t.Fatalf("CountEmbedded: %d (%v)", n, err)
	}
	// a forgotten memory's vector no longer counts
	s.Forget(id1, "test", "cleanup")
	if n, _ := s.CountEmbedded(); n != 0 {
		t.Fatalf("forgotten memory must not count as embedded: %d", n)
	}
	// stats include the embedded count
	st, _ := s.CollectStats()
	if st.Embedded != 0 || st.Memories != 1 {
		t.Fatalf("stats: %+v", st)
	}
}
