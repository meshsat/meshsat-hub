# Design — Wire E2E Encryption + Key Management API

## Goal

Close the v1.1 P0 silent-failure in `docs/ROADMAP.md` L75. The `crypto.KeyStore` already implements AES-256-GCM decryption per-device. The wire format matches `meshsat-hub/adr/0007-two-key-encryption-model.md`. What's missing: instantiation in `main.go`, integration into the MO ingress path (currently hardcodes `encrypted=false`), and the REST API for operators to provision device keys.

## Wire diagram

```
                                     ┌─────────────────────┐
device (Bridge / Android)            │  ingress handler    │
encrypts payload AES-256-GCM         │  (rockblock OR      │
with per-device key                  │   cloudloop)        │
[12B nonce][ct + 16B tag]            │                     │
       │                             │  if keystore != nil │ ← REQ-002
       │ via satellite               │  AND len(p) >= 28:  │
       ▼                             │     pt, err :=      │
   webhook POST                ────► │       keystore.     │
                                     │       DecryptMessage│
                                     │         (imei, p)   │
                                     │     if err == nil:  │
                                     │       p = pt        │ ← REQ-003
                                     │       encrypted=    │
                                     │         true        │
                                     │     else:           │
                                     │       encrypted=    │ ← REQ-004
                                     │         false       │
                                     │       (keep orig p) │
                                     │                     │
                                     │  publish mo/raw +   │
                                     │  mo/decoded with    │
                                     │  encrypted flag     │
                                     └─────────────────────┘
                                                │
                                                ▼
            ┌────────────────────────────────────────────────┐
            │   Operator-facing REST API                      │
            │                                                 │
            │   POST   /api/devices/{imei}/keys → gen key,    │ ← REQ-007
            │          return raw ONCE + hash to DB           │
            │   GET    /api/devices/{imei}/keys → list (no    │ ← REQ-009
            │          raw key in response)                   │
            │   DELETE /api/devices/{imei}/keys/{key_id} →    │ ← REQ-010
            │          set revoked_at                         │
            │                                                 │
            │   Tenant filter on every call (REQ-011)          │
            └────────────────────────────────────────────────┘
```

## Tables touched

- `audit_log` (existing) — new event types `device_key.created`, `device_key.revoked`.
- `device_keys` (new) — schema per REQ-012. Lands in BOTH `internal/store/sqlite/` AND `internal/store/mariadb/mariadb.go` per Constitution Article XII.

```sql
CREATE TABLE device_keys (
  key_id           TEXT PRIMARY KEY,
  tenant_id        TEXT NOT NULL,
  device_imei      TEXT NOT NULL,
  key_hash         BLOB NOT NULL,          -- SHA-256 of raw key, for verification only
  key_hex_first_8  TEXT NOT NULL,          -- safe-to-log identifier
  algorithm        TEXT NOT NULL,          -- "AES-256-GCM"
  created_at       INTEGER NOT NULL,
  revoked_at       INTEGER
);
CREATE INDEX idx_device_keys_imei_active ON device_keys (device_imei) WHERE revoked_at IS NULL;
CREATE INDEX idx_device_keys_tenant ON device_keys (tenant_id);
```

**Important note on `key_hash`:** the raw AES-256 key is NOT stored in the DB. The keystore holds the raw key in-memory only AFTER first creation (and zeroizes it on revoke). The `key_hash` column lets the keystore verify a candidate key matches without revealing the raw key — used during key-rotation handshakes if added later. v1.1 does NOT need this verification path; the column is present for forward compatibility.

**Operational gotcha:** because the raw key is returned ONCE on creation and never stored in plaintext, an operator who loses the raw key MUST generate a new one and re-provision the device. This is an intentional security tradeoff — same model as RockBLOCK API keys.

## Decryption fallback semantics (REQ-004)

A common operator concern: what if a payload looks encrypted but isn't, or the wrong key is on the device? Two failure modes:

1. **No key for IMEI** — payload is treated as plaintext. Counter `hub_decrypt_failure_total{reason="no-key"}` increments. Downstream `encrypted=false`.
2. **Key exists but MAC fails** — payload is treated as plaintext (caller may want to debug). Counter `hub_decrypt_failure_total{reason="mac"}` increments. Downstream `encrypted=false`. Audit entry deliberately NOT written here (would let an attacker DoS the audit log).

The KEY decision (REQ-004): a failed decrypt does NOT drop the message. It passes the original opaque bytes through. The operator sees `encrypted=false` AND can correlate with the Prometheus counter. This matches the existing behavior where bridge-decoded plaintext flows through unchanged.

## Tenant isolation (REQ-011)

Every `/api/devices/{imei}/keys*` handler MUST:
1. Extract `tenant_id` from JWT.
2. Look up the device by `(tenant_id, imei)` in the devices table.
3. Reject with 403 if device not found in caller's tenant.
4. Filter `device_keys` queries by `tenant_id` AND `device_imei`.

This is the same pattern as the existing `/api/devices/{imei}/config` endpoint — re-use the `auth.RequireDeviceOwnership` middleware if it exists, or write a similar guard.

## Per-IMEI multi-key handling (REQ-017)

An operator may rotate keys (issue a new one, gracefully retire the old). During the overlap window, multiple `revoked_at IS NULL` rows exist for one IMEI. The keystore tries each in `created_at DESC` order (newest first) and returns on first success. The same `hub_decrypt_failure_total{reason="mac"}` counter increments only ONCE per MO regardless of how many keys were tried (REQ-020 specifies once-per-MO accounting).

## Out of scope

- Key rotation API ("rotate this key without invalidating outstanding traffic") — deferred. Operator must create new + revoke old manually.
- Hardware-backed key storage (HSM, KMS) — deferred. Keys live in process memory + DB.
- Forward-secrecy session keys (X25519 ECDH) — not applicable to satellite-store-and-forward; per ADR-0007 we explicitly use static per-device AES.
