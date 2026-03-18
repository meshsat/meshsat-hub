# MeshSat Hub — Deployment Guide

MeshSat Hub supports three deployment tiers, selectable via the `HUB_MODE` environment variable. Each tier adds shared infrastructure for higher availability and cross-instance state consistency.

## Deployment Tiers

| Tier | Mode | Compose File | Store | Message Bus | Dedup / Rate Limit | Leader Election | Use Case |
|------|------|-------------|-------|-------------|-------------------|-----------------|----------|
| **Tier 1** | `standalone` | `docker-compose.prod.yml` | SQLite (local) | Mosquitto (local) | In-memory | Noop (always leader) | Single VPS, dev, edge |
| **Tier 2** | `cluster` | `docker-compose.cluster.yml` | PostgreSQL (shared) | NATS + MQTT adapter (clustered) | Redis (shared) | NATS queue groups | Multi-host Docker Compose |
| **Tier 3** | `kubernetes` | `deploy/helm/meshsat-hub/` | PostgreSQL (shared) | NATS (StatefulSet, 3 replicas) | Redis (shared) | Kubernetes Lease API | k8s / cloud-native |

**Current production deployment:** Tier 2 cluster with 2 nodes — `nllei01dmz01` (NL) + `grskg01dmz01` (GR), connected via NATS route clustering over WireGuard.

---

## Tier 1: Standalone (Single Node)

Best for development, single-VPS deployments, and edge/field use.

### Prerequisites

- VPS with public IP (1 CPU, 512MB RAM minimum, 1GB recommended)
- Ubuntu 22.04+ or Debian 12+
- Docker Engine 24+ and Docker Compose v2
- Domain name pointed at the VPS IP (for TLS)

### Quick Start

```bash
# 1. Clone
git clone https://github.com/cubeos-app/meshsat-hub.git
cd meshsat-hub

# 2. Configure
cp .env.example .env
nano .env  # fill in secrets

# 3. Edit Caddyfile — replace hub.meshsat.io with your domain
nano Caddyfile

# 4. Start
docker compose -f docker-compose.prod.yml up -d

# 5. Verify
curl https://your-domain.com/healthz
curl https://your-domain.com/readyz
```

### Architecture

```
Internet
    |
    v
Caddy (port 80/443, automatic TLS via Let's Encrypt)
    |
    +-- /api/*     -> Hub (port 6070, internal only)
    +-- /healthz   -> Hub
    +-- /readyz    -> Hub

Field devices connect directly to:
    +-- MQTT TCP   -> Mosquitto (port 6071)
    +-- MQTT WS    -> Mosquitto (port 6072)
    +-- Tor        -> .onion:80 (Hub), .onion:1883 (MQTT)
```

### Components

| Service | Image | Port | Purpose |
|---------|-------|------|---------|
| Hub | `ghcr.io/cubeos-app/meshsat-hub` | 6070 (internal) | Go API + embedded Vue SPA |
| Mosquitto | `eclipse-mosquitto:2` | 6071 (MQTT), 6072 (WS) | MQTT broker |
| Caddy | `caddy:2-alpine` | 80, 443 | TLS reverse proxy |
| Tor | `cmehay/docker-tor-hidden-service` | .onion | Hidden service |

### Data

- SQLite database: `/data/hub.db` (inside `hub-data` volume)
- Each instance is fully independent — no shared state

---

## Tier 2: Cluster (Multi-Host Docker Compose)

Best for production HA deployments across 2+ hosts. All Hub instances share PostgreSQL, Redis, and NATS for consistent state.

### Prerequisites

- 2+ hosts with Docker Engine 24+ and Docker Compose v2
- Network connectivity between hosts (WireGuard recommended for cross-site)
- Domain name for TLS

### Architecture

```
                    Internet
                       |
            +----------+----------+
            |                     |
      nllei01dmz01 (NL)    grskg01dmz01 (GR)
            |                     |
     +------+------+       +------+------+
     | nginx (8451) |       | nginx (8451) |
     +------+------+       +------+------+
            |                     |
     +------+------+       +------+------+
     | Hub (6070)   |       | Hub (6070)   |
     | mode=standalone|     | mode=standalone|
     | client=hub-nl |       | client=hub-gr |
     +------+------+       +------+------+
            |                     |
     +------+------+       +------+------+
     | NATS (1883)  |<----->| NATS (1883)  |
     | JetStream    | route | JetStream    |
     | mqtt adapter |       | mqtt adapter |
     +-------------+       +-------------+

     NATS cluster routes connect both sites.
     Each site has its own SQLite (standalone mode).
     Shared MQTT via NATS route clustering.
```

