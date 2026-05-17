# Steering — Coding standards (MeshSat Hub)

## Go

- Go version: **1.25.0** (`go.mod:3`). Bump only via ADR.
- `CGO_ENABLED=0` everywhere (Article V). Use pure-Go alternatives (`modernc.org/sqlite`, `go-sql-driver/mysql` is pure-Go).
- Format: `gofmt -w .` (Makefile `make fmt`). CI fails on drift.
- Linter: `make lint` (golangci-lint).
- Module structure: `cmd/<binary>/` for entries (only `meshsat-hub` + `meshsat-sim`), `internal/<subsystem>/` for everything else, `deploy/` for cluster/ansible/k8s.
- Test files colocated. Integration tests use `-tags=integration` build tag.
- Error wrapping: `fmt.Errorf("context: %w", err)`. NEVER `panic()` in handler code.
- Logging: structured `log/slog` (JSON in prod). Include `request_id` (xid correlation), `tenant_id`, `user_id`. **NEVER log secrets, JWT tokens, password hashes, API keys, mTLS private keys.**
- Concurrency: prefer channels for orchestration. Mutex for state-machine protection. `context.Context` first-arg on every blocking call.

## Vue 3 SPA (`web/`)

- Vue 3 Composition API ONLY (Article XIV). No Options API.
- Tailwind CSS ONLY (Article XIV). No other CSS framework.
- Pinia for state. Composition over inheritance.
- 35 views in `web/src/views/`. New views go there + extend the router.
- Components in `web/src/components/`. Tactical design tokens (dark theme primary).
- Build: `cd web && npm install && npm run build` → output to `cmd/meshsat-hub/web/dist` (embedded via `go:embed`).
- Test: `cd web && npx playwright test` (53 E2E specs).

## Database access

- ALL store access goes through `internal/store/store.go` interface (548 lines, 100+ methods).
- Both `sqlite/sqlite.go` AND `mariadb/mariadb.go` must implement every method.
- Schema changes update BOTH migration slices (Article XIII).
- `tenantID string` is the SECOND parameter on every method (Article IV).
- `internal/store/dbwrap/` provides timing + slow-query instrumentation — wrap your `*sql.DB` once.

## Comments + docs

- Swagger annotations REQUIRED on new handlers (Article XVIII). CI lint validates.
- Subsystem docs in `docs/`. Major incidents get `docs/INCIDENT_NN_POSTMORTEM.md`.
- ADRs for every "would argue in 6mo" decision.
- CLAUDE.md (706 lines, gitignored) is operator-only context — never reference from committed code.
