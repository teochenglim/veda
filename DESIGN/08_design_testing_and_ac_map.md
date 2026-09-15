# DESIGN 08 — Testing & AC Map

## Principle

Every acceptance criterion in `RELEASE/v0.1.0.md` is a named Go test. The
naming convention `TestAC<n>_<Behavior>` makes the release notes auditable:
grep the test tree and every claim has a witness. `make test` runs the whole
contract with the race detector and coverage.

## Map

| AC | Claim | Test |
|---|---|---|
| AC1 | init schema + telemetry default off | `internal/store.TestAC1_SchemaCreatedOnOpen`, `internal/config.TestAC1_TelemetryDefaultsOffAndConfigRoundTrips` |
| AC2 | five MCP tools, spec signatures, round trip | `internal/mcpserver.TestAC2_ToolCatalog`, `TestAC2_RememberRecallForget`, `TestAC2_ExportTool` |
| AC3 | WAL append-only NDJSON, non-blocking | `internal/wal.TestAC3_WALAppendOnlyNDJSON`, `TestAC3_AppendNeverBlocks` |
| AC4 | cheap gate heuristics | `internal/gate.TestAC4_CheapGate` (table-driven: passes, vetoes, secrets) |
| AC5 | worker pipeline, no-key, LLM, retry | `internal/worker.TestAC5_*` (httptest fake LLM server) |
| AC6 | FTS5 recall, limit, min_confidence, LIKE fallback | `internal/store.TestAC6_RecallFTS5LimitConfidence` |
| AC7 | UI tabs + approve/edit/delete/reject | `internal/ui.TestAC7_ReviewUITabsAndActions` (httptest against the real handler) |
| AC8 | telemetry subcommands, default off | `internal/config.TestAC1_...` + CLI surface |
| AC9 | counts-only payload, flush, DELETE erasure | `internal/telemetry.TestAC9_*` (httptest endpoint asserts method + query) |
| AC10 | export/import round trip, idempotent | `internal/store.TestAC10_ExportImportRoundTrip` |

v0.2.0 adds a second block (see [RELEASE/v0.2.0.md](../RELEASE/v0.2.0.md)):
semantic paraphrase recall (`store.TestAC1_SemanticParaphraseRecall`),
exact-keyword stays top under hybrid fusion (`store.TestAC2_ExactKeywordStaysTop`),
degradation on embedder failure (`store.TestAC3_...`,
`worker.TestWorkerBackfillEmbedderFailureIsBestEffort`), and the single
network destination of the embed client (`embed.TestAC4_SingleConfiguredDestination`).

## Techniques worth noting

- **MCP over in-memory transports.** The SDK's `NewInMemoryTransports`
  connects a real client session to the real server — tools are exercised
  through the wire protocol, not by calling handlers directly, so schema
  inference and SDK-side validation are under test too.
- **Fake LLM via httptest.** The worker tests spin up an HTTP server that
  returns a fenced-JSON body; the parser must tolerate markdown fences, and
  the retry test returns 500 and asserts the batch was requeued.
- **`VEDA_HOME` as the seam.** Every path flows through `config.Home()`,
  which honors the env override — tests and `make smoke` run against scratch
  installs without touching a real `~/.veda`.
- **Known concurrency caveat:** the SDK runs tool calls concurrently. Tests
  are request/response sequential (as real clients are); a test that
  pipelines calls without reading responses can legitimately race a write
  against a later read.
