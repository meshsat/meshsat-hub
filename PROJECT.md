# Project Charter — MeshSat Hub

> Operator-facing charter. Vision, mission, scope, deliverables, success metrics. Authored 2026-05-17 by drawing from the existing strategic documents listed in **Source Trace** at the foot of this file. The machine-readable companion is `PROJECT.json`. Hard rules live in `constitution.md`. Decisions live in `adr/`.

---

## Vision

A **self-hosted, off-grid-capable satellite-communications gateway** that lets search-and-rescue teams, remote operators, and field deployments stay reachable when nothing else works — without depending on a vendor cloud or a monthly seat fee.

Source: `README.md` L3 ("for search-and-rescue, remote monitoring, and off-grid communications where reliability matters more than features").

## Mission

Sit between satellite ground stations (Iridium / Globalstar) and operators, ingest compressed binary messages from field devices, decode + store + fan out every message to every configured output (dashboard, TAK, APRS-IS, webhooks, push notifications), and deliver the reverse path with the same reliability — so a SOS event from a `RockBLOCK 9704` on the ocean reaches a watchstander before the next satellite pass completes.

Source: `README.md` L11–L20.

## Scope (in / out)

**In scope:**
- Multi-tenant SaaS management platform for satellite-connected field devices
- Three deployment tiers: `standalone` (Tier 1 — single-host Docker Compose), `cluster` (Tier 2 — active-active MariaDB Galera across geographically distributed sites), `kubernetes` (Tier 3 — Helm + StatefulSets, code-complete but untested). Mode selected at startup via `HUB_MODE`.
- Bridge lifecycle: auto-provisioning on first contact, MQTT credential generation (bcrypt), TLS client certificate issuance (Hub-as-CA, ECDSA P-256, 90-day expiry).
- Multi-channel routing: MQTT, satellite SBD/IMT, TAK/CoT, APRS-IS, SMS (Twilio/Vonage), PGP email, webhooks, ntfy, Apprise (90+ services).
- Reticulum transport relay (announces + routing table, wire-format compatible with Reticulum Network Stack).
- Triple-layer tenant isolation: storage + transport + identity.
- Tamper-evident SHA-256 hash-chain audit log with independent verification.

Source: `README.md` L40–L77 (Features), `docs/ROADMAP.md` L10–L31 (Tri-Mode Architecture).

**Out of scope** (lives in sibling repos):
- Bridge code — serial AT commands, direct Meshtastic protobuf, USB hotplug, ZigBee, GPIO, Iridium serial-mutex — belongs in [`meshsat`](../meshsat/) (project 27).
- Android-specific code — Jetpack Compose UI, BLE peripheral, Android paths — belongs in [`meshsat-android`](../meshsat-android/) (project 31).
- Field-device firmware (Meshtastic, ESP32 sketches) — third-party.

Source: `constitution.md` Article I (Identity boundary).

## Current production state (as of 2026-05-17)

| Dimension | State | Source |
|---|---|---|
| Version | **v1.7** — Fleet management UI, full observability stack, mTLS bridge authentication, Reticulum transport relay | `README.md` L7 |
| Deployment | **Tier 2 cluster** active-active across `nllei01dmz01` (NL) + `grskg01dmz01` (GR), garbd arbitrator on NL | `docs/ROADMAP.md` L31, `docs/RUNBOOK.md` L19–L28 |
| Galera | 3-voter quorum (2 data + 1 garbd), synchronous replication, HAProxy SNI-passthrough at the edge | `docs/RUNBOOK.md` L30–L33, `docs/deployment.md` L468–L520 |
| Public surface | `hub.meshsat.net` via HAProxy LB | `docs/RUNBOOK.md` L26 |
| Tier 3 (Kubernetes) | Code complete, untested in production | `docs/ROADMAP.md` L29 |

## Active priorities (next 4–8 weeks)

The 2026-03-19 post-v1.0 audit (in `docs/ROADMAP.md` L67–L92) found that **several v0.1–v1.0 features are coded but not wired into the runtime** — they exist in the package, but `main.go` never instantiates them. The next phase closes that integration gap **before** adding new features, because every "built-but-not-wired" item is a silent failure in production.

| Phase | Goal | Status |
|---|---|---|
| **v1.1 — Wire the Unwired** | Complete last-mile integration: fragment reassembly (MESHSAT-168), E2E encryption (MESHSAT-169), SOS trigger logic (MESHSAT-170), dead-man's-switch heartbeats (MESHSAT-171), OpenAPI spec generation (MESHSAT-173) | **In progress — highest priority** |
| **v1.2 — Channel completion** | SMS + PGP email + WG auto-provisioning + Tor .onion API (MESHSAT-174/175/186/176/177) | **Complete (2026-04-04)** |
| **v1.3 — Routing engine** | Any-to-any per-tenant routing with default zero-config fanout + Vue UI (MESHSAT-178/179/180/181) | Planned after v1.1 |
| **v2.0 — Developer experience** | MeshSat Simulator (virtual modem), IPoUGRS adapter, `make dev` quickstart, sensor payload framework (MESHSAT-182/183/184/185) | Planned after v1.3 |

