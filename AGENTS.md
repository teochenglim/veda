# AGENTS.md

Veda: one local memory store (`~/.veda`, SQLite+FTS5) every MCP agent reads/writes. Local-only, single binary, telemetry opt-in.

Docs: `prd.md` = requirements · `ROADMAP.md` = plan · `RELEASE/vX.Y.Z.md` = release notes · `DESIGN/` = why · `README.md` = end users.

## Commands

- `make vet test` — must pass before claiming anything works. ACs live as `TestAC<n>_*`; new AC ⇒ new test + row in `RELEASE/vX.Y.Z.md`.
- `make build`, `make smoke`, `make release VERSION=x.y.z`.

## Git & release caveat (important)

- **The user does ALL git commits, pushes, tags, and releases manually. The agent never runs git commit/push/tag, `make bump`, or `make release`.**
- The release command is `make release VERSION=x.y.z` (space + `VERSION=`, not `make release=x.y.z`).
- When an implementation is finished, give a 1-line commit-message summary; the user runs the git commit and `make release` themselves.
- `make release` amends the VERSION bump into HEAD and pushes `origin HEAD` — it fails unless the repo already has ≥1 commit AND an `origin` remote. User must commit first, add the remote, then run it.

## Rules

- Telemetry **default off**; no auto-consent path, ever. Counts-only payloads, no content.
- Pure Go, no CGO, no new dependencies without a DESIGN note. Keep `go.mod` small.
- LLM key lives in env, never on disk. UI binds loopback only.
- Soft delete + audit for every memory mutation.
- Changed behavior ⇒ update the `DESIGN/` or `RELEASE/` doc it touches, unprompted. Close with a one-line commit message; user commits.

## Gotchas

- MCP tool calls run concurrently; don't assume cross-request ordering.
- CI is tag-driven (`v*`), not push-driven.
