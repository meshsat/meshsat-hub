Feature: E2E Encryption + Key Management API (meshsat-hub v1.1, MESHSAT-169)

  Parent epic: MESHSAT-169. P0 silent failure per docs/ROADMAP.md L75.

  Background:
    Given the Hub is running with the crypto.KeyStore wired into main.go
    And the RockBLOCK handler has the keystore attached
    And the Cloudloop handler has the same keystore attached
    And the device_keys table is empty
    And the audit_log is enabled

  # REQ-206 + REQ-207 + REQ-213 — key creation
  Scenario: Operator creates a key for a device they own
    Given the operator authenticates with JWT containing tenant_id="tenant-A"
    And device imei=300258060902280 belongs to tenant-A
    When the operator POSTs /api/devices/300258060902280/keys
    Then the response status is 201
    And the response body contains key_id, created_at, key_hex, key_hex_first_8, algorithm="AES-256-GCM"
    And the response body's key_hex is a 64-char lowercase hex string
    And the response body's key_hex_first_8 equals the first 8 chars of key_hex
    And a device_keys row exists with the matching key_id
    And an audit_log entry of type "device_key.created" exists for imei=300258060902280
    And the audit entry's key_hex_first_8 matches the response

  # REQ-208 — list omits raw key
  Scenario: List endpoint never returns raw key material
    Given a key exists for device imei=300258060902280 in tenant-A
    When the operator GETs /api/devices/300258060902280/keys
    Then the response status is 200
    And each element has key_id, created_at, algorithm, key_hex_first_8
    And NO element has a "key_hex" or "key" or "raw_key" field

  # REQ-209 + REQ-214 — revoke
  Scenario: Operator revokes a key
    Given a key with key_id="k1" exists for device imei=300258060902280 in tenant-A
    When the operator DELETEs /api/devices/300258060902280/keys/k1
    Then the response status is 204
    And the device_keys row for k1 has revoked_at set to a Unix epoch within the last 2 seconds
    And an audit_log entry of type "device_key.revoked" exists for imei=300258060902280 key_id=k1

  # REQ-210 — cross-tenant access forbidden
  Scenario: Cross-tenant device-key access returns 403
    Given the operator authenticates with JWT containing tenant_id="tenant-A"
    And device imei=300258060902280 belongs to tenant-B
    When the operator GETs /api/devices/300258060902280/keys
    Then the response status is 403

  # REQ-201 + REQ-202 + REQ-217 — happy-path decrypt
  Scenario: MO arrives encrypted with a registered key
    Given device imei=300258060902280 has a registered key in the keystore
    When a MO payload arrives with valid [12B nonce][ciphertext + 16B tag] encrypted to that key
    Then the published mo/raw MQTT message has encrypted=true
    And the published mo/raw MQTT message has key_id set to the device's key_id
    And the published mo/decoded MQTT message contains the plaintext
    And the counter hub_decrypt_success_total with label device_imei="300258060902280" is incremented by 1

  # REQ-203 + REQ-219 — no key, payload passes through unchanged
  Scenario: MO arrives for a device with no registered key
    Given device imei=300258060902281 has NO registered key
    When a MO payload arrives for imei=300258060902281
    Then the published mo/raw MQTT message has encrypted=false
    And the payload bytes are unchanged from the wire bytes
    And the counter hub_decrypt_failure_total{reason="no-key"} is incremented by 1 exactly

  # REQ-203 + REQ-218 — wrong key (MAC fails)
  Scenario: MO arrives encrypted with a key that no longer matches (rotation gap)
    Given device imei=300258060902280 has a registered key key_id="new"
    And the device is still using an old key
    When a MO payload arrives encrypted with the old key
    Then the published mo/raw MQTT message has encrypted=false
    And the payload bytes are the ORIGINAL ciphertext (NOT decrypted plaintext)
    And the counter hub_decrypt_failure_total{reason="mac"} is incremented by 1

  # REQ-204 — RockBLOCK no longer hardcodes false
  Scenario: RockBLOCK handler reports actual encryption status
    Given a registered key exists for imei=300258060902280
    When a non-encrypted MO arrives via RockBLOCK for imei=300258060902280
    Then the published mo/raw MQTT message has encrypted=false
    When an encrypted MO arrives via RockBLOCK for imei=300258060902280
    Then the published mo/raw MQTT message has encrypted=true

  # REQ-205 — Cloudloop uses the same keystore
  Scenario: Cloudloop handler uses the same keystore instance
    Given a registered key exists for imei=300258060902280
    When an encrypted MO arrives via the Cloudloop webhook for imei=300258060902280
    Then the published mo/raw MQTT message has encrypted=true

  # REQ-215 — logs never contain raw keys
  Scenario: Raw key bytes never appear in logs
    Given a key has been created and used for decryption
    When all logs from the last 60 seconds are scanned for the raw key hex
    Then NO log line contains the full 64-char raw key hex
    And the only key references in logs are key_id and key_hex_first_8

  # REQ-216 — multi-key per-IMEI rotation grace
  Scenario: Two non-revoked keys for one IMEI — newest tried first
    Given device imei=300258060902280 has 2 non-revoked keys: key_id="old" (created 2 hours ago) and key_id="new" (created 1 minute ago)
    When an encrypted MO arrives encrypted with the "old" key
    Then the published mo/raw MQTT message has encrypted=true
    And the published key_id field is "old"
    And the counter hub_decrypt_failure_total{reason="mac"} is incremented by exactly 1 (the "new" attempt before the "old" success)
