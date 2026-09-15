# Veda Roadmap

> **Veda — the knowledge of you, owned by you, readable by every agent.**
> Local capture + recall is free and open source, forever. Paid features sit
> on top of sync and evaluation, never on top of your own memories.

Status: ✅ shipped · 🚧 in progress · 📋 planned · 🔭 exploratory

## v0.1.x — MVP hardening ✅

One local memory file. Every MCP agent reads and writes it.

- [x] `veda init` → `~/.veda/` (SQLite + FTS5, config.toml)
- [x] MCP server over stdio: `remember` `recall` `list` `forget` `export`
- [x] Write-ahead log (append-only NDJSON, zero blocking on MCP responses)
- [x] Cheap gate: regex + length + role heuristics, no LLM
- [x] Async worker (15 min): batch candidates → summarize with *your* LLM key
- [x] Local review UI on `127.0.0.1:7331` (Pending / All / Audit / Digest)
- [x] Telemetry subcommands, **default off**, counts-only, GDPR erasure
- [x] JSON export / import
- [ ] Homebrew tap + install script *(weeks 8–9 of the launch plan)*
- [ ] Launch: HN / X / r/LocalLLaMA / MCP Discord

## v0.2.0 — Semantic recall ✅

- [x] Vector search alongside FTS5 (embeddings via any OpenAI-compatible `/embeddings` endpoint; Ollama/LM Studio on localhost work unchanged)
- [x] Hybrid ranker: weighted RRF, FTS5 bm25 (0.6) ⊕ cosine (0.4) — exact keyword matches keep their top spot
- [x] `recall` gains `semantic: true`; default stays hybrid
- [x] Graceful degradation to keyword-only when the embedding provider fails
- [x] Lazy vector backfill by the worker — v0.1 databases upgrade with no migration
- [ ] Eval scenario drafts from every recall failure reported at launch *(ongoing)*

Details: [RELEASE/v0.2.0.md](RELEASE/v0.2.0.md) · design in [DESIGN/10](DESIGN/10_design_semantic_recall.md)

## v0.3.0 — Cross-agent conflict resolution 📋

- [ ] Contradiction detection on write (supersede, don't duplicate)
- [ ] Recall returns the current side of a conflict by default
- [ ] UI conflicts view: keep-both / prefer-new / prefer-old

Details: [RELEASE/v0.3.0.md](RELEASE/v0.3.0.md)

## v0.4.0 — Hosted sync (first paid tier) 💰

- [ ] End-to-end encrypted cross-device sync (ciphertext-only server)
- [ ] Tombstone-based deletes, last-writer-wins per memory
- [ ] `veda sync status / push / pull`
- [ ] Pricing: $5–15/mo (local features stay free forever)

Details: [RELEASE/v0.4.0.md](RELEASE/v0.4.0.md)

## v0.5.0 — Advanced eval harness (paid) 💰

The moat. Cross-install recall benchmarks, interference detection,
faithfulness scoring of distilled memories.

- [ ] Scenario format: fixture conversations + expected recalls
- [ ] `veda eval` runs a suite against any config and scores it
- [ ] Paid tier: cross-install benchmarks, interference detection

## v0.6.0 — Team compliance / audit export (paid) 💰

- [ ] Signed audit-log export for compliance regimes
- [ ] Org memory policies (retention, redaction rules)

## v1.0 — UOMP protocol 📋

Veda is the product; **UOMP** (User-Owned Personal Memory Protocol) is the
open spec — like Chrome vs HTTP. v1.0 freezes the protocol: the tool
contract, the on-disk schema, the export format, and the sync wire format,
with a second implementation as the compatibility proof.

## Success metrics (90 days, from the PRD)

| Metric | Target |
|---|---|
| GitHub stars | 500 |
| Weekly active installs | 100 |
| Users who say "can't go back" | 10 |
| Paying users | 0 (paid features start v0.4) |
