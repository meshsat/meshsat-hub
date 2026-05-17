# Requirements — SOS Trigger Logic (meshsat-hub v1.1, MESHSAT-170)

Source: `docs/ROADMAP.md` L99 ("SOS is the #1 safety feature, currently never fires"), `docs/EXECUTION_PLAN.md` §"Task 3: SOS trigger logic (MESHSAT-170)".

Constitution invariants in scope: Article II (security #1), Article III (zero trust on inputs), Article VII (SOS bypasses rate-limit), Article VIII (audit log SHA-256 hash chain), Article IX (webhook HMAC, applies to inbound MO source via rockblock), Article XIV (Prometheus + audit + secret-redaction).

## Functional requirements

REQ-001: The system shall subscribe to the `meshsat/+/mo/decoded` MQTT topic on startup.

REQ-002: When a MO message text contains the literal token "SOS", the SOS detector shall flag it as an emergency event.

REQ-003: When a MO message text contains the literal token "MAYDAY", the SOS detector shall flag it as an emergency event.

REQ-004: When a MO message text contains the literal token "EMERGENCY", the SOS detector shall flag it as an emergency event.

REQ-005: When a MO message arrives from a device whose registry entry has `sos_flagged=true`, the SOS detector shall flag it as an emergency event.

REQ-006: When a MO payload parses as JSON and contains `"sos": true`, the SOS detector shall flag it as an emergency event.

REQ-007: When the SOS detector flags an emergency event, the system shall publish a SOS event to the `meshsat/{device_id}/sos` MQTT topic.

REQ-008: When the SOS detector flags an emergency event, the system shall invoke `escalation.Engine.Trigger(deviceID, sosEvent)`.

REQ-009: The system shall bypass per-device rate limits for SOS events.

REQ-010: When an SOS event is detected, the system shall write an audit log entry of type "sos.detected" containing the source MQTT topic.

REQ-011: The SOS event payload published to MQTT shall conform to the `sos-event.json` schema.

REQ-012: While the SOS detector is starting, the system shall block new MQTT publishes on SOS topics.

REQ-013: If the escalation engine returns an error, then the system shall retry SOS escalation up to 3 times with exponential backoff starting at 1 second.

REQ-014: When an SOS event is detected for a device with no per-device escalation chain, the system shall fall back to the global default escalation chain.

REQ-015: When the global default escalation chain is also unconfigured, the system shall log a `sos.no-chain` warning and shall write an audit entry of type `sos.no-chain`.

REQ-016: When the SOS detector receives a payload that fails UTF-8 decoding or fails JSON parse, the system shall NOT flag it and shall write an audit entry of type `sos.malformed`.

REQ-017: When an SOS event publishes successfully, the system shall increment the `hub_sos_events_total` Prometheus counter labelled by `device_id`.

REQ-018: The system shall expose `GET /api/sos/recent?limit=N` returning the last N SOS events.

REQ-019: When a tenant operator queries `/api/sos/recent`, the system shall filter results by the operator's `tenant_id` JWT claim.

REQ-020: When an SOS detection rule is changed via `POST /api/sos/rules`, the system shall validate the request body against the `sos-detection-rule.json` schema before applying.
