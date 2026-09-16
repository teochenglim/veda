# UOMP draft-02 — Tool Contract & Sync Wire

> **User-Owned Personal Memory Protocol**, draft-02.
> Veda is the product; UOMP is the open spec — like Chrome vs HTTP.

**Status:** draft-02 · **Date:** 2026-09-16
**Companions:** [uomp-draft-01.md](uomp-draft-01.md) (data at rest) ·
[VERSIONING.md](VERSIONING.md) · [DEPRECATIONS.md](DEPRECATIONS.md)

The key words MUST, SHOULD, and MAY are to be interpreted as described in
RFC 2119. Draft-02 covers **data in motion**: the MCP tool interface every
agent programs against (§2) and the sync bundle contract every backend
implements (§3). Both were already stable in practice — the tool contract is
unchanged in shape since v0.1, the sync wire since v0.4 — so this draft is
codification, plus the conformance machinery to keep it true.

## 0. Scope

**In:** the five-tool MCP contract, the sync REST wire, envelope crypto
parameters, the 402 paid gate, and the watermark rule.
**Out (1.0.0):** governance, second implementation, stability freeze.

## 1. Transport and session

Tools are spoken over stdio as MCP (JSON-RPC 2.0, the Model Context
Protocol lifecycle): `initialize` → `initialized` → tool calls. A conforming
server MUST serve exactly the five tools of §2; it MAY register additional
tools or optional parameters additively (see VERSIONING.md). Server name and
version strings are informational.

## 2. The tool contract

Tool inputs are JSON objects; unspecified/absent optional fields take the
documented defaults. Memory objects in tool outputs use the draft-01 §3
memory field set (`id`, `type`, `content`, `confidence`, `salience`,
`created_at`, `updated_at`, …).

### 2.1 `remember`

| Input | Type | Rule |
|---|---|---|
| `content` | string | **required.** MUST fail when empty |
| `type` | string | optional; recommended values `preference` · `fact` · `identity` · `goal` |
| `source_turn_ids` | string | optional provenance |
| `ttl_seconds` | integer | optional; absent/0 = never expires |

Output: `{"id": "<opaque memory id>"}`. A save MUST be audited (action
`create`) and MUST be durable — a tool-conformant save is visible to `recall`,
`list`, and `export`.

### 2.2 `recall`

| Input | Type | Rule |
|---|---|---|
| `query` | string | required free-text search |
| `limit` | integer | absent or ≤ 0 ⇒ **default 5** |
| `min_confidence` | number | absent or ≤ 0 ⇒ **default 0.5** |
| `semantic` | boolean | **optional-to-implement**: `true` = pure semantic (embedding) ranking; the default is hybrid keyword+semantic |

Output: `{"memories": […]}` ranked by relevance, at most `limit` entries.
A conforming implementation without embeddings MAY rank keyword-only;
`semantic: true` MUST then still succeed (degraded, not error).

### 2.3 `list`

| Input | Type | Rule |
|---|---|---|
| `type`, `agent_id` | string | optional filters |
| `since`, `until` | integer | optional unix-seconds bounds on `created_at` |
| `include_superseded` | boolean | MUST be accepted; default output hides memories replaced by newer conflicting ones |

Output: `{"memories": […]}`.

### 2.4 `forget`

Input: `{"id": "<memory id>"}`. Output: `{"deleted": <bool>}`.

- A live id MUST return `deleted: true`, and the memory MUST disappear from
  every recall/list/export result. The deletion MUST be **soft** (the store
  retains the row for sync/audit) and MUST record an audit entry with action
  `forget`. A sync-capable implementation MUST also tombstone the deletion.
- An unknown id MUST return `deleted: false` and MUST NOT error.

### 2.5 `export`

Input: `{}`. Output: `{"json": "<string>"}` where the string is exactly a
draft-01 §3 export document (version `"1"`). `veda conformance tools`
validates it with the draft-01 `ValidateExport` rules.

### 2.6 Error behavior

