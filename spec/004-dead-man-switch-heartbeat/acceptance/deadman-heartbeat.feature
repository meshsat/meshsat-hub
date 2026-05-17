Feature: Dead Man's Switch Heartbeats (meshsat-hub v1.1, MESHSAT-171)

  Parent epic: MESHSAT-171. P1 silent failure per docs/ROADMAP.md L77.

  Background:
    Given the Hub is running with deadmanMonitor wired into main.go
    And the RockBLOCK handler has the deadman monitor attached
    And the Cloudloop handler has the same deadman monitor attached
    And the position subscriber has the same deadman monitor attached
    And the escalation engine is configured for tenant-A

  # REQ-300 — MO check-in via RockBLOCK
  Scenario: Successful MO via RockBLOCK calls CheckIn
    Given device imei=300258060902280 has deadman enabled with timeout_seconds=3600
    When a MO message for imei=300258060902280 is decoded successfully via the RockBLOCK webhook
    Then deadmanMonitor.CheckIn was called with imei=300258060902280
    And device_deadman_config.last_seen_at for imei=300258060902280 equals approximately now()

  # REQ-301 — MO check-in via Cloudloop
  Scenario: Successful MO via Cloudloop calls CheckIn on the same monitor
    Given device imei=300258060902280 has deadman enabled
    When a MO message for imei=300258060902280 is decoded successfully via the Cloudloop webhook
    Then deadmanMonitor.CheckIn was called with imei=300258060902280

  # REQ-302 — position check-in
  Scenario: Position update calls CheckIn
    Given device imei=300258060902280 has deadman enabled
    When a position update for imei=300258060902280 is received by the position subscriber
    Then deadmanMonitor.CheckIn was called with imei=300258060902280

  # REQ-303 + REQ-304 + REQ-305 + REQ-314 + REQ-316 — fire on timeout
  Scenario: Timeout fires escalation + audit + MQTT + sets triggered_at + increments counter
    Given device imei=300258060902280 has deadman enabled with timeout_seconds=60
    And device_deadman_config.last_seen_at for imei=300258060902280 was 120 seconds ago
    When the dead-man monitor tick runs
    Then escalation.Engine.Trigger was invoked for imei=300258060902280
    And an audit_log entry of type "deadman.triggered" exists for imei=300258060902280 with last_seen_at and timeout_seconds=60
    And an MQTT message was published to meshsat/300258060902280/deadman/triggered conforming to deadman-event.json
    And device_deadman_config.triggered_at for imei=300258060902280 equals approximately now()
    And the counter hub_deadman_triggered_total with label device_imei="300258060902280" is incremented by 1

  # REQ-315 + REQ-317 — recovery
  Scenario: Check-in after trigger clears triggered_at + writes recovery audit + counter
    Given device imei=300258060902280 was previously triggered with triggered_at=120s_ago
    When a successful MO arrives for imei=300258060902280
    Then device_deadman_config.triggered_at for imei=300258060902280 is null
    And an audit_log entry of type "deadman.recovered" exists for imei=300258060902280
    And the counter hub_deadman_recovered_total with label device_imei="300258060902280" is incremented by 1

  # REQ-308 — minimum timeout enforcement
  Scenario: POST with timeout_seconds=30 returns 400
    Given the operator authenticates with JWT containing tenant_id="tenant-A"
    And device imei=300258060902280 belongs to tenant-A
    When POST /api/devices/300258060902280/deadman is called with body {"enabled": true, "timeout_seconds": 30}
    Then the response status is 400
    And the response body field "error" describes the minimum 60-second window

  # REQ-309 — maximum timeout enforcement
  Scenario: POST with timeout_seconds=100000 returns 400
    When POST /api/devices/300258060902280/deadman is called with body {"enabled": true, "timeout_seconds": 100000}
    Then the response status is 400
    And the response body field "error" describes the maximum 86400-second window

  # REQ-310 — cross-tenant access forbidden
  Scenario: Cross-tenant GET returns 403
    Given the operator authenticates with JWT containing tenant_id="tenant-A"
    And device imei=300258060902280 belongs to tenant-B
    When GET /api/devices/300258060902280/deadman is called
    Then the response status is 403

  # REQ-306 — GET returns current state
  Scenario: GET returns enabled + timeout + last_seen + triggered_at
    Given device imei=300258060902280 belongs to tenant-A
    And device_deadman_config for imei=300258060902280 has enabled=true timeout_seconds=3600 last_seen_at=1733000000 triggered_at=null
    When the operator GETs /api/devices/300258060902280/deadman
    Then the response status is 200
    And the response body has enabled=true, timeout_seconds=3600, last_seen_at=1733000000, triggered_at=null

  # REQ-318 — bounded detection latency
  Scenario: Monitor tick interval is at most 30 seconds
    Given the dead-man monitor is running
    When the elapsed time between consecutive tick invocations is measured over a 5-minute window
    Then no two consecutive ticks are more than 30 seconds apart
