# UOMP Golden Vectors

Byte-exact fixtures frozen at draft-02 time (v0.9.0). Implementations load
these and assert byte-for-byte or field-for-field equality — offline, across
versions. Verification lives in
`internal/conformance/vectors_test.go` (`TestAC4_GoldenVectors`).

| File | What it is |
|---|---|
| `export.v1.json` | Canonical draft-01 §3 export document (version `"1"`, two memories, one turn, one audit entry, fixed timestamps). Must pass `ValidateExport` and import into a conforming store |
| `audit-signed.json` | Draft-01 §4 signed audit envelope. Signature verifies over the payload's exact bytes with the public key embedded in the file |
| `sync-envelope.json` | Draft-02 §3.3 sealed envelope. Opens under the passphrase below to a known one-memory bundle |

## Fixed test material (deliberately committed — these keys guard nothing)

- Ed25519 private key (hex): see `internal/conformance/vectors_test.go`
  (`vectorPrivHex`); the public key is embedded in `audit-signed.json`.
- Sync passphrase: `uomp-golden-vector`
- KDF salt (hex): `5f549156482c4b0ea456d64ca4a26932` (scrypt N=32768 r=8 p=1)
- Nonce (hex): `000102030405060708090a0b0c0d0e0f1011121314151617`
- Decrypted bundle: `device_id = "dev_vector"`, one memory
  (`mem_00000000000000000000000000000001`,
  content `"Vector: user prefers window seats"`)

## Regenerating

Vectors are deterministic (fixed keys, salt, nonce, timestamps):

```sh
VEDA_UPDATE_VECTORS=1 go test ./internal/conformance -run TestGenerateVectors
```

The generator re-runs the verification assertions after writing. Changing a
vector because behavior changed is itself a deprecation — see
[DEPRECATIONS.md](../DEPRECATIONS.md).