**Note:** The current production deployment runs `HUB_MODE=standalone` on each host with NATS providing cross-site MQTT replication via route clustering. For true shared-state cluster mode (shared PostgreSQL + Redis), use `docker-compose.cluster.yml` on a single host or across hosts with shared database access.

### Single-Host Cluster (Both Instances on One Host)

```bash
# 1. Configure
cp .env.cluster.example .env
nano .env  # set passwords, domain, etc.

# 2. Start (PostgreSQL, Redis, NATS, Hub x2, Caddy)
docker compose -f docker-compose.cluster.yml up -d

# 3. Verify
curl https://your-domain.com/healthz
curl https://your-domain.com/readyz
# readyz should show: {"status":"ok","checks":{"mqtt":"ok","postgres":"ok","redis":"ok"}}
```

### Multi-Host Cluster (Current Production Topology)

Each host runs its own compose stack. NATS provides cross-site MQTT message replication.

**Per-host setup:**

```bash
# On each host at /srv/meshsat-hub/
cp .env.cluster.example .env
nano .env

# CRITICAL: Set a UNIQUE client ID per host to avoid NATS session conflicts
# Host 1: HUB_MQTT_CLIENT_ID=meshsat-hub-nl
# Host 2: HUB_MQTT_CLIENT_ID=meshsat-hub-gr

# NATS config: each host's nats.conf must include the peer's route
# cluster { routes = [ nats://<peer-ip>:6222 ] }
```

### Migrating from Standalone to Cluster

```bash
# Export data from SQLite
./scripts/migrate-sqlite-to-pg.sh /data/hub.db "postgres://user:pass@postgres:5432/meshsat_hub"
```

### Components (Cluster Mode)

| Service | Image | Port | Purpose |
|---------|-------|------|---------|
| Hub x2 | `ghcr.io/cubeos-app/meshsat-hub` | 6070 (internal) | Go API, `HUB_MODE=cluster` |
| PostgreSQL | `postgres:16-alpine` | 5432 (internal) | Shared persistent store |
| Redis | `redis:7-alpine` | 6379 (internal) | Shared dedup + rate limit state |
| NATS | `nats:2.10-alpine` | 4222 (NATS), 1883 (MQTT), 8222 (monitoring) | Message bus with MQTT adapter + JetStream |
| Caddy | `caddy:2-alpine` | 80, 443 | TLS reverse proxy with LB |

### Health Checks (Cluster)

The `/readyz` endpoint checks all dependencies:

```json
{
  "status": "ok",
  "checks": {
    "mqtt": "ok",
    "postgres": "ok",
    "redis": "ok"
  }
}
```

In standalone mode, only `mqtt` is checked.

---

## Tier 3: Kubernetes

Best for cloud-native deployments with auto-scaling, rolling updates, and managed infrastructure.

### Quick Start (Helm)

```bash
helm install meshsat-hub deploy/helm/meshsat-hub/ \
  --namespace meshsat-hub --create-namespace \
  --set secrets.databaseUrl="postgres://..." \
  --set secrets.redisUrl="redis://..." \
  --set secrets.authToken="your-token" \
  --set ingress.enabled=true \
  --set ingress.host=hub.example.com
```

### Quick Start (Raw Manifests)

```bash
kubectl apply -f deploy/k8s/namespace.yaml
kubectl apply -f deploy/k8s/secret.yaml       # edit first!
kubectl apply -f deploy/k8s/configmap.yaml
kubectl apply -f deploy/k8s/postgres-statefulset.yaml
kubectl apply -f deploy/k8s/redis-deployment.yaml
kubectl apply -f deploy/k8s/nats-statefulset.yaml
kubectl apply -f deploy/k8s/hub-deployment.yaml
kubectl apply -f deploy/k8s/ingress.yaml       # edit host!
```

### Components

| Resource | Kind | Replicas | Purpose |
|----------|------|----------|---------|
| Hub | Deployment | 2 | API + SPA, `HUB_MODE=kubernetes` |
| PostgreSQL | StatefulSet | 1 | Persistent store (10Gi PVC) |
| Redis | Deployment | 1 | Dedup + rate limit (1Gi PVC) |
| NATS | StatefulSet | 3 | Clustered message bus (2Gi PVC each) |
| Ingress | Ingress | — | TLS termination |

### Leader Election

In Kubernetes mode, singleton services (TAK, APRS-IS) use the Kubernetes Lease API for leader election. The Hub ServiceAccount has an RBAC Role granting `get`, `create`, `update` on `coordination.k8s.io/leases`.

```yaml
env:
  - name: POD_NAMESPACE
    valueFrom:
      fieldRef:
        fieldPath: metadata.namespace
```

### Helm Values

Toggle built-in infrastructure (set `false` to use external/managed services):

```yaml
postgres:
  enabled: true    # false = use external PostgreSQL
redis:
  enabled: true    # false = use external Redis
nats:
  enabled: true    # false = use external NATS
  replicaCount: 3  # 3 for HA
```

