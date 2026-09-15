package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/teochenglim/veda/internal/gate"
	"github.com/teochenglim/veda/internal/llm"
	"github.com/teochenglim/veda/internal/store"
)

// Options configures one run. Embedder enables semantic cases; Distiller
// enables turn-based seeding through the real gate + distillation path.
type Options struct {
	Embedder  store.Embedder
	Distiller *llm.Client
}

// CaseResult is one recall expectation, scored.
type CaseResult struct {
	Name     string   `json:"name"`
	Query    string   `json:"query"`
	Passed   bool     `json:"passed"`
	Returned []string `json:"returned"`
	Failures []string `json:"failures,omitempty"`
}

// InterferenceResult holds guard pass-rates before/after disruption.
type InterferenceResult struct {
	Before float64      `json:"before"`
	After  float64      `json:"after"`
	Drop   float64      `json:"drop"`
	Passed bool         `json:"passed"`
	Guard  []CaseResult `json:"guard"`
}

// FaithItem is one memory's attribution score against its source turns.
type FaithItem struct {
	Content string  `json:"content"`
	Score   float64 `json:"score"`
	Flagged bool    `json:"flagged"`
}

// FaithfulnessResult aggregates attribution over seeded/distilled memories.
type FaithfulnessResult struct {
	AvgScore float64     `json:"avg_score"`
	Flagged  int         `json:"flagged"`
	Items    []FaithItem `json:"items"`
}

// Report is the machine-readable result for one scenario.
type Report struct {
	Name         string              `json:"name"`
	Description  string              `json:"description,omitempty"`
	Passed       bool                `json:"passed"`
	Score        float64             `json:"score"` // recall case pass rate 0..1
	Cases        []CaseResult        `json:"cases"`
	Interference *InterferenceResult `json:"interference,omitempty"`
	Faithfulness *FaithfulnessResult `json:"faithfulness,omitempty"`
}

// Run scores one scenario against a throwaway store. The caller's store is
// never touched: the runner creates its own store under a fresh temp dir.
func Run(sc Scenario, opts Options) (*Report, error) {
	dir, err := os.MkdirTemp("", "veda-eval-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	s, err := store.Open(filepath.Join(dir, "veda.db"))
	if err != nil {
		return nil, err
	}
	defer s.Close()
	ctx := context.Background()

	// seed: pre-distilled memories
	var distilled []*store.Memory
	for _, m := range sc.Setup.Memories {
		conf, sal := m.Confidence, m.Salience
		if conf == 0 {
			conf = 1.0
		}
		if sal == 0 {
			sal = 0.5
		}
		if _, err := s.Remember(&store.Memory{
			Content: m.Content, Type: m.Type, Confidence: conf, Salience: sal,
		}, "eval"); err != nil {
			return nil, fmt.Errorf("seed memory: %w", err)
		}
	}

	// seed: raw turns through the real gate; distill when an LLM is wired
	if len(sc.Setup.Turns) > 0 {
		var candidates []string
		for _, t := range sc.Setup.Turns {
			s.InsertTurns([]store.Turn{{SessionID: "eval", Role: t.Role, Content: t.Content}})
			if r := gate.Check(t.Role, t.Content); r.Candidate {
				candidates = append(candidates, t.Role+": "+t.Content)
			}
		}
		if opts.Distiller != nil {
			facts, err := opts.Distiller.Distill(ctx, candidates)
			if err != nil {
				return nil, fmt.Errorf("distill fixture turns: %w", err)
			}
			for _, f := range facts {
				id, err := s.Remember(&store.Memory{
					Content: f.Content, Type: f.Type,
					Confidence: f.Confidence, Salience: f.Salience,
				}, "eval-worker")
				if err != nil {
					return nil, err
				}
				if m, _ := s.Get(id); m != nil {
					distilled = append(distilled, m)
				}
			}
		}
	}

	// embed seeds when a semantic run needs them
	if opts.Embedder != nil {
		mems, _ := s.MissingEmbeddings(0)
		for i := 0; i < len(mems); i += 16 {
			end := i + 16
			if end > len(mems) {
				end = len(mems)
			}
			texts := make([]string, 0, end-i)
			for _, m := range mems[i:end] {
				texts = append(texts, m.Type+": "+m.Content)
			}
			vecs, err := opts.Embedder.Embed(ctx, texts)
			if err != nil {
				return nil, fmt.Errorf("embed seeds: %w", err)
			}
			for j, m := range mems[i:end] {
				if j < len(vecs) && len(vecs[j]) > 0 {
					s.SetEmbedding(m.ID, vecs[j])
				}
			}
		}
	}

	// recall cases
	rep := &Report{Name: sc.Name, Description: sc.Description}
	rep.Cases = runCases(ctx, s, sc.Cases, opts)
	var passed int
	for _, c := range rep.Cases {
		if c.Passed {
			passed++
		}
	}
	rep.Score = rate(passed, len(rep.Cases))
	rep.Passed = passed == len(rep.Cases)

	// faithfulness: attribute distilled (or seeded) memories to fixture turns
	if len(sc.Setup.Turns) > 0 {
		items := distilled
		if items == nil {
			for _, m := range sc.Setup.Memories {
				items = append(items, &store.Memory{Content: m.Content})
			}
		}
		rep.Faithfulness = faithfulness(items, sc.Setup.Turns)
		if rep.Faithfulness.Flagged > 0 {
			rep.Passed = false
		}
	}

	// interference: guard cases must survive the disruptor setup
	if sc.Interference != nil {
		before := runCases(ctx, s, sc.Interference.Guard, opts)
		beforeRate := passRate(before)
		for _, m := range sc.Interference.Disrupt.Memories {
			conf, sal := m.Confidence, m.Salience
			if conf == 0 {
				conf = 1.0
			}
			if sal == 0 {
				sal = 0.5
			}
			s.Remember(&store.Memory{Content: m.Content, Type: m.Type, Confidence: conf, Salience: sal}, "eval-disrupt")
		}
		after := runCases(ctx, s, sc.Interference.Guard, opts)
		afterRate := passRate(after)
		maxDrop := sc.Interference.MaxDrop
		drop := beforeRate - afterRate
		rep.Interference = &InterferenceResult{
			Before: beforeRate, After: afterRate, Drop: drop,
			Passed: drop <= maxDrop, Guard: after,
		}
		if !rep.Interference.Passed {
			rep.Passed = false
		}
	}
	return rep, nil
}

func runCases(ctx context.Context, s *store.Store, cases []Case, opts Options) []CaseResult {
	out := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		topK := c.TopK
		if topK <= 0 {
			topK = 5
		}
		minConf := c.MinConfidence
		if minConf <= 0 {
			minConf = 0.5
		}
		mems, err := s.RecallHybrid(ctx, c.Query, topK, minConf, opts.Embedder, c.Semantic)
		res := CaseResult{Name: c.Name, Query: c.Query}
		if err != nil {
			res.Failures = append(res.Failures, "recall error: "+err.Error())
			res.Passed = false
			out = append(out, res)
			continue
		}
		for _, m := range mems {
			res.Returned = append(res.Returned, m.Content)
		}
		joined := strings.ToLower(strings.Join(res.Returned, "\n"))
		for _, want := range c.ExpectContains {
			if !strings.Contains(joined, strings.ToLower(want)) {
				res.Failures = append(res.Failures, fmt.Sprintf("expected %q in top-%d results", want, topK))
			}
		}
		for _, ban := range c.ExpectNotContains {
			if strings.Contains(joined, strings.ToLower(ban)) {
				res.Failures = append(res.Failures, fmt.Sprintf("forbidden %q appeared in results", ban))
			}
		}
		res.Passed = len(res.Failures) == 0
		out = append(out, res)
	}
	return out
}

