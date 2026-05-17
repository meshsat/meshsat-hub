# Requirements — OpenAPI Spec Generation (meshsat-hub v1.1, MESHSAT-173)

Source: `docs/ROADMAP.md` L78 ("OpenAPI spec generation (swagger.json): No machine-readable API docs"), `docs/EXECUTION_PLAN.md` §"Task 5: OpenAPI spec generation (MESHSAT-173)" — P3 developer-experience improvement.

Constitution invariants in scope: Article II (security #1), Article III (httpjson.ReadJSON — the swagger.json endpoint itself is read-only, no body to decode), Article XIV (lint/security gating).

The existing API handlers already carry partial Swagger annotations (`@Router`, `@Summary` per `internal/api/bond_groups.go` etc.). Roughly 211 of 236 endpoints are annotated per `docs/PRODUCTION_READINESS_AUDIT_2026-04-04.md` §2.1. The missing pieces are: a build target that compiles annotations to `docs/swagger.json`, the runtime endpoint serving the JSON, the CI gate that fails on annotation drift, and the remaining 25 unannotated handlers.

## Functional requirements

REQ-400: The system shall add a `make swagger` target to the top-level Makefile that runs `swag init -g cmd/meshsat-hub/main.go -o docs/` and produces `docs/swagger.json` and `docs/swagger.yaml`.

REQ-401: The system shall include `github.com/swaggo/swag/cmd/swag` as a Go tools-mode dependency referenced via `tools/tools.go` so `go install` reproduces a known version.

REQ-402: When the Hub starts, the system shall mount `GET /api/docs/swagger.json` returning the embedded `docs/swagger.json` file with `Content-Type: application/json`.

REQ-403: When the Hub starts, the system shall mount `GET /api/docs/swagger.yaml` returning the embedded `docs/swagger.yaml` file with `Content-Type: application/yaml`.

REQ-404: The `docs/swagger.json` content shall be embedded at build time via `go:embed docs/swagger.json` so deployment artifacts are self-contained.

REQ-405: When `make swagger` runs and any annotated handler has changed since the last build, the system shall regenerate `docs/swagger.json` to reflect the new annotations.

REQ-406: The system shall add a `make swagger-check` target that runs `swag init` against a temp directory and exits non-zero if the resulting swagger.json differs from the committed `docs/swagger.json`.

REQ-407: The CI pipeline's `lint` stage shall invoke `make swagger-check` so a commit that adds an endpoint without regenerating the swagger.json fails CI.

REQ-408: When the CI `lint` stage finds annotation drift, the failure message shall instruct the developer to run `make swagger && git add docs/swagger.json docs/swagger.yaml`.

REQ-409: The `make swagger-check` CI gate shall fail when any exported handler in `internal/api/` lacks a complete set of `@Summary`, `@Tags`, `@Router`, and at least one `@Success` annotation.

REQ-410: When a handler accepts a request body, the handler shall declare `@Accept json` and `@Param body body <Type> true` annotations.

REQ-411: When a handler returns a tenant-scoped resource, the handler shall be tagged with `@Security ApiKeyAuth []` so generated docs surface the auth requirement.

REQ-412: The system shall expose `GET /api/docs/` returning a minimal HTML page that loads Swagger UI (CDN-hosted) pointed at `/api/docs/swagger.json`.

REQ-413: The Swagger UI page shall NOT make external network calls beyond the Swagger UI assets — no telemetry, no analytics, no external image loads.

REQ-414: The swagger.json `info.version` field shall be sourced from a Go-side `var Version string` populated at build time via `-ldflags "-X main.Version=$(git describe --tags)"`.

REQ-415: The swagger.json `servers` array shall include `https://hub.meshsat.net` as the default server URL and shall be operator-overridable via the `HUB_PUBLIC_BASE_URL` environment variable rendered into the JSON at startup if non-empty.

REQ-416: The `docs/swagger.json` file shall be committed to git so external consumers can fetch it from the repo without running the build.

REQ-417: The `docs/swagger.json` file shall conform to OpenAPI 3.0.3 (the format `swaggo/swag` generates by default).
