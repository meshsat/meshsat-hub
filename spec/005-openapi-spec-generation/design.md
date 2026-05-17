# Design — OpenAPI Spec Generation

## Goal

Close the v1.1 P3 dev-experience gap in `docs/ROADMAP.md` L78. The Hub already has Swagger-style annotations on ~211 of 236 handlers; what's missing is the build target to compile them, the runtime endpoint to serve them, and the CI gate to prevent annotation drift.

## Wire diagram

```
                       ┌──────────────────────────────────┐
                       │   developer workflow             │
                       │                                  │
                       │   add new handler with @Router   │
                       │   make swagger  ← REQ-400        │
                       │     swag init -g main.go         │
                       │     → docs/swagger.json          │
                       │     → docs/swagger.yaml          │
                       │   git add docs/swagger.json      │
                       │   git commit                     │
                       └─────────────┬────────────────────┘
                                     │
                                     ▼
                       ┌──────────────────────────────────┐
                       │   CI lint stage                  │
                       │                                  │
                       │   make swagger-check ← REQ-406   │
                       │     diff committed vs            │
                       │     freshly-regenerated          │
                       │   FAIL → annotation drift  ← REQ-407
                       │   message instructs operator     │ ← REQ-408
                       └─────────────┬────────────────────┘
                                     │
                                     ▼
                       ┌──────────────────────────────────┐
                       │   runtime                        │
                       │                                  │
                       │   //go:embed docs/swagger.json  ← REQ-404
                       │   var swaggerJSON []byte         │
                       │                                  │
                       │   GET /api/docs/swagger.json     │ ← REQ-402
                       │   GET /api/docs/swagger.yaml     │ ← REQ-403
                       │   GET /api/docs/   (Swagger UI)  │ ← REQ-412
                       └──────────────────────────────────┘
```

## Tools-mode dependency pinning (REQ-401)

`tools/tools.go` is the Go-idiomatic way to pin build-time tool versions:

```go
//go:build tools
// +build tools

package tools

import (
    _ "github.com/swaggo/swag/cmd/swag"
)
```

`go install github.com/swaggo/swag/cmd/swag` then resolves the version from `go.sum`. Reproducible builds.

## Versioning + server URL (REQ-414 + REQ-415)

The swagger.json `info.version` and `servers` arrays are static once generated. Two hooks for runtime override:

- **Build-time version injection:** `go build -ldflags "-X main.Version=$(git describe --tags)"` populates `var Version string` in `main.go`. The `/api/docs/swagger.json` handler patches `info.version` into the JSON response.
- **Runtime server URL:** if `HUB_PUBLIC_BASE_URL` env var is set, the handler patches `servers[0].url` into the JSON response. Useful for dev/staging environments.

Both patches happen on every request; the canonical `docs/swagger.json` in git stays static.

## Why mount Swagger UI ourselves (REQ-412 + REQ-413)

External Swagger UI services (e.g. SwaggerHub-hosted UI loading our JSON) leak request-pattern data to third parties. Hosting Swagger UI ourselves keeps API surface entirely private to Hub.

The minimal HTML page is ~80 lines, references `swagger-ui-dist@5.x` from `unpkg.com` for the JS/CSS. REQ-413 enforces no other external loads (no Google Analytics, no Sentry, no telemetry).

Future hardening: vendor the Swagger UI assets locally to remove the unpkg dependency.

## What's NOT in scope

- No "try it out" credentials handling (the unmodified Swagger UI's "Authorize" button works for tests via the existing JWT bearer scheme; nothing to wire).
- No spec-driven client codegen — that's a downstream consumer choice.
- No alternative formats (HAR, RAML, Postman) — OpenAPI 3.0.3 only.
- No automatic ChangeLog from spec diffs — manual `docs/ROADMAP.md` continues to be the source of truth.

## Cross-spec interaction

Specs 001/002/003/004 each add new endpoints with Swagger annotations:
- `spec/001-sos-trigger/` adds `GET /api/sos/recent`, `GET/POST /api/sos/rules`
- `spec/002-mo-fragment-reassembly/` adds `GET /api/fragments/inflight`
- `spec/003-e2e-encryption-wire/` adds `POST/GET/DELETE /api/devices/{imei}/keys[/{key_id}]`
- `spec/004-dead-man-switch-heartbeat/` adds `GET/POST /api/devices/{imei}/deadman`

After each of those features lands, the operator runs `make swagger` once + commits. The CI gate (REQ-407) catches anyone who forgets.
