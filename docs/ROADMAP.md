# MeshSat Hub — Roadmap

**Updated:** 2026-03-19
**YouTrack:** Project `MESHSAT`, issues prefixed `MESHSAT-`

---

## Tri-Mode Architecture

MeshSat Hub is designed as a **3-tier deployment system**, selectable at startup via `HUB_MODE`:

```
Tier 1: STANDALONE           Tier 2: CLUSTER              Tier 3: KUBERNETES
(single node, edge)          (multi-host HA)              (cloud-native, auto-scale)

+----------+                 +----------+ +----------+   +---+ +---+ +---+
|   Hub    |                 |  Hub-NL  | |  Hub-GR  |   |Hub| |Hub| |Hub|
| (SQLite) |                 | (Galera)   (Galera)   |   +---+ +---+ +---+
|Mosquitto |                 |  NATS       NATS      |      |     |     |
+----------+                 +----------+ +----------+   +-MariaDB Galera+
                              + garbd +                  +----Redis-----+
                              (arbiter)                  +--NATS (x3)---+
```

| Tier | Mode | Store | Bus | Dedup/RL | Leader | Status |
|------|------|-------|-----|----------|--------|--------|
| 1 | `standalone` | SQLite | Mosquitto | In-memory | Noop | **Production** |
| 2 | `cluster` | MariaDB Galera | NATS+MQTT | Redis | NATS queues | **Production** (2 nodes, active-active) |
| 3 | `kubernetes` | MariaDB Galera | NATS StatefulSet | Redis | k8s Lease API | **Code complete**, untested |

**Current production:** Tier 2 cluster across `nllei01dmz01` (NL) + `grskg01dmz01` (GR).

---

## Version History (Complete)

### v0.1 — Foundation (Complete)

Infrastructure, MQTT, SBD (MO + MT), SMAZ2 compression, Tor hidden service, health probes, integration tests, deployment documentation.

### v0.2 — Multi-Tenant Device Management (Complete, 2026-03-18)

OAuth2/OIDC, tenant isolation, RBAC (viewer/operator/owner), API keys, device registry, config versioning, credit polling, per-device budgets, audit log, Vue.js dashboard (Tailwind, Pinia, auth guards).

### v0.2 — Cluster Infrastructure Sprint (Complete, 2026-03-18)

MariaDB Galera, Redis, NATS, leader election, E2E smoke tests, Helm chart, Ansible playbooks. Production: Tier 2 active-active on NL + GR.

### v0.3 — SOS and Safety (Complete, 2026-03-18)

Escalation chains, dead man's switch, SBD fragmentation, message dedup, Apprise notifications, ntfy push.

### v0.4 — Situational Awareness (Complete, 2026-03-18)

GPS position model, Douglas-Peucker simplification, polygon geofencing with escalation integration, map view (Leaflet).

### v0.5 — Connectivity and Federation (Complete, 2026-03-18)

MPTCP concentrator, WireGuard peer management, multi-constellation router (4 strategies).

### v1.0 — Production Hardening (Complete, 2026-03-18)

hawkBit OTA, built-in user management (Argon2id + JWT + refresh tokens), security headers (HSTS/CSP/CORS), OWASP testing, security MCP tools. Tech debt: MQTT reconnect fix, API handler tests, Swagger annotations, goroutine pool.

---

## Post-v1.0 Gap Analysis (2026-03-19 Audit)

A comprehensive code audit revealed that while all v0.1–v1.0 epics are closed, several features have **code that exists but is not wired into the runtime**, and several expected capabilities are **completely missing**.

### Built But Not Wired

| Feature | Package | Gap |
|---------|---------|-----|
| MO fragment reassembly | `internal/fragment/` | `Reassembler` complete but `SetReassembler()` never called in main.go |
| E2E encryption | `internal/crypto/` | AES-256-GCM + per-device keystore complete. Never instantiated. No API for key CRUD. RockBLOCK handler hardcodes `encrypted=false` |
| SOS trigger logic | `internal/escalation/` | Engine + tiers + notifiers all work. But nothing detects SOS messages or publishes to `meshsat/{imei}/sos` |
| Dead man's switch triggers | `internal/deadman/` | Monitor runs but integration with actual device heartbeats unclear |

### Completely Missing

| Feature | Impact |
|---------|--------|
| SMS gateway (inbound + outbound) | Can't reach operators without internet/app |
| PGP Email gateway (inbound + outbound) | No email channel for store-and-forward comms |
| OpenAPI spec generation (swagger.json) | No machine-readable API docs |
| Configurable message routing engine | Fanout is hardcoded MQTT subscriptions, not user-configurable rules |
| MeshSat Simulator / virtual modem | Can't develop or demo without hardware |
| IPoUGRS adapter (IP-over-satellite) | Experimental differentiator, not started |
| Tor device discovery | Field devices can't auto-discover Hub .onion address |
| WireGuard auto-provisioning | Devices don't get WG configs automatically on registration |

---

## v1.1 — Wire the Unwired (Next)

**Goal:** Complete the last-mile integration for features that are built but disconnected. After v1.1, every implemented feature is functional end-to-end.

**Priority:** HIGH — these are silent failures in production (fragments silently dropped, encrypted messages silently rejected, SOS silently ignored).

