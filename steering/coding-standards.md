# Steering — Coding standards

CGC-grounded against 357 files / 9936 functions / 972 classes / 53+ internal packages.

## Package layout

```
cmd/meshsat-hub/main.go              ← entry point
cmd/meshsat-sim/main.go              ← simulator binary
internal/
  alerting/  api/  apprise/  aprsis/  audit/  auth/  backup/  bridge/
  bus/  cloudloop/  cluster/  codec/  compress/  config/  constellation/
  crypto/  deadman/  dedup/  directory/  email/  escalation/  fragment/
  geo/  globalstar/  hawkbit/  health/  httpjson/  ipougrs/  leader/
  message/  metrics/  middleware/  mptcp/  mqtt/  msvqsc/  ntfy/
  observability/  position/  protocol/  ratelimit/  reticulum/  rock7/
  rockblock/  routing/  scheduler/  sms/  sos/  store/  tak/  timesync/
  tlspin/  tor/  webhook/  wireguard/
web/                                  ← Vue 3 SPA
deploy/                               ← Docker Compose + Helm
scripts/                              ← galera-entrypoint, check-galera-health, etc.
```

## Type naming

- Per-package types use Go idioms (PascalCase).
- HTTP handlers as methods on a Handler struct injected with deps.
- Store interface in `internal/store/store.go`; SQLite impl in `internal/store/sqlite/*.go`; MariaDB impl in `internal/store/mariadb/mariadb.go`.

## Forbidden patterns

- `json.NewDecoder(r.Body).Decode()` without size-limit + `DisallowUnknownFields()` — use `httpjson.ReadJSON` per Article III.
- Webhook handlers that skip HMAC verification (where provider supports HMAC) per Article IX.
- Direct SQL `WHERE` clauses without `tenant_id` predicate per Article IV.
- Modifying `WSREP_CLUSTER_ADDRESS` in `.env` per Article XI.
