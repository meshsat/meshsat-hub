# Project Charter — MeshSat Hub

> Component-scoped charter. Parent: `/home/claude-runner/gitlab/products/cubeos/docs/PROJECT.md` (Track D). CGC-grounded 2026-05-18.

## Role in the MeshSat family

`meshsat-hub` (GitLab project 35) is the **cloud counterpart** to MeshSat Bridge devices deployed in the field. While the Bridge runs on a Pi and handles direct radio/satellite/cellular transports, Hub runs as a multi-tenant SaaS platform that:

- Receives satellite messages (Iridium SBD via RockBLOCK/Ground Control webhooks; Globalstar/Cloudloop)
- Sends Mobile Terminated (MT) messages back to field devices
- Bridges field-device telemetry into MQTT, TAK, notifications (Apprise/ntfy/Email/SMS)
- Provides a web dashboard for device management, message logs, situational awareness
- Terminates WireGuard/Tor tunnels from field devices for secure backhaul

## CGC-verified scope (2026-05-18)

- 357 files / 9936 functions / 972 classes / 40 modules / **53+ internal packages**
- Entry points: `cmd/meshsat-hub/main.go` (Hub service) + `cmd/meshsat-sim/main.go` (simulator)
- Strict-decode helper: `internal/httpjson/readjson.go` (Article III)
- Storage split: `internal/store/sqlite/{sqlite,bridges,bond_groups}.go` + `internal/store/mariadb/mariadb.go` (Article XII)
- Cloudloop IP-allowlist (no HMAC): `internal/config/config.go` `DefaultCloudloopWebhookIPs` + `ResolveCloudloopAllowedIPs` (Article IX carve-out)
- Tri-mode deployment: `internal/config/config.go` `HUB_MODE` env ∈ {standalone, cluster, kubernetes}
- 112 test files

## What this repo owns

1. **REST API surface** + Swagger spec
2. **MQTT broker integration** for Bridge↔Hub `meshsat-uplink/v1` Sparkplug-B-inspired protocol
3. **Tenant isolation** at storage + transport + identity layers (per-tenant SQLite OR Galera schema; MQTT topic ACL `meshsat/{tenant_id}/{device_id}/...`; JWT tenant_id claim)
4. **Bridge lifecycle** (Reaper + 5-min-threshold stale-birth detection)
5. **External adapters** — Cloudloop webhook, Globalstar webhook, Rock7, hawkbit (firmware-OTA), TAK, MQTT, Apprise/ntfy/Email/SMS notifications, WireGuard, Tor, MPTCP, IPoUGRS
6. **Routing engine** (v1.3) — per-rule message routing across providers
7. **Crypto** — E2E encryption + TLS pinning + key management
8. **Compression** — SMAZ2 + MSVQ-SC for bandwidth-constrained transports
9. **Audit log** — SHA-256 hash chain tamper-evident
10. **Cluster coordination** — NATS leader election (Tier 2) / k8s Lease (Tier 3)

## Constitutional inheritance

Inherits CubeOS project-level constitution. Component constitution adds 17 articles (security #1, tenant isolation, etc.).

## Source trace

- `meshsat-hub/CLAUDE.md` (local-only)
- `meshsat-hub/docs/ROADMAP.md` + `docs/EXECUTION_PLAN.md`
- CGC audit: not separately authored — CGC index queries cover this repo
- Parent: `docs/spec/008-network-modes/` + the rest of docs/ describe the substrate
