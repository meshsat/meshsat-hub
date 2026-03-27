# MeshSat Hub — Deployment Guide

MeshSat Hub supports three deployment tiers. Pick the one that matches your environment.

| Tier | Mode | What you get | Use case |
|------|------|-------------|----------|
| **Tier 1** | Standalone | SQLite + Mosquitto + Caddy on a single host | Dev, lab, single VPS, edge |
| **Tier 2** | Cluster | MariaDB Galera + NATS + Redis across 2+ hosts | Production HA, geo-distributed |
| **Tier 3** | Kubernetes | Helm chart, StatefulSets, Lease-based leader election | Cloud-native, auto-scaling |

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
git clone https://github.com/cubeos-app/meshsat-hub.git
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

## Tier 2: Cluster

Two or more hosts running MariaDB Galera (active-active replication), NATS (cross-site MQTT routing via leaf nodes), and Redis (shared rate limit + dedup state). Each host runs the full stack independently — if one host goes down, the other continues serving.

### Prerequisites

- 2+ Linux hosts with Docker Engine 24+ and Compose v2
- 2 vCPU, 2GB RAM, 20GB disk per host
- Private network between hosts (WireGuard VPN recommended for cross-site)
- TLS certificate for your domain (wildcard recommended: `*.example.com`)
- 1+ edge/VPS node with public IP for HAProxy (optional but recommended for mTLS)

### Architecture

```
                         Internet
                            |
                   +--------+--------+
                   |                 |
             VPS / Edge #1     VPS / Edge #2
             HAProxy :443      HAProxy :443
             (SNI passthrough) (SNI passthrough)
                   |                 |
           +-------+-------+ +------+-------+
           |               | |              |
      Node A (primary)     Node B (secondary)
      +-----------+        +-----------+
      | nginx     | :8451  | nginx     | :8451
      | Hub       | :6070  | Hub       | :6070
      | NATS      | :1883  | NATS      | :1883
      |           | :9443  |           | :9443
      | Redis     |        | Redis     |
      +-----------+        +-----------+
      | MariaDB   |<------>| MariaDB   |  Galera replication
      | garbd     | (host) |           |  (host network)
      +-----------+        +-----------+
           |                      |
           +--- NATS leaf --------+  (port 7422, bidirectional MQTT)

  Leaf topology: one side is "hub" (listen only), other is "spoke" (connects).
  garbd runs on ONE node only (3rd quorum voter to prevent split-brain).
```

### Step 1: Prepare both nodes

On **each** node:

```bash
mkdir -p /srv/meshsat-hub && cd /srv/meshsat-hub

# Get the files (or copy from your repo checkout)
# You need: docker-compose.galera.yml, nats.conf (or nats-mtls.conf),
#           nginx.conf, galera-entrypoint.sh, Dockerfile.garbd, .env

cp .env.cluster.example .env
nano .env
```

Edit `.env` on each node — these values **differ per node**:

| Variable | Node A | Node B |
|----------|--------|--------|
| `SITE_NAME` | `meshsat-hub-a` | `meshsat-hub-b` |
| `WSREP_NODE_ADDRESS` | `10.0.0.1` | `10.0.0.2` |
| `HUB_MQTT_CLIENT_ID` | `meshsat-hub-a` | `meshsat-hub-b` |

These values are **the same** on both nodes:

| Variable | Value |
|----------|-------|
| `WSREP_CLUSTER_ADDRESS` | `gcomm://10.0.0.1,10.0.0.2` |
| `MARIADB_ROOT_PASSWORD` | (same strong password) |
| `MARIADB_PASSWORD` | (same strong password) |
| `HUB_AUTH_TOKEN` | (same token) |

### Step 2: Provision TLS certificates

You need a TLS certificate for your domain. Options:

**Option A: Let's Encrypt wildcard (recommended)**
```bash
# Using certbot with DNS challenge
certbot certonly --manual --preferred-challenges dns \
  -d "*.example.com" -d "example.com"

# Copy certs to each node
scp /etc/letsencrypt/live/example.com/fullchain.pem node:/srv/certs/example.com.crt
scp /etc/letsencrypt/live/example.com/privkey.pem node:/srv/certs/example.com.key
```

**Option B: Self-signed (lab only)**
```bash
openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
  -keyout /srv/certs/example.com.key \
  -out /srv/certs/example.com.crt \
  -subj "/CN=*.example.com" \
  -addext "subjectAltName=DNS:*.example.com,DNS:example.com"
```

