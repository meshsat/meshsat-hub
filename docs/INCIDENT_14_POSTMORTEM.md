# Incident 14 Postmortem — garbd solo PRIMARY poisons cluster with proto 127/127

**Date:** 2026-04-09
**Duration:** ~45 minutes (detection to recovery)
**Severity:** P1 — full cluster outage, both Hub instances down
**Issue:** MESHSAT-507

## Timeline

| Time (UTC) | Event |
|------------|-------|
| ~03:00 | Unknown trigger causes all Galera nodes to restart simultaneously |
| ~03:10 | garbd restarts first, recovers saved gvwstate.dat, self-promotes to PRIMARY |
| ~03:10 | garbd forms single-member PRIMARY with protocols `2/127/127` |
| ~03:15 | GR MariaDB restarts, attempts to join garbd's PRIMARY |
| ~03:15 | GR sees `repl_proto_ver: 127` (max supported: 10) — aborts with SIGSEGV (exit 139) |
| ~03:15 | GR enters crash loop: restart → join → proto mismatch → SIGSEGV → repeat |
| ~03:20 | NL MariaDB restarts, also stuck in NON-PRIMARY (cannot join poisoned group) |
| ~03:22 | Hub services on both nodes exit (MariaDB unavailable) |
| 03:25 | Issue detected, investigation begins |
| 03:27 | Root cause identified: garbd proto 127/127 poison |
| 03:31 | First bootstrap attempt — NL bootstrapped but GR auto-restarted during SST, quorum lost |
| 03:34 | Second attempt — bootstrap NL, start garbd (2/3 quorum), then GR. Failed: tc.log corruption |
| 03:38 | Third attempt — clean tc.log + galera.cache + gvwstate.dat, fresh bootstrap. **Success.** |
| 03:39 | NL PRIMARY, GR joins via SST, garbd joins. cluster_size=3, all Synced |
| 03:40 | Hub services started on both nodes. readyz checks pass. |

## Root Cause

**garbd formed a solo PRIMARY component and advertised Galera protocol versions 127/127.**

When all cluster members restart simultaneously (e.g., host reboot, power event), Docker's `restart: unless-stopped` policy restarts containers independently — `depends_on` ordering is only enforced during `docker compose up`, not during crash recovery.

garbd restarted before either MariaDB data node. It found its saved `gvwstate.dat` with the old PRIMARY view and, with `pc.recovery=TRUE`, self-promoted to PRIMARY as a single member (1/3 of the cluster, but the only online member).

garbd's internal protocol implementation uses max values (`repl_proto: 127, appl_proto: 127`) since it doesn't actually process replication data. When MariaDB nodes subsequently restarted and tried to join garbd's PRIMARY component, they encountered:

```
Group requested repl_proto_ver: 127, max supported by this node: 10
Group requested appl_proto_ver: 127, max supported by this node: 4
```

MariaDB called `abort()` inside `libgalera_smm.so`, triggering SIGSEGV (signal 11, exit code 139). With `restart: unless-stopped`, MariaDB entered an infinite crash loop.

## Impact

- **Full outage**: Both Hub instances down for ~45 minutes
- **No data loss**: InnoDB data files were intact throughout; only Galera replication state was corrupted
- **Recovery complexity**: Required 3 attempts due to cascading state corruption (tc.log, galera.cache)

## Why It Took 3 Attempts

1. **Attempt 1**: Bootstrapped NL, but GR's `restart: unless-stopped` auto-restarted GR during the SST. GR connected, NL became donor, then GR was partitioned — NL lost quorum (1 of 2 < 50%).
2. **Attempt 2**: Bootstrapped NL, started garbd first for quorum safety. But `tc.log` from the crash cycles blocked InnoDB crash recovery (`Recovery failed! You must enable all engines`). All 3 nodes formed a PRIMARY but none could serve as SST donor (all had zeroed positions).
3. **Attempt 3**: Cleaned `tc.log`, `galera.cache`, and `gvwstate.dat` on both nodes. Bootstrap NL → wait for Synced → start garbd (cluster_size=2) → start GR (SST from NL) → cluster_size=3. Success.

**Key lesson**: `docker update --restart=no` must be applied to ALL nodes before recovery, not just the ones you're currently working on. GR's auto-restart during NL's SST caused the first attempt to fail.

## Fix (Incident 14 prevention)

### 1. garbd startup wrapper (`scripts/garbd-entrypoint.sh`)

Waits for at least one MariaDB data node to be reachable on port 4567 before starting garbd. Checks both NL and GR every 5 seconds, gives up after 5 minutes.

**Effect**: garbd can never form a solo PRIMARY. It always JOINS an existing cluster formed by a data node.