Source: `docs/ROADMAP.md` L95–L156, `docs/EXECUTION_PLAN.md` (per-task breakdown).

**Why integration before new features?** Silent failures are worse than missing features. A user pointing at a "feature checkbox" that doesn't actually fire (encrypted MO silently rejected, SOS silently ignored, fragments silently dropped) is a safety regression dressed up as completeness.

Source: `docs/ROADMAP.md` L99 ("Priority: HIGH — these are silent failures in production").

## Success metrics

| Dimension | Metric | Source |
|---|---|---|
| Reliability | `/readyz` 200 on each node for ≥99.5% of any 30-day window; `wsrep_cluster_size = 3` (2 data + garbd) maintained | `docs/RUNBOOK.md` L146–L184 |
| Performance | MO ingest → MQTT fanout p99 < 800ms (mirrors bridge pair-handshake budget) | `internal/metrics/` (~25 metric families per `README.md` L70) |
| Security | gosec + govulncheck HIGH-severity findings = 0; OWASP ZAP scan run per `v0.X` release; readJSON() on every handler | `docs/SECURITY_AUDIT.md` §3+§9+§11, `constitution.md` Article III |
| Tenant isolation | Zero cross-tenant data leak findings under tenancy fuzzing | `constitution.md` Article IV (triple-layer) |
| Audit integrity | Hash-chain verification endpoint returns valid for entire retention window | `constitution.md` Article VIII |
| Galera | `wsrep_flow_control_paused < 0.5` averaged over any rolling 10-min window | `docs/RUNBOOK.md` L183 |

## Non-goals

