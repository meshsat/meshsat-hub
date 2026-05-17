# Steering — Test strategy (MeshSat Hub)

## Pyramid

- **Unit:** colocated `*_test.go`. 44 test packages, all passing today.
- **Integration:** `test/integration/` (run with `-tags=integration`). Uses embedded `mochi-mqtt` broker.
- **E2E (browser):** `web/e2e/*.spec.js` Playwright (53 specs across 5 files: app, fleet, hemb, alert-rules, coverage).
- **Post-deploy smoke:** `test/e2e/` triggered from CI verify stage against deployed Hub.
- **OWASP:** `test/owasp/` ZAP baseline + active scan, gated by `HUB_TARGET_URL` + `HUB_AUTH_TOKEN`.
- **RNS interop:** `test/rns_compat/` Python tests verify Reticulum wire-format compat with stock RNS.

## CI

```bash
make test               # go test ./... (unit only)
make test-integration   # go test -tags=integration ./...
cd web && npx playwright test   # browser E2E
make owasp              # zap-baseline.py (~10min)
make owasp-full         # zap-full-scan.py (~45min, release only)
```

11-stage pipeline: lint → security → test → build → package → pre-deploy → deploy → verify → owasp → pages → release.

## Pre-deploy gate (Galera)

`scripts/check-galera-health.sh` blocks deploy if:
- `wsrep_cluster_size != 3`
- `wsrep_ready != ON`
- `WSREP_CLUSTER_ADDRESS` is bare `gcomm://` (defensive against Incident 11/13/14 pattern)
- garbd not running on its arbiter node

## Migration testing

**Critical gap today:** `mariadb_test.go` does NOT exist. Only `sqlite_test.go` is tested. Any new schema change MUST verify the MariaDB path manually OR add `mariadb_test.go` coverage. Sub-task in any migration task: "verify MariaDB pass on `make test-integration`".

## Multi-tenant test fixtures

Synthetic fixtures use 3 tenant IDs: `tenant_a`, `tenant_b`, `tenant_default`. Cross-tenant query tests assert ZERO rows leak. Add tests for any new Store method to prove tenant scoping.

## Coverage targets

- Unit: 80% per package for `internal/auth/`, `internal/audit/`, `internal/api/`, `internal/store/`, `internal/crypto/` (security-critical surface).
- Other packages: 60% baseline.
- E2E: every Vue view exercised by at least one Playwright scenario.

## Parallel-dev workers

Workers must run `make lint && make test && gosec ./...` as `acceptance_test`. Per-package tests are insufficient — they miss cross-tenant + cross-package regressions.

## Test ergonomics

- `make dev` brings up mqtt + Hub + simulator (3 virtual devices, 30s interval).
- `test/integration/` integration tests run against the embedded broker, not a real one.
- Playwright runs against a local dev Hub OR a deployed Hub via `BASE_URL` env.

## What's NOT tested today (honest gaps)

- MariaDB schema migration coverage (no `mariadb_test.go`).
- Galera failover under load (covered by manual incident response).
- OWASP active scan in pre-prod (only baseline today; full scan only on release).
- Browser compatibility outside Chromium (Playwright default).

The constitution declares test-first as Article XVII; these gaps are pragma, not policy. New code shall meet the bar.
