# DESIGN 14 — Team Compliance: Signed Audit Export + Org Policies (v0.7)

## Audience

Organizations that let employees use Veda with compliance obligations:
prove what happened to memories over time (signed audit export) and enforce
house rules automatically (retention, redaction). Both are configured
locally via `[policies]` in config.toml — orgs distribute this section with
their managed-config story; enforcement is entirely on-device.

## Signed audit export

`veda audit keygen` generates an Ed25519 key pair; the private key is
written to `~/.veda/audit_signing_key` (0600), the public key printed.
`veda audit export` renders the **complete** audit log — create, update,
forget, supersede, resolve, retention — as one document and signs the exact
payload bytes with Ed25519. The public key and signature are embedded, so
any reviewer can verify with `veda audit verify -f export.json`; only the
signer ever needed the private half.

Implementation detail that matters: the payload bytes on disk are the
bytes that were signed. `json.MarshalIndent` re-indents embedded
`json.RawMessage`, which would silently break verification — so the file
is hand-assembled with the payload embedded verbatim. The AC1 test flips a
byte inside the payload and requires the loud
`SIGNATURE INVALID — this export has been altered` failure.

## Org policies

```toml
[policies]
retention_days = 365                  # 0 = off (default)
redact = ["sk-[a-zA-Z0-9]{10,}",      # scrubbed to "[redacted]" on write
          "\\b\\d{3}-\\d{2}-\\d{4}\\b"]
```

- **Retention** (`veda policy enforce`, also enforced at `serve`/`worker`
  start): memories created before the cutoff are soft-deleted, tombstoned
  (so the delete propagates to synced devices), vector-invalidated, and
  audited per record with `action=retention, actor=policy`.
- **Redaction**: patterns are applied on the write paths — `remember`,
  worker distillation output, UI approvals, and WAL-turn capture — before
  persistence. A redacted memory is marked `type += " redacted"` so the
  audit trail shows it. Bad regexes fail startup with the offending
  pattern named.

## Defaults

No `[policies]` section ⇒ behavior identical to v0.6 (AC5): no scrubbing,
no sweeps. Redaction is not retroactive; retention is (it sweeps whatever
exceeds the cutoff at enforce time).

## Paid tier

Team compliance is the v0.7 paid feature: orgs obtain managed
`[policies]`-stamped configs and long-term signed-export storage from the
organization backend (separate repository, same 402 pattern as sync and
benchmarks). Local enforcement is complete without it.
