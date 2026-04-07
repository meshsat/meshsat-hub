# Incident 11 — Galera Cluster Total Loss from Network Partition + UUID Fragmentation

**Date:** 2026-04-07
**Duration:** ~110 minutes (09:41 – 11:31 UTC)
**Severity:** P0 — Full service outage (hub.meshsat.net unreachable)
**Type:** E (new) — Network partition + Docker UUID fragmentation
**Author:** Claude Code (post-mortem analysis)

---

## Summary

A transient S2S IPsec tunnel instability between NL and GR caused a Galera quorum loss. Despite garbd providing a 3rd quorum voter, the cluster could not self-recover because:

1. garbd is co-located with NL — useless for site-level partitions
2. Docker container restarts generate new WSREP UUIDs, fragmenting cluster membership
3. Hub containers died after 30s of WSREP 1047 errors instead of retrying
4. gvwstate.dat (pc.recovery state) was not preserved across the crash

Manual bootstrap was required after ~90 minutes of failed automated recovery attempts.

---

## Timeline

| Time (UTC) | Event |
|------------|-------|
| 09:41:18 | GR loses Galera connectivity to ALL NL endpoints (MariaDB :4567 + garbd :4570). TCP handshakes succeed intermittently but Galera messages time out within 3s. |
| 09:41:25 | GR suspects garbd, then NL MariaDB |
| 09:41:28 | **Both** Hub containers start receiving WSREP Error 1047 ("not yet prepared for application use") |
| 09:41:30 | GR goes NON_PRIM (1/3, no quorum). NL + garbd form view 18 (2/3, should be PRIMARY), but the view transition briefly rejects queries with 1047 |
| 09:41:33–52 | GR intermittently establishes TCP connections to NL, but Galera protocol messages never complete. Classic half-open connection behavior (IPsec tunnel). |
| 09:42:01 | NL Hub shuts down (30s of persistent 1047 errors) |
| 09:42:02 | GR Hub shuts down. GR MariaDB shuts down from NON_PRIM state — **does NOT write gvwstate.dat** |
| 09:42:07 | GR MariaDB auto-restarts (Docker `unless-stopped`). Gets new WSREP UUID. Can't reach NL → crash loop. |
| 09:42:11 | NL MariaDB restarts (new WSREP UUID). **gvwstate.dat missing** → pc.recovery fails. Connects to garbd, but garbd now knows 4 members (old-NL, new-NL, GR, garbd) → 2/4 ≠ majority → **NON_PRIM** |
| 09:42:43 | NL MariaDB: "failed to reach primary view: Connection timed out" → abort → crash loop repeats |
| ~10:00–11:28 | Multiple manual recovery attempts. rsync SST on GR fails with error 114 (EALREADY) repeatedly |
| 11:28:50 | NL bootstrapped via flag-file method |
| 11:29:17 | GR SST succeeds, joins cluster. cluster_size=2 |
| 11:31:19 | garbd restarted. cluster_size=3. Full recovery confirmed. |

---

## Root Cause Analysis

### Trigger

Transient S2S IPsec tunnel instability between NL (192.168.192.10) and GR (192.168.15.10). TCP SYN/ACK handshakes succeeded intermittently but Galera group communication (higher-level protocol) messages timed out within 3 seconds. Same tunnel class that caused Incident 10 (2026-04-03).

### Why garbd didn't save us (architectural flaw)

garbd is co-located with NL MariaDB on 192.168.192.10. When the failure is an NL↔GR network partition, garbd can never act as a tiebreaker:

```
       NL site (192.168.192.10)              GR site (192.168.15.10)
    ┌─────────────────────────┐           ┌─────────────────────────┐
    │  MariaDB + garbd (2/3)  │──── X ────│  MariaDB (1/3)          │
    │  garbd sees MariaDB ✓   │  tunnel   │  sees nothing ✗         │
    │  garbd sees GR ✗        │  down     │                         │
    └─────────────────────────┘           └─────────────────────────┘
```

NL + garbd = 2/3 → they DID initially keep quorum and formed view 18. But the intermittent tunnel caused repeated view transitions, destabilizing the cluster.

### Why recovery failed (the UUID fragmentation bug)

This is the core finding — a previously unknown failure mode:

1. Both Hub containers died after 30s of WSREP 1047 errors (view transition noise)
2. NL MariaDB was restarted by Docker (`unless-stopped` policy)
3. **Each container restart generates a new WSREP UUID.** garbd's quorum calculation now saw **4** known members:

```
garbd's view of the cluster:
  - old NL MariaDB (2ad4b7fd) — partitioned (dead)
  - new NL MariaDB (0df9d6f0) — present
  - GR MariaDB (61ba1b0f) — partitioned (unreachable)
  - garbd itself (51de54d0) — present

Present: 2 / Known: 4 = NOT a majority → NON_PRIM
```

4. gvwstate.dat was missing (Galera deletes it after joining; only written on clean primary shutdown — NOT written when shutting down from NON_PRIM state)
5. Without gvwstate.dat, pc.recovery couldn't restore the previous primary component
6. **Each subsequent restart generated yet another UUID**, adding more phantom members and making quorum impossible

### The vicious cycle

```
Container restarts → New UUID → More "known" members → Quorum denominator grows →
Less likely to achieve majority → NON_PRIM → Container crashes → Repeat
```

This is fundamentally incompatible with Docker's container lifecycle model.

---

## Contributing Factors

| Factor | Impact | Severity |
|--------|--------|----------|
| **garbd co-located with NL** | Zero benefit for site-level partitions — the #1 failure mode | Critical |
| **Docker restart = new WSREP UUID** | Fragments cluster membership, dilutes quorum denominator | Critical |
| **Hub dies on 30s of WSREP 1047** | Transient view transitions (~5s) escalate to full outage | High |
| **gvwstate.dat lost on non-clean shutdown** | pc.recovery can't work after NON_PRIM crash | Medium |
| **No S2S tunnel monitoring** | 2nd incident caused by same tunnel, no alerting | Medium |
| **GR NATS unhealthy since Mar 31** | JetStream account MESHSAT unresolvable (pre-existing, unrelated) | Low |

---

## Preventive Actions

### P0 — This week

**1. Move garbd to a 3rd network location.**
Deploy garbd on a lightweight VM outside both NL and GR networks (management VLAN, VPS, or cloud instance). This is the only way garbd can act as a true tiebreaker for site-level partitions. The current placement is security theater.

**2. Hub: retry WSREP 1047 with exponential backoff instead of shutting down.** (MESHSAT-TBD)
Hub currently treats 30s of 1047 as fatal and exits. WSREP 1047 during view transitions is expected and transient (typically <5s). Hub should retry DB connections indefinitely with backoff (1s, 2s, 4s, 8s... cap at 60s).

### P1 — This sprint

**3. S2S IPsec tunnel health monitoring.**
Add a cron probe (every 30s) that tests TCP connectivity between DMZ nodes on Galera port 4567. On 3 consecutive failures → alert via ntfy/Matrix.

**4. Entrypoint smart auto-bootstrap.** (MESHSAT-TBD)
Enhance `galera-entrypoint.sh`: if gvwstate.dat is missing AND this node can reach garbd (local) but NOT the remote data node, automatically set safe_to_bootstrap=1 and bootstrap. Add a 60s delay + recheck before bootstrapping to avoid racing with a recovering tunnel.

### P2 — Medium-term

**5. Fix GR NATS JetStream account resolution.** (MESHSAT-TBD)
The MESHSAT JetStream account healthcheck has been failing continuously since 2026-03-31.

**6. Investigate Galera wsrep_node_name persistence.** (MESHSAT-TBD)
Research whether setting a fixed wsrep_node_name and wsrep_node_address prevents UUID regeneration across container restarts.

---

## Root Cause Taxonomy (updated)

| Type | Description | Incidents |
|------|-------------|-----------|
| A | Bare `gcomm://` left in `.env` | 2, 3, 5, 9 |
| B | `.env` correct but running container stale | 6, 7, 8 |
| C | Infrastructure (unknown) | 6 |
| D | AWX generic playbook + Docker Compose v5 + IPsec stale SA | 10 |
| **E** | **Network partition + Docker UUID fragmentation** | **11** |

Types A–D are configuration/deployment errors (preventable by rules and guards).
Type E is an **architectural deficiency** — garbd placement + Docker lifecycle = false quorum safety.

---

## Verification

Post-recovery cluster state (2026-04-07 11:33 UTC):

| Check | NL | GR |
|-------|----|----|
| MariaDB | Synced, healthy | Synced, healthy |
| cluster_size | 3 | 3 |
| wsrep_last_committed | 128 | 128 |
| wsrep_ready | ON | ON |
| garbd | Running | — |
| Hub | healthy | healthy |
| hub.meshsat.net | healthz=ok, readyz=ok (mariadb 1ms, mqtt ok, redis ok, reticulum ok) |

InnoDB "LSN in the future" warning on GR after SST is transient and harmless — GR is Synced and applying transactions normally.
