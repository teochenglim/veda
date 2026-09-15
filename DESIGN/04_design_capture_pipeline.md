# DESIGN 04 — Capture Pipeline (WAL, Gate, Worker)

## Flow

```
agent writes ──► wal.Append (channel, non-blocking) ──► wal.ndjson (NDJSON, 0600)
                                                            │
worker (every 15 min or `veda worker`)                      ▼
  1. Drain: read all entries, rotate the file atomically
     (rename → recreate; crash between the two loses nothing
     because the renamed file is only deleted after commit)
  2. Insert drained turns into `turns` (full history, unfiltered)
  3. Gate each turn; candidates land in `pending_turns`
  4. TakePending(batch=20): atomic fetch-and-delete
  5. llm.Distill: one chat-completions call per batch, temperature 0,
     system prompt demands a JSON array of {type, content, confidence, salience}
  6. Each fact → store.Remember(actor="worker") → audited
```

## Cheap gate (AC4)

Deliberately dumb, deliberately biased to over-include:

- **Role**: only `user` / `assistant` turns qualify.
- **Length**: 16–2000 bytes. Below: nothing to distill. Above: a paste, not a
  fact.
- **Secret veto**: `api_key|secret|password|token|bearer|PRIVATE KEY` — a
  turn like "my password is X" must never become a durable memory. This is a
  hard reject, before any positive signal.
- **Signals**: first-person durable-fact phrasings ("I prefer…", "my daughter…",
  "remember that…", "I use…", third-person "the user…" from assistants). Each
  match adds 0.25; score capped at 1; zero matches ⇒ reject.

The gate costs microseconds and calls nothing remote. Precision is not the
goal — the Pending review queue is.

## LLM contract

The distillation prompt returns only JSON; the parser tolerates markdown
fences and treats a non-array as "nothing qualifies" (empty result, not an
error). The user's endpoint is any OpenAI-compatible `/chat/completions`
(`base_url` configurable ⇒ works with OpenAI, Azure-shaped gateways, Ollama,
LM Studio). The key is resolved from an environment variable named in
`config.toml` — never stored on disk, never logged.

Without a key configured, step 4–6 are skipped: nothing leaves the machine,
and the pending queue grows until reviewed in the UI.

## Why capture is not "zero data loss"

`wal.Append` drops entries on a full buffer (1024 entries deep) rather than
blocking the MCP response. This is a deliberate inversion of a classic WAL:
capture is best-effort durability for *conversation turns*; *memories* — the
data that matters — go through SQLite transactions, which are never dropped.
