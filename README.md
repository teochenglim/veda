# Veda

> **The knowledge of you, owned by you, readable by every agent.**

Veda (Sanskrit वेद — "to know") is one local memory file that every MCP agent
on your machine can read and write. Your machine, your key, your DB.

Every AI agent is a goldfish with a different bowl. Veda gives them one shared
memory — stored in a plain SQLite database in your home directory, distilled
by your own LLM key, exportable and deletable at any time.

---

## How it works

```
Claude ─┐
Cursor ─┼──► MCP ──► Veda (one Go binary) ──► ~/.veda/veda.db (SQLite + FTS5)
Codex ──┘              │
                       └── async worker: every 15 min, distills raw turns into
                           memories using YOUR LLM key, pending your review
```

- **Local-first.** Everything lives in `~/.veda/`. No account, no cloud, no sync.
- **MCP-native.** Veda is a Model Context Protocol server over stdio. Any
  MCP-capable agent can use the same memory.
- **You review.** Raw conversation turns are gated cheaply, queued as
  *pending*, and only become memories after the worker distills them — and
  you can review, edit, or delete everything in the local UI.
- **Audit by default.** Every create, update, and delete is recorded.
- **Telemetry is off.** There is no auto-consent. `veda telemetry status`
  proves it.

## Install

Requires Go 1.27+:

```sh
go install github.com/teochenglim/veda@latest
veda init
```

`veda init` creates `~/.veda/` with `veda.db` (SQLite) and `config.toml`, and
asks once whether you want anonymous usage stats (default: **no**).

## Connect your agents

Add Veda to any MCP client config:

```json
{"mcpServers": {"veda": {"command": "veda", "args": ["serve", "--stdio"]}}}
```

Examples:

- **Claude Desktop / Claude Code** — add the snippet above to the MCP config.
- **Cursor** — `~/.cursor/mcp.json`.
- **Codex** — `~/.codex/config.toml`, MCP servers section.
- **Any agent** — point it at `veda serve --stdio`.

To identify which agent wrote a memory, give each one a name:

```json
{"mcpServers": {"veda": {"command": "veda", "args": ["serve", "--stdio"], "env": {"VEDA_AGENT_ID": "cursor"}}}}
```

## The five tools your agents get

| Tool | What it does |
|---|---|
| `remember(content, type?, ttl_seconds?)` | Save a durable fact or preference now |
| `recall(query, limit?, min_confidence?, semantic?)` | Hybrid keyword+semantic search over your memories |
| `list(type?, agent_id?, since?, until?, include_superseded?)` | Browse memories |
| `forget(id)` | Delete a memory (soft delete, audited) |
| `export()` | Dump the whole store as JSON |

## Review UI

```sh
veda ui            # → http://127.0.0.1:7331
```

Four tabs:

- **Pending** — candidate facts caught from conversations. Approve with edits
  or discard.
- **All** — every memory. Edit or delete inline.
- **Audit** — who did what, when.
- **Digest** — counts and recall hit rate.

## Semantic recall (v0.2)

Recall is hybrid by default: SQLite FTS5 keyword matching fused with
embedding similarity (0.6 / 0.4 weighted rank fusion), so an exact keyword
always keeps its top spot while paraphrases surface too. Agents can opt into
pure semantic ranking with `recall(query, semantic: true)`.

To enable it, point Veda at any OpenAI-compatible `/embeddings` endpoint —
a local Ollama or LM Studio keeps everything on your machine:

```toml
# ~/.veda/config.toml
[embed]
enabled = true
base_url = "http://localhost:11434/v1"   # Ollama default
model = "bge-m3"
# api_key_env = "VEDA_EMBED_API_KEY"     # only for hosted providers
```

The worker backfills vectors for existing memories automatically — no
migration, no re-import. If the embedding endpoint is down, recall quietly
falls back to keyword-only. Nothing is ever sent anywhere except the URL you
configure.

