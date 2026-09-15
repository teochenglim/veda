# DESIGN 01 — Design Overview

Veda is a single-user, local-first personal memory store exposed to AI agents
over MCP. The product thesis constrains the design more than any technical
requirement does:

1. **Ownership is non-negotiable.** Memories live in `~/.veda/veda.db`
   (SQLite). No hosted component is required for any core feature. If Veda
   the service disappeared tomorrow, every memory stays readable.
2. **Consent is explicit or nothing.** Telemetry defaults to off; there is no
   auto-consent path anywhere in the codebase. The first-run prompt writes a
   boolean to `config.toml`, and `veda telemetry preview` prints the exact
   payload before anything is ever enabled.
3. **The human is the quality loop.** Capture is cheap and over-inclusive;
   distillation is conservative; review is one local UI. We do not try to
   make the LLM "smart enough" to skip the human.
4. **One static binary.** Pure-Go SQLite (`modernc.org/sqlite`, FTS5
   included), embedded UI assets (`embed`), official MCP Go SDK. No CGO, no
   runtime deps, trivial cross-compilation for 6 platform targets.

## Component map

| Component | Package | Responsibility |
|---|---|---|
| CLI | `cmd/veda` | init, serve, ui, worker, export/import, telemetry |
| Persistence | `internal/store` | PRD schema, FTS5 recall, audit, pending queue, export |
| Write-ahead log | `internal/wal` | append-only NDJSON, non-blocking append |
| Cheap gate | `internal/gate` | regex + length + role candidate filter (no LLM) |
| Async worker | `internal/worker` | drain → gate → batch → distill loop |
| LLM client | `internal/llm` | OpenAI-compatible chat-completions, JSON fact parsing |
| MCP server | `internal/mcpserver` | the five spec tools over stdio |
| Review UI | `internal/ui` | embedded SPA + JSON API on 127.0.0.1:7331 |
| Telemetry | `internal/telemetry` | opt-in counts-only queue/flush/erase |
| Config | `internal/config` | `~/.veda/config.toml`, `VEDA_HOME` override |

Document map: architecture (02), storage (03), capture pipeline (04),
review UI (05), MCP layer (06), telemetry & privacy (07), testing/AC map (08),
tech stack & roadmap (09).
