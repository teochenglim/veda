# DESIGN 13 — Advanced Eval Harness (v0.6)

## Purpose

Recall quality is Veda's product; the eval harness is how it is measured.
Every launch-post failure report ("break it, tell me where recall fails")
becomes a fixture scenario, so a fixed regression can never silently
return. It is also the paid moat: cross-install benchmarks rank recall
across installs.

## Isolation guarantee

`eval.Run` creates its own store under a fresh temp dir and never touches
`VEDA_HOME` — the user's memories cannot leak into a benchmark, and a
benchmark cannot damage real data (AC5 is a test: seed the real home, run
eval, assert it is byte-for-byte unchanged).

## Scenario format

```json
{
  "name": "residence-move",
  "setup": {
    "memories": [{"content": "User lives in Tokyo", "type": "fact"}],
    "turns": [{"role": "user", "content": "I moved to Tokyo in April"}]
  },
  "cases": [
    {"query": "lives Tokyo", "top_k": 3, "expect_contains": ["Tokyo"],
     "expect_not_contains": ["Singapore"]}
  ],
  "interference": {
    "disrupt": {"memories": [{"content": "…", "salience": 1.0}]},
    "guard": [{"query": "…", "expect_contains": ["…"]}],
    "max_drop": 0.0
  }
}
```

A file is either one scenario or `{"scenarios": [...]}`. Malformed files
fail with the scenario and case named (AC1).

## What is scored

- **Recall cases** run through the real `RecallHybrid` path (keyword,
  hybrid, or `semantic: true` with the configured embedder). A case passes
  when every `expect_contains` appears in the top-k results and no
  `expect_not_contains` does. Scenario score = pass rate.
- **Interference**: guard cases are scored before and after ingesting a
  disruptor setup; a pass-rate drop beyond `max_drop` (default 0) fails.
  This is the "learning B wiped out A" detector.
- **Faithfulness**: for each memory distilled from (or seeded beside)
  fixture turns, score = max share of the memory's significant tokens
  attributable to any single turn. Below 0.3 ⇒ flagged as an invention;
  any flag fails the scenario. Only meaningful when `turns` are present.

Fixture turns flow through the real gate; with the user's LLM configured
they distill through the real prompt path, so evals measure the actual
pipeline, not a parallel implementation.

## Paid tier: cross-install benchmarks

`veda eval --upload-url` posts **aggregates only** — scenario name, score,
pass flag, case count — never fixture text or memory content (asserted by
test). The endpoint's `402` maps to `ErrPaymentRequired`, the same
paid-gate pattern as sync. The aggregation backend ships in a separate
repository.

## Limits

- Faithfulness is lexical: a faithful paraphrase with disjoint vocabulary
  scores low. The LLM path mitigates this (its output is generated from the
  turns), but the score is a tripwire, not a proof.
- Scenario recall is deterministic per store state; embedding providers
  with non-deterministic outputs (none known today) would add noise.
