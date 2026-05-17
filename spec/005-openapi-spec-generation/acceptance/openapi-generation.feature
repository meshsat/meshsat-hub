Feature: OpenAPI Spec Generation (meshsat-hub v1.1, MESHSAT-173)

  Parent epic: MESHSAT-173. P3 developer-experience improvement per docs/ROADMAP.md L78.

  Background:
    Given the meshsat-hub repo has `make swagger` and `make swagger-check` targets
    And tools/tools.go pins github.com/swaggo/swag/cmd/swag
    And the Hub binary embeds docs/swagger.json at build time

  # REQ-400 — make swagger produces docs/swagger.json
  Scenario: make swagger generates docs/swagger.json + docs/swagger.yaml
    When the developer runs `make swagger` from the repo root
    Then docs/swagger.json exists and parses as JSON
    And docs/swagger.yaml exists and parses as YAML
    And both files declare openapi version "3.0.3"

  # REQ-402 + REQ-404 — runtime endpoint serves embedded JSON
  Scenario: GET /api/docs/swagger.json returns the embedded spec
    Given the Hub is running
    When GET /api/docs/swagger.json is called
    Then the response status is 200
    And the response Content-Type starts with "application/json"
    And the response body parses as OpenAPI 3.0.3 JSON

  # REQ-403 — yaml endpoint
  Scenario: GET /api/docs/swagger.yaml returns the embedded spec as YAML
    Given the Hub is running
    When GET /api/docs/swagger.yaml is called
    Then the response status is 200
    And the response Content-Type starts with "application/yaml"

  # REQ-406 + REQ-407 — CI gate
  Scenario: make swagger-check fails when annotations drift from committed swagger.json
    Given docs/swagger.json was committed at commit X
    And a new handler was added with @Router annotation after commit X
    But `make swagger` was NOT re-run
    When `make swagger-check` runs
    Then the exit code is non-zero
    And the error message includes the substring "annotation drift"

  # REQ-408 — drift error message is actionable
  Scenario: Drift failure message tells the developer how to fix
    Given annotation drift exists
    When `make swagger-check` runs
    Then the error message contains the substring "make swagger && git add docs/swagger.json docs/swagger.yaml"

  # REQ-409 — every exported API handler has annotations
  Scenario: All exported handlers in internal/api/ declare @Summary @Tags @Router
    When the developer scans every file in internal/api/ for exported HTTP handler functions
    Then every handler has a `@Summary` comment
    And every handler has a `@Tags` comment
    And every handler has a `@Router` comment
    And every handler has at least one `@Success` comment

  # REQ-412 + REQ-413 — Swagger UI page
  Scenario: GET /api/docs/ serves Swagger UI HTML
    When GET /api/docs/ is called
    Then the response status is 200
    And the response Content-Type starts with "text/html"
    And the response body references unpkg.com/swagger-ui-dist
    And the response body does NOT reference google-analytics.com
    And the response body does NOT reference sentry.io

  # REQ-414 — version sourced from git describe
  Scenario: info.version reflects the build-time git tag
    Given the Hub was built with `-ldflags "-X main.Version=v1.5.7"`
    When GET /api/docs/swagger.json is called
    Then the response body's info.version field equals "v1.5.7"

  # REQ-415 — runtime server URL override
  Scenario: HUB_PUBLIC_BASE_URL env var overrides server URL in spec
    Given the Hub is started with HUB_PUBLIC_BASE_URL=https://staging.hub.example.com
    When GET /api/docs/swagger.json is called
    Then the response body's servers[0].url equals "https://staging.hub.example.com"

  # REQ-416 — swagger.json committed to git
  Scenario: docs/swagger.json is tracked by git
    When the developer runs `git ls-files docs/swagger.json`
    Then the file is listed (committed, not ignored)