- Calling an unknown tool MUST fail.
- Violating an input rule (e.g. empty `content`) MUST fail the call.
- Tool failures SHOULD be reported in-band (`isError: true`) rather than as
  protocol errors, so agents can see and self-correct the mistake.

## 3. The sync wire

An untrusted storage relay: the endpoint stores opaque sealed envelopes and
never sees memory content. Two verbs, both authenticated (§3.2):

```
POST {url}/bundles
  body:  the envelope (§3.3) as JSON
  200:   {"id": <int64>}          — server-assigned, globally monotonic

GET {url}/bundles?since_id=N&device_id=X
  200:   {"bundles": [{"id": <int64>, "device_id": "<id>", "envelope": {…}}]}
```

**§3.1 Ordering.** Bundle ids are server-assigned and strictly increasing.
`GET` MUST return only bundles with `id > since_id`, ascending, and MUST
NEVER include device X's own bundles — a device must not re-receive what it
pushed.

**§3.2 Auth and the 402 gate.** Both verbs take `Authorization: Bearer
<token>`; the token is the entitlement (a plan token). Endpoints MUST reject
requests with no/invalid credentials (401 or 403). An authenticated but
unentitled token MUST be answered with **402 Payment Required** — this is
the contract's billing hook; conforming clients map it to an explicit
paid-feature error and queue nothing.

**§3.3 Envelope.** Routing metadata in the clear, the bundle only as
ciphertext:

```json
{"device_id": "dev_…", "created_at": 1789440000,
 "crypto": {"algo": "xsalsa20poly1305", "kdf": "scrypt",
            "salt": "<hex, ≥8 bytes>", "N": 32768, "r": 8, "p": 1},
 "nonce": "<24 raw bytes, hex>", "ciphertext": "<base64 std>"}
```

The crypto parameter set is deliberately fixed: XSalsa20-Poly1305 with a key
derived by scrypt (N=32768, r=8, p=1, 32-byte key) from the user's sync
passphrase and the envelope's salt (which is not secret and rides in the
clear so any device with the passphrase can decrypt any device's bundle).
The decrypted payload is `{"device_id", "memories": [<draft-01 memory rows,
including superseded ones>], "tombstones": [{"memory_id", "deleted_at",
"device_id"}]}`. The endpoint MUST treat `ciphertext` as opaque bytes.

**§3.4 Watermark rule.** The pull cursor is the server-assigned bundle
**sequence**, never wall-clock time. Clients advance their watermark only
after a pulled bundle is successfully decrypted **and** merged, so a failed
merge retries the same bundles. Merges are last-writer-wins per memory id on
`updated_at`; merge-applied rows MUST NOT re-enter the push feed (no
ping-pong).

## 4. Conformance

- `veda conformance tools --command <cmd>` drives any MCP server over stdio
  and checks §2: catalog, remember save/required-content, recall finds,
  default limit 5, limit flag, list filters and `include_superseded`,
  forget semantics (unknown id, deletion, audit), export shape (draft-01),
  unknown-tool error.
- `veda conformance sync --endpoint <url> [--token T] [--unpaid-token T]`
  checks §3: monotonic ingest, device exclusion, since_id paging, envelope
  wire shape and decryption fidelity, the auth gate, and — when
  `--unpaid-token` is given — the 402 gate.
- `ReferenceBackend` (in `internal/conformance`) is the executable reference
  implementation of §3; the sync suite must pass against it.

## 5. Golden vectors

`spec/vectors/` freezes byte-exact fixtures so implementations test offline
and across versions: a canonical draft-01 export document, a signed audit
export with a committed test-only Ed25519 key, and a fixed-key sync envelope
(passphrase + salt + nonce documented there) whose decrypted bundle is
known. Regenerate with `VEDA_UPDATE_VECTORS=1 go test ./internal/conformance
-run TestGenerateVectors`.

## 6. Deltas from current behavior

None. Draft-02 matches shipped behavior exactly; v0.9.0 introduces no
deprecation warnings. Any future delta ships as a warning first — see
[DEPRECATIONS.md](DEPRECATIONS.md).
