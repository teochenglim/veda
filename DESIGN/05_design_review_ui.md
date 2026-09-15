# DESIGN 05 — Review UI

## Scope

A single embedded page (`embed`, zero external assets, works offline) served
at `127.0.0.1:7331` — loopback only, because the API is unauthenticated by
design. Four tabs map 1:1 onto the PRD:

| Tab | Data | Actions |
|---|---|---|
| Pending | `GET /api/pending` | approve (with inline edit) / reject |
| All | `GET /api/memories?type&agent_id` | edit / delete |
| Audit | `GET /api/audit` | read-only |
| Digest | `GET /api/digest` | counts + recall hit rate |

## Decisions

- **Thin JSON API, vanilla JS frontend.** No build step, no node_modules —
  the binary stays self-contained and the UI is readable as a single file.
- **Approve creates a memory via the same `store.Remember` path as MCP
  writes** (`actor="ui"`), so audit semantics are identical no matter which
  surface made the change.
- **Approve accepts an edited content string.** Human review that couldn't
  fix anything would be review theater; the edit box is the point.
- **Delete from the UI is the same soft delete as `forget`** — audited,
  recoverable from the DB file by hand, excluded from recall and export.
- **Digest surfaces recall hit rate** (`recall_hits / recall_calls`) because
  it is the metric the launch post asks users to report ("break it. tell me
  where recall fails") — make it self-serve.

## Security posture

The UI trusts the local user because it *is* the local user. It binds
127.0.0.1 by default; `-addr` can override, and `config.toml` documents why
loopback is the default. No CSRF story is needed for a loopback-only,
no-cookie, no-credential API; if remote binding is ever requested, an auth
token becomes a precondition for that change.
