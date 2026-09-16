# UOMP Spec Versioning Policy

UOMP follows semver for the spec itself:

| Stage | Meaning | Change rules |
|---|---|---|
| `draft-N` | Feedback wanted, nothing frozen | Anything may change between drafts, including the on-disk schema and export shape |
| `1.0.0` | Freeze | Published as part of UOMP 1.0 (veda v1.0.0); governance starts here |
| `1.x` | Maintained | **Additive only**: new optional export fields, new tables, new conformance suites. Exports that validated under 1.0 MUST keep validating |
| `2.0.0` | Breaking | Requires a migration note and a deprecation window of at least one minor release of every conforming implementation |

Rules that hold at every stage:

1. **Drafts describe reality.** A draft MUST NOT specify behavior that does
   not ship. Where a draft finds drift between spec and implementation, the
   implementation is fixed in the same release that publishes the draft
   (v0.8.0 fixed one such drift: export arrays are never `null`).
2. **The export `version` field maps to the format, not the spec.** Export
   format v1 (`"version": "1"`) stays v1 through all of UOMP 1.x.
3. **Conformance suites are versioned with the draft they certify.**
   `veda conformance storage` certifies draft-01; later drafts may add
   suites (`tools`, `sync`) and tighten checks, never loosen a published
   draft's checks.
4. **Readability is forever.** Once 1.0 freezes, conforming implementations
   MUST be able to read exports and audit envelopes produced under 1.x for
   the life of UOMP 1.x.