- Replacing satellite providers (Hub is a concentrator + decoder; RockBLOCK / Cloudloop / Globalstar remain the carriers).
- Becoming a Meshtastic-replacement mesh stack — Meshtastic bindings are consumed, not reimplemented (`compress/` mirrors Meshtastic's SMAZ2 dict byte-for-byte per `constitution.md` Article VI).
- Multi-region active-active beyond two sites in Tier 2 — Galera at 3+ data nodes has known WAN-replication pathology; expand horizontally via NATS leaf federation instead.
- Browser/mobile SDK — Hub is the API surface; clients consume `/api`. SDKs are deferred until v2.0+.

## Stakeholders

- **Owner:** `ufwtqkgz@meshsat.net` (sole operator, also bridge + android repos)
- **Users:** SAR teams, off-grid operators, remote monitoring deployments — see `README.md` L3.
- **Adjacent projects:**
  - [`meshsat`](../meshsat/) — field-side gateway firmware (bridge)
  - [`meshsat-android`](../meshsat-android/) — phone-as-bridge companion
  - [Meshtastic](https://meshtastic.org) — upstream LoRa mesh stack (consumed)
  - RockBLOCK, Cloudloop, Globalstar — satellite carriers (HTTP webhook + REST API)
- **Compliance targets:** WCAG 2.1 AAA (MESHSAT-567 in-progress), OWASP ZAP per release, FIPS-140-3 deferred (MESHSAT-573 — blocked by CGO + BoringSSL trade-off, see Article V).

## Architectural pillars (linked artifacts)

Each pillar has a citable artifact — strategic document, ADR, or constitution article. **Read these before changing the surface area.**

| Pillar | Authoritative source |
|---|---|
| Identity boundary (what belongs / does NOT belong here) | `constitution.md` Article I |
| Security-first priority above features | `adr/0002-security-first-above-features.md` |
| Tri-mode deployment (standalone / cluster / kubernetes) | `adr/0005-tri-mode-deployment-selectable-via-hub-mode.md` + `docs/ROADMAP.md` L10–L31 + `docs/deployment.md` |
| Galera safety + flag-file bootstrap | `adr/0003-galera-bootstrap-via-flag-file.md` + `constitution.md` Article XI + `docs/RUNBOOK.md` L40–L142 |
| Bridge-Hub wire protocol (Sparkplug B BIRTH/DEATH/DATA) | `adr/0006-sparkplug-b-bridge-hub-protocol.md` + `docs/bridge-hub-protocol.md` |
| Encryption model (static AES-256 hub↔device + X25519 ECDH bridge↔bridge) | `adr/0007-two-key-encryption-model.md` + `docs/ENCRYPTION.md` |
| Hub-as-CA + HAProxy SNI passthrough for mTLS | `adr/0008-hub-as-ca-haproxy-sni-passthrough.md` + `docs/deployment.md` L468–L520 |
| Triple-layer tenant isolation (storage + transport + identity) | `constitution.md` Article IV |
| Audit log (SHA-256 hash chain) | `constitution.md` Article VIII + `docs/SECURITY_AUDIT.md` §4 |
| Webhook HMAC verification before processing | `constitution.md` Article IX + `docs/SECURITY_AUDIT.md` §5 |
| Single-source migrations (sqlite/* + mariadb.go in lockstep) | `constitution.md` Article XII |
| Parallel-dev workflow override | `adr/0004-parallel-dev-workflow-override.md` + `constitution.md` Article XV |

## Where decisions live

| Type of change | Where it gets recorded |
|---|---|
| New architectural commitment | New `adr/NNNN-<slug>.md` (MADR template); link from this charter's pillar table |
| Tightening / loosening a security rule | New ADR + edit to relevant `constitution.md` Article |
| Operational gotcha for ops/oncall | New entry in `docs/RUNBOOK.md` |
| Deployment-tier procedural change | Edit `docs/deployment.md` + (if invariant) new ADR |
| Wire-protocol contract change | Edit `docs/bridge-hub-protocol.md` + bump `protocol` version field per L327 of that doc + new ADR if BREAKING |
| Roadmap update | Edit `docs/ROADMAP.md` (not this file) + edit "Active priorities" section above if reordered |
| Routing / disambiguation | Edit `ROUTING.md` (existing file) |

## What this charter is NOT

- **Not** a roadmap — see `docs/ROADMAP.md` (versioned).
- **Not** a runbook — see `docs/RUNBOOK.md` (per-scenario recovery).
- **Not** a deployment guide — see `docs/deployment.md` (per-tier setup).
- **Not** the wire protocol — see `docs/bridge-hub-protocol.md` (per-topic format).
- **Not** the security audit — see `docs/SECURITY_AUDIT.md` (per-finding evidence).

This charter is the load-bearing summary that should let an incoming engineer (human or agent) understand WHAT the project is, WHY it exists, and WHERE the canonical source for each operational decision lives — without re-reading every doc.

---

## Source Trace

Every fact in this charter is sourced from a strategic document in this repo. Concrete references:

| Statement in charter | Source file | Line range |
|---|---|---|
| Vision: SAR + remote + off-grid use cases | `README.md` | L3 |
| Mission: ingest → decode → fan out → reverse-path | `README.md` | L11–L20 |
| 3-tier architecture (HUB_MODE) | `docs/ROADMAP.md` | L10–L31 |
| Current production: Tier 2 NL + GR | `docs/ROADMAP.md` | L31 |
| Tier 3 (k8s) is code-complete, untested | `docs/ROADMAP.md` | L29 |
| v0.1–v1.0 epic timeline | `docs/ROADMAP.md` | L35–L65 |
| Built-but-not-wired audit findings | `docs/ROADMAP.md` | L67–L92 |
| v1.1 task breakdown (MESHSAT-168..173) | `docs/EXECUTION_PLAN.md` | L8–L65 |
| v1.2 complete + closed dates | `docs/EXECUTION_PLAN.md` | L69–L113 |
| v1.3 routing-engine plan | `docs/EXECUTION_PLAN.md` | L116–L145 |
| v2.0 simulator + IPoUGRS + sensor-framework | `docs/EXECUTION_PLAN.md` | L147–L153 |
| Critical-path dependency graph | `docs/EXECUTION_PLAN.md` | L158–L173 |
| Sparkplug B BIRTH/DEATH/DATA pattern | `docs/bridge-hub-protocol.md` | L13–L17 |
| CoT-native type mapping | `docs/bridge-hub-protocol.md` | L239–L251 |
| Progressive auth (Phase 1/2/3) | `docs/bridge-hub-protocol.md` | L306–L320 |
| Two-key encryption model rationale | `docs/ENCRYPTION.md` | L4–L94 |
| "Why not X25519 for hub↔device" | `docs/ENCRYPTION.md` | L25–L30 |
| Audit log SHA-256 hash chain | `docs/SECURITY_AUDIT.md` | §4 (L60–L70) |
| readJSON() on every handler | `docs/SECURITY_AUDIT.md` | §5 (L77–L87) |
| OWASP ZAP recurring per release | `docs/SECURITY_AUDIT.md` | §11 (L148–L193) |
| Galera 3-voter quorum (2 data + garbd) | `docs/RUNBOOK.md` | L30–L33 |
| Production hosts NL + GR DMZ | `docs/RUNBOOK.md` | L19–L28 |
| Galera "NEVER" safety rules | `docs/deployment.md` | L346–L356 |
| mTLS bridge auth (Hub-as-CA, ECDSA P-256, 90d) | `docs/deployment.md` | L468–L520 |
| HAProxy SNI passthrough | `docs/deployment.md` | L468–L488 |
| Feature inventory (21 Vue views, 42 packages) | `README.md` | L75–L77, L170 |
| Reticulum transport relay current | `README.md` | L7, L60–L62 |
