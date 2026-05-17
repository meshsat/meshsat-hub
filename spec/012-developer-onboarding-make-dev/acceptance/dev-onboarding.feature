Feature: Developer Onboarding `make dev` (meshsat-hub v2.0, MESHSAT-184)

  Parent epic: MESHSAT-184. Zero-friction contributor onboarding per docs/ROADMAP.md L151.

  Background:
    Given a contributor has cloned meshsat-hub
    And docker compose is installed

  # REQ-1100 + REQ-1101 + REQ-1102 — make dev brings up Hub
  Scenario: make dev starts the full dev environment
    When the contributor runs `make dev`
    Then within 60 seconds the Hub responds 200 to GET http://localhost:6070/healthz
    And mosquitto is running on port 1883

  # REQ-1103 + REQ-1110 — auto-seeded simulators produce traffic
  Scenario: 3 simulators auto-start and the dashboard shows traffic
    When the contributor runs `make dev`
    Then 3 simulators are running for imei=000000000000001, 000000000000002, 000000000000003
    And within 30 seconds the GET /api/simulator/active response has 3 entries
    And within 30 seconds at least 1 mo/decoded MQTT message is observed for each simulator

  # REQ-1104 — clear error on missing prereq
  Scenario: make dev without docker compose prints actionable error
    Given docker compose is NOT installed
    When the contributor runs `make dev`
    Then stderr contains the string "docker compose"
    And the exit code is non-zero

  # REQ-1105 + REQ-1106 — dev-down + dev-logs
  Scenario: dev-down stops the environment cleanly
    Given `make dev` ran successfully
    When the contributor runs `make dev-down`
    Then docker compose ps shows no running services
    And the named volume meshsat-dev-data still exists

  # REQ-1109 — pre-seeded dev tenant
  Scenario: Default routes are seeded for dev-tenant
    When the contributor runs `make dev`
    Then GET /api/routes (authenticated as dev-tenant owner) returns at least 8 default routes
    And the routes have name starting with "default:"

  # REQ-1111 + REQ-1112 — state persistence + clean reset
  Scenario: dev-down then dev preserves SQLite state
    Given a simulator was running with imei=000000000000001 and message_count=10
    When the contributor runs `make dev-down && make dev`
    Then SQLite state persists (audit_log entries from the previous run still exist)

  Scenario: dev-clean removes the volume
    When the contributor runs `make dev-clean && make dev`
    Then audit_log is empty (fresh state)

  # REQ-1113 — no external credentials required
  Scenario: Dev environment runs without external service credentials
    Given the contributor has set NO env vars for Twilio, AWS, Cloudloop
    When the contributor runs `make dev`
    Then the Hub starts and is reachable
    And every external-service gateway (sms, email, cloudloop MT) is in no-op or disabled mode

  # REQ-1108 — debug logging
  Scenario: Dev Hub logs at debug level
    Given `make dev` ran successfully
    When the contributor runs `make dev-logs`
    Then the log stream contains at least one entry at level=debug
