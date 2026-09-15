# DESIGN 12 — Hosted Sync (v0.4)

## Position

Sync is Veda's first paid feature ($5–15/mo per the pricing model) and the
only one that talks to a server. The local product — capture, recall,
review, export — stays free and fully functional offline, forever. The sync
backend (Supabase-based) ships in a separate repository; this document is
the wire + storage contract and the client design.

## Privacy: end-to-end encryption first

The server is an untrusted storage relay:

- Bundles are sealed with XSalsa20-Poly1305 (`golang.org/x/crypto/nacl/secretbox`).
- The key is derived from the user's sync passphrase with scrypt
  (N=32768, r=8, p=1, 32-byte key). The passphrase lives only in an
  environment variable (`VEDA_SYNC_PASSPHRASE` by default) — never on disk,
  never on the wire.
- The KDF salt rides in the plaintext envelope (it is not secret), so any
  device with the same passphrase decrypts any bundle; per-device salts
  would break cross-device reads.
- What the server sees per bundle: device id, timestamp, crypto parameters,
  nonce, ciphertext. Nothing else (AC1 is a test, not a promise).

## Data model

Two additions to the local store:

```sql
CREATE TABLE tombstones (
  memory_id TEXT PRIMARY KEY, deleted_at INTEGER NOT NULL, device_id TEXT
);
CREATE TABLE change_log (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, memory_id TEXT NOT NULL
);
CREATE TABLE sync_state (key TEXT PRIMARY KEY, value TEXT);
```

- **tombstones** carry cross-device deletes. `Forget` records one; a
  tombstone removes the peer's memory only if that memory was not edited
  after the delete (last-writer-wins by timestamp).
- **change_log** is the push feed's watermark source. Store write paths
  (`remember`, `update`, `forget`, `supersede`, `resolve`) append
  explicitly; `MergeIncoming` deliberately does not — pulled rows must
  never re-propagate to their origin device (no ping-pong). The watermark
  is the change-log **sequence**, not wall-clock time: same-second writes
  after a push would otherwise be missed (found in live testing).
- Migration is automatic: pre-existing rows are backfilled into
  change_log, so the first v0.4 push carries the whole store.
- `memories.deleted` now round-trips in bundles (JSON tag fixed for sync).

## Wire contract (for the backend repo)

```
POST /bundles                         {"id": <server-assigned, monotonic>}
     body: envelope (see below)       auth: Authorization: Bearer <plan token>
GET  /bundles?since_id=N&device_id=X  {"bundles": [{"id", "device_id", "envelope"}]}
     — never returns device X's own bundles
```

Envelope (plaintext routing metadata + ciphertext):

```json
{"device_id": "dev_…", "created_at": 1789…,
 "crypto": {"algo": "xsalsa20poly1305", "kdf": "scrypt",
            "salt": "<hex>", "N": 32768, "r": 8, "p": 1},
 "nonce": "<hex>", "ciphertext": "<base64>"}
```

Decrypted payload: `{"device_id", "memories": [full rows], "tombstones": [...]}`.

**Paid-tier gate:** `402 Payment Required` on either verb maps to
`ErrPaymentRequired` and a friendly CLI message. This is the billing hook —
entitlement is the plan token (`VEDA_SYNC_TOKEN`), checked server-side.

## Convergence

Merge is last-writer-wins per memory id on `updated_at`, applied per row
inside one transaction: insert-or-update full rows, apply tombstones
(delete unless locally newer), invalidate vectors for replaced/removed rows
(the worker re-embeds). Per-row decisions make the merge order-independent,
so two devices applying the same bundle set in any order reach identical
state (AC2). Supersession status syncs as part of the row, so a conflict
resolution on one device propagates as data, not as a special case.

## Failure posture

- Sync disabled (the default) ⇒ **zero** network calls anywhere (AC4).
- Wrong passphrase / corrupted bundle ⇒ pull errors out; the watermark does
  not advance; the next pull retries after the human fixes the passphrase.
- 402 ⇒ explicit "paid feature" error, nothing queued or lost.
- Every backend failure leaves local data untouched: sync only ever writes
  through the same audited store paths as local edits.

## Known limits (v0.4)

- Single passphrase per user; rotation = re-sync from scratch (export/import
  remains the escape hatch).
- No incremental sub-bundling: a first push serializes the whole store
  (fine at single-user scale; revisit if bundles exceed a few MB).
- Conflict-review decisions are row data and sync; the `conflicts` table
  itself is device-local history.
