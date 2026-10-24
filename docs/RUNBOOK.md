# MeshSat Hub — Operational Runbook

> **2026-09-08:** the Hub moved to notrf01cl01k8s (CNPG Postgres, Argo CD; see `k8s/NOTES.md`
> and `docs/deployment.md`). The Galera/compose procedures below are kept as history for the
> incident post-mortems; none of the commands apply to the running system any more.

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
# retired: merge to main; CI bumps the image pin and Argo CD rolls the k8s Deployment
```

**Manual equivalent (per host):**
```bash
cd /srv/meshsat-hub
docker pull ghcr.io/meshsat/meshsat-hub:latest
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
# retired with the Galera cluster (2026-09-08)
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

### 7. The map loads slowly, or shows no streets or street names when zoomed in

The map reads two PMTiles archives. The world archive (`/basemap/basemap.pmtiles`, zoom 0-11)
comes through the Hub from the object store. The deep Europe archive (`/basemap/local.pmtiles`,
street geometry from zoom 13, names from 15) comes from the `basemap` StatefulSet, and the
Ingress routes it there from the ingress-nginx controller pod. If the deep archive does not
answer, the SPA's `HEAD` probe delays the first paint by up to 30 s, and nothing past zoom 11
draws. Probe each edge address separately:
```bash
for ip in $(getent ahostsv4 hub.meshsat.net | awk '{print $1}' | sort -u); do
  curl -s -o /dev/null --resolve hub.meshsat.net:443:$ip -H 'Range: bytes=0-16383' --max-time 30 \
    -w "$ip %{http_code} %{time_total}s\n" https://hub.meshsat.net/basemap/local.pmtiles
done
# Expect 206 in about a second. A 504 or a 30 s hang: check the basemap pods are Ready, then
# `kubectl -n ingress-nginx logs <controller> | grep local.pmtiles` for "upstream timed out (110)",
# which means a NetworkPolicy is dropping ingress-nginx -> basemap:8080 (MESHSAT-1229).
```

### 8. Support access and view-as (a customer asks for help inside their workspace)

The platform console (nav group **Platform**, platform admins only) has a Tenants directory and
a tenant detail page with "Open as this tenant". That button works only after the CUSTOMER has
granted support access, and it needs their PIN (MESHSAT-1366). There is no operator override:
a signed-in admin's `X-Tenant-ID` for another tenant is refused by the tenant middleware until a
grant exists and has been opened with its PIN. The break-glass token and platform API keys keep
the header (the verify suites and billing probes run on them, and both are audited per use).

1. The customer, as an owner, opens Settings, section **Support access**: types or generates a
   PIN (10 to 128 characters), picks a window (bounded by `HUB_SUPPORT_ACCESS_MIN_MINUTES`,
   default 15, and `HUB_SUPPORT_ACCESS_MAX_MINUTES`, default 4320 = 72 h) and clicks Grant.
   The PIN is stored Argon2id-hashed and never shown again. They tell support the PIN out of band.
   Their audit log gets `support_access_granted`.
2. The operator opens Platform > Tenants > the tenant > "Open as this tenant", enters the PIN.
   Five wrong PINs revoke the grant (`support_access_locked`, both chains); the customer has to
   grant again. On success the console shows a caution banner with the expiry, every API call
   carries `X-Tenant-ID`, and BOTH audit chains get `tenant_view_started`; Exit writes
   `tenant_view_ended`. The window ends at the grant's expiry or when the customer clicks Revoke.
3. To check state by hand: `GET /api/admin/tenants/{id}` carries `support_access`
   (`active`, `expires_at`, `used_at`, `used_by_email`); the table is `support_grants`
   (`pin_hash` is withheld from exports).

Other operator actions on the same page (plan, expiry, send caps, suspend, reactivate, close) need
no grant, because they are the platform's own decisions; each is written to the tenant's chain as
`tenant_admin_updated` or `tenant_deleted` and mirrored to the platform chain with `tenant=<id>`.

### 9. A read-only platform key (a tool that only looks customers up)

A key minted with `platform_read` (MESHSAT-1527) reads the platform directory and nothing else:
`GET /api/admin/tenants` (`?email=` narrows to the tenants that have a user with that address,
case-insensitive; `?q=` and `?include_deleted=1` as before), `GET /api/admin/tenants/{id}`,
`/{id}/users`, `/{id}/usage`, `GET /api/admin/signups` and `/api/admin/signups/history`. Every
write, the tenant audit chain, payments, receipts, refunds and VAT answer it 403. Never hand such
a consumer the platform-admin key instead: that one can close tenants whatever its role says.

Minting needs a credential that already holds `platform_admin`: the platform-admin API key
(`HUB_VERIFY_ADMIN_KEY` in OpenBao `secret/ci-no/apps/meshsat-hub/verify`, a key of the `default`
tenant) or a signed-in platform admin's session. The break-glass `HUB_AUTH_TOKEN` is refused in
production. The new key belongs to the caller's tenant, which for that key is `default`; the role
must be `viewer` (the default), and `platform_read` cannot be combined with `platform_admin`.

