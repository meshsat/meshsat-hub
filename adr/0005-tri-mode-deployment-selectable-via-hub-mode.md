# ADR-0005 — Tri-mode deployment, selectable via `HUB_MODE`

* Status: Accepted — codified after the fact 2026-05-17. The decision is already shipped (Tier 1+2 production, Tier 3 code-complete-untested per `docs/ROADMAP.md` L29).
* Date: Originally decided c. 2026-03 during the v0.2 Cluster Infrastructure Sprint; recorded as ADR 2026-05-17 during the deep-spec audit.
* Deciders: `ufwtqkgz@meshsat.net`
* Source documents: `docs/ROADMAP.md` L8–L31, `docs/deployment.md`, `docs/RUNBOOK.md`

## Context

MeshSat Hub serves three deployment shapes with mutually incompatible operational characteristics:

1. **Single edge host** (lab, single VPS, one-site deployment, dev work). Minimal hardware, no cluster, no quorum.
2. **Active-active geo-distributed pair** (production, NL+GR DMZ). Synchronous replication, mTLS bridge auth, NATS leaf federation, garbd arbitrator for 3-voter quorum.
3. **Cloud-native / auto-scaling** (future / not yet operated). StatefulSets, Lease-API leader election, Helm chart.

These shapes can't all be served by one configuration. Picking just one would force the wrong tradeoffs everywhere (e.g. a Galera+NATS+Redis stack on a single Pi5 is gratuitous; SQLite alone for the production pair leaves no path to multi-site).

## Decision

A single binary serves all three shapes, with the shape selected at startup via the `HUB_MODE` environment variable. The binary contains the union of code paths; mode-specific behavior is hidden behind interfaces.

| `HUB_MODE` | Store implementation | Message bus | Dedup / rate-limit state | Leader election | Use case |
|---|---|---|---|---|---|
| `standalone` (default) | `internal/store/sqlite/` (`modernc.org/sqlite`) | Mosquitto | In-memory | `internal/leader/Noop` | Dev, lab, single VPS, edge |
| `cluster` | `internal/store/mariadb/` (`go-sql-driver/mysql`, Galera-aware) | NATS (MQTT adapter + leaf nodes) | Redis | `internal/leader/NATS` (queues) | Production HA, multi-site |
| `kubernetes` | Same MariaDB store as cluster | NATS StatefulSet | Redis | `internal/leader/KubeLease` | Cloud-native, auto-scaling |

Mode is read once at startup (`internal/config/`). Mode-aware components select their implementation via the `Store`, `Bus`, `Leader`, and dedup/ratelimit interfaces. Adding a fourth mode would require a new branch through this same pattern, not a new binary.

## Consequences

**Positive**
- One Dockerfile, one binary, one CI pipeline, one set of integration tests. The operator picks the tier by setting a single env var.
- Tier 1 → Tier 2 migration is a single `scripts/migrate-sqlite-to-mariadb.sh` invocation, not a re-platforming.
- Constitution Article XII ("update BOTH sqlite/*.go AND mariadb/mariadb.go for any schema change") is the load-bearing rule that keeps this decision honest — schema drift between tiers breaks the substitutability guarantee.

**Negative**
- The binary is larger than a single-mode build would be (Galera client libs are pulled in even for `standalone`). Acceptable for an operator-installed Docker image.
- Three CI paths to keep green. Tier 3 is currently the weakest link (untested in production per `docs/ROADMAP.md` L29).
- `HUB_MODE` is a foot-gun if mis-set in production (e.g. accidentally running a cluster node in `standalone` mode would fork its data). Mitigated by health probes that surface mode via `/healthz` + `/readyz` + `/api/cluster/node`.

**Forward direction**
- Tier 3 hardening (Helm chart e2e tests on a real k8s cluster) is the next quality bar — currently tracked under v2.0 Developer Experience but logically belongs to "production-ready Tier 3."
- A fourth mode (`HUB_MODE=edge-mesh` — federated peer-to-peer with no central authority) is a possibility for v3.0 and would slot in via the same interface pattern.

## Alternatives considered

- **Three separate binaries** (`meshsat-hub-standalone`, `meshsat-hub-cluster`, `meshsat-hub-k8s`): rejected — triples CI surface, fragments docs, makes Tier 1→2 migration painful.
- **Mode auto-detection from environment** (sniff for MariaDB, NATS, etc.): rejected — silent mis-detection is worse than a missing env var; explicit beats implicit for production posture.
- **Code-only "all clustered, all the time"**: rejected — would push every Tier-1 user into operating Galera + NATS + Redis for a lab/dev install.

## Compliance

- `internal/config/config.go` must validate `HUB_MODE` at startup and refuse to start on unknown values.
- `internal/store/` substitutability must be guaranteed by the schema-parity check in Constitution Article XII (`bond_groups`, `bridges`, etc. exist in both stores).
- `/api/cluster/node` and `/api/cluster/status` (per `docs/RUNBOOK.md` L150–L153) MUST report which mode is active.
