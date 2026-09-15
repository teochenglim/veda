# DESIGN 09 — Tech Stack & Release Engineering

## Stack

| Concern | Choice | Why |
|---|---|---|
| Language | Go 1.27 | single static binary, trivial cross-compile |
| MCP | `github.com/modelcontextprotocol/go-sdk` v1.8.0 | official SDK, protocol 2026+, schemas inferred from structs |
| SQLite | `modernc.org/sqlite` | pure Go (no CGO) with FTS5 — 6-platform releases with no C toolchain |
| TOML | `github.com/BurntSushi/toml` | the PRD names `config.toml` |
| UI | stdlib `net/http` + embedded vanilla JS | zero build step, offline-capable, loopback-only |
| LLM | raw `net/http` against OpenAI-compatible `/chat/completions` | one endpoint, any provider, no SDK lock-in |
| Embeddings | raw `net/http` against OpenAI-compatible `/embeddings` (v0.2) | same shape as the LLM client; localhost-friendly for Ollama/LM Studio |

No other runtime dependencies. `go.mod` stays intentionally small; new
dependencies require a DESIGN note.

## Release engineering (boilerplate inherited from circa)

- **`VERSION` file** is the single source of truth; `make release
  VERSION=x.y.z` amends the bump into HEAD, pushes, tags `vX.Y.Z`, pushes the
  tag.
- **Tag push triggers two workflows:**
  - `ci.yml` — vet + build + `go test -race -cover` (also runs on PRs).
  - `release.yml` — test gate, then a 6-cell GOOS/GOARCH matrix
    (linux/darwin/windows × amd64/arm64), `CGO_ENABLED=0`, `-trimpath
    -ldflags "-s -w -X main.version=$TAG"`; archives land on a GitHub
    Release with generated notes.
- **Supply-chain:** workflow actions are SHA-pinned; `make
  github-action-bump` (pinact) updates and verifies the pins.
- Version stamping flows into the MCP `implementation.version` and the
  telemetry payload, so field reports map to exact releases.

## Local loop

```
make build   # bin/veda, version-stamped from VERSION
make test    # race + coverage (the AC contract)
make smoke   # scratch VEDA_HOME → init → MCP round trip → export
make run     # veda ui against the real ~/.veda
```

## Distribution plan (weeks 8–9 of the PRD)

1. GitHub Release archives (works today via `make release`).
2. `go install github.com/teochenglim/veda@latest` (works on tag).
3. Homebrew tap + Scoop manifest — after the first tagged release proves the
   archive layout.
4. curl install script — last, once the tap exists (script just wraps it).
