Feature: MeshSat Simulator (meshsat-hub v2.0, MESHSAT-182)

  Parent epic: MESHSAT-182. Developer-experience unblock per docs/ROADMAP.md L149.

  Background:
    Given the Hub is running in standalone mode
    And the operator has authenticated as owner

  # REQ-900 + REQ-908 — start + real-pipeline routing
  Scenario: Start a text simulator and observe full-pipeline traffic
    When POST /api/simulator/start is called with body {"device_imei":"300258060902280","message_rate_per_minute":6,"payload_pattern":"text"}
    Then the response status is 201
    And within 30 seconds the audit_log contains at least 2 entries of type "message_received" for imei=300258060902280
    And within 30 seconds at least 2 mo/decoded MQTT messages are observed for imei=300258060902280

  # REQ-907 — SOS pattern fires the detector
  Scenario: SOS pattern triggers spec/001 SOS detector
    When POST /api/simulator/start is called with body {"device_imei":"300258060902281","message_rate_per_minute":6,"payload_pattern":"sos"}
    Then within 30 seconds the audit_log contains at least 1 entry of type "sos.detected" for imei=300258060902281

  # REQ-901 — duplicate start rejected
  Scenario: Starting a second simulator for the same device returns 409
    Given a simulator is running for imei=300258060902280
    When POST /api/simulator/start is called again for the same imei
    Then the response status is 409

  # REQ-902 — stop
  Scenario: POST /api/simulator/stop terminates the simulator
    Given a simulator is running for imei=300258060902280
    When POST /api/simulator/stop/300258060902280 is called
    Then the response status is 204
    And no new audit_log entries appear for imei=300258060902280 after 5 seconds

  # REQ-903 — list active
  Scenario: GET /api/simulator/active returns the running list
    Given simulators are running for imei=300258060902280 and imei=300258060902281
    When GET /api/simulator/active is called
    Then the response body contains 2 entries
    And each entry has device_imei, started_at, message_count_so_far, message_rate_per_minute

  # REQ-909 — non-owner gated
  Scenario: Operator role cannot start simulator
    Given the operator authenticates with role="operator"
    When POST /api/simulator/start is called
    Then the response status is 403

  # REQ-910 — endpoint absent in cluster mode without opt-in
  Scenario: Simulator endpoints return 404 in cluster mode without HUB_SIMULATOR_ENABLED
    Given the Hub is running in cluster mode with HUB_SIMULATOR_ENABLED unset
    When POST /api/simulator/start is called
    Then the response status is 404

  # REQ-911 — Prometheus counter
  Scenario: Each simulator message increments the counter
    Given a simulator is running for imei=300258060902280 with payload_pattern="text"
    When 10 messages have been generated
    Then the counter hub_simulator_messages_total{device_imei="300258060902280", payload_pattern="text"} is incremented by 10

  # REQ-912 — graceful shutdown stops simulators + audit
  Scenario: Hub shutdown stops simulators and writes audit
    Given 2 simulators are running
    When the Hub receives SIGTERM
    Then within 5 seconds 2 audit_log entries of type "simulator.stopped" exist
    And no simulator goroutines remain