---

## Configuration Reference

### Required Environment Variables

| Variable | Tier | Description |
|----------|------|-------------|
| `HUB_MODE` | All | `standalone` (default), `cluster`, `kubernetes` |
| `HUB_AUTH_TOKEN` | All | Auth token for API access |
| `HUB_ROCKBLOCK_SECRET` | All | Shared secret for RockBLOCK webhook |
| `HUB_CLOUDLOOP_API_KEY` | All | Cloudloop REST API key for MT sends |
| `HUB_DATABASE_URL` | 2, 3 | PostgreSQL connection string |
| `HUB_REDIS_URL` | 2, 3 | Redis connection string |
| `HUB_NATS_URL` | 2, 3 | NATS MQTT adapter URL (`tcp://nats:1883`) |
| `HUB_MQTT_CLIENT_ID` | All | **Must be unique per instance** in cluster/k8s |

### Optional Features

| Variable | Default | Description |
|----------|---------|-------------|
| `HUB_TAK_ENABLED` | `false` | Enable TAK/CoT gateway |
| `HUB_APRSIS_ENABLED` | `false` | Enable APRS-IS IGate |
| `HUB_WG_ENABLED` | `false` | Enable WireGuard peer management |
| `HUB_TENANT_ENFORCE` | `false` | Require tenant context on all requests |
| `HUB_RATELIMIT_DAILY_CAP` | `100` | Max MT sends per device per day |
| `HUB_RATELIMIT_MONTHLY_CAP` | `0` | Max MT sends per device per month (0=unlimited) |

---

## Resource Sizing

| Tier | CPU | RAM | Disk | Devices |
|------|-----|-----|------|---------|
| Tier 1 (standalone) | 1 vCPU | 512MB | 5GB | 1-50 |
| Tier 2 (cluster, per host) | 2 vCPU | 2GB | 20GB | 50-500 |
| Tier 3 (k8s, total) | 4 vCPU | 4GB | 50GB | 500+ |

Hub binary: ~30MB RAM. PostgreSQL: ~128MB. Redis: ~64MB. NATS: ~64MB per replica.

---

## CI/CD Pipeline

```
lint -> test -> build -> package -> deploy -> verify -> pages
```

| Stage | Description |
|-------|-------------|
| lint | golangci-lint |
| test | `go test ./...` (unit tests) |
| build | `CGO_ENABLED=0 go build` |
| package | Docker build + push to GHCR |
| deploy | AWX triggers pull + restart on both DMZ hosts |
| verify | E2E smoke tests (`test/e2e/smoke_test.sh`) against live cluster |
| pages | Hugo documentation site |

### E2E Smoke Tests

The `verify` stage runs after deploy and checks:
1. `/healthz` and `/readyz` on both hosts
2. Auth flow (API key create on host 1, validate on host 2)
3. Device replication (create on host 1, read on host 2)
4. Audit chain integrity
5. Rate limit shared state
6. API endpoint availability

Requires CI variables: `HUB_HOST_1`, `HUB_HOST_2`, `HUB_E2E_TOKEN`.

---

## Backup and Restore

```bash
# Export
curl -o backup.zip https://your-domain.com/api/backup/export \
  -H "Authorization: Bearer $HUB_AUTH_TOKEN"

# Preview changes before import
curl -X POST https://your-domain.com/api/backup/diff \
  -H "Content-Type: application/zip" --data-binary @backup.zip

# Import
curl -X POST https://your-domain.com/api/backup/import \
  -H "Authorization: Bearer $HUB_AUTH_TOKEN" \
  -H "Content-Type: application/zip" --data-binary @backup.zip
```

---

## Tor Hidden Service

```bash
docker exec meshsat-tor cat /var/lib/tor/hidden_service/hostname
```

Back up Tor keys:
```bash
docker run --rm -v meshsat-hub_tor-keys:/data -v $(pwd):/backup alpine \
  tar czf /backup/tor-keys-backup.tar.gz /data
```

---

## Troubleshooting

| Issue | Fix |
|-------|-----|
| `readyz` returns 503 / mqtt unhealthy | MQTT not connected — check `docker logs meshsat-nats` |
| MQTT connect/disconnect flapping | Duplicate `HUB_MQTT_CLIENT_ID` across hosts — set unique IDs |
| NATS "older protocol MQIsdp not supported" | Hub image too old — needs `SetProtocolVersion(4)` fix |
| Webhook returns 401 | Wrong `HUB_ROCKBLOCK_SECRET` |
| Caddy won't start | Port 80/443 in use |
| PostgreSQL ping fails in readyz | Check `HUB_DATABASE_URL` and `docker logs meshsat-postgres` |
