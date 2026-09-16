# UOMP draft-01 — Storage & Export Formats

> **User-Owned Personal Memory Protocol**, draft-01.
> Veda is the product; UOMP is the open spec — like Chrome vs HTTP.

**Status:** draft-01 · **Date:** 2026-09-16 · **Companion policy:**
[VERSIONING.md](VERSIONING.md)

The key words MUST, SHOULD, and MAY are to be interpreted as described in
RFC 2119. This draft describes what Veda v0.7+ already ships: it is
**codification, not new behavior**. Everything normative here is verified by
`veda conformance storage`.

## 0. Scope

**In this draft:**
1. The on-disk SQLite schema of a Veda data directory (§2): tables, the FTS
   mirror and its triggers, tombstones, the change log.
2. The export format v1 — the exact JSON document produced by `veda export`
   and consumed by `veda import` (§3).
3. The signed audit envelope produced by `veda audit export` (§4).

**Out (draft-02, v0.9.0):** the MCP tool contract and the sync wire format.
**Out (v1.0.0):** governance, second implementation, freeze.

## 1. The data directory

A Veda home directory (default `~/.veda/`, overridable with `VEDA_HOME`)
contains `veda.db` (the normative store), `config.toml` (application
settings — out of scope), `wal.ndjson` (the write-ahead log — out of scope),
and optionally `audit_signing_key` (§4). Only `veda.db` is normative here.

## 2. On-disk schema (`veda.db`)

### 2.1 Opening rules

A compliant implementation MUST open the database with WAL journaling and a
busy timeout; Veda uses a 5000 ms timeout and a single writer connection
(the MCP server, worker, and UI share one process pool). Timestamps are
unix seconds (INTEGER). Memory ids MUST be opaque strings; Veda uses
`mem_<32 lowercase hex>` (128 bits of randomness).

### 2.2 Tables

All objects are created `IF NOT EXISTS`; a compliant store MUST contain all
of them with these columns:

```sql
CREATE TABLE sessions (
  id TEXT PRIMARY KEY, agent_id TEXT,
  started_at INTEGER, ended_at INTEGER
);
CREATE TABLE turns (
  id INTEGER PRIMARY KEY, session_id TEXT,
  role TEXT, content TEXT, ts INTEGER
);
CREATE TABLE memories (
  id TEXT PRIMARY KEY, type TEXT, content TEXT,
  source_turn_ids TEXT, agent_id TEXT,
  confidence REAL, salience REAL,
  created_at INTEGER, updated_at INTEGER,
  ttl INTEGER, deleted INTEGER DEFAULT 0,
  status TEXT NOT NULL DEFAULT 'active',
  superseded_by TEXT NOT NULL DEFAULT ''
);
CREATE TABLE conflicts (
  id INTEGER PRIMARY KEY, old_id TEXT NOT NULL, new_id TEXT NOT NULL,
  detected_at INTEGER, resolution TEXT NOT NULL DEFAULT 'new'
);
CREATE TABLE tombstones (
  memory_id TEXT PRIMARY KEY, deleted_at INTEGER NOT NULL, device_id TEXT
);
CREATE TABLE sync_state (
  key TEXT PRIMARY KEY, value TEXT
);
CREATE TABLE change_log (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  memory_id TEXT NOT NULL
);
CREATE INDEX idx_change_log_mem ON change_log(memory_id);
CREATE VIRTUAL TABLE memories_fts USING fts5(
  content, type, content='memories', content_rowid='rowid'
);
CREATE TABLE audit (
  id INTEGER PRIMARY KEY, memory_id TEXT,
  action TEXT, actor TEXT, reason TEXT, ts INTEGER
);
CREATE TABLE telemetry_queue (
  id INTEGER PRIMARY KEY, payload TEXT, queued_at INTEGER
);
CREATE TABLE pending_turns (
  id INTEGER PRIMARY KEY, session_id TEXT,
  role TEXT, content TEXT, ts INTEGER, gate_score REAL
);
CREATE TABLE memories_vec (
  memory_id TEXT PRIMARY KEY, vec BLOB NOT NULL, dims INTEGER NOT NULL
);
CREATE INDEX idx_memories_vec_memory ON memories_vec(memory_id);
```

`memories.status` MUST be `'active'` or `'superseded'`. Soft delete is
`deleted = 1`; `conflicts` records contradiction pairs (old/new id, detected
at, resolution); `tombstones` and `change_log` are sync machinery (§2.4).

### 2.3 The FTS mirror and its triggers

`memories_fts` is an **external-content** FTS5 index over `memories` with
columns `content` and `type`, keyed by `rowid`. A compliant store MUST
maintain the mirror with these three triggers:

```sql
CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
  INSERT INTO memories_fts(rowid, content, type) VALUES (new.rowid, new.content, new.type);
END;
CREATE TRIGGER memories_ad AFTER DELETE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, content, type) VALUES('delete', old.rowid, old.content, old.type);
END;
CREATE TRIGGER memories_au AFTER UPDATE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, content, type) VALUES('delete', old.rowid, old.content, old.type);
  INSERT INTO memories_fts(rowid, content, type) VALUES (new.rowid, new.content, new.type);
END;
```

**Mirror invariant:** every `memories` row MUST have exactly one `memories_fts`
row with the same rowid and content. A store MAY restore the mirror in
bulk with `INSERT INTO memories_fts(memories_fts) VALUES('rebuild')`.

> Note for verifiers: external-content FTS5 answers `COUNT(*)` and full scans
> through the content table — they cannot detect index drift. The reliable
> detectors are the `%_docsize` shadow table (cardinality) and `MATCH` phrase
> probes (per-row content).

### 2.4 Tombstones and the change log

Cross-device deletes are tombstones: `DELETE`-equivalents on one device MUST
record `(memory_id, deleted_at, device_id)` in `tombstones` (re-inserts MUST
keep the earlier `deleted_at`: `ON CONFLICT(memory_id) DO UPDATE SET
deleted_at = MAX(deleted_at, excluded.deleted_at)`). Every local memory
mutation MUST append `(memory_id)` to `change_log`; merge-in (pull) MUST NOT.
`sync_state` holds per-key push/pull watermarks; the push feed is
`change_log.seq > since` ordered by `seq`.

### 2.5 Migration

Implementations MUST migrate in place, additively only: new tables with `IF
NOT EXISTS`, new columns with `ADD COLUMN` defaulting existing rows into the
right state (`'active'` / `''`). A store upgraded from a pre-mirror state
MUST `'rebuild'` the FTS mirror once, and a store first gaining `change_log`
MUST backfill it with every existing memory id exactly once.

## 3. Export format v1 (`veda export`)

An export is a UTF-8 JSON document, pretty-printed with two spaces:

```json
{
  "version": "1",
  "exported_at": 1789440000,
  "memories": [ ... ],
  "turns": [ ... ],
  "audit": [ ... ]
}
```

- `version` MUST be the string `"1"`. `exported_at` MUST be a unix-seconds
  number (export time).
- `memories`, `turns`, and `audit` MUST always be **arrays, never null**.
- `memories` contains every memory with `deleted = 0 AND status = 'active'`,
  ordered by `created_at` ascending. Per memory:

| Field | Type | Required |
|---|---|---|
| `id` | string | yes |
| `type` | string | yes |
| `content` | string | yes |
| `confidence`, `salience` | number | yes |
| `created_at`, `updated_at` | number | yes |
| `source_turn_ids`, `agent_id`, `status`, `superseded_by` | string | optional |
| `ttl` | number | optional (unix seconds, 0 = never) |
| `deleted` | boolean | optional |

- `turns` contains every captured turn ordered by `id`: `id` (number),
  `session_id`, `role`, `content` (strings), `ts` (number).
- `audit` contains every audit entry ordered by `id`: `id` (number),
  `memory_id`, `action`, `actor` (strings), `ts` (number), optional `reason`
  (string).

An implementation MUST be able to import a v1 document, skipping memories
whose id already exists. Exported content is the user's own data and MUST
NOT be transformed on export or import.

## 4. Signed audit envelope (`veda audit export`)

```json
{
  "type": "veda-audit-export",
  "version": 1,
  "exported_at": 1789440000,
  "payload": { "type": "veda-audit-export", "version": 1, "exported_at": …,
               "install_id": "…", "entries": [ …§3 audit shape… ] },
  "public_key": "<hex Ed25519 public key>",
  "signature": "<hex Ed25519 signature over the exact payload bytes>"
}
```

The signature MUST be over **the exact bytes of the embedded `payload`**
— verifiers re-check the file's own bytes, never a re-marshal. The envelope
MUST be hand-assembled (not re-indented) so those bytes survive on disk.

## 5. Conformance

`veda conformance storage [--dir <veda-home>]` checks, in order:
`db-open` · `schema-tables` (§2.2 set) · `schema-columns` (§2.2 memories
columns) · `fts-triggers` (§2.3 set) · `fts-sync` (§2.3 mirror invariant,
via `%_docsize` cardinality and `MATCH` phrase probes) ·
`export-shape` (§3, via `ValidateExport`). Every failure names its reason;
any failure fails the run. Conformance itself MUST be read-only (the export
check opens the store only after the schema checks pass, which makes the
open a no-op on compliant stores).
