# MeshSat Hub

Self-hosted management platform for satellite-connected field devices. Ingests messages from Iridium and Astrocast constellations, provides a web dashboard with live mapping, and bridges traffic to TAK, APRS-IS, webhooks, and push notifications. Designed for search-and-rescue, remote monitoring, and off-grid communications where reliability matters more than features.

Runs as a single Docker Compose stack or an active-active cluster with MariaDB Galera synchronous replication.

## What it does

Hub sits between satellite ground stations and operators. Field devices (running [MeshSat Bridge](https://github.com/cubeos-app/meshsat) or the [Android app](https://github.com/cubeos-app/meshsat-android)) send compressed binary messages over Iridium or Astrocast. Ground Control delivers those messages to Hub via webhook. Hub decompresses, decodes, stores, and fans out each message to every configured output: the web dashboard, a TAK server, APRS-IS, outbound webhooks, and notification services.

The reverse path works too. Operators send commands from the dashboard or API; Hub compresses, optionally encrypts, fragments if needed, and submits to the satellite provider's REST API for delivery on the next pass.

```
Field Device                                             Operators
  (Iridium 9603N)                                    (Dashboard, TAK, APRS)
       |                                                      ^
       v                                                      |
  Iridium Constellation                              MeshSat Hub
       |                                              ├─ RockBLOCK webhook
       v                                              ├─ Cloudloop MT API
  Ground Control ──webhook──> Hub ──MQTT──> TAK Server
                                       └──> APRS-IS
                                       └──> Webhooks
                                       └──> Apprise/ntfy
```

## Features

**Satellite Communications** --
Iridium SBD via RockBLOCK/Cloudloop webhook (MO) and REST API (MT). Astrocast Astronode S via REST API. Multi-constellation router with four selection strategies (cheapest, fastest, available, preferred). SMAZ2 compression with Meshtastic-compatible dictionary. SBD fragmentation and reassembly. Message deduplication.

**Device Management** --
Multi-tenant device registry with per-device YAML config versioning. Per-device daily and monthly rate limiting with SOS bypass. Iridium credit balance polling and budget tracking. OTA firmware management via Eclipse hawkBit.

**Safety** --
SOS escalation chains with multi-tier notification through Apprise (90+ services) and ntfy. Dead man's switch with configurable check-in intervals and grace periods. Polygon geofencing engine with enter/exit/both triggers and automatic escalation on breach. Tamper-evident SHA-256 hash-chain audit log with independent verification.

**Situational Awareness** --
GPS position storage with time-range queries and Douglas-Peucker track simplification. Leaflet map view with live device positions. TAK/CoT gateway (bidirectional TCP/TLS to OpenTAK Server). APRS-IS IGate with rate-limited position injection and inbound message forwarding.

**Authentication and Authorization** --
Built-in user management with Argon2id password hashing, JWT access tokens, and rotating refresh tokens. API keys with SHA-256 hashing and RBAC (viewer, operator, owner). OIDC/OAuth2 support for enterprise SSO. Per-IP login rate limiting and account lockout.

**Networking** --
Tor v3 hidden service for CGNAT bypass (HTTP + MQTT on .onion). WireGuard peer management via wg-easy REST API. MPTCP concentrator for satellite + cellular link aggregation.

**Outbound Integration** --
Configurable outbound webhooks with HMAC-SHA256 signing, retry with exponential backoff, and delivery logging. Apprise (Slack, email, Telegram, SMS via Twilio/Vonage, 90+ services). ntfy self-hosted push notifications.

**Dashboard** --
Vue 3 SPA with 17 views: devices, messages, map, escalation chains, dead man's switch, device config, notifications, webhooks, OTA, users, API keys, audit log, cluster health, network, and settings. Tailwind CSS, dark theme, all timestamps in UTC 24h.

## Deployment

Three deployment tiers, selected at startup via `HUB_MODE`:

| Tier | Mode | Database | Message Bus | Use Case |
|------|------|----------|-------------|----------|
| 1 | `standalone` | SQLite | Mosquitto | Single server, development, edge |
| 2 | `cluster` | MariaDB Galera | NATS | Production (active-active, multi-site) |
| 3 | `kubernetes` | MariaDB Galera | NATS StatefulSet | Cloud-native, auto-scaling |

### Standalone

```bash
cp config.example.yaml config.yaml
# Edit: set rockblock_secret, jwt_signing_key
docker compose up -d
curl http://localhost:6070/healthz
```

The dashboard is served at `http://localhost:6070`. Create the first user via the API:

```bash
curl -X POST http://localhost:6070/api/users \
  -H "Authorization: Bearer $HUB_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"email":"admin@example.com","password":"...","role":"owner"}'
```

### Cluster

Requires two or more hosts with VPN connectivity and Ansible. See [docs/deployment.md](docs/deployment.md) for the full guide and [docs/RUNBOOK.md](docs/RUNBOOK.md) for operational procedures.

```bash
cd deploy/ansible
cp group_vars/vault.yml.example group_vars/vault.yml
# Edit vault.yml with MariaDB, JWT, and auth credentials
ansible-playbook -i inventory.yml playbooks/bootstrap.yml --ask-vault-pass
```

Rolling updates deploy the Hub container without touching the database:

```bash
ansible-playbook -i inventory.yml playbooks/deploy-hub.yml
```

### Kubernetes

Helm chart and raw manifests in `deploy/k8s/` and `deploy/helm/`. Code complete, not yet tested in production.

## Architecture

```
              hub.meshsat.net (HAProxy)
                     |
            +--------+--------+
            v                 v
      NL (primary)      GR (secondary)
      +-- Hub (6070)    +-- Hub (6070)
      +-- MariaDB  <--> +-- MariaDB       Galera sync replication
      +-- garbd         +-- Redis
      +-- Redis         +-- NATS
      +-- NATS          +-- nginx (:8451)
      +-- nginx (:8451)
```

Each Hub instance is stateless. MariaDB Galera provides synchronous multi-master replication with a third-node arbitrator (garbd) for quorum. NATS handles cross-site MQTT topic replication. Redis provides distributed rate limiting and deduplication. Leader election (via NATS or Kubernetes Lease API) ensures singleton tasks (TAK subscriber, APRS-IS IGate, credit poller) run on exactly one node.

## Configuration

Hub reads `config.yaml` (path: `HUB_CONFIG_FILE`) with environment variable overrides (prefix `HUB_`):

| Variable | Default | Description |
|----------|---------|-------------|
| `HUB_MODE` | `standalone` | Deployment tier: `standalone`, `cluster`, `kubernetes` |
| `HUB_PORT` | `6070` | HTTP listen port |
| `HUB_DATABASE_URL` | — | MariaDB DSN (cluster/kubernetes mode) |
| `HUB_JWT_SIGNING_KEY` | — | HMAC-SHA256 key for JWT tokens (min 32 chars) |
| `HUB_AUTH_TOKEN` | — | Static bearer token (standalone fallback) |
| `HUB_ROCKBLOCK_SECRET` | — | RockBLOCK webhook HMAC secret |
| `HUB_CLOUDLOOP_API_KEY` | — | Cloudloop REST API key (Iridium MT) |
| `HUB_ASTROCAST_API_KEY` | — | Astrocast REST API key |
| `HUB_MQTT_BROKER_URL` | `tcp://mqtt:1883` | MQTT broker connection |
| `HUB_CLUSTER_PEERS` | — | Comma-separated peer Hub URLs |
| `HUB_APPRISE_URL` | — | Apprise API URL for notifications |
| `HUB_NTFY_URL` | — | ntfy server URL for push notifications |
| `HUB_HAWKBIT_URL` | — | hawkBit server URL for OTA |
| `HUB_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `HUB_LOG_FORMAT` | `json` | `json` or `text` |

## Development

Prerequisites: Go 1.24+, Node.js 20+, Docker Compose v2.

```bash
make build              # Build Go binary
make test               # Unit tests (486 tests, 28 packages)
make test-integration   # Integration tests (embedded MQTT broker)
make lint               # golangci-lint
make security           # gosec + govulncheck
make fmt                # gofmt

cd web && npm install
cd web && npm run build       # Build Vue SPA to cmd/meshsat-hub/web/dist/
cd web && npm run test:e2e    # Playwright browser tests (37 tests)
```

## Project Layout

```
meshsat-hub/
+-- cmd/meshsat-hub/            Entry point, router, graceful shutdown
|   +-- web/dist/               Embedded Vue SPA (committed)
+-- internal/
|   +-- api/                    REST handlers + request validation (readJSON)
|   +-- apprise/                Apprise notification client
|   +-- aprsis/                 APRS-IS IGate (bidirectional)
|   +-- astrocast/              Astrocast Astronode S API client
|   +-- audit/                  SHA-256 hash-chain audit log
|   +-- auth/                   Auth middleware, Argon2id, JWT, RBAC, API keys
|   +-- backup/                 Full-state ZIP export, diff, import
|   +-- bus/                    MQTT message bus (Paho client)
|   +-- cloudloop/              Cloudloop/Iridium MT sender + credit poller
|   +-- cluster/                Galera cluster health + node discovery
|   +-- compress/               SMAZ2 compression (Meshtastic dictionary)
|   +-- config/                 YAML + env config loading
|   +-- constellation/          Multi-constellation router (4 strategies)
|   +-- crypto/                 AES-256-GCM encryption + per-device keystore
|   +-- deadman/                Dead man's switch monitor
|   +-- dedup/                  Message deduplication (memory + Redis)
|   +-- escalation/             SOS escalation engine (multi-tier)
|   +-- fragment/               SBD fragmentation/reassembly
|   +-- geo/                    GPS codec, geofencing, Douglas-Peucker
|   +-- handler/                Shared handler utilities
|   +-- hawkbit/                Eclipse hawkBit OTA client
|   +-- health/                 /healthz and /readyz with live probes
|   +-- leader/                 Leader election (Noop, NATS, KubeLease)
|   +-- mptcp/                  MPTCP concentrator
|   +-- mqtt/                   MQTT topic namespace helpers
|   +-- ntfy/                   ntfy push notification client
|   +-- position/               GPS position storage + subscriber
|   +-- ratelimit/              Per-device rate limiting (memory + Redis)
|   +-- rockblock/              RockBLOCK MO webhook handler
|   +-- store/                  Store interface + SQLite + MariaDB impls
|   +-- tak/                    TAK/CoT gateway (bidirectional)
|   +-- webhook/                Outbound webhook dispatcher
|   +-- wireguard/              WireGuard peer management
+-- web/                        Vue 3 SPA source (17 views)
+-- deploy/
|   +-- ansible/                Playbooks: bootstrap, deploy, add-node, recover
|   +-- k8s/                    Kubernetes manifests
|   +-- helm/meshsat-hub/       Helm chart
+-- test/
|   +-- integration/            End-to-end tests (embedded MQTT)
|   +-- e2e/                    Post-deploy smoke tests
+-- docs/
|   +-- deployment.md           Tier 1/2/3 deployment guide
|   +-- RUNBOOK.md              Operational runbook
|   +-- ROADMAP.md              Version history + planned work
|   +-- SECURITY_AUDIT.md       SAST/SCA/OWASP findings
+-- docker-compose.yml          Development
+-- docker-compose.prod.yml     Standalone production
+-- docker-compose.cluster.yml  Cluster (Galera + Redis + NATS)
+-- Dockerfile                  Multi-stage Alpine build
+-- Makefile                    Build, test, lint, security targets
+-- .gitlab-ci.yml              7-stage CI/CD pipeline
```

## CI/CD Pipeline

```
lint --> security --> test --> build --> package --> deploy --> verify
```

The `security` stage runs gosec (SAST) and govulncheck (known CVEs) on every push. The `verify` stage runs Playwright browser tests and E2E smoke tests against the live cluster after deployment.

## Documentation

- [Deployment Guide](docs/deployment.md) -- Tier 1/2/3 setup instructions
- [Operational Runbook](docs/RUNBOOK.md) -- Cluster operations, Galera recovery, troubleshooting
- [Roadmap](docs/ROADMAP.md) -- Version history and planned work
- [Security Audit](docs/SECURITY_AUDIT.md) -- SAST, SCA, and OWASP scan results

## Related Projects

| Project | Description |
|---------|-------------|
| [MeshSat Bridge](https://github.com/cubeos-app/meshsat) | Field gateway firmware (Go, Raspberry Pi) -- connects Meshtastic mesh radios to Iridium/Astrocast satellites |
| [MeshSat Android](https://github.com/cubeos-app/meshsat-android) | Mobile gateway app (Kotlin) -- phone-as-bridge with BLE mesh and SPP Iridium |

## License

[Apache 2.0](LICENSE)
