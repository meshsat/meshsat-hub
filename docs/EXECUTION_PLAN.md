# MeshSat Hub — Execution Plan

**Updated:** 2026-03-19
**Covers:** v1.1 through v2.0

---

## Phase 1: v1.1 — Wire the Unwired (5 tasks, ~19h)

Complete last-mile integration for features that are built but disconnected from the runtime. After this phase, every implemented package is functional end-to-end.

### Task 1: Wire MO fragment reassembly (MESHSAT-168)
**Priority:** P0 — incoming multi-fragment SBD messages are silently dropped
**Files:** `cmd/meshsat-hub/main.go`, `internal/rockblock/handler.go`
**Steps:**
1. Instantiate `fragment.NewReassembler(5*time.Minute)` in main.go
2. Call `rbHandler.SetReassembler(reassembler)` after handler creation
3. Start `reassembler.Expire()` ticker in a goroutine
4. Add integration test: send 3-fragment MO message, verify reassembled output on MQTT
**Acceptance:** Multi-fragment MO messages are reassembled and published as single decoded message.

### Task 2: Wire E2E encryption + key management API (MESHSAT-169)
**Priority:** P0 — encrypted MO messages are silently rejected (hardcoded false)
**Depends on:** MESHSAT-168 (fragments must reassemble before decrypt)
**Files:** `cmd/meshsat-hub/main.go`, `internal/rockblock/handler.go`, `internal/api/` (new: `keys.go`)
**Steps:**
1. Instantiate `crypto.NewKeyStore()` in main.go, pass to rockblock handler
2. In handler: after decompression, check if payload starts with GCM nonce (12 bytes); if so, attempt decrypt with device keystore
3. Add API endpoints: `POST /api/devices/{imei}/keys` (generate key), `GET /api/devices/{imei}/keys` (list keys), `DELETE /api/devices/{imei}/keys/{id}` (revoke)
4. Add Vue view for key management per device
5. Store keys in database (new migration: `device_keys` table)
**Acceptance:** Encrypted MO payload decrypted using stored device key. Key CRUD via API. Key shown once on creation.

### Task 3: SOS trigger logic (MESHSAT-170)
**Priority:** P0 — SOS is the #1 safety feature, currently never fires
**Files:** `internal/rockblock/handler.go`, `internal/escalation/engine.go`, new: `internal/sos/detector.go`
**Steps:**
1. Define SOS detection: keyword match ("SOS", "MAYDAY", "EMERGENCY") in decoded MO text, OR MO from SOS-flagged device, OR explicit sos field in JSON payload
2. Create `sos.Detector` that subscribes to `meshsat/+/mo/decoded`, checks for SOS indicators
3. On SOS detection: publish to `meshsat/{device_id}/sos`, call `escalation.Engine.Trigger()`
4. Ensure SOS bypasses rate limiting (already implemented in sender)
5. Add integration test: MO message with "SOS" text triggers escalation
**Acceptance:** MO message containing "SOS" triggers escalation chain. Alert visible in UI. Notification sent via Apprise/ntfy.

### Task 4: Dead man's switch heartbeat wiring (MESHSAT-171)
**Priority:** P1 — monitor runs but doesn't see actual device activity
**Depends on:** MESHSAT-170 (shares escalation integration)
**Files:** `internal/deadman/monitor.go`, `internal/position/subscriber.go`
**Steps:**
1. In position subscriber: on each position update, call `deadman.Monitor.CheckIn(deviceIMEI)`
2. In MO handler: on each decoded message, call `deadman.Monitor.CheckIn(deviceIMEI)`
3. Dead man's switch already triggers escalation on missed check-in — verify end-to-end
4. Add integration test: configure 1-minute window, verify escalation fires after 1 minute with no check-in
**Acceptance:** Device sending positions resets its DMS timer. Missing check-in triggers escalation.

### Task 5: OpenAPI spec generation (MESHSAT-173)
**Priority:** P3 — developer experience, not runtime
**Files:** `Makefile`, go.mod, new: `docs/swagger.json`
**Steps:**
1. Add `github.com/swaggo/swag/cmd/swag` as tool dependency
2. Add `make swagger` target: `swag init -g cmd/meshsat-hub/main.go -o docs/`
3. Complete missing Swagger annotations on remaining handlers
4. Serve swagger.json at `/api/docs/swagger.json` via embed
5. Add to CI: `make swagger` in lint stage, fail if swagger.json not up to date
**Acceptance:** `/api/docs/swagger.json` returns valid OpenAPI 3.0 spec. All endpoints documented.

---

## Phase 2: v1.2 — Channel Completion — Complete (2026-04-04)

### Task 7: SMS gateway (MESHSAT-174) — DONE
**Files:** new: `internal/sms/`, `cmd/meshsat-hub/main.go`
**Steps:**
1. Create `sms.Client` with Twilio REST API (send SMS, check status)
2. Create inbound webhook handler (`POST /api/webhook/sms`) for Twilio/Vonage callbacks
3. On inbound SMS: parse sender, body, publish to `meshsat/hub/sms/inbound` on MQTT
4. On outbound: subscribe to `meshsat/+/mt/sms`, send via Twilio
5. Config: `sms_provider` (twilio|vonage), `sms_api_key`, `sms_from_number`

