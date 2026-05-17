# Requirements — Wire E2E Encryption + Key Management API (meshsat-hub v1.1, MESHSAT-169)

Source: `docs/ROADMAP.md` L75 ("E2E encryption: AES-256-GCM + per-device keystore complete. Never instantiated. No API for key CRUD. RockBLOCK handler hardcodes encrypted=false"), `docs/EXECUTION_PLAN.md` §"Task 2: Wire E2E encryption + key management API (MESHSAT-169)" — P0 silent failure ("encrypted MO messages are silently rejected (hardcoded false)").

Constitution invariants in scope: Article II (security #1), Article III (httpjson.ReadJSON), Article IV (triple-layer tenant isolation), Article VIII (audit log hash chain), Article XII (single-source migrations: SQLite + MariaDB), Article XIV (no secrets in logs).

The `internal/crypto/` package already implements `KeyStore` (`NewKeyStore()` constructor, `DecryptMessage(deviceIMEI, ciphertext)` method per `crypto/crypto_test.go:130+`). The wire format is `[12-byte GCM nonce][ciphertext + 16-byte tag]`. `main.go` never instantiates the keystore, the RockBLOCK handler hardcodes `encrypted=false`, and there's no API to create/list/revoke device keys.

**Cross-repo reference:** the device-side wire format is documented in `meshsat-hub/adr/0007-two-key-encryption-model.md` (this repo) — bytes-on-the-wire MUST match the Bridge's encryption path in `meshsat/internal/keystore/` + `meshsat-android/crypto/AesGcmCrypto`.

## Functional requirements

REQ-200: The system shall instantiate `crypto.NewKeyStore(store)` exactly once during Hub startup in `cmd/meshsat-hub/main.go` and pass the instance to the RockBLOCK + Cloudloop ingress handlers.

REQ-201: When a MO payload arrives whose first 12 bytes form a valid AES-GCM nonce AND a device key is registered for the source IMEI, the ingress handler shall attempt `keystore.DecryptMessage(imei, payload)` BEFORE decompression.

REQ-202: When `DecryptMessage` returns successfully, the system shall use the decrypted bytes as the downstream payload and shall mark the published MQTT message with `encrypted=true`.

REQ-203: When `DecryptMessage` returns an error (no key for IMEI, MAC mismatch, malformed ciphertext), the system shall NOT mark the message encrypted=true and shall pass the ORIGINAL bytes downstream — encrypted-looking-but-undecryptable payloads degrade to opaque-bytes mode rather than being dropped.

REQ-204: The RockBLOCK handler shall stop hardcoding `encrypted=false` and shall instead use the value returned from the decryption step (REQ-201 / REQ-202 / REQ-203).

REQ-205: The Cloudloop handler shall apply the same decryption step (REQ-201) against the same KeyStore instance.

REQ-206: The system shall expose `POST /api/devices/{imei}/keys` that generates a new AES-256 key for the device, returns the raw key in the response body ONCE, and stores the hash in the `device_keys` table.

REQ-207: When `POST /api/devices/{imei}/keys` succeeds, the response body shall contain `key_id`, `created_at`, `key_hex` (the only time the raw key is returned), and `algorithm` ("AES-256-GCM").

REQ-208: The system shall expose `GET /api/devices/{imei}/keys` that lists all active keys for the device WITHOUT the raw key material — only `key_id`, `created_at`, `revoked_at`, `algorithm`.

REQ-209: The system shall expose `DELETE /api/devices/{imei}/keys/{key_id}` that sets `revoked_at = now()` for the named key.

REQ-210: When an operator calls any `/api/devices/{imei}/keys*` endpoint, the system shall filter results by the operator's `tenant_id` JWT claim and shall verify the named device belongs to the operator's tenant — cross-tenant access returns 403.

REQ-211: The `device_keys` table shall have columns `key_id TEXT PRIMARY KEY`, `tenant_id TEXT NOT NULL`, `device_imei TEXT NOT NULL`, `key_hash BLOB NOT NULL`, `key_hex_first_8 TEXT NOT NULL`, `algorithm TEXT NOT NULL`, `created_at INTEGER NOT NULL`, `revoked_at INTEGER`.

REQ-212: The schema migration adding the `device_keys` table shall land in BOTH `internal/store/sqlite/` AND `internal/store/mariadb/mariadb.go` per Constitution Article XII.

REQ-213: When a key is created, the system shall write an audit log entry of type `device_key.created` containing `device_imei`, `key_id`, `key_hex_first_8` (NEVER the full key).

REQ-214: When a key is revoked, the system shall write an audit log entry of type `device_key.revoked` containing `device_imei`, `key_id`.

REQ-215: The system shall NEVER write raw key bytes to any log level; only `key_id` and the first 8 hex chars (`key_hex_first_8`) are acceptable in log output per Constitution Article XIV.

REQ-216: The keystore shall use the latest non-revoked key for the given IMEI when decrypting; if multiple non-revoked keys exist for one IMEI, the keystore shall try each in `created_at DESC` order.

REQ-217: When a decryption succeeds, the system shall increment the `hub_decrypt_success_total` Prometheus counter labelled by `device_imei`.

REQ-218: When a decryption fails for a registered device (key exists but ciphertext fails MAC check), the system shall increment `hub_decrypt_failure_total{reason="mac"}`.

REQ-219: When a MO arrives for a device with no registered key, the system shall increment `hub_decrypt_failure_total{reason="no-key"}` exactly once per MO (NOT once per attempted key trial).

REQ-220: The Bundle-format wire compatibility shall match the device-side encryption per `adr/0007-two-key-encryption-model.md` — bytes 0–11 are the random nonce, bytes 12..N-16 are the ciphertext, bytes N-16..N are the GCM authentication tag.

REQ-221: The `POST /api/devices/{imei}/keys` endpoint shall use `httpjson.ReadJSON` per Constitution Article III; the request body schema is empty (key generation is fully server-side).