```bash
curl -sS -X POST https://hub.meshsat.net/api/auth/keys \
  -H "Authorization: Bearer $HUB_VERIFY_ADMIN_KEY" -H "Content-Type: application/json" \
  -d '{"label":"<consumer> (read-only platform)","role":"viewer","platform_read":true,"expires_in":"8760h"}'
# 201: {"key":"meshsat_...","id":"key-...","key_prefix":"meshsat_...","role":"viewer",
#       "label":"...","expires_at":"...","platform_read":true}
```

The plaintext `key` is shown once; store it in OpenBao under the consumer's path, never in git.
The minting writes `api_key_created ... platform_read=true` to the `default` tenant's chain. Each
directory read the key makes leaves an Info line `auth: platform read actor=apikey:<id>` on the
replica that served it, and the row's `last_used` moves.

To list or revoke it, use the SAME tenant it was minted in (`default`), or the delete matches
nothing and still answers 204:

```bash
curl -sS https://hub.meshsat.net/api/auth/keys -H "Authorization: Bearer $HUB_VERIFY_ADMIN_KEY"
curl -sS -X DELETE https://hub.meshsat.net/api/auth/keys/<id> -H "Authorization: Bearer $HUB_VERIFY_ADMIN_KEY"
```

Revocation is immediate on both replicas (the key is looked up on every request) and writes
`api_key_deleted`. An owner signed in to the console in that tenant can do the same on the
API keys page (`/api-keys`).

---

### 10. The Reticulum node, a tenant's LXMF address, a client that cannot reach it

The upstream-compatible Reticulum node runs on the LEADER replica only; the `hub-rns` Service follows
the leader's pod label and stunnel connects through it (MESHSAT-1555). Full detail and the API in
`docs/reticulum-lxmf.md`.

```bash
kubectl -n meshsat-hub get pods -l meshsat.net/rns-leader=true   # exactly one pod
kubectl -n meshsat-hub get endpoints hub-rns                      # that pod, ports 4242 and 4244
kubectl -n meshsat-hub logs <leader pod> | grep 'rns: upstream-compatible Reticulum node started'
curl -s -H "Authorization: Bearer $HUB_VERIFY_ADMIN_KEY" https://hub.meshsat.net/api/rns/status  # read twice: one replica says active
```

- No endpoint on `hub-rns`: the leader could not label itself (RBAC on pods, see the leader's log) or
  the running image predates the label. `kubectl label pod <running hub pod> meshsat.net/rns-leader=true`
  bridges it until the next rollout.
- A tenant's address is live when the leader logs `lxmf: address registered` for it and
  `GET /api/lxmf/identity` answers `active_on_this_replica: true` there.
- A fresh Sideband or CrossTalk connection shows the Hub's addresses 15 to 20 s after connecting:
  upstream ingress control holds announce bursts on a new interface. Not a fault.
- The public endpoint `rns.meshsat.net:4242` is the VPS edge plus DNS, outside this repo.

### 11. The "@" lane: a device's message to a person, or a reply that did not arrive

A tenant's satellite modem writes `@+31612345678 text` / `@name@example.org text` / `@Contact text`;
the Hub sends it on the tenant's own Twilio or mail gateway and the reply comes back to the modem as
`[from <address>] text` (MESHSAT-1685). Contract: `docs/bridge-hub-protocol.md`, "The @ lane".

```bash
for p in $(kubectl -n meshsat-hub get pods -l app.kubernetes.io/name=hub -o name); do
  kubectl -n meshsat-hub logs $p | grep 'personchat:' | tail -20     # read BOTH pods
done
curl -s -H "Authorization: Bearer $KEY" https://hub.meshsat.net/api/tenant | jq '.person_chat_mode, .person_chat_mode_default'
```

- Nothing sent, and the device got `[MeshSat Hub] not sent to …: <reason>`: the reason is the
  answer (not in contacts, no SMS provider, no mail gateway, a limit, an own number or a kit SIM).
- Nothing sent and no notice: the message did not come through the satellite provider (a bridge's
  own mo/decoded is never sent), the modem is not registered to that tenant, the address did not
  parse (`@word` that is no contact is ordinary traffic), the tenant's mode is `off`, the satellite
  chain was `sealed` (the Hub cannot read the address), or the tenant's notices for the day are used.
- A reply that did not reach the device: outside the 168 h window, an SMS without a Twilio
  MessageSid, a mail without `[ref …]` in its subject or from another address, an auto-reply, a
  failing domain verdict, a closed conversation's tag, the device no longer the tenant's, or a
  ceiling (the person is told once a day). The rows: `person_conversations`, the ledger
  `person_chat_sends`, the ceilings `person_chat_buckets`, all keyed by tenant.
- Inbound mail reaches the lane only through the tenant's mail service forwarding to
  `POST /api/webhook/email/{secret}` (Settings, Integrations, Email gateway; replies go to its Reply
  address).
- Off switch: `HUB_PERSONCHAT_ENABLED=false` in the ConfigMap (Reloader rolls the Hub). Per tenant:
  Settings, "Devices may write to: Nobody".
- Never test it by publishing a synthetic satmo message on production: it sends a real SMS or mail
  and a real satellite MT.

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
