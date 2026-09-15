# DESIGN 06 — MCP Layer

## SDK and transport

Official Go SDK (`github.com/modelcontextprotocol/go-sdk` v1.8.0) over
`StdioTransport`. Tool input schemas are inferred from Go structs with
`jsonschema` struct tags — the spec's `remember(content, type?,
source_turn_ids?, ttl_seconds?)` etc. map 1:1 onto struct fields, and the SDK
validates inputs before the handler runs.

## Tool semantics

| Tool | Notes beyond the obvious |
|---|---|
| `remember` | Defaults: type empty ⇒ shown as "fact"; confidence 1.0 (an agent asserting a fact directly is high confidence); salience 0.5. `ttl_seconds` is converted to an absolute deadline at write time. `agent_id` comes from `VEDA_AGENT_ID` env (clients pass it in their MCP config `env` block) so memories are attributable per agent. |
| `recall` | Defaults per spec: `limit=5`, `min_confidence=0.5`. Every call is audited as `recall_hit`/`recall_miss` — this feeds the Digest hit rate and the (opt-in) telemetry metric. |
| `list` | All filters optional; time filters are unix seconds. |
| `forget` | Soft delete; returns `{deleted: false}` if the id is unknown or already gone — idempotent, not an error. |
| `export` | Full store as a JSON document string, identical shape to `veda export`. |

## Versioning

`main.version` is stamped by `-ldflags` at build time (`make build`, release
CI); dev builds report `dev`. The MCP `implementation.version` reports the
same string, so agent-side logs and telemetry correlate with a release tag.

## Extensions beyond the PRD tool list

None in v0.1. The five tools are exactly the spec; capture of raw turns
happens through the WAL, not through an extra MCP tool. If agents later need
to push raw conversation turns (not just distilled facts), that is a v0.2
addition and will be additive — never a change to the five.