### 2. MariaDB proto poison guard (`scripts/galera-entrypoint.sh`)

On startup, checks `gvwstate.dat` for the garbd proto signature (`Protocols : 2 / 127 / 127`). If found, removes the poisoned file and falls through to auto-bootstrap logic.

**Effect**: Even if garbd somehow poisoned the group view, MariaDB detects it before attempting to join, preventing the SIGSEGV crash loop.

### 3. Compose file updated

garbd service mounts the wrapper script. Dockerfile copies it as the entrypoint.

## Recovery Procedure (for future reference)

If this happens again despite the prevention layers:

```bash
# 1. Stop everything
ssh NL 'docker update --restart=no meshsat-garbd meshsat-mariadb && docker stop meshsat-garbd meshsat-mariadb'
ssh GR 'docker update --restart=no meshsat-mariadb && docker stop meshsat-mariadb'

# 2. Clean state on BOTH nodes
for host in NL GR; do
  ssh $host 'docker run --rm -v meshsat-hub_mariadb-data:/data alpine:3.21 \
    sh -c "rm -f /data/gvwstate.dat /data/tc.log /data/galera.cache /data/auto-bootstrap-retries"'
done

# 3. Set bootstrap flag on authoritative node (NL)
ssh NL 'docker run --rm -v meshsat-hub_mariadb-data:/data alpine:3.21 touch /data/force-bootstrap'

# 4. Bootstrap NL (alone!)
ssh NL 'cd /srv/meshsat-hub && docker compose up -d --force-recreate --no-deps mariadb'
# Wait for: Synced, PRIMARY, wsrep_ready=ON

# 5. Start garbd (cluster_size → 2)
ssh NL 'cd /srv/meshsat-hub && docker compose up -d --force-recreate --no-deps garbd'
# Wait for: cluster_size=2

# 6. Start GR (joins via SST, cluster_size → 3)
ssh GR 'cd /srv/meshsat-hub && docker compose up -d --force-recreate --no-deps mariadb'
# Wait for: cluster_size=3, both Synced

# 7. Re-enable restart policies
ssh NL 'docker update --restart=unless-stopped meshsat-garbd meshsat-mariadb'
ssh GR 'docker update --restart=unless-stopped meshsat-mariadb'

# 8. Start Hub
ssh NL 'cd /srv/meshsat-hub && docker compose up -d --no-deps --force-recreate hub'
ssh GR 'cd /srv/meshsat-hub && docker compose up -d --no-deps --force-recreate hub'
```

## Prevention Layer Summary (15 layers total, updated)

| # | Layer | Prevents | Added |
|---|-------|----------|-------|
| 1 | galera-entrypoint.sh: bare gcomm:// guard | Independent bootstrap via stale compose | Incident 1 |
| 2 | galera-entrypoint.sh: flag-file bootstrap | .env mutation during bootstrap | Incident 4 |
| 3 | galera-entrypoint.sh: auto-bootstrap Path A | Stuck after partition (remote unreachable) | Incident 11 |
| 4 | galera-entrypoint.sh: auto-bootstrap Path B | Stuck after tunnel recovery (both NON_PRIM) | Incident 13 |
| 5 | pc.recovery=TRUE | Auto-reform after simultaneous restart | Incident 3 |
| 6 | evs.view_forget_timeout=PT5M | Stale WSREP UUID quorum fragmentation | Incident 7 |
| 7 | garbd (3rd voter) | Split-brain on network partition | Incident 5 |
| 8 | CI pre-deploy gate | Deploying to unhealthy cluster | Incident 8 |
| 9 | check-galera-health.sh | Manual deploy to unhealthy cluster | Incident 8 |
| 10 | Hub dbwrap WSREP 1047 retry | Hub crash during Galera view transitions | Incident 9 |
| 11 | galera-watchdog.sh: --force-recreate --no-deps | Compose v5 stale container state | Incident 10 |
| 12 | WSREP_CLUSTER_ADDRESS hardcoded | Variable expansion differences across hosts | Incident 10 |
| 13 | AWX dedicated deploy playbook | Generic playbook touching MariaDB | Incident 12 |
| 14 | **garbd-entrypoint.sh: data node wait** | **garbd solo PRIMARY (proto 127/127 poison)** | **Incident 14** |
| 15 | **galera-entrypoint.sh: proto poison guard** | **Joining garbd-poisoned group (SIGSEGV)** | **Incident 14** |

## Type Classification

**Type H (new): Arbitrator quorum poisoning**
garbd forming PRIMARY alone advertises incompatible protocol versions. Different from Type G (entrypoint env mismatch) because the arbitrator itself is the poison source, not a configuration error.
