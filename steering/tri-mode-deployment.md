# Steering — Tri-mode deployment

Per Article XVI. Selected at startup via `HUB_MODE` env var.

## Modes

| Mode | Store | Bus | Leader | Use case |
|---|---|---|---|---|
| `standalone` | SQLite per-tenant file | Mosquitto | n/a (single-node) | Solo operator, small fleet |
| `cluster` | MariaDB Galera | NATS | NATS leader election | Production NL+GR sites |
| `kubernetes` | StatefulSets + MariaDB Galera Operator | NATS in-cluster | k8s Lease API | Future SaaS scale |

## Code shape

The binary is single — mode-specific behaviour hides behind interfaces:

```go
type Store interface { ... TenantStore methods ... }
type Bus interface { Publish, Subscribe }
type Leader interface { Acquire, Release, IsLeader }
type Deduper interface { Seen(key string) bool }
type RateLimiter interface { Allow(key string) bool }
```

`internal/config/config.go` reads `HUB_MODE` + constructs the appropriate concrete impls.

## Endpoints

`/api/cluster/node` returns the local nodeID. `/api/cluster/status` returns whether this node is leader + the active mode.

## Failure mode

If `HUB_MODE` is unset or unknown, `internal/config/` refuses to start with an explicit error. Don't default — operator must opt-in.
