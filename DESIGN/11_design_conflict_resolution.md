# DESIGN 11 — Conflict Resolution (v0.3)

## Problem

Two agents hold two bowls; both write memories. When Claude writes "user
moved to Tokyo" a week after Cursor wrote "user lives in Singapore", recall
serves both answers and the agent picks arbitrarily. v0.2's semantic search
made this more visible, not less.

## Detection: cheap, explainable, fail-open

`internal/conflict` runs on every write path (MCP `remember`, worker
distillation, UI approve) — no LLM call, microseconds, two rules:

1. **Slot change.** A small pattern table extracts life-slot bindings from
   memory text: residence ("lives in / moved to / based in …"), employer
   ("works at / joined / quit …"), tool ("uses / switched to …"), role
   ("works as / promoted to …"). Captured values are normalized: clause
   boundaries cut, trailing temporal phrases ("last month") and qualifier
   prepositions ("near the coast") stripped. Same slot + different
   normalized value ⇒ conflict. This is what makes "lives in Singapore" →
   "moved to Tokyo" detectable despite zero meaningful token overlap.
2. **Negation.** Incoming text says "no longer / not anymore / used to" and
   shares ≥2 significant tokens (≥4 chars, minus stopwords) with an existing
   memory ⇒ conflict.

Anything not confidently pairable is left alone — **fail open**. False
positives cost one click in the Conflicts tab; false negatives leave both
memories active, exactly the pre-v0.3 behavior. Detection never fails the
write (errors and panics are swallowed by design).

## Storage and lifecycle

- `memories.status`: `active` | `superseded`; `memories.superseded_by` names
  the replacement. Superseded ≠ deleted: it stays in the DB and audit, is
  excluded from recall/list/export/embeddings by default, and is restored by
  resolution.
- `conflicts` table records each detected pair `(old_id, new_id,
  resolution)`. Auto-supersede resolves to `new` immediately.
- v0.1/v0.2 databases upgrade in place: `migrate()` ALTERs the two columns
  on (existing rows default to `active`) and creates the conflicts table.
  No rebuild, no FTS churn.

## Recall semantics

Default recall (keyword, hybrid, and semantic paths) filters
`status = 'active'` — a conflict can never answer twice (AC2). MCP `list`
gains `include_superseded: true`; the All tab hides superseded unless asked.

## Human override

The Conflicts tab lists pairs (old strikethrough → new) with three
resolutions, each audited on both memories via the `resolve` action:

| Resolution | Effect |
|---|---|
| `new` (default) | keep the auto-decision |
| `old` | reinstate the old memory, supersede the new one |
| `both` | reinstate the old; detector was wrong, keep both active |

## Known limits

- Detection is English-patterned for now; the slot table is the extension
  point for more languages and more slots (timezone, device, diet, team).
- LLM-assisted contradiction detection (ask the user's own model "does this
  new memory contradict any existing one?") is deliberately deferred: it
  belongs behind the same fail-open interface but costs latency and a key.
- Conflict detection does not run retroactively on import; imported memories
  supersede only from the next contradicting write onward.
