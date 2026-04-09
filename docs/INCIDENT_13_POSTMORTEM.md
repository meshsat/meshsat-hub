# Incident 13 — Galera Auto-Bootstrap Never Worked + Both-NON_PRIM Deadlock

**Date:** 2026-04-09
**Duration:** ~110 minutes (00:19 – 02:08 UTC)
**Severity:** P0 — Full service outage (Hub down on both nodes)
**Type:** E + G (network partition + UUID fragmentation + entrypoint env var mismatch)
**Author:** Claude Code (post-mortem analysis)
**YouTrack:** MESHSAT-506

---

## Summary

A flapping S2S IPsec tunnel between NL and GR caused repeated Galera cluster transitions. When the tunnel dropped during an active SST (State Snapshot Transfer), NL MariaDB crashed from donor state, losing its primary view history. Both nodes entered NON_PRIM and could not self-recover.

**Critical finding:** The Incident 11 auto-bootstrap fix (Layer 9, MESHSAT-504) **never actually worked in production.** `WSREP_NODE_ADDRESS` was only passed as a `--wsrep-node-address` command arg but the entrypoint read it from the container's environment, where it was never set. Every single restart since Incident 11 printed `"Could not determine remote node address — skipping auto-bootstrap"` and fell through to normal start.

Additionally, the auto-bootstrap had no handling for the scenario where both nodes are reachable but both are NON_PRIM — it only covered the "remote unreachable" case.

---

## Timeline

| Time (UTC) | Event |
|------------|-------|
| 23:05:11 (Apr 8) | First tunnel flap. GR goes NON_PRIM. NL + garbd maintain PRIMARY (2/3 quorum). |
| 23:05:14 | GR MariaDB crashes, restarts. Entrypoint: `"Could not determine remote node address — skipping auto-bootstrap"`. Crash loop begins. |
| 23:06–23:17 | GR crash-loops 7+ times (every ~30s). Each time the entrypoint logs the same "Could not determine" message. |
| 23:18:32 | Tunnel recovers. GR rejoins cluster. NL performs SST to GR. GR synced at 23:19:33. |
| 23:49:17 | Second tunnel flap. NL briefly NON_PRIM. `pc.recovery` successfully re-bootstraps in ~1 second. Cluster recovers. |
| 00:19:36 (Apr 9) | Third tunnel flap. GR departs. NL + garbd remain PRIMARY (cluster_size=2). |
| 00:20:01 | GR Hub exhausts WSREP 1047 retries (5 attempts), shuts down. |
| 00:33:54 | Tunnel recovers. GR rejoins. NL starts **donor SST** to GR. |
| **00:34:03** | **Tunnel drops DURING SST.** NL goes NON_PRIM while in donor state. `rsync_sst` fails. NL MariaDB crashes. |
| 00:34:03 | NL Hub receives graceful shutdown (SIGTERM from crashing MariaDB socket loss). Hub exits cleanly. |
| 00:34:19 | NL MariaDB restarts. Entrypoint: `"gvwstate.dat missing"` → `"Could not determine remote node address — skipping auto-bootstrap"` → normal start. |
| 00:34:22 | NL joins cluster as **joiner** (role reversed — was donor, now joiner). Gets SST from GR. SST wipes NL's grastate to `00000000:-1`. |
| 00:36–02:02 | Tunnel keeps flapping. Each flap generates new WSREP UUIDs. After several cycles, 50+ stale UUIDs accumulate in EVS partition list. Install timer expirations on every view change attempt. |
| 02:02:32 | Both MariaDB nodes stuck in NON_PRIM. garbd also NON_PRIM. `"no nodes coming from prim view, prim not possible"` on all voters. |
| 02:05:31 | Manual recovery: flag-file bootstrap on NL. NL becomes PRIMARY. |
| 02:07:59 | GR joins via SST. Synced. |
| 02:08:15 | garbd started. cluster_size=3. Hub started on both nodes. Full recovery. |

---

## Root Cause Analysis

### Trigger

Repeated S2S IPsec tunnel flapping between NL (192.168.192.10) and GR (192.168.15.10). Same tunnel class that caused Incidents 10 and 11. The flapping pattern (~23:05, ~23:49, ~00:19, ~00:33) suggests periodic SA rekeying failures or keepalive timeouts.

### Bug 1: `WSREP_NODE_ADDRESS` not available in container environment (Type G — new)

The Incident 11 auto-bootstrap entrypoint reads the local node address:

