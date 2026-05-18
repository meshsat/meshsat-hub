# Steering — Repo conventions

## Build

```bash
cd meshsat-hub
make build
make test
make lint
make security      # gosec + govulncheck
make build-docker  # multi-tier docker images
```

## Branches + commits

Per CubeOS Article XIX. Operator identity:
```
git -c user.name="Kyriakos Papadopoulos" -c user.email="ncpjfuzl@mxmx.email" commit ...
```

## File layout

```
/
  cmd/meshsat-hub/main.go
  cmd/meshsat-sim/main.go
  internal/                ← 53+ packages
  web/                     ← Vue 3 SPA
  deploy/                  ← compose + helm
  scripts/                 ← ops scripts (galera-entrypoint, check-galera-health, ...)
  docs/                    ← ROADMAP.md, EXECUTION_PLAN.md, RUNBOOK.md
  PROJECT.json + PROJECT.md
  constitution.md          ← 17 Articles
  steering/  adr/  spec/
  .agentic/slot-config.entry.json
```

## Release

Per CubeOS Article XV. For parallel-dev: ADR-0003.