Mount the certs in nginx and NATS (update `docker-compose.galera.yml`):
```yaml
nginx:
  volumes:
    - /srv/certs/example.com.crt:/etc/nginx/certs/tls.crt:ro
    - /srv/certs/example.com.key:/etc/nginx/certs/tls.key:ro
```

For NATS mTLS, copy the server cert into the `nats-certs` volume after first start:
```bash
docker cp /srv/certs/example.com.crt meshsat-nats:/etc/nats/certs/server.crt
docker cp /srv/certs/example.com.key meshsat-nats:/etc/nats/certs/server.key
docker restart meshsat-nats
```

### Step 3: Bootstrap Galera cluster

**On Node A only** (the first node to start):

```bash
cd /srv/meshsat-hub

# Build the garbd image
docker build -f deploy/galera/Dockerfile.garbd -t meshsat-garbd:latest deploy/galera/

# Make the entrypoint executable
chmod +x galera-entrypoint.sh

# Set the bootstrap flag (one-time, consumed automatically)
docker volume create meshsat-hub_mariadb-data 2>/dev/null || true
docker run --rm -v meshsat-hub_mariadb-data:/var/lib/mysql alpine:3.21 \
  touch /var/lib/mysql/force-bootstrap

# Start MariaDB (entrypoint detects flag, bootstraps new cluster)
docker compose -f docker-compose.galera.yml up -d mariadb
sleep 30

# Verify: cluster_size=1, ready=ON
docker exec meshsat-mariadb mariadb -u root -p"$MARIADB_ROOT_PASSWORD" \
  -e "SHOW STATUS LIKE 'wsrep_cluster_size'; SHOW STATUS LIKE 'wsrep_ready';"
```

**On Node B** (joins the existing cluster):

```bash
cd /srv/meshsat-hub
chmod +x galera-entrypoint.sh

# Start MariaDB (joins Node A via IST/SST)
docker compose -f docker-compose.galera.yml up -d mariadb
sleep 30

# Verify: cluster_size=2
docker exec meshsat-mariadb mariadb -u root -p"$MARIADB_ROOT_PASSWORD" \
  -e "SHOW STATUS LIKE 'wsrep_cluster_size';"
```

**On Node A** (start garbd — 3rd quorum voter):

```bash
docker compose -f docker-compose.galera.yml up -d garbd
sleep 10

# Verify: cluster_size=3
docker exec meshsat-mariadb mariadb -u root -p"$MARIADB_ROOT_PASSWORD" \
  -e "SHOW STATUS LIKE 'wsrep_cluster_size';"
```

### Step 4: Configure NATS leaf nodes

NATS leaf nodes provide cross-site MQTT routing. One node is the "hub" (listen only), the other is the "spoke" (connects to the hub).

**Node A (hub)** — `nats.conf` has no `remotes`:
```
leafnodes {
  port: 7422
}
```

**Node B (spoke)** — `nats.conf` includes remotes pointing to Node A:
```
leafnodes {
  port: 7422
  remotes [
    { url: "nats-leaf://10.0.0.1:7422" }
  ]
}
```

> **Important:** Only ONE side should have `remotes`. If both sides connect to each other, NATS detects a loop and rejects the connection.

### Step 5: Start the application stack

On **each** node:

```bash
cd /srv/meshsat-hub

# Start Redis, NATS, Hub, nginx (never recreates MariaDB)
docker compose -f docker-compose.galera.yml up -d --no-deps redis nats
sleep 5
docker compose -f docker-compose.galera.yml up -d --no-deps hub nginx

# Verify
curl -k https://localhost:8451/healthz
curl -k https://localhost:8451/readyz
# Should show: {"status":"ok","checks":{"mariadb":{"status":"ok"},"mqtt":{"status":"ok"},"redis":{"status":"ok"},...}}
```

### Step 6: Set up HAProxy (for public access + mTLS)

If bridges need to connect from the internet via mTLS, deploy HAProxy on an edge/VPS node with a public IP.

```bash
# On the VPS/edge node
apt install haproxy
cp deploy/haproxy/haproxy.cfg.example /etc/haproxy/haproxy.cfg

# Edit: replace example.com with your domain, replace IPs with your node IPs
nano /etc/haproxy/haproxy.cfg

# Test and start
haproxy -c -f /etc/haproxy/haproxy.cfg
systemctl restart haproxy
```

**DNS setup:**
- `hub.example.com` A record → VPS public IP (Hub dashboard + API)
- `mqtt.example.com` A record → VPS public IP (mTLS MQTT for bridges)

