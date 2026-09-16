# UOMP Deprecation Policy

Any delta between a published UOMP draft and shipped behavior ships as a
**warning first**: nothing breaks in the same release that documents the
delta.

## Rules

1. A conforming implementation that finds itself diverging from a published
   draft MUST, in one release: (a) restore the draft behavior, **or** (b)
   emit a deprecation warning while continuing the old behavior.
2. Old behavior is removed no earlier than **one minor release** after the
   warning first appears, and the removal is recorded here.
3. Warnings are visible to the affected party: tool-contract warnings on the
   tool result; sync warnings in the CLI; storage warnings at `veda init`
   and `veda conformance storage`.
4. Golden vectors are never silently changed: a vector that must change
   because of a delta is itself a registered deprecation.

## Register

| Since | Area | What is deprecated | Removal | Status |
|---|---|---|---|---|
| — | — | (no active deprecations) | — | — |

Historical: v0.8.0 fixed one draft/reality delta (empty stores exported
`null` arrays) **in the same release that published draft-01**, per the
"the draft must describe reality" rule — no warning was required because the
old shape was never specified and `veda import` accepted both.
