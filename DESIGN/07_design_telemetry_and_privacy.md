# DESIGN 07 — Telemetry & Privacy

## Policy, enforced by structure

Telemetry is the single most brand-sensitive feature in Veda. The PRD's rule
— *telemetry default off, first-run prompt only, auto-consent breaks the
thesis* — is enforced by making the default a constant
(`config.Default().Telemetry.Enabled == false`), not a missing file. A
missing `config.toml` yields defaults; `veda init` only flips the bit after
an explicit interactive `[y/N]` answer, and non-interactive runs (pipe,
`--no-telemetry`) can never enable it.

## Payload

Counts only. The `Stats` struct is the entire surface:

```
install_id (random, pseudonymous) · version · os · arch · ts
memories · turns · sessions · recall_calls · recall_hits · forget_count
```

There is no free-text field, no path, no timestamp granularity beyond the
flush moment. A regression test asserts that memory content never appears in
a serialized payload, and the store layer has no API that could leak one
(`CollectStats` returns counts).

## Lifecycle

```
opt-in (first run / `telemetry enable`)
  → serve loop queues a snapshot every 12h, flushes every 24h
  → flush POSTs each queued payload to the configured endpoint
  → queue cleared only on HTTP 2xx
disable ⇒ nothing further queued or sent (queued rows stay local,
          visible via `telemetry export`, removable by deleting ~/.veda)
```

`veda telemetry preview` prints the exact next payload with no consent
requirement — inspection must never require opting in.

## Backend is a separate repository

The Cloudflare Worker (D1 insert on POST, DELETE-by-install_id) ships in its
own repo and is deployed independently; Veda only stores the endpoint URL in
`config.toml`. This keeps the binary free of infra code and lets the endpoint
be self-hosted: point `url` anywhere that accepts the two verbs and the
privacy contract holds.

## GDPR erasure

`DELETE <url>/?install_id=<id>` — exposed locally as
`veda telemetry forget`, and `veda telemetry export` prints the install id
plus every queued payload so a user can exercise erasure manually even if
this binary is uninstalled. Local erasure is stronger still: delete
`~/.veda/`.
