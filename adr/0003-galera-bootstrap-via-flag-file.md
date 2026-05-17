# ADR-0003 — Galera bootstrap via flag-file, NEVER via WSREP_CLUSTER_ADDRESS edit

## Status

Accepted — 2026-05-17 (codifies the post-Incident-11/13/14 rule)

## Context

Galera (MariaDB multi-master) bootstrapping is a well-known operator footgun. The "right" way per MariaDB docs: bring the first node up with `gcomm://` (empty cluster address), then add others. The "wrong" way: leave `gcomm://` in the persistent config after bootstrap — every subsequent restart re-bootstraps a NEW cluster, causing split-brain.

Five split-brain incidents in 2 days (March 19-20, 2026) + three more (Incidents 13/14/15 on April 9) all traced back to operators editing `.env` to `WSREP_CLUSTER_ADDRESS=gcomm://` during recovery attempts.

## Decision

**Bootstrap via flag-file mechanism (`/galera-bootstrap.flag`), NEVER via `WSREP_CLUSTER_ADDRESS` edit.**

The mechanism (`scripts/galera-entrypoint.sh`):
1. Persistent config keeps `WSREP_CLUSTER_ADDRESS=gcomm://node-nl,node-gr,node-arb` (multi-node)
2. To bootstrap, operator creates `/var/lib/mysql/galera-bootstrap.flag` BEFORE starting the first node
3. Entrypoint detects the flag, uses `--wsrep-new-cluster` flag (one-shot), DELETES the flag, starts mysqld
4. Subsequent nodes join the cluster normally (no flag needed)
5. If the entrypoint sees both the flag AND `wsrep_cluster_size > 0`, it ABORTS (defensive)

Plus 14 additional safety layers in `galera-entrypoint.sh`:
- gcomm:// in persistent config → block
- `WSREP_CLUSTER_ADDRESS` env-override that bypasses persistent → block
- bootstrap flag with no peers reachable → wait 30s + retry
- bootstrap flag with peers reachable + already in cluster → abort
- ... (full list in CLAUDE.md L634-651)

## Consequences

**Positive:**
- 8 split-brain incidents → 0 since 2026-04-09 (when entrypoint hardening completed).
- Recovery from clean shutdown: no operator action needed.
- Recovery from dirty crash: explicit operator decision required (create flag), so the "OK to restart cluster" judgment is human-mediated.

**Negative:**
- Operator must remember the flag-file mechanism (documented in `docs/RUNBOOK.md` + CLAUDE.md).
- Recovery from "lost all 3 nodes simultaneously" still requires operator judgment about which node has the most-recent data — that's an irreducible operator decision Galera can't automate.

## Operational corollaries (also Constitution Article XII)

- NEVER `docker compose up -d` — that recreates MariaDB. Use `docker compose up -d --no-deps <service>`.
- NEVER `docker compose pull` — it evaluates ALL services including MariaDB.
- NEVER deploy if `scripts/check-galera-health.sh` fails. CI's pre-deploy stage enforces.
- `galera-watchdog.sh` cron is currently DISABLED because it had mutated `.env` to bare `gcomm://` in a previous version.

## Alternatives considered

- **Use cockroachdb / yugabyte / fauna instead** (rejected — too big a migration; Galera is working since hardening landed)
- **Move to PostgreSQL streaming replication** (rejected — different consistency model; Hub design assumes synchronous multi-master)
- **Single-master Tier 1 only, no cluster** (rejected — production needs HA across NL + GR DCs)

## References

- `docs/INCIDENT_11_POSTMORTEM.md`
- `docs/INCIDENT_13_POSTMORTEM.md`
- `docs/INCIDENT_14_POSTMORTEM.md`
- `scripts/galera-entrypoint.sh` (15 numbered safety layers)
- `scripts/check-galera-health.sh`
- Constitution Articles XI + XII