| Issue | Summary | Depends On | Est. |
|-------|---------|-----------|------|
| MESHSAT-168 | Wire MO fragment reassembly in main.go | — | 2h |
| MESHSAT-169 | Wire E2E encryption: instantiate keystore, decrypt in RockBLOCK handler, add key CRUD API | MESHSAT-168 | 6h |
| MESHSAT-170 | Implement SOS trigger: detect SOS in MO messages, publish to sos topic, fire escalation | — | 4h |
| MESHSAT-171 | Wire dead man's switch to device heartbeats from position/telemetry updates | MESHSAT-170 | 3h |
| ~~MESHSAT-172~~ | ~~Astrocast MO webhook receiver~~ | — | ~~Removed~~ |
| MESHSAT-173 | OpenAPI spec generation with swaggo/swag + CI auto-generate | — | 4h |

**Acceptance:** All unit tests pass. Integration test for fragment reassembly + encrypted MO message. SOS trigger fires escalation chain in test. swagger.json served at /api/docs. (Note: MESHSAT-172 Astrocast webhook removed -- Astrocast discontinued from project.)

---

## v1.2 — Channel Completion — Complete (2026-04-04)

**Goal:** Add the missing communication channels that make Hub a complete multi-channel gateway.

| Issue | Summary | Status |
|-------|---------|--------|
| MESHSAT-174 | SMS gateway: outbound via Twilio/Vonage REST API, inbound via webhook receiver | Done |
| MESHSAT-175 | SMS escalation notifier: wire SMS gateway to escalation engine as Notifier | Done |
| MESHSAT-186 | PGP Email gateway: SMTP outbound + inbound webhook, auto PGP encrypt/decrypt, key management API + Vue UI | Done |
| MESHSAT-176 | WireGuard auto-provisioning: generate peer config on device registration, assign VPN IP | Done |
| MESHSAT-177 | Tor .onion address API: expose Hub's .onion in /api/tor/onion for device provisioning | Done |

**Acceptance:** All channels operational. SMS send+receive via Twilio. PGP email with auto-encryption and key management UI. WG config auto-generated on device creation. .onion address exposed via API.

---

## v1.3 — Routing Engine (After v1.2)

**Goal:** The killer feature — configurable any-to-any message routing with zero-config defaults.

| Issue | Summary | Depends On | Est. |
|-------|---------|-----------|------|
| MESHSAT-178 | Routing engine: per-tenant routing rules (source → destinations) with CRUD API | — | 12h |
| MESHSAT-179 | Default routes: zero-config fanout (satellite → TAK + APRS + webhook + notifications) | MESHSAT-178 | 4h |
| MESHSAT-180 | Routing UI: Vue view for creating/editing/testing routing rules | MESHSAT-178 | 8h |
| MESHSAT-181 | SMS as routing destination: integrate SMS gateway into routing engine | MESHSAT-174, MESHSAT-178 | 3h |
| — | Email as routing destination: integrate PGP email into routing engine | MESHSAT-186, MESHSAT-178 | 3h |

**Acceptance:** New tenant gets default routes that fan out to all enabled channels. Owner can add/remove/modify routes via UI. Any inbound message from any source reaches all configured destinations.

---

## v2.0 — Developer Experience (After v1.3)

**Goal:** Make Hub approachable for developers and the open-source community.

| Issue | Summary | Depends On | Est. |
|-------|---------|-----------|------|
| MESHSAT-182 | MeshSat Simulator: virtual Iridium modem, synthetic MO generator | — | 12h |
| MESHSAT-183 | IPoUGRS adapter: IP-over-Iridium tunnel (experimental, mark as alpha) | — | 16h |
| MESHSAT-184 | Developer onboarding: `make dev` starts Hub + Simulator + all deps, no hardware needed | MESHSAT-182 | 4h |
| MESHSAT-185 | Sensor payload framework: pluggable decoders (ZigBee, LoRa, Protobuf) with registration API | — | 8h |

**Acceptance:** `make dev` starts full stack with simulated devices. GitHub README has working quickstart. Sensor payloads decoded and routed.

---

## Security Track (Ongoing)

| Practice | Frequency |
|----------|-----------|
| gosec + govulncheck in CI | Every push |
| OWASP ZAP on production URLs | Every minor release |
| Dependency audit (CVE + license) | Every new dependency |
| readJSON() for all request bodies | Every new handler |
| Pre-deploy Galera health gate | Every deployment |

---

## Deployment Strategy

### Testing Matrix

| What | Where | How |
|------|-------|-----|
| Unit tests | CI (golang:1.25-alpine) | `go test ./...` — 28 packages |
| Integration tests | CI (embedded MQTT) | `go test -tags=integration` |
| E2E smoke tests | Post-deploy on live cluster | `test/e2e/smoke_test.sh` via `verify` CI stage |
| Playwright browser tests | Post-deploy | 37 tests across 4 categories |

### CI/CD Pipeline

```
lint -> security -> test -> build -> package (GHCR) -> deploy (AWX) -> verify (E2E) -> pages
```

### Production Hosts

| Host | Location | Role |
|------|----------|------|
| nllei01dmz01 | Netherlands | Hub-NL, Galera node 1, garbd, Redis, NATS |
| grskg01dmz01 | Greece | Hub-GR, Galera node 2, Redis, NATS |

### Deployment

Use `/deploy` slash command or Ansible:
```bash
cd deploy/ansible && ansible-playbook -i inventory.yml playbooks/deploy-hub.yml
```
Pre-deploy Galera health gate: `scripts/check-galera-health.sh`