### Task 8: SMS as escalation notifier (MESHSAT-175) — DONE
**Depends on:** MESHSAT-174
**Steps:**
1. Implement `escalation.Notifier` interface in sms package
2. Register as notifier in main.go if SMS configured
3. Escalation chains can include phone numbers as targets

### Task 9: PGP Email gateway (MESHSAT-186) — DONE
**Files:** new: `internal/email/`, `cmd/meshsat-hub/main.go`
**Steps:**
1. Create `email.Client` — SMTP sender with optional PGP encryption (golang.org/x/crypto/openpgp)
2. Create IMAP poller or inbound webhook (`POST /api/webhook/email`) for Mailgun/SendGrid callbacks
3. Hub PGP keypair: generate on first start, store in config volume
4. Per-contact PGP key management: POST/GET/DELETE /api/email/keys
5. Public key export: GET /api/email/keys/public
6. Implement `escalation.Notifier` interface — PGP-encrypt alerts if recipient key available
7. On inbound email: decrypt if PGP, verify signature, publish to `meshsat/hub/email/inbound` on MQTT
8. Config: `email_smtp_host`, `email_smtp_port`, `email_from`, `email_imap_host`, `email_pgp_key_path`

### Task 10: WireGuard auto-provisioning (MESHSAT-176) — DONE
**Files:** `internal/wireguard/client.go`, `internal/api/devices.go`
**Steps:**
1. On device creation (POST /api/devices): auto-create WG peer if WG enabled
2. Assign VPN IP from configured subnet (track allocated IPs)
3. Return WG config in device creation response
4. Add `GET /api/devices/{imei}/wireguard` for config download

### Task 11: Tor .onion address API (MESHSAT-177) — DONE
**Files:** new: `internal/tor/onion.go`, `internal/api/`
**Steps:**
1. Read .onion hostname from Tor volume (`/var/lib/tor/hidden_service/hostname`)
2. Expose in `GET /api/config` response and `GET /api/tor/onion`
3. Device provisioning can include .onion address

---

## Phase 3: v1.3 — Routing Engine (4 tasks, ~27h)

### Task 12: Routing engine core (MESHSAT-178)
**Files:** new: `internal/routing/`, store migration
**Steps:**
1. `Route` model: source_type, destination_type, filter (keyword/portnum/device), enabled, tenant_id
2. `Engine` subscribes to all inbound topics, evaluates routes, dispatches
3. Store routes in database with CRUD
4. API: `GET/POST/PUT/DELETE /api/routes`

### Task 13: Default zero-config routes (MESHSAT-179)
**Depends on:** MESHSAT-178
**Steps:**
1. On tenant creation, seed default routes: satellite→TAK, satellite→APRS, satellite→webhooks, satellite→notifications, satellite→email
2. All defaults enabled, user can disable/modify

### Task 14: Routing UI (MESHSAT-180)
**Depends on:** MESHSAT-178
**Steps:**
1. Vue view: list routes, create/edit/delete, test route with sample message
2. Visual source→destination flow diagram

### Task 15: SMS + Email as routing destinations (MESHSAT-181 + future)
**Depends on:** MESHSAT-174, MESHSAT-186, MESHSAT-178
**Steps:**
1. Register SMS as destination type in routing engine
2. Register PGP Email as destination type in routing engine
3. Route evaluator dispatches to SMS/email gateways for matching routes

---

## Phase 4: v2.0 — Developer Experience (4 tasks, ~40h)

### Task 16: MeshSat Simulator (MESHSAT-182)
### Task 17: IPoUGRS adapter (MESHSAT-183)
### Task 18: Developer onboarding (MESHSAT-184)
### Task 19: Sensor payload framework (MESHSAT-185)

---

## Development Order (Critical Path)

```
MESHSAT-168 (fragments)
    └→ MESHSAT-169 (encryption, depends on 168)
MESHSAT-170 (SOS trigger)
    └→ MESHSAT-171 (dead man's switch, depends on 170)
MESHSAT-173 (OpenAPI)                   ← independent
MESHSAT-174 (SMS gateway)              ← independent, start of v1.2
    └→ MESHSAT-175 (SMS notifier, depends on 174)
    └→ MESHSAT-181 (SMS routing, depends on 174 + 178)
MESHSAT-186 (PGP Email gateway)         ← independent, v1.2
MESHSAT-176 (WG auto-provision)         ← independent
MESHSAT-177 (Tor .onion API)            ← independent
MESHSAT-178 (routing engine)            ← start of v1.3
    └→ MESHSAT-179 (default routes, depends on 178)
    └→ MESHSAT-180 (routing UI, depends on 178)
```

**Recommended session order:**
1. MESHSAT-168 + MESHSAT-170 (quick wins, fix silent failures)
2. MESHSAT-169 (encryption wiring, builds on 168)
3. MESHSAT-171 (DMS wiring, builds on 170)
4. MESHSAT-173 (OpenAPI, independent, improves DX)
5. MESHSAT-174 → MESHSAT-175 (SMS, new channel)
7. MESHSAT-186 (PGP Email, new channel)
8. MESHSAT-176 + MESHSAT-177 (WG + Tor, network provisioning)
9. MESHSAT-178 → MESHSAT-179 → MESHSAT-180 → MESHSAT-181 (routing engine)
10. v2.0 tasks (simulator, IPoUGRS, sensor framework)
