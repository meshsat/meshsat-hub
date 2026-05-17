Feature: SOS trigger logic (meshsat-hub v1.1, MESHSAT-170)

  Parent epic: MESHSAT-170. Single feature; no child subtasks in YouTrack.

  Background:
    Given the Hub is running in cluster mode
    And the RockBLOCK MO handler is subscribed to meshsat/+/mo/decoded
    And the SOS detector is started

  # REQ-002 — keyword detection
  Scenario: MO message containing "SOS" triggers detection
    When a MO message arrives on meshsat/imei-001/mo/decoded with text "SOS at 52.16, 4.51"
    Then an SOS event is published to meshsat/imei-001/sos
    And an audit_log entry of type "sos.detected" exists

  # REQ-003 — MAYDAY keyword
  Scenario: MO message containing "MAYDAY" triggers detection
    When a MO message arrives on meshsat/imei-002/mo/decoded with text "MAYDAY MAYDAY MAYDAY"
    Then an SOS event is published to meshsat/imei-002/sos

  # REQ-005 — device-flag detection
  Scenario: MO from sos-flagged device triggers detection regardless of text
    Given a device "imei-123" has sos_flagged=true in the registry
    When a MO message arrives from "imei-123" with text "OK"
    Then an SOS event is published to meshsat/imei-123/sos
    And the published event has detection_reason "device_flag"

  # REQ-006 — payload-field detection
  Scenario: MO with JSON sos:true field triggers detection
    When a MO message arrives with JSON payload {"sos": true, "note": "rescue"}
    Then an SOS event is published
    And the published event has detection_reason "payload_field"

  # REQ-008 + REQ-009 — escalation invocation + rate-limit bypass
  Scenario: SOS triggers escalation chain regardless of rate limit
    Given the device "imei-456" is at its per-device rate limit
    When an SOS is detected for "imei-456"
    Then the escalation.Engine.Trigger function is invoked exactly once
    And the rate limiter does NOT throttle the SOS event

  # REQ-013 — escalation retry
  Scenario: Escalation engine error triggers retry with exponential backoff
    Given the escalation engine returns an error on the first call
    When an SOS is detected
    Then the escalation engine is retried after 1 second
    And the escalation engine is retried after 2 seconds
    And the escalation engine is retried after 4 seconds

  # REQ-014 + REQ-015 — fallback to global default
  Scenario: SOS for device with no escalation chain falls back to default
    Given the device "imei-789" has no per-device escalation chain
    And a global default escalation chain is configured
    When an SOS is detected for "imei-789"
    Then the global default escalation chain is invoked

  Scenario: SOS for device with no chain and no global default logs warning
    Given the device "imei-999" has no per-device escalation chain
    And no global default escalation chain is configured
    When an SOS is detected for "imei-999"
    Then a log entry at WARN level with type "sos.no-chain" is written
    And an audit_log entry of type "sos.no-chain" exists

  # REQ-016 — malformed payload does NOT trigger
  Scenario: Malformed UTF-8 MO payload does NOT flag as SOS
    When a MO message arrives with invalid UTF-8 bytes
    Then NO SOS event is published
    And an audit_log entry of type "sos.malformed" exists

  Scenario: Malformed JSON MO payload does NOT flag as SOS
    When a MO message arrives with text "{not valid json"
    Then NO SOS event is published
    And an audit_log entry of type "sos.malformed" exists

  # REQ-017 — Prometheus counter increments
  Scenario: Successful SOS publish increments hub_sos_events_total
    When an SOS is detected for device "imei-100"
    Then the hub_sos_events_total counter with label device_id="imei-100" is incremented by 1

  # REQ-018 + REQ-019 — recent-events API + tenant filter
  Scenario: GET /api/sos/recent returns tenant-scoped events only
    Given the operator authenticates with a JWT containing tenant_id="tenant-A"
    And 3 SOS events exist for tenant_id="tenant-A"
    And 2 SOS events exist for tenant_id="tenant-B"
    When GET /api/sos/recent is called by the operator
    Then the response status is 200
    And the response body contains exactly 3 events
    And every event in the response has tenant_id "tenant-A"

  # REQ-020 — rule validation on POST
  Scenario: POST /api/sos/rules with invalid body returns 400
    When POST /api/sos/rules is called with body missing the keywords field
    Then the response status is 400
    And the response body field "error" describes the schema violation

  Scenario: POST /api/sos/rules with empty keywords array returns 400
    When POST /api/sos/rules is called with body {"rule_id":"r1","tenant_id":"tA","keywords":[],"enabled":true}
    Then the response status is 400
