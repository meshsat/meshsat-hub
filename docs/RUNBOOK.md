# MeshSat Hub — Operational Runbook

## Quick Reference

| Action | Command |
|--------|---------|
| Deploy Hub (safe) | `ansible-playbook -i inventory.yml playbooks/deploy-hub.yml` |
| Bootstrap new cluster | `ansible-playbook -i inventory.yml playbooks/bootstrap.yml` |
| Recover after outage | `ansible-playbook -i inventory.yml playbooks/recover.yml` |
| Add a node | `ansible-playbook -i inventory.yml playbooks/add-node.yml --limit new-host` |
| Check cluster health | `curl https://hub.meshsat.net/api/cluster/status` |
| Check single node | `curl https://hub.meshsat.net/api/cluster/node` |

---

## Architecture

```
NL (nllei01dmz01)                   GR (grskg01dmz01)
├─ MariaDB Galera node 1            ├─ MariaDB Galera node 2
├─ garbd (quorum arbitrator)        ├─ Redis
├─ Redis                            ├─ NATS
├─ NATS                             ├─ Hub (cluster mode)
├─ Hub (cluster mode)               └─ nginx (:8451)
└─ nginx (:8451)
         ▲                                    ▲
         └── hub.meshsat.net (HAProxy LB) ────┘
```

- **Galera cluster size: 3** (2 data nodes + 1 garbd arbitrator on NL)
- **Synchronous replication** — writes on NL visible on GR instantly
- **HAProxy health check**: `/readyz` checks `wsrep_ready` — returns 503 if node can't accept writes

---

## Common Scenarios

### 1. Deploy new Hub code (CI/CD does this automatically)

```bash
# This is what the pipeline runs. NEVER use docker compose pull or docker compose up -d
# as it will recreate MariaDB and break the cluster.
ansible-playbook -i deploy/ansible/inventory.yml deploy/ansible/playbooks/deploy-hub.yml
```

**Manual equivalent (per host):**
```bash
cd /srv/meshsat-hub
docker pull ghcr.io/cubeos-app/meshsat-hub:latest
docker compose up -d --no-deps --force-recreate hub
```

**CRITICAL**: Always use `--no-deps --force-recreate hub`. NEVER run bare `docker compose up -d`.

### 2. GR goes down (NL + garbd maintain quorum)

**What happens:**
- NL + garbd = 2/3 quorum → NL continues read/write
- HAProxy stops routing to GR (readyz returns 503)
- GR comes back → MariaDB auto-rejoins, SST syncs data
- HAProxy resumes routing to GR

**If GR doesn't auto-recover:**
```bash
ssh grskg01dmz01
cd /srv/meshsat-hub
docker compose up -d mariadb           # Rejoin cluster
# Wait for sync (check: docker exec meshsat-mariadb mariadb -u root -p... -e "SHOW STATUS LIKE 'wsrep_local_state_comment'")
docker compose up -d --no-deps redis nats hub nginx
```

### 3. NL goes down (GR goes read-only)

**What happens:**
- GR alone = 1/3 quorum → MariaDB enters non-primary state (read-only)
- HAProxy detects via readyz 503 → **site goes down** (both backends fail)
- When NL recovers, cluster reforms automatically

**Recovery:**
```bash
# On NL:
cd /srv/meshsat-hub
docker compose up -d mariadb           # Will rejoin GR
# Wait for sync, then start the rest
docker compose up -d --no-deps redis nats hub nginx
# Restart garbd
docker start meshsat-garbd
```

### 4. BOTH nodes down (total cluster failure)

```bash
# Use the Ansible recovery playbook — finds the most advanced node automatically
ansible-playbook -i deploy/ansible/inventory.yml deploy/ansible/playbooks/recover.yml --ask-vault-pass
```

**Manual recovery:**
```bash
# 1. Find which node has the latest data
# On each node:
docker run --rm -v meshsat-hub_mariadb-data:/var/lib/mysql mariadb:11-jammy cat /var/lib/mysql/grastate.dat
# Look for the highest seqno

# 2. Bootstrap from the most advanced node
ssh <best-node>
cd /srv/meshsat-hub
# Edit grastate.dat: safe_to_bootstrap: 1
WSREP_CLUSTER_ADDRESS=gcomm:// docker compose up -d mariadb

# 3. Join other nodes
ssh <other-node>
cd /srv/meshsat-hub
docker compose up -d mariadb

# 4. Start everything else
# On all nodes:
docker compose up -d --no-deps redis nats hub nginx
# On NL: docker start meshsat-garbd
```

### 5. Add a third data node

1. Provision the new host with Docker
2. Copy `/srv/meshsat-hub/docker-compose.yml`, `nats.conf`, `nginx.conf` to it
3. Add to `inventory.yml`
4. Update `galera_cluster_address` on all nodes to include the new VPN IP
5. Open firewall ports between all nodes
6. Run: `ansible-playbook -i inventory.yml playbooks/add-node.yml --limit new-host`

### 6. MariaDB split-brain (should not happen with garbd)

If garbd is down and both data nodes lose connectivity:
```bash
# Check cluster state on each node
docker exec meshsat-mariadb mariadb -u root -p... -e "SHOW STATUS LIKE 'wsrep_cluster_status'"
# If both show "Non-primary":
# 1. Pick the node with highest seqno (check grastate.dat)
# 2. Bootstrap that node: WSREP_CLUSTER_ADDRESS=gcomm:// docker compose up -d mariadb
# 3. Join the other node normally
# WARNING: The non-bootstrapped node will lose any writes that happened during split-brain
```

---

## Health Checks

| Endpoint | What it checks | Used by |
|----------|---------------|---------|
| `GET /healthz` | Process alive | Docker healthcheck |
| `GET /readyz` | MariaDB (wsrep_ready) + MQTT + Redis | HAProxy, Kubernetes |
| `GET /api/cluster/node` | Full Galera metrics (22 vars) | Cluster UI, peer queries |
| `GET /api/cluster/status` | Aggregated all-node view | Dashboard |

---

## Key Ports

| Port | Service | Network |
|------|---------|---------|
| 3306 | MariaDB | VPN only (host network) |
| 4567 | Galera replication | VPN only (host network) |
| 4568 | Galera IST | VPN only (host network) |
| 4444 | Galera SST (rsync) | VPN only (host network) |
| 4570 | garbd listen | VPN only (host network) |
| 6070 | Hub HTTP | Docker internal |
| 8451 | nginx (TLS) | External |
| 1883 | NATS MQTT | Docker internal |

---

## Monitoring

The Cluster Health page at `https://hub.meshsat.net/#/cluster` shows:
- Per-node Galera state (Synced/Donor/Joined/Initialized)
- Cluster partition status (Primary/Non-primary)
- Write readiness (wsrep_ready)
- Replication queue depths
- Flow control percentage
- Auto-refreshes every 10 seconds

**Alerting**: Set up external monitoring to poll `/readyz` on each node. Alert if:
- Status is not 200 for > 60 seconds
- `wsrep_cluster_size` drops below expected count
- `wsrep_flow_control_paused` > 0.5 (replication bottleneck)
