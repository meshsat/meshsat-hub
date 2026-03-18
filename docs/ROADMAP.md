# MeshSat Hub — Roadmap

**Updated:** 2026-03-18
**YouTrack:** Project `MESHSAT`, issues prefixed `MESHSAT-`

---

## Tri-Mode Architecture

MeshSat Hub is designed as a **3-tier deployment system**, selectable at startup via `HUB_MODE`:

```
Tier 1: STANDALONE           Tier 2: CLUSTER              Tier 3: KUBERNETES
(single node, edge)          (multi-host HA)              (cloud-native, auto-scale)

+----------+                 +----------+ +----------+   +---+ +---+ +---+
|   Hub    |                 |  Hub-NL  | |  Hub-GR  |   |Hub| |Hub| |Hub|
| (SQLite) |                 | (PG+Redis) (PG+Redis) |   +---+ +---+ +---+
|Mosquitto |                 |  NATS <-----> NATS    |      |     |     |
+----------+                 +----------+ +----------+   +--PostgreSQL--+
                                                         +----Redis-----+
                                                         +--NATS (x3)---+
```

| Tier | Mode | Store | Bus | Dedup/RL | Leader | Status |
|------|------|-------|-----|----------|--------|--------|
| 1 | `standalone` | SQLite | Mosquitto | In-memory | Noop | **Production** |
| 2 | `cluster` | PostgreSQL | NATS+MQTT | Redis | NATS queues | **Production** (2 nodes) |
| 3 | `kubernetes` | PostgreSQL | NATS StatefulSet | Redis | k8s Lease API | **Code complete**, untested |

**Current production:** Tier 2 cluster across `nllei01dmz01` (NL) + `grskg01dmz01` (GR).

---

## Version History

### v0.1 — Foundation (Complete)

Infrastructure, MQTT, SBD (MO + MT), SMAZ2 compression, Tor hidden service, health probes, integration tests, deployment documentation.

### v0.2 — Multi-Tenant Device Management (Complete, 2026-03-18)

| Issue | Summary | Status |
|-------|---------|--------|
| MESHSAT-94 | OAuth2/OIDC authentication | Done |
| MESHSAT-96 | Tenant isolation middleware | Done |
| MESHSAT-97 | API keys + RBAC (viewer/operator/owner) | Done |
| MESHSAT-98 | Device registry CRUD | Done |
| MESHSAT-99 | Device config versioning | Done |
| MESHSAT-100 | Cloudloop credit balance polling | Done |
| MESHSAT-101 | Per-device daily + monthly budget limits | Done |
| MESHSAT-102 | Tamper-evident SHA-256 hash-chain audit log | Done |
| MESHSAT-103 | Vue.js dashboard scaffold (Tailwind, Pinia, auth) | Done |
| MESHSAT-104-106 | Enhanced device/message/credit views | Done |
| MESHSAT-148 | API key management UI + role-based nav | Done |

### v0.2 — Cluster Infrastructure Sprint (Complete, 2026-03-18)

| Issue | Summary | Status |
|-------|---------|--------|
| MESHSAT-152 | Enhanced `/readyz` with live dependency probes (PG, Redis, MQTT) | Done |
| MESHSAT-149 | `docker-compose.cluster.yml` (PG 16, Redis 7, NATS 2.10, Hub x2, Caddy) | Done |
| MESHSAT-151 | Kubernetes Lease API leader election (`internal/leader/kubelease.go`) | Done |
| MESHSAT-150 | Post-deploy E2E smoke tests + `verify` CI stage | Done |
| MESHSAT-153 | Kubernetes raw manifests (`deploy/k8s/`) + Helm chart (`deploy/helm/`) | Done |

**Bug fixes during deployment:**
- Paho MQTT client defaulted to v3.1 protocol — NATS requires v3.1.1 (`SetProtocolVersion(4)`)
- MQTT health check was one-shot `Set()` — converted to live `AddProbe()` for reconnect awareness
- Duplicate MQTT client IDs across NATS cluster caused session flapping — unique IDs per host required

### v0.3 — SOS and Safety (In Progress)

| Issue | Summary | Status |
|-------|---------|--------|
| MESHSAT-107 | SOS escalation chains | Open |
| MESHSAT-108 | Dead man's switch | Open |
| MESHSAT-109 | SBD fragmentation (compatible with bridge) | Open |
| MESHSAT-110 | Message dedup queue | Open |
| MESHSAT-111 | E2E encryption key management | Open |
| MESHSAT-112 | Apprise multi-channel notifications | Open |
| MESHSAT-113 | ntfy push notifications | Open |

### Future

- **v0.4:** Map view (Leaflet/MapLibre), geofencing, position history
- **v0.5:** Multi-constellation (Astrocast Astronode S)
- **v1.0:** Stable API, production hardening, documentation

---

## Deployment Strategy

### Testing Matrix

| What | Where | How |
|------|-------|-----|
| Unit tests | CI (golang:1.25-alpine) | `go test ./...` — 16 packages |
| Integration tests | CI (embedded MQTT) | `go test -tags=integration` |
| E2E smoke tests | Post-deploy on live cluster | `test/e2e/smoke_test.sh` via `verify` CI stage |
| Manual QA | Both DMZ hosts via SSH | docker exec + alpine/curl |

### CI/CD Pipeline

```
lint -> test -> build -> package (GHCR) -> deploy (AWX) -> verify (E2E) -> pages
```

AWX template ID 57 deploys to both DMZ hosts at `/srv/meshsat-hub/` as user `kyriakosp`.

### Production Hosts

| Host | Location | IP | Role |
|------|----------|-----|------|
| nllei01dmz01 | Netherlands | 192.168.x.x | Hub-NL + NATS-NL |
| grskg01dmz01 | Greece | 192.168.x.x | Hub-GR + NATS-GR |

NATS instances are clustered via route connections over WireGuard, providing cross-site MQTT message replication. Each Hub instance uses a unique MQTT client ID (`meshsat-hub-nl` / `meshsat-hub-gr`).

---

## File Reference

| File | Purpose |
|------|---------|
| `docker-compose.yml` | Development (build from source) |
| `docker-compose.prod.yml` | Tier 1 standalone production |
| `docker-compose.cluster.yml` | Tier 2 cluster (single-host, both instances) |
| `Caddyfile.cluster` | Caddy config for cluster LB |
| `nats.conf` | NATS server config with MQTT adapter + JetStream |
| `.env.cluster.example` | Template for cluster env vars |
| `scripts/migrate-sqlite-to-pg.sh` | SQLite to PostgreSQL data migration |
| `test/e2e/smoke_test.sh` | Post-deploy E2E smoke tests |
| `deploy/k8s/` | Raw Kubernetes manifests |
| `deploy/helm/meshsat-hub/` | Helm chart with `values.yaml` |
