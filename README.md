# MeshSat Hub

Multi-tenant SaaS platform for managing satellite-connected field devices. Receives Iridium/Astrocast messages, provides a web dashboard, SOS escalation, geofencing, and OTA firmware updates. Runs as an active-active cluster with MariaDB Galera synchronous replication.

## Features

**Satellite Communications**
- Iridium SBD (RockBLOCK/Cloudloop) + Astrocast Astronode S — multi-constellation
- MO ingestion via webhook, MT sending via REST API, SMAZ2 compression
- SBD fragmentation/reassembly, message deduplication, E2E encryption (AES-256-GCM)

**Device Management**
- Multi-tenant device registry with config versioning
- Per-device daily/monthly rate limiting and budget tracking
- Dead man's switch with configurable check-in intervals

**Safety**
- SOS escalation chains with multi-tier notification (Apprise + ntfy)
- Polygon geofencing engine with breach alerts
- Tamper-evident SHA-256 hash-chain audit log

**Authentication**
- Built-in user management with Argon2id passwords + JWT sessions
- API keys with RBAC (viewer / operator / owner)
- OIDC support for enterprise SSO (optional)

**Infrastructure**
- Active-active MariaDB Galera cluster (synchronous multi-master)
- MPTCP concentrator for satellite + cellular link aggregation
- HAProxy health checks via Galera write-readiness probe
- hawkBit OTA firmware management
- Tor hidden service for field device backhaul

**Dashboard**
- Vue 3 SPA with 16 views (Tailwind CSS, dark theme)
- Live cluster health monitoring with remediation actions
- Leaflet map with device positions
- All timestamps in UTC 24h format

## Architecture

```
         hub.meshsat.net
              │
       ┌──────┴──────┐
       ▼              ▼
  NL (primary)    GR (secondary)
  ├─ MariaDB ◄──►├─ MariaDB        ← Galera sync replication
  ├─ garbd       ├─ Redis
  ├─ Redis       ├─ NATS
  ├─ NATS        ├─ Hub
  ├─ Hub         └─ nginx
  └─ nginx
```

## Quick Start

### Standalone (single node)

```bash
cp config.example.yaml config.yaml
# Edit: set rockblock_secret, auth_token
docker compose up -d
curl http://localhost:6070/healthz
```

### Cluster (active-active)

See [Deployment Guide](docs/deployment.md) and [Operational Runbook](docs/RUNBOOK.md).

```bash
# Requires: 2+ hosts with VPN connectivity, Ansible
cd deploy/ansible
cp group_vars/vault.yml.example group_vars/vault.yml
# Edit vault.yml with passwords
ansible-playbook -i inventory.yml playbooks/bootstrap.yml --ask-vault-pass
```

## Deployment Tiers

| Tier | Mode | Database | Bus | Use Case |
|------|------|----------|-----|----------|
| 1 | `standalone` | SQLite | NATS/Mosquitto | Single VPS, dev, edge |
| 2 | `cluster` | MariaDB Galera | NATS | **Production** (active-active) |
| 3 | `kubernetes` | MariaDB Galera | NATS StatefulSet | Cloud-native |

## Configuration

All settings via `config.yaml` or `HUB_` environment variables:

| Env Var | Default | Description |
|---------|---------|-------------|
| `HUB_MODE` | `standalone` | `standalone`, `cluster`, `kubernetes` |
| `HUB_DATABASE_URL` | — | MariaDB DSN (cluster/k8s) |
| `HUB_AUTH_TOKEN` | — | Static bearer token |
| `HUB_JWT_SIGNING_KEY` | — | HMAC-SHA256 key for local user auth (min 32 chars) |
| `HUB_CLUSTER_PEERS` | — | Comma-separated peer Hub URLs |
| `HUB_ROCKBLOCK_SECRET` | — | Webhook HMAC verification |
| `HUB_CLOUDLOOP_API_KEY` | — | Iridium MT sending |
| `HUB_ASTROCAST_API_KEY` | — | Astrocast constellation |

## Development

```bash
# Prerequisites: Go 1.24+, Node.js 20+, Docker Compose v2
make build          # Go binary
make test           # Unit tests
make lint           # golangci-lint
cd web && npm run build  # Vue SPA
cd web && npm run test:e2e  # Playwright tests
```

## Project Structure

```
meshsat-hub/
├── cmd/meshsat-hub/         # Entrypoint + embedded Vue SPA
├── internal/
│   ├── api/                 # REST API handlers (devices, users, login, etc.)
│   ├── astrocast/           # Astrocast Astronode S API client
│   ├── auth/                # Auth middleware, Argon2id, JWT sessions, RBAC
│   ├── bus/paho/            # MQTT client (Paho)
│   ├── cloudloop/           # Cloudloop/Iridium API client + MT sender
│   ├── cluster/             # Galera cluster health monitoring
│   ├── constellation/       # Multi-constellation router
│   ├── crypto/              # AES-256-GCM encryption
│   ├── escalation/          # SOS escalation engine
│   ├── geo/                 # Polygon geofencing
│   ├── store/sqlite/        # SQLite store (standalone)
│   ├── store/mariadb/       # MariaDB Galera store (cluster)
│   └── ...                  # 25+ packages
├── web/                     # Vue 3 SPA (16 views)
├── deploy/
│   ├── ansible/             # Cluster deployment playbooks
│   ├── k8s/                 # Kubernetes manifests
│   └── helm/                # Helm chart
├── docs/
│   ├── deployment.md        # Deployment guide
│   ├── RUNBOOK.md           # Operational runbook
│   └── ROADMAP.md           # Version history
├── docker-compose.yml       # Standalone + cluster compose
├── Dockerfile               # Multi-stage Alpine build
└── .gitlab-ci.yml           # 7-stage CI/CD pipeline
```

## Documentation

| Document | Description |
|----------|-------------|
| [Deployment Guide](docs/deployment.md) | Tier 1/2/3 deployment instructions |
| [Operational Runbook](docs/RUNBOOK.md) | Cluster operations, recovery, troubleshooting |
| [Roadmap](docs/ROADMAP.md) | Version history and planned features |
| [Security Audit](docs/SECURITY_AUDIT.md) | SAST/SCA findings, security headers |

## License

Apache 2.0