```bash
LOCAL_ADDR="${WSREP_NODE_ADDRESS:-}"
```

But `WSREP_NODE_ADDRESS` was only passed as a **command argument** in the compose file:

```yaml
command:
  - --wsrep-node-address=${WSREP_NODE_ADDRESS}
```

Docker Compose interpolates `${WSREP_NODE_ADDRESS}` from `.env` into the command string at container creation time, but it was **never listed in the `environment:` block**. Inside the container:

```
$ printenv | grep WSREP
(empty)
```

With `LOCAL_ADDR=""`, the address matching loop:
```bash
if [ -n "$LOCAL_ADDR" ] && [ "$host" != "$LOCAL_ADDR" ]; then
```
...always evaluated to false (`-n ""` fails). `REMOTE_ADDR` was never set, and auto-bootstrap was always skipped.

**This means the Incident 11 auto-bootstrap fix was dead code in production for 2 days.** Every restart since Incident 11 logged `"Could not determine remote node address — skipping auto-bootstrap"` but nobody noticed because the cluster recovered via other mechanisms (`pc.recovery`, manual bootstrap).

### Bug 2: No "both NON_PRIM" handling

The auto-bootstrap only had one path:

```
if garbd_reachable AND remote_unreachable:
    wait 60s → auto-bootstrap
```

When the tunnel recovered, the remote was reachable. The entrypoint saw `remote=REACHABLE` and fell through to normal start, expecting `pc.recovery` to form a primary. But with `gvwstate.dat` missing and no primary view history on either side, `pc.recovery` failed with `"no nodes coming from prim view, prim not possible"`.

### The fatal sequence: SST during tunnel drop

The specific escalation path was:

1. Tunnel recovers at 00:33 → GR rejoins → NL becomes **donor** for SST
2. Tunnel drops at 00:34:03 → NL goes NON_PRIM **while in donor state**
3. Donor state is special: MariaDB has flushed tables and is streaming data. Going NON_PRIM during SST is catastrophic — gvwstate.dat is not written
4. On restart, NL has no primary view history. GR also has no primary view history (was in the middle of receiving SST)
5. Both nodes are reachable (tunnel is back) but neither can form a primary → deadlock

---

## Contributing Factors

| Factor | Impact | Severity |
|--------|--------|----------|
| **`WSREP_NODE_ADDRESS` not in container env** | Auto-bootstrap (Layer 9) was dead code since Incident 11 | Critical |
| **No "both NON_PRIM" recovery path** | Tunnel recovery creates deadlock instead of partition | Critical |
| **SST during tunnel flap** | Donor crash wipes primary view history on both sides | High |
| **garbd co-located with NL** | Cannot tiebreak site-level partitions (same as Incident 11) | High |
| **50+ stale UUIDs despite PT5M timeout** | Rapid restarts generate UUIDs faster than 5min can expire them | Medium |
| **No alerting on cluster health** | 2+ hour outage before detection | Medium |

---

## Preventive Actions

### Completed (2026-04-09, MESHSAT-506)

**1. Entrypoint: parse `--wsrep-node-address` from command args.**
The entrypoint now parses both `--wsrep-node-address=` and `--wsrep-cluster-address=` from command args in a single loop. Falls back to `${WSREP_NODE_ADDRESS}` env var if arg parsing fails. This fixes Bug 1 — `LOCAL_ADDR` is now correctly populated on every startup.

**2. Entrypoint: Path B — retry counter for "both NON_PRIM" deadlock.**
When `gvwstate.dat` is missing, the remote IS reachable, and garbd is locally reachable, the entrypoint tracks join failures via `/var/lib/mysql/auto-bootstrap-retries`. After 3 consecutive failed normal joins (configurable via `GALERA_MAX_JOIN_RETRIES`), auto-bootstraps with `--wsrep-new-cluster`.

Split-brain safety: only the garbd node (NL) can trigger Path B. GR has no local garbd → `GARBD_REACHABLE=false` → skips bootstrap → keeps retrying normal join until NL becomes PRIMARY.

**3. Compose: `WSREP_NODE_ADDRESS` added to `environment:` block.**
Belt-and-suspenders — even if arg parsing fails, the env var is now available.

**4. Deployed to both nodes.**
Entrypoint and compose files deployed to NL and GR via SCP. Entrypoint is bind-mounted read-only, so the fix is live on next MariaDB restart without container recreate.

### Still Open

