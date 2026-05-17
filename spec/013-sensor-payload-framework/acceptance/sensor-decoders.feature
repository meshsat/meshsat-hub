Feature: Sensor Payload Framework (meshsat-hub v2.0, MESHSAT-185)

  Parent epic: MESHSAT-185. Pluggable decoders per docs/ROADMAP.md L152.

  Background:
    Given the Hub is running with the routing.Engine started
    And the operator has authenticated as tenant-A owner

  # REQ-1201 + REQ-1216 — built-in decoders load at startup
  Scenario: Built-in decoders are loaded
    When the Hub starts
    Then the counter hub_sensor_decoders_loaded_total has values for zigbee_cluster, lora_cayenne_lpp, protobuf, json_passthrough each >= 1

  # REQ-1202 + REQ-1206 — list scoped to tenant
  Scenario: GET /api/sensor-decoders is tenant-scoped
    Given 2 decoders exist for tenant-A and 1 decoder exists for tenant-B
    When the operator (tenant-A) calls GET /api/sensor-decoders
    Then the response body has exactly 2 entries
    And every entry has tenant_id "tenant-A"

  # REQ-1203 + REQ-1217 — create with audit
  Scenario: POST /api/sensor-decoders registers a decoder
    When the operator POSTs {"name":"temp-sensor","kind":"zigbee_cluster","config":{"enabled_clusters":["0x0402"]}}
    Then the response status is 201
    And the response body contains a decoder_id assigned by the server
    And an audit_log entry of type "sensor_decoder.created" exists

  # REQ-1209 + REQ-1210 — decoder enriches dispatched messages
  Scenario: Zigbee decoder enriches messages routed to destinations
    Given a sensor decoder exists: kind=zigbee_cluster, name="my-temp"
    And a route exists: source=mo_decoded, destination=webhook, enabled=true
    When an MO arrives with a ZCL temperature frame from a device tagged class="zigbee_cluster"
    Then the webhook destination handler receives a message with a decoded subobject containing temperature_celsius

  # REQ-1211 — decoder failure logs WARN and passes original through
  Scenario: Malformed input to decoder logs WARN and passes original
    Given a sensor decoder exists: kind=zigbee_cluster
    When an MO arrives with bytes that fail ZCL parsing
    Then a log entry at WARN level mentions the decode failure
    And the counter hub_sensor_decode_error_total{kind="zigbee_cluster"} is incremented by 1
    And the webhook destination handler receives the ORIGINAL message without a decoded subobject

  # REQ-1212 — ZCL clusters
  Scenario: Zigbee decoder handles Basic + Temperature + Humidity + Battery clusters
    Given decoder enabled_clusters=["0x0000","0x0402","0x0405","0x0001"]
    When MO frames arrive for each cluster
    Then the decoded subobject contains the corresponding fields for each cluster

  # REQ-1215 — json_passthrough emits decoded as-is
  Scenario: json_passthrough decoder emits the JSON unchanged
    Given a decoder exists: kind=json_passthrough
    When an MO arrives with text body `{"a":1,"b":2}` from a device tagged class="json_passthrough"
    Then the dispatched message has decoded={"a":1,"b":2}

  # REQ-1205 + REQ-1217 — delete with audit
  Scenario: DELETE removes decoder and writes audit
    Given a decoder exists with decoder_id="d-1"
    When DELETE /api/sensor-decoders/d-1 is called
    Then the response status is 204
    And an audit_log entry of type "sensor_decoder.deleted" exists

  # REQ-1206 — cross-tenant forbidden
  Scenario: Cross-tenant decoder access returns 403
    Given a decoder exists with decoder_id="d-X" and tenant_id="tenant-B"
    And the operator authenticates as tenant-A
    When PUT /api/sensor-decoders/d-X is called
    Then the response status is 403

  # REQ-1218 — wasm kind rejected in v2.0
  Scenario: POST /api/sensor-decoders with kind=wasm returns 400
    When the operator POSTs {"name":"custom","kind":"wasm","config":{}}
    Then the response status is 400
    And the response body field "error" mentions wasm is deferred