func passRate(rs []CaseResult) float64 {
	passed := 0
	for _, r := range rs {
		if r.Passed {
			passed++
		}
	}
	return rate(passed, len(rs))
}

func rate(passed, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(passed) / float64(total)
}

// faithfulness scores each memory against the fixture turns: the max share
// of the memory's significant tokens found in any single turn. Low scores
// mean the memory says something no turn said — an invention.
func faithfulness(mems []*store.Memory, turns []Turn) *FaithfulnessResult {
	res := &FaithfulnessResult{}
	var total float64
	for _, m := range mems {
		score := maxAttribution(m.Content, turns)
		flagged := score < FaithThreshold
		res.Items = append(res.Items, FaithItem{Content: m.Content, Score: round2(score), Flagged: flagged})
		if flagged {
			res.Flagged++
		}
		total += score
	}
	if len(mems) > 0 {
		res.AvgScore = round2(total / float64(len(mems)))
	}
	return res
}

func maxAttribution(content string, turns []Turn) float64 {
	toks := significant(content)
	if len(toks) == 0 {
		return 1 // nothing attributable either way; don't punish
	}
	best := 0.0
	for _, t := range turns {
		shared := 0
		for tok := range significant(t.Content) {
			if toks[tok] {
				shared++
			}
		}
		if r := float64(shared) / float64(len(toks)); r > best {
			best = r
		}
	}
	return best
}

var stopwords = map[string]bool{
	"user": true, "they": true, "their": true, "them": true, "with": true,
	"that": true, "this": true, "from": true, "have": true, "has": true,
	"was": true, "were": true, "because": true, "the": true, "and": true,
	"for": true, "are": true,
}

func significant(text string) map[string]bool {
	out := map[string]bool{}
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
			continue
		}
		addToken(&out, &b)
	}
	addToken(&out, &b)
	return out
}

func addToken(out *map[string]bool, b *strings.Builder) {
	if b.Len() >= 4 {
		tok := b.String()
		if !stopwords[tok] {
			(*out)[tok] = true
		}
	}
	b.Reset()
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
