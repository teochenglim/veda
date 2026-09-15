# DESIGN 10 — Semantic Recall (v0.2)

## Storage

```sql
CREATE TABLE memories_vec (
  memory_id TEXT PRIMARY KEY, vec BLOB NOT NULL, dims INTEGER NOT NULL
);
```

Vectors are added lazily: `SetEmbedding` upserts JSON-encoded `[]float32`,
the worker's `backfillVectors` embeds everything missing on each pass. A
v0.1 database therefore upgrades with zero migration — first `veda worker`
(or the next 15-min serve tick) backfills. Vectors are not exported
(`veda export` stays the portable, human-readable truth); they are
recreatable, derived data.

## Client and boundary

`internal/embed.Client` POSTs `{model, input}` to `<base_url>/embeddings`
and nothing else — one destination, one call shape, key optional for local
servers. Privacy follows from structure, not promises: there is no code path
that can send text anywhere except the user-configured BaseURL, and
`[embed] enabled` defaults to `false`.

## Ranking: weighted RRF

Both paths produce a ranked list per query; fusion is weighted reciprocal
rank fusion:

```
score(m) = 0.6/(60 + rank_fts(m)) + 0.4/(60 + rank_sem(m))
```

A memory absent from a list contributes 0 for that term. The FTS weight is
deliberately higher: a user's exact keyword hit must never be demoted by a
semantic neighbor (AC2), while semantic-only hits still surface (AC1) and
hybrid re-orders everything below rank 1. The FTS pool runs 4× the limit so
fusion has candidates to work with.

`semantic: true` skips fusion and returns the pure cosine ordering; with no
vectors yet it falls back to keyword results.

## Degradation ladder (never an error)

1. embedder disabled/nil → keyword-only
2. embedding request fails → keyword-only (same response shape)
3. no vectors in the table → keyword-only
4. everything healthy → hybrid fusion

The MCP `recall` tool never surfaces an embedding problem to the agent;
recalls are audited with hit/miss regardless of which path produced them.

## Cost control

Backfill chunks at 32 memories per pass; recall embeds only the query (one
call). At single-user scale (thousands of memories) `loadVectors` (all live
id+vec rows) is sub-millisecond in SQLite; if it ever isn't, the fix is an
in-memory vector cache, not a vector database — same one-binary constraint
as ever.
