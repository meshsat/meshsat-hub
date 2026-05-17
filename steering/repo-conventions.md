# Steering — Repo conventions (MeshSat Hub)

## Branch + workflow

- **Default:** push directly to main. No branches, no MRs. Pipeline deploys to nllei01dmz01 + grskg01dmz01 automatically.
- **Parallel-dev exception** (Constitution Article XX, ADR-0003): ONE short-lived `merge/<feature_id>` branch per parallel-dev wave, ONE MR per feature opened by `merge-coordinator.sh`, auto-deleted on merge.
- Snapshot branches: `snapshot/<YYYY-MM-DD>-<purpose>` for rollback.
- Workers' `parallel-dev/<feature>/<task>` branches are intermediates — never pushed to remote.

## Commit messages

- Format: `type(scope): description [MESHSAT-NNN]`
- Type: `feat | fix | refactor | test | docs | chore | perf | build | ci | security`
- ALWAYS reference a YouTrack issue ID. CI rule warns on missing.
- Workers' commits auto-append `Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>`.

## File layout

- `cmd/<binary>/` — entries (`meshsat-hub` + `meshsat-sim`)
- `internal/<subsystem>/` — all production code (organised by bounded context)
- `web/` — Vue 3 SPA, embedded via `go:embed`
- `deploy/{galera,ansible,k8s,helm,haproxy,grafana}/` — deployment artifacts
- `scripts/` — operator-facing scripts (`galera-entrypoint.sh`, `check-galera-health.sh`, `init-secrets.sh`)
- `test/{integration,e2e,owasp,rns_compat}/` — test sources
- `docs/` — runbook, ENCRYPTION, SECURITY_AUDIT, postmortems, OpenAPI swagger

## YouTrack discipline

- All 3 meshsat-family repos share the `MESHSAT` project. Disambiguation:
  - Bridge (`meshsat/`): no `tag:hub` AND no `tag:android` — default
  - Hub (THIS REPO): `tag:hub` REQUIRED
  - Android (`meshsat-android/`): `tag:android`
- Parallel-dev Planner uses these tags to route correctly. Issues without disambiguator default to bridge.
- **Tag every new Hub issue with `tag:hub`** at creation. Mass-retag of historicals tracked separately.

## CI (11 stages)

`lint → security → test → build → package → pre-deploy → deploy → verify → owasp → pages → release`

- **lint:** golangci-lint, gofmt drift, swagger annotation check
- **security:** gosec + govulncheck (HIGH blocks)
- **test:** make test + integration
- **build:** ARM64 + x86_64 binaries
- **package:** Docker image push to GHCR
- **pre-deploy:** Galera health gate (`check-galera-health.sh`)
- **deploy:** Ansible playbook to NL + GR
- **verify:** post-deploy smoke (`test/e2e/`)
- **owasp:** ZAP baseline (full on release)
- **pages:** Hugo docs site
- **release:** GitHub release + GHCR tag

## Pre-commit (developer-side)

```bash
make lint && make test && gosec ./... && govulncheck ./...
make swagger    # regenerate if handlers changed
gofmt -w .
```

## Galera operational rules (Article XI + XII, repeated for emphasis)

- NEVER `WSREP_CLUSTER_ADDRESS=gcomm://` in `.env`
- NEVER `docker compose up -d` (always `--no-deps`)
- NEVER `docker compose pull`
- NEVER deploy if `check-galera-health.sh` fails
- ALWAYS use flag-file bootstrap for first-node election
- Operator override requires explicit sign-off + ADR
