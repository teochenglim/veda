# DESIGN 02 — Architecture

## Process shape

`veda serve --stdio` is the primary long-running process. It hosts three
concerns in one binary because they share one SQLite handle and one user:

```
                 ┌────────────────────────────────────────────┐
 stdio (MCP) ───►│ mcpserver ──► store ──► ~/.veda/veda.db    │
                 │     │                        ▲             │
                 │ worker goroutine ───────────┘             │
                 │  (every 15 min: wal.Drain → gate →        │
                 │   TakePending → llm.Distill → Remember)   │
                 └────────────────────────────────────────────┘
```

`veda ui` is a separate short-lived process over the same DB (SQLite WAL
journal mode allows a reader alongside `serve`). `veda worker` runs one
pipeline pass and exits — for users who invoke Veda from cron instead of
keeping `serve` alive between sessions.

## Concurrency model

- `store.Open` sets `SetMaxOpenConns(1)`: a single serialized connection with
  a 5 s busy timeout. This converts every potential `SQLITE_BUSY` between the
  MCP handlers, the worker and the UI process into queueing instead of
  errors. Local single-user write volume is tiny; throughput is irrelevant
  compared to determinism.
- The MCP Go SDK may run tool calls concurrently. Each call is a short
  transaction; clients that pipeline requests must not assume cross-request
  ordering (standard clients are request/response sequential).
- `wal.Append` hands the entry to a buffered channel and returns — disk I/O
  happens on a writer goroutine. If the buffer is full the entry is dropped:
  the WAL is a durability optimization for capture, never a blocker of the
  MCP response (AC3).

## Failure philosophy

- **Never lose user data on error.** Worker: if distillation fails, the
  taken batch is put back into `pending_turns` before the error propagates.
- **Never block on optional features.** No LLM key ⇒ the worker only drains
  and gates; candidates wait for human review. Telemetry off (default) ⇒ the
  flusher goroutine is never started.
- **Fail loud on setup.** Every subcommand refuses to run (exit 1) against a
  missing `veda.db` with the hint `run veda init first`.