### Step 7: Configure MQTT public URL

Set the URL that bridges see during onboarding:

```bash
curl -X PUT -H "Authorization: Bearer $HUB_AUTH_TOKEN" \
  -H "Content-Type: application/json" \
  https://hub.example.com/api/settings/mqtt-url \
  -d '{"mqtt_url":"wss://mqtt.example.com/mqtt"}'
```

### Firewall rules

Open these ports between cluster nodes (private network only):

| Port | Protocol | Purpose |
|------|----------|---------|
| 3306 | TCP | MariaDB client connections |
| 4444 | TCP | Galera SST (State Snapshot Transfer) |
| 4567 | TCP+UDP | Galera replication |
| 4568 | TCP | Galera IST (Incremental State Transfer) |
| 4570 | TCP | garbd (Galera arbitrator) |
| 7422 | TCP | NATS leaf node routing |

Example UFW rules (run on each node, replace `PEER_IP` with the other node's IP):

```bash
sudo ufw allow from PEER_IP to any port 3306,4444,4567,4568 proto tcp comment "Galera"
sudo ufw allow from PEER_IP to any port 4570 proto tcp comment "garbd"
sudo ufw allow from PEER_IP to any port 7422 proto tcp comment "NATS leaf"
```

### Galera safety rules

These rules prevent split-brain. Violating them can cause data loss.

1. **NEVER** modify `WSREP_CLUSTER_ADDRESS` in `.env` — not even for bootstrap. Use the flag-file method instead.
2. **NEVER** run `docker compose up -d` without `--no-deps` — it recreates MariaDB.
3. **NEVER** run `docker compose pull` — it evaluates all services including MariaDB.
4. **Always** deploy Hub with: `docker compose up -d --no-deps --force-recreate hub`

**To bootstrap after a full outage:**
```bash
# Find the node with highest seqno
docker run --rm -v meshsat-hub_mariadb-data:/var/lib/mysql alpine:3.21 \
  cat /var/lib/mysql/grastate.dat

# On the most advanced node ONLY:
docker run --rm -v meshsat-hub_mariadb-data:/var/lib/mysql alpine:3.21 \
  touch /var/lib/mysql/force-bootstrap
docker compose -f docker-compose.galera.yml up -d mariadb

# Then start the other node (joins via IST/SST):
# (on other node)
docker compose -f docker-compose.galera.yml up -d mariadb

# Then start garbd:
docker compose -f docker-compose.galera.yml up -d garbd
```

### Migrating from Standalone to Cluster

```bash
./scripts/migrate-sqlite-to-mariadb.sh /data/hub.db "mariadb://user:pass@node:3306/meshsat_hub"
```

---

## Tier 3: Kubernetes (minimal)

> Tier 3 is functional but less battle-tested than Tier 2. Use for cloud-native deployments.

### Quick Start (Helm)

```bash
helm install meshsat-hub deploy/helm/meshsat-hub/ \
  --namespace meshsat-hub --create-namespace \
  --set secrets.authToken="$(openssl rand -hex 32)" \
  --set secrets.databaseUrl="mariadb://..." \
  --set secrets.redisUrl="redis://..." \
  --set ingress.enabled=true \
  --set ingress.host=hub.example.com
```

### Quick Start (Raw Manifests)

```bash
kubectl apply -f deploy/k8s/namespace.yaml
kubectl apply -f deploy/k8s/secret.yaml       # edit first!
kubectl apply -f deploy/k8s/configmap.yaml
kubectl apply -f deploy/k8s/mariadb-statefulset.yaml
kubectl apply -f deploy/k8s/redis-deployment.yaml
kubectl apply -f deploy/k8s/nats-statefulset.yaml
kubectl apply -f deploy/k8s/hub-deployment.yaml
kubectl apply -f deploy/k8s/ingress.yaml       # edit host!
```

### Components

| Resource | Kind | Replicas | Purpose |
|----------|------|----------|---------|
| Hub | Deployment | 2 | API + SPA, `HUB_MODE=kubernetes` |
| MariaDB | StatefulSet | 1 | Persistent store |
| Redis | Deployment | 1 | Dedup + rate limit |
| NATS | StatefulSet | 3 | Clustered message bus |
| Ingress | Ingress | -- | TLS termination |

Leader election for singleton services (TAK, APRS-IS) uses the Kubernetes Lease API.

---

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

Use `nats-mtls.conf` instead of `nats.conf` (set `NATS_CONFIG=./nats-mtls.conf` in `.env`). The key section:

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