## Conflict resolution (v0.3)

When one agent writes "User lives in Singapore" and another later writes
"User moved to Tokyo", Veda detects the contradiction on write and marks the
older memory **superseded** — recall then answers with the current side
only, on every path (keyword, hybrid, semantic).

Detection is instant heuristics (life-slot changes like residence/employer/
tool, plus explicit "no longer…" retractions) — no LLM call, and it fails
open: unsure pairs are simply kept both.

Every detected pair lands in the review UI's **Conflicts** tab where you can
**Prefer new** (the default), **Prefer old**, or **Keep both** — all audited.
Superseded memories aren't deleted; `list(include_superseded: true)` shows
them with the replacement named. Existing databases upgrade in place.

## Teach Veda about you (optional summarization)

Raw turns are captured to a write-ahead log and distilled by an async worker
every 15 minutes using an OpenAI-compatible endpoint **with your own API
key**. The key is read from the environment — it is never written to disk.

```toml
# ~/.veda/config.toml
[llm]
base_url = "https://api.openai.com/v1"   # any OpenAI-compatible endpoint
model = "gpt-4o-mini"
api_key_env = "VEDA_LLM_API_KEY"
```

```sh
export VEDA_LLM_API_KEY=sk-...
```

Without a key, nothing is summarized — candidates simply wait in **Pending**
for you to review. You can also run one pipeline pass manually:
`veda worker`.

## Export / import — portable by design

```sh
veda export -o my-memory.json      # everything, human-readable JSON
veda import -f my-memory.json      # back into any Veda install
```

## Telemetry: off, provably

```sh
veda telemetry status     # → off (default)
veda telemetry preview    # exactly what WOULD be sent (counts only, sends nothing)
veda telemetry disable    # turn it off forever
veda telemetry export     # your pseudonymous install id + anything queued
veda telemetry forget     # ask the endpoint to erase your install id (GDPR)
```

What is sent (only if you ever opt in): counts, recall hit rate, error codes,
OS/arch, a random install id. What is **never** sent: your memories,
conversations, API keys, or identity.

## All commands

```
veda init                Create ~/.veda (veda.db + config.toml)
veda serve --stdio       Run the MCP server (used in agent configs)
veda ui                  Open the review UI at http://127.0.0.1:7331
veda worker              Run one capture/distill pipeline pass and exit
veda export [-o file]    Export all memories as JSON
veda import -f file      Import memories from a JSON export
veda telemetry ...       Manage anonymous stats (default: off)
veda version             Print the version
```

`VEDA_HOME` overrides `~/.veda` (useful for tests or isolated installs).

## What Veda does NOT do (yet)

See the full plan in [ROADMAP.md](ROADMAP.md) — sync and conflict resolution
are coming; everything hosted stays out of the local product.

- Cross-device sync (planned v0.4, paid)
- Cross-agent conflict resolution (v0.3)
- Anything hosted. Local only.

## Privacy model

1. Memories live in `~/.veda/veda.db`. OS file permissions are the boundary.
2. The LLM key lives in your shell environment, not on disk.
3. Summarization sends *conversation text you chose to capture* to *the
   endpoint you configured* — nothing else, nowhere else.
4. Telemetry is opt-in, counts-only, with a published preview and a delete
   endpoint.

## Development

```sh
make help          # target menu
make test          # race + coverage; every acceptance criterion is a test
make smoke         # init → MCP round trip → export
make release VERSION=0.2.0   # tag & let CI build all platforms
```

Design docs live in [DESIGN/](DESIGN/); per-release notes and plans in
[RELEASE/](RELEASE/); the product plan in [ROADMAP.md](ROADMAP.md).

## License

Apache 2.0 — commercial-friendly (explicit patent grant, permissive for
commercial use and derivative products). Dual/commercial licensing of later
paid features remains with the copyright holder; see [ROADMAP.md](ROADMAP.md)
for the pricing plan.