**5. Move garbd to a 3rd site.** (P0, open since Incident 11)
garbd on NL is useless for NL↔GR partitions. Until garbd is on a 3rd network, this class of incident can recur — the auto-bootstrap fix reduces recovery time from hours to ~2 minutes, but prevention requires a true tiebreaker.

**6. S2S IPsec tunnel monitoring/alerting.** (P1, open since Incident 11)
Three incidents (10, 11, 13) caused by the same tunnel. No automated alerting.

**7. Cluster health alerting.** (P1)
No monitoring triggered during the 2-hour outage. Need a probe that checks `wsrep_cluster_status` and alerts when NON_PRIM persists > 5 minutes.

---

## Remediation Impact Assessment

If the same tunnel flap pattern recurs with the fixes deployed:

| Phase | Before (Incident 13) | After (fixes deployed) |
|-------|----------------------|----------------------|
| **GR crash loop after tunnel drop** | Entrypoint: "Could not determine remote" → normal start → crash (30s/cycle) | Entrypoint: correctly detects remote, checks garbd. GR has no garbd → normal start → crash (same speed, but expected) |
| **NL crash after donor SST failure** | Entrypoint: same broken detection → normal start → crash loop | Entrypoint: detects remote, garbd reachable. **Path A fires** if remote unreachable, **Path B fires** after 3 retries if remote reachable. Auto-bootstraps in ~90s. |
| **Both NON_PRIM, tunnel back** | Deadlock. Both crash-loop forever. Manual bootstrap required. | NL: Path B fires after 3 failed joins (~90s). Auto-bootstraps. GR: joins NL on next restart (~30s more). **Total recovery: ~2 minutes.** |
| **Total outage duration** | ~110 minutes (until human intervenes) | **~2 minutes (fully automated)** |

### Remaining gaps

1. **Full NL site failure** — garbd is co-located, no tiebreaker. Requires 3rd-site garbd.
2. **SST-during-flap data churn** — each flap triggers a full SST. With high flap frequency, this generates disk I/O pressure. Not fixable at the Galera level — requires tunnel stability.
3. **Hub on GR stays down during NL bootstrap** — Hub exits on WSREP 1047 after 5 retries. The retry window (1s+2s+4s+8s+16s = ~31s) may not be long enough for the ~90s bootstrap. Consider increasing `GALERA_MAX_JOIN_RETRIES` for Hub's WSREP retry or adding a reconnect loop.

---

## Verification

Post-recovery cluster state (2026-04-09 02:08 UTC):

| Check | NL | GR |
|-------|----|----|
| MariaDB | Synced, healthy | Synced, healthy |
| cluster_size | 3 | 3 |
| wsrep_ready | ON | ON |
| garbd | Running | -- |
| Hub | healthy | healthy |
| readyz | ok (mariadb, mqtt, redis, reticulum) | ok (mariadb, mqtt, redis, reticulum) |
| NATS | healthy (11h) | healthy (11h) |

Post-fix deployment (2026-04-09 02:33 UTC, commit cb1d621, pipeline 18372):

| Check | Status |
|-------|--------|
| Pipeline lint | pass |
| Pipeline security (gosec + govulncheck) | pass |
| Pipeline test | pass |
| Pipeline build + package | pass |
| Pipeline pre-deploy (Galera health gate) | pass |
| Pipeline deploy | pass |
| Pipeline verify | pass |
| Entrypoint deployed (NL) | v3 (Path A + Path B + arg parsing) |
| Entrypoint deployed (GR) | v3 (Path A + Path B + arg parsing) |
| Compose env WSREP_NODE_ADDRESS (NL) | set |
| Compose env WSREP_NODE_ADDRESS (GR) | set |

---

## Lessons Learned

1. **Test the actual recovery path, not just the code.** The Incident 11 auto-bootstrap was correct logic that referenced an env var that didn't exist in the container. A single `docker exec meshsat-mariadb printenv | grep WSREP` would have caught it.

2. **Every entrypoint should log its resolved variables.** The fix adds `log "Address detection: local=... remote=..."` so the next failure will immediately show whether address parsing worked.

3. **"Reachable" does not mean "healthy."** TCP connection to port 4567 succeeding means the process is listening, not that it's in PRIMARY state. The retry counter is a pragmatic workaround — the proper fix would be a Galera-aware health probe.

4. **SST during network instability is a force multiplier.** When the tunnel flaps, SST starts and fails repeatedly. Each failed SST can corrupt the donor's state. Consider `wsrep_sst_donor_rejects_queries=OFF` or switching from rsync to mariabackup SST (non-blocking).
