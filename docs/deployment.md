# MeshSat Hub — Deployment Guide

MeshSat Hub supports three deployment tiers. Pick the one that matches your environment.

| Tier | Mode | What you get | Use case |
|------|------|-------------|----------|
| **Tier 1** | Standalone | SQLite + Mosquitto + Caddy on a single host | Dev, lab, single VPS, edge |
| **Tier 2** | Cluster | Retired 2026-09-08 (was MariaDB Galera + NATS + Redis across 2 hosts) | — |
| **Tier 3** | Kubernetes | kustomize tree `k8s/` (CNPG Postgres, NATS, Redis, stunnel, edge relay), Argo CD | **Production** (notrf01cl01k8s) |

---

## Tier 1: Standalone

A single Docker Compose stack with automatic TLS. Everything runs on one host.

### Prerequisites

- Linux host with Docker Engine 24+ and Compose v2
- 1 vCPU, 512MB RAM, 5GB disk (minimum)
- Public IP + domain name (for Let's Encrypt TLS)
- Ports 80 and 443 available

### Architecture

```
Internet
  |
  v
Caddy (:80/:443, auto-TLS)
  |
  +-- /api/*    --> Hub (:6070)
  +-- /mqtt     --> Mosquitto (:9001 WS)
  +-- /*        --> Hub (Vue SPA)

Field bridges:
  +-- MQTT TCP  --> Mosquitto (:6071)
  +-- MQTT WS   --> Mosquitto (:6072)
  +-- Tor       --> .onion:80 (Hub), .onion:1883 (MQTT)
```

### Setup

```bash
# 1. Clone
git clone https://github.com/meshsat/meshsat-hub.git
cd meshsat-hub

# 2. Configure
cp .env.standalone.example .env
nano .env   # set HUB_AUTH_TOKEN, CADDY_DOMAIN, CADDY_EMAIL

# 3. Set your domain in the Caddyfile
sed -i "s/hub.meshsat.io/$(grep CADDY_DOMAIN .env | cut -d= -f2)/" Caddyfile

# 4. Start
docker compose -f docker-compose.prod.yml up -d

# 5. Verify
curl https://your-domain.com/healthz   # {"status":"ok"}
curl https://your-domain.com/readyz    # {"status":"ok","checks":{"mqtt":{"status":"ok"}}}
```

### Data

- SQLite database: `hub-data` Docker volume (`/data/hub.db` inside container)
- MQTT persistence: `mqtt-data` Docker volume
- Tor keys: `tor-keys` Docker volume (back up to preserve .onion address)

### Optional profiles

```bash
# Enable TAK/CoT server
docker compose -f docker-compose.prod.yml --profile tak up -d

# Enable Prometheus monitoring
docker compose -f docker-compose.prod.yml --profile monitoring up -d

# Enable multi-channel notifications (Apprise)
docker compose -f docker-compose.prod.yml --profile notifications up -d
```

---

## Tier 2: Cluster (retired)

The two-host MariaDB Galera + NATS + Redis compose deployment ran on `nllei01dmz01` and
`grskg01dmz01` until 2026-09-08 and was retired with the move to Kubernetes (MESHSAT-864).
Its compose file, Galera entrypoint, health gate and Ansible playbooks were removed from the
repository in MR 23; the `cluster` mode still exists for a single Postgres + NATS + Redis
compose deployment, but nothing in this repository deploys it.

## Tier 3: Kubernetes (production)

The Hub runs on `notrf01cl01k8s` from the kustomize tree in `k8s/` (synced by the Argo CD
Application `meshsat-hub`): CloudNativePG cluster `meshsat-hub-main` (3 instances, barman
backups to S3), NATS StatefulSet (MQTT :1883 in-cluster, WebSocket+mTLS :9443 for bridges),
Redis, the stunnel Deployment for the Reticulum leg, a hostNetwork edge-relay DaemonSet that
the VPS HAProxy nodes reach, ExternalSecrets from OpenBao, and the Hub Deployment
(`HUB_MODE=kubernetes`, `HUB_DB_DRIVER=postgres`, OIDC login against the shared authentik).
Conventions in `k8s/CONVENTIONS.md`, bootstrap and hand-applied state in `k8s/NOTES.md`,
operator tooling under `k8s/scripts/`.

```bash
kubectl kustomize k8s/ | kubeconform -strict -ignore-missing-schemas   # what CI checks
kubectl --context notrf01 -n meshsat-hub get pods                       # hub, nats-0, redis-0, stunnel, meshsat-edge-relay
```

| Resource | Kind | Replicas | Purpose |
|----------|------|----------|---------|
| hub | Deployment | 1 | API + SPA (`Recreate`; claims + Lease election make 2 safe once proven) |
| meshsat-hub-main | CNPG Cluster | 3 | Postgres, synchronous replication, daily backup |
| nats | StatefulSet | 1 | MQTT + WebSocket mTLS bus, JetStream |
| redis | StatefulSet | 1 | Dedup + rate limit |
| stunnel | Deployment | 1 | Reticulum TLS termination |
| meshsat-edge-relay | DaemonSet | workers | TCP relay :9443/:4243 for the VPS edge |
| hub.meshsat.net / auth.meshsat.net | Ingress | -- | ingress-nginx, wildcard cert from OpenBao |

Leader election for singleton services (OTS poller, reapers, retention) uses the Kubernetes
Lease API; message dispatch is protected by database claims, not by the leader.

## Configuration Reference

### Required variables (all tiers)

| Variable | Description |
|----------|-------------|
| `HUB_AUTH_TOKEN` | API authentication token |

### Cluster-only variables

| Variable | Description |
|----------|-------------|
| `HUB_MODE` | `standalone` (default), `cluster`, `kubernetes` |
| `HUB_DATABASE_URL` | MariaDB connection string |
| `HUB_REDIS_URL` | Redis connection string |
| `HUB_NATS_URL` | NATS MQTT adapter URL |
| `HUB_MQTT_CLIENT_ID` | **Must be unique per node** |
| `WSREP_CLUSTER_ADDRESS` | Galera cluster address (all node IPs) |
| `WSREP_NODE_ADDRESS` | This node's private IP |
| `SITE_NAME` | Unique node identifier |

### Optional features

| Variable | Default | Description |
|----------|---------|-------------|
| `HUB_TAK_ENABLED` | `false` | TAK/CoT gateway |
| `HUB_APRSIS_ENABLED` | `false` | APRS-IS IGate |
| `HUB_WG_ENABLED` | `false` | WireGuard peer management |
| `HUB_PPROF_ENABLED` | `false` | pprof debug endpoints |
| `HUB_BRIDGE_OFFLINE_TIMEOUT` | `300` | Seconds before marking bridge offline |
| `HUB_AUDIT_RETENTION_DAYS` | `90` | Days to keep audit entries |
| `HUB_OTEL_ENDPOINT` | (empty) | OpenTelemetry OTLP endpoint |

### Resource sizing

| Tier | CPU | RAM | Disk | Devices |
|------|-----|-----|------|---------|
| Standalone | 1 vCPU | 512MB | 5GB | 1-50 |
| Cluster (per host) | 2 vCPU | 2GB | 20GB | 50-500 |
| Kubernetes (total) | 4 vCPU | 4GB | 50GB | 500+ |

---

## Map basemap (self-hosted)

The Hub's map and geofence pages render a vector basemap that the Hub serves
itself, so an operator's browser never asks a third-party tile host for tiles
and the areas they look at, which are roughly where their devices are, stay
inside the deployment.

One PMTiles archive and its glyph and sprite assets live in an S3-compatible
bucket; the Hub streams them at `/basemap/basemap.pmtiles` and
`/basemap/assets/`, forwarding HTTP range requests, so a session transfers the
few tiles it displays rather than the whole archive. The routes serve public
OpenStreetMap-derived data and need no authentication.

| Variable | Description |
|----------|-------------|
| `HUB_BASEMAP_S3_KEY` | Object key of the PMTiles archive. Empty disables the map backdrop. |
| `HUB_BASEMAP_S3_ASSET_PREFIX` | Key prefix of the glyphs and sprites (default `basemap/assets`) |
| `HUB_BASEMAP_S3_ENDPOINT` / `_BUCKET` / `_REGION` | Object store; default to the audit archive's values |
| `HUB_BASEMAP_S3_ACCESS_KEY` / `_SECRET_KEY` | Credentials; default to the audit archive's |
| `HUB_BASEMAP_CACHE_MAX_AGE` | `Cache-Control` max-age of the archive (default `24h`) |

Build and publish an archive with `k8s/scripts/basemap/build-basemap.sh`, which
extracts a world basemap from the Protomaps daily planet build over range
requests (a world at zoom 0-8 is about 530 MB, zoom 0-7 about 180 MB), uploads
it with the font and sprite assets, and prints the two config values to set.
Without the key the map still draws devices, tracks and geofences on an empty
backdrop and says so.

Attribution: map data (c) OpenStreetMap contributors (ODbL), basemap tiles by
Protomaps, label fonts Noto Sans under the SIL Open Font License. The
attribution control on the map carries the first two.

---

## mTLS Bridge Authentication

Bridges connect to the Hub via MQTT-over-WebSocket with mutual TLS (mTLS). The Hub acts as a Certificate Authority, issuing ECDSA P-256 client certificates to each bridge.

### How it works

```
Bridge (field device)
  |
  | wss://mqtt.example.com:443/mqtt  (client cert + key)
  v
HAProxy (VPS/edge, port 443)
  | SNI peek: mqtt.example.com → TCP passthrough (no TLS termination)
  v
NATS (:9443, TLS + verify: true)
  | TLS handshake: server cert (*.example.com) + client cert verification
  | Client cert CN = bridge_id, signed by Hub CA
  v
MQTT session established over WebSocket
```

### Onboarding a bridge

```bash
# 1. Generate MQTT credentials (password shown once)
curl -X POST -H "Authorization: Bearer $TOKEN" \
  https://hub.example.com/api/bridges/my-bridge/credentials

# 2. Issue TLS certificate (cert + key shown once, 90-day expiry)
curl -X POST -H "Authorization: Bearer $TOKEN" \
  https://hub.example.com/api/bridges/my-bridge/certificate

# 3. Configure the bridge with the URL, credentials, and cert
#    (via bridge API at http://bridge-ip:6050/api/routing/hub)
```

### NATS mTLS configuration

The production NATS config is `k8s/nats/configmap.yaml`; for a compose deployment start from `nats.conf` and add the section below. The key section:

```
websocket {
  port: 9443
  tls {
    cert_file: /etc/nats/certs/server.crt    # your domain cert
    key_file: /etc/nats/certs/server.key     # your domain key
    ca_file: /etc/nats/certs/bridge-ca.crt   # Hub CA (auto-exported)
    verify: true                              # require client cert
  }
}
```

The Hub automatically exports its bridge CA certificate to the shared `nats-certs` Docker volume when `HUB_BRIDGE_CA_CERT_EXPORT_PATH` is set.

---

## Backup and Restore

```bash
# Export
curl -o backup.zip https://hub.example.com/api/backup/export \
  -H "Authorization: Bearer $HUB_AUTH_TOKEN"

# Preview changes before import
curl -X POST https://hub.example.com/api/backup/diff \
  -H "Content-Type: application/zip" --data-binary @backup.zip

# Import
curl -X POST https://hub.example.com/api/backup/import \
  -H "Authorization: Bearer $HUB_AUTH_TOKEN" \
  -H "Content-Type: application/zip" --data-binary @backup.zip
```

---

## Troubleshooting

| Issue | Fix |
|-------|-----|
| `readyz` returns 503 / mqtt unhealthy | Check `docker logs meshsat-nats` |
| MQTT connect/disconnect flapping | Duplicate `HUB_MQTT_CLIENT_ID` — must be unique per node |
| Galera `cluster_size < 3` | Check garbd: `docker logs meshsat-garbd` |
| `WSREP_CLUSTER_ADDRESS=gcomm://` in .env | **Critical** — restore full address immediately |
| NATS leaf `Loop detected` | Only one side should have `remotes` in leafnodes config |
| Bridges show "offline" despite health flowing | Deploy latest Hub (health messages now re-set online) |
| Fleet page shows "MQTT: Not set" | Deploy latest Hub (credential columns now included in queries) |
| mTLS handshake timeout | Check NATS certs are mounted and readable |
| TLS cert expired | Renew and restart nginx + NATS |
