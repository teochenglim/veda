# DESIGN 03 — Storage

## Schema (PRD-verbatim, plus one addition)

The five PRD tables are created exactly as specified (`sessions`, `turns`,
`memories`, `memories_fts`, `audit`, `telemetry_queue`), with one addition:

```sql
CREATE TABLE pending_turns (
  id INTEGER PRIMARY KEY, session_id TEXT,
  role TEXT, content TEXT, ts INTEGER, gate_score REAL
);
```

The PRD's UI scope requires a Pending tab and the worker scope requires a
batching queue; deriving "pending" from `turns` via a flag column would
couple capture state to history. A dedicated table keeps `TakePending`
(atomic fetch-and-delete) a single statement.

## FTS5 as external content

`memories_fts` is an external-content table (`content='memories'`) kept in
sync by `AFTER INSERT / UPDATE / DELETE` triggers on `memories`. Soft delete
(`deleted = 1`) stays out of FTS — deleted rows remain in the index but every
recall path filters `deleted = 0`.

## Query strategy (recall)

1. User input is rewritten to a safe FTS5 expression: whitespace-split terms,
   FTS syntax characters stripped, each term double-quoted, ANDed together.
   User input can never inject query syntax.
2. Ranking: `bm25(memories_fts)` then `salience DESC`.
3. Filters enforced at the SQL layer: `deleted = 0`, `confidence >= min`,
   `ttl = 0 OR ttl > now`.
4. FTS5's unicode61 tokenizer does not stem; punctuation-heavy queries may
   match zero rows. A `LIKE '%q%'` fallback with identical filters runs when
   the FTS path returns nothing, so recall degrades gracefully instead of
   returning empty (AC6 test covers this).

## TTL and soft delete

`ttl` is an absolute unix deadline (0 = immortal); expiry is evaluated at
read time, no sweeper needed. `forget` sets `deleted = 1` and writes an audit
row — "delete anytime" must be verifiable, so deletion itself is a first-class
record. `veda export` exports only live memories; a forgotten memory is
invisible to every tool including export.

## Data volumes

Single user, one machine: even an aggressive year of daily use is thousands
of memories and tens of thousands of turns — far inside SQLite's comfort
zone. No sharding, pruning, or vacuum strategy is needed for MVP; `turns`
can be truncated manually and is not surfaced in the UI.
