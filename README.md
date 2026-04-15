# MeshSat Hub

Self-hosted management platform for satellite-connected field devices. Ingests messages from Iridium constellations, provides a web dashboard with live mapping, and bridges traffic to TAK, APRS-IS, webhooks, and push notifications. Designed for search-and-rescue, remote monitoring, and off-grid communications where reliability matters more than features.

Runs as a single Docker Compose stack or an active-active cluster with MariaDB Galera synchronous replication across geographically distributed sites.

**Current version: v1.7** -- Fleet management UI, full observability stack, mTLS bridge authentication, Reticulum transport relay.

## What it does

Hub sits between satellite ground stations and operators. Field devices (running [MeshSat Bridge](https://github.com/cubeos-app/meshsat) or the [Android app](https://github.com/cubeos-app/meshsat-android)) send compressed binary messages over Iridium. Ground Control delivers those messages to Hub via webhook. Hub decompresses, decodes, stores, and fans out each message to every configured output: the web dashboard, a TAK server, APRS-IS, outbound webhooks, and notification services.

The reverse path works too. Operators send commands from the dashboard or API; Hub compresses, optionally encrypts, fragments if needed, and submits to the satellite provider's REST API for delivery on the next pass.

```
Field Device                                            Operators
  (Iridium 9603N)                                   (Dashboard, TAK, APRS)
       |                                                     ^
       v                                                     |
  Iridium Constellation                             MeshSat Hub
       |                                             +-- RockBLOCK webhook (MO)
       v                                             +-- Cloudloop MT API
  Ground Control --webhook--> Hub --MQTT--> Dashboard
                                      +--> TAK Server
                                      +--> APRS-IS
                                      +--> Webhooks
                                      +--> Apprise/ntfy

  Bridges connect directly via:
    mTLS WebSocket (wss://mqtt.example.com/mqtt)
    |   or Tor hidden service (.onion:1883)
    |   or WireGuard tunnel
    v
  NATS (MQTT adapter + JetStream persistence)
```

## Features

**Satellite Communications** --
Iridium SBD via RockBLOCK/Cloudloop webhook (MO) and REST API (MT). Multi-constellation router with four selection strategies (cheapest, fastest, available, preferred). SMAZ2 compression with Meshtastic-compatible dictionary. SBD fragmentation and reassembly (340B MO / 270B MT). Message deduplication (in-memory + Redis). End-to-end AES-256-GCM encryption with per-device keystore.

**Fleet Management** --
Bridge lifecycle management with auto-provisioning on first contact. MQTT credential generation (bcrypt) and TLS client certificate issuance (ECDSA P-256, 90-day expiry). Command dispatch (ping, reboot, flush) with round-trip response. 3-step onboarding wizard. ACL regeneration. Android device type support. Real-time online/offline status with configurable reaper timeout.

**mTLS Bridge Authentication** --
Bridges connect via MQTT-over-WebSocket with mutual TLS. Hub acts as a Certificate Authority, issuing client certificates per bridge. NATS handles TLS termination and client cert verification. HAProxy on the edge does SNI-based TCP passthrough -- no TLS termination, preserving the mTLS chain end-to-end. Verified at 266-345ms round-trip latency.

**Multi-Tenant Device Management** --
Per-tenant device registry with YAML config versioning. Per-device daily and monthly rate limiting with SOS bypass. Iridium credit balance polling and budget tracking. OTA firmware management via Eclipse hawkBit.

**Safety** --
SOS escalation chains with multi-tier notification through Apprise (90+ services) and ntfy. Dead man's switch with configurable check-in intervals and grace periods. Polygon geofencing engine with enter/exit/both triggers and automatic escalation on breach. Tamper-evident SHA-256 hash-chain audit log with independent verification and JSONL archival.

**Routing Engine** --
Any-to-any message routing with 7 destination handlers, default routes, and a Vue UI with visual flow diagram. SMS gateway with Twilio/Vonage integration and E2E encryption.

**Situational Awareness** --
GPS position storage with time-range queries and Douglas-Peucker track simplification. Full-width Leaflet map with live device positions. TAK/CoT gateway (bidirectional TCP/TLS to OpenTAK Server). APRS-IS IGate with rate-limited position injection and inbound message forwarding.

**Reticulum Transport** --
Hub operates as a Reticulum transport node, relaying announces and maintaining a routing table. Bridges with Reticulum identity get routes injected on birth and removed on death. Health reports refresh route TTLs. Wire format compatible with the Reticulum Network Stack.

**Authentication and Authorization** --
Built-in user management with Argon2id password hashing, JWT access tokens, and rotating refresh tokens. API keys with SHA-256 hashing and RBAC (viewer, operator, owner). OIDC/OAuth2 support for enterprise SSO. Per-IP login rate limiting and account lockout.

**Networking** --
Tor v3 hidden service for CGNAT bypass (HTTP + MQTT on .onion). WireGuard peer management via wg-easy REST API with auto-provisioning on device creation.

**Observability** --
Correlation IDs (xid) on every request. Structured JSON logging with slog. HTTP request/response logging middleware. DB query timing with slow query detection. MQTT bus metrics (ObservedBus wrapper). ~25 Prometheus metric families at `/metrics`. pprof endpoints (opt-in). Health probes: `/healthz`, `/readyz`, `/startupz` with latency tracking and detailed dependency checks. Audit log retention with JSONL archival. OpenTelemetry tracing scaffold (opt-in). 3 Grafana dashboards (overview, infrastructure, business).

**Outbound Integration** --
Configurable outbound webhooks with HMAC-SHA256 signing, retry with exponential backoff, and delivery logging. Apprise (Slack, email, Telegram, SMS via Twilio/Vonage, 90+ services). ntfy self-hosted push notifications.

**Dashboard** --
Vue 3 SPA with 21 views: dashboard with KPI sparklines, devices, messages, full-width map, fleet management, escalation chains, dead man's switch, geofencing, device config, routing with flow diagram, notifications, webhooks, OTA, users, API keys, audit log, cluster health, network, inline help, and settings. Tactical design system with Tailwind CSS. Grouped dropdown navigation. Compact status bar. Dark theme. Responsive. All timestamps in UTC 24h.

## Deployment

Three deployment tiers. See [docs/deployment.md](docs/deployment.md) for complete step-by-step guides.

| Tier | Mode | Database | Message Bus | Use Case |
|------|------|----------|-------------|----------|
| 1 | `standalone` | SQLite | Mosquitto | Single server, development, edge |
| 2 | `cluster` | MariaDB Galera | NATS + leaf nodes | Production HA, multi-site, mTLS |
| 3 | `kubernetes` | MariaDB Galera | NATS StatefulSet | Cloud-native, auto-scaling |

### Standalone (quickstart)

```bash
git clone https://github.com/cubeos-app/meshsat-hub.git
cd meshsat-hub
cp .env.standalone.example .env
nano .env   # set HUB_AUTH_TOKEN, CADDY_DOMAIN

docker compose -f docker-compose.prod.yml up -d
curl https://your-domain.com/healthz
```

### Cluster

Two or more hosts with MariaDB Galera (synchronous replication), NATS (cross-site MQTT via leaf nodes), Redis (shared rate limits), and HAProxy (SNI passthrough for mTLS). Includes garbd arbitrator for 3-voter quorum.

```bash
cp .env.cluster.example .env   # on each node, fill in IPs
# Follow the 7-step guide in docs/deployment.md
```

Templates provided:
- `.env.standalone.example` -- standalone configuration
- `.env.cluster.example` -- cluster configuration (no hardcoded IPs)
- `deploy/haproxy/haproxy.cfg.example` -- HAProxy SNI passthrough for mTLS
- `nats-mtls.conf` -- NATS with mTLS WebSocket
- `deploy/galera/Dockerfile.garbd` -- Galera arbitrator image

## Architecture

```
                    Internet
                       |
              +--------+--------+
              |                 |
        VPS (HAProxy)    VPS (HAProxy)
        SNI passthrough  SNI passthrough
              |                 |
       +------+------+  +------+------+
       |   Node A    |  |   Node B    |
       |  nginx:8451 |  |  nginx:8451 |
       |  Hub:6070   |  |  Hub:6070   |
       |  NATS:1883  |  |  NATS:1883  |
       |       :9443 |  |       :9443 |  <-- mTLS WebSocket
       |  Redis      |  |  Redis      |
       |  MariaDB  <==>  |  MariaDB   |  <-- Galera sync replication
       |  garbd      |  |             |
       +------+------+  +------+------+
              |                 |
              +-- NATS leaf ----+         <-- Cross-site MQTT routing
```

Each Hub instance is stateless. MariaDB Galera provides synchronous multi-master replication with a third-node arbitrator (garbd) for quorum. NATS handles cross-site MQTT topic replication via leaf nodes (hub/spoke topology). Redis provides distributed rate limiting and deduplication. Leader election (via NATS or Kubernetes Lease API) ensures singleton tasks run on exactly one node.

## Configuration

Hub reads `config.yaml` (path: `HUB_CONFIG_FILE`) with environment variable overrides (prefix `HUB_`):

| Variable | Default | Description |
|----------|---------|-------------|
| `HUB_MODE` | `standalone` | `standalone`, `cluster`, `kubernetes` |
| `HUB_PORT` | `6070` | HTTP listen port |
| `HUB_AUTH_TOKEN` | -- | Static bearer token |
| `HUB_DATABASE_URL` | -- | MariaDB DSN (cluster/k8s) |
| `HUB_REDIS_URL` | -- | Redis URL (cluster/k8s) |
| `HUB_MQTT_BROKER_URL` | `tcp://mqtt:1883` | MQTT broker |
| `HUB_MQTT_CLIENT_ID` | `meshsat-hub` | **Must be unique per node in cluster** |
| `HUB_ROCKBLOCK_SECRET` | -- | RockBLOCK webhook HMAC secret |
| `HUB_CLOUDLOOP_API_KEY` | -- | Cloudloop REST API key (Iridium MT) |
| `HUB_BRIDGE_OFFLINE_TIMEOUT` | `300` | Seconds before marking bridge offline |
| `HUB_BRIDGE_CA_CERT_EXPORT_PATH` | -- | Path to export bridge CA cert for NATS mTLS |
| `HUB_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `HUB_LOG_FORMAT` | `json` | `json` or `text` |

See `.env.standalone.example` and `.env.cluster.example` for the complete list.

## Development

Prerequisites: Go 1.24+, Node.js 20+, Docker Compose v2.

```bash
make build              # Build Go binary
make test               # Unit tests (42 packages)
make test-integration   # Integration tests (embedded MQTT broker)
make lint               # golangci-lint
make security           # gosec + govulncheck
make fmt                # gofmt

cd web && npm install && npm run build   # Build Vue SPA
cd web && npx playwright test            # Playwright E2E tests (53 tests)
```

## Project Layout

```
meshsat-hub/
+-- cmd/meshsat-hub/            Entry point, router, graceful shutdown
|   +-- web/dist/               Embedded Vue SPA (committed)
+-- internal/
|   +-- api/                    REST handlers + strict request validation
|   +-- aprsis/                 APRS-IS IGate (bidirectional)
|   +-- audit/                  SHA-256 hash-chain audit log + retention
|   +-- auth/                   Auth middleware, Argon2id, JWT, RBAC, API keys
|   +-- backup/                 Full-state ZIP export, diff, import
|   +-- bridge/                 Bridge lifecycle: subscriber, reaper, cert authority, ACL, commander
|   +-- bus/                    MQTT message bus (Paho) + ObservedBus metrics wrapper
|   +-- cloudloop/              Cloudloop/Iridium MT sender + credit poller
|   +-- compress/               SMAZ2 compression (Meshtastic dictionary)
|   +-- config/                 YAML + env config loading
|   +-- constellation/          Multi-constellation send router
|   +-- dedup/                  Message deduplication (memory + Redis)
|   +-- health/                 /healthz, /readyz, /startupz + DetailedProbe
|   +-- leader/                 Leader election (Noop, NATS, KubeLease)
|   +-- metrics/                Prometheus metrics (~25 families) + chi middleware
|   +-- middleware/              RequestID (xid), HTTP logging
|   +-- observability/          Error categorization + OTel tracing scaffold
|   +-- protocol/               Wire format: birth/death/health/command structs
|   +-- ratelimit/              Per-device rate limiting (memory + Redis)
|   +-- rockblock/              RockBLOCK MO webhook handler + audit
|   +-- store/                  Store interface + SQLite + MariaDB (Galera) impls
|   |   +-- dbwrap/             DB query instrumentation (timing, slow query, pool stats)
|   +-- tak/                    TAK/CoT gateway
|   +-- webhook/                Outbound webhook dispatcher
|   +-- wireguard/              WireGuard peer management + auto-provisioning
+-- web/                        Vue 3 SPA source (21 views)
+-- deploy/
|   +-- ansible/                Playbooks: bootstrap, deploy, recover, infra
|   +-- galera/                 Galera compose, entrypoint, garbd Dockerfile
|   +-- haproxy/                HAProxy SNI passthrough template
|   +-- grafana/                3 Grafana dashboards + provisioning
|   +-- k8s/                    Kubernetes manifests
|   +-- helm/meshsat-hub/       Helm chart
+-- test/integration/           End-to-end tests (embedded MQTT)
+-- web/e2e/                    Playwright browser tests
+-- test/e2e/                   Post-deploy smoke tests
+-- scripts/                    Galera watchdog, health check, migration
+-- docs/
|   +-- deployment.md           Tier 1/2/3 deployment guide
|   +-- ROADMAP.md              Version history + planned work
|   +-- SECURITY_AUDIT.md       SAST/SCA/OWASP findings
+-- docker-compose.yml          Development
+-- docker-compose.prod.yml     Tier 1 standalone production
+-- docker-compose.cluster.yml  Tier 2 single-host cluster
+-- nats.conf                   NATS base config (MQTT adapter + JetStream)
+-- nats-mtls.conf              NATS config with mTLS WebSocket
+-- Dockerfile                  Multi-stage Alpine build (CGO_ENABLED=0)
+-- Makefile                    Build, test, lint, security, fmt
+-- .gitlab-ci.yml              7-stage CI/CD pipeline
```

## CI/CD Pipeline

```
lint --> security --> test --> build --> package --> pre-deploy --> deploy --> verify --> owasp
```

| Stage | Description |
|-------|-------------|
| lint | golangci-lint + swagger validation |
| security | gosec (SAST, blocks on HIGH) + govulncheck (known CVEs) |
| test | `go test ./...` (42 packages) |
| build | `CGO_ENABLED=0 go build` |
| package | Docker multi-stage build + push to GHCR |
| pre-deploy | Galera health gate (cluster_size, wsrep_ready, .env validation) |
| deploy | Rolling deploy to both DMZ nodes (--no-deps) |
| verify | Post-deploy healthz + readyz + Galera cluster verification |
| owasp | Automated OWASP ZAP scan (non-blocking) |

## Documentation

- [Deployment Guide](docs/deployment.md) -- Tier 1/2/3 setup with templates
- [Roadmap](docs/ROADMAP.md) -- Version history and planned work
- [Security Audit](docs/SECURITY_AUDIT.md) -- SAST, SCA, and OWASP scan results

## Related Projects

| Project | Description |
|---------|-------------|
| [MeshSat Bridge](https://github.com/cubeos-app/meshsat) | Field gateway firmware (Go, Raspberry Pi) -- connects Meshtastic mesh radios to Iridium satellites via mTLS |
| [MeshSat Android](https://github.com/cubeos-app/meshsat-android) | Mobile gateway app (Kotlin) -- phone-as-bridge with BLE mesh, ONNX ML, and SPP Iridium |

## License

[Apache 2.0](LICENSE)
