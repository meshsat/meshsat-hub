# Requirements — SOS trigger logic (spec/001 — RETROSPECTIVE)

Source: `internal/sos/detector.go` + `detector_test.go` (CGC-verified 2026-05-18). Roadmap reference: MESHSAT-170.

> Retrospective. Functionality shipped in `internal/sos/Detector` type.
> ID convention: 001-block (`001..099`).

## Subscription + dispatch

REQ-001: The system shall subscribe to the `meshsat/+/mo/decoded` MQTT topic on startup via `bus.MessageBus`.
REQ-002: When a MO decoded message arrives on the subscribed topic, the system shall unmarshal it into the `moDecodedMsg` struct (`IMEI`, `Text`, optional `SOS` pointer).
REQ-003: While a MO decoded message text contains the case-insensitive token `SOS`, `MAYDAY`, or `EMERGENCY`, the system shall flag it as an emergency event with `Source="keyword"` and `Keyword=<matched>`.
REQ-004: When a MO decoded message has the explicit `sos: true` field set, the system shall flag it as an emergency event with `Source="field"`.

## Event publication

REQ-005: When an emergency event is detected, the system shall publish an `SOSEvent` JSON struct to `meshsat/{IMEI}/sos`.
REQ-006: The system shall include `IMEI`, `Text`, `Keyword` (when `Source="keyword"`), and `Source` (one of `"keyword"` / `"field"`) in the SOSEvent payload.
REQ-007: The system shall propagate the inbound message's MQTT QoS to the SOS publication.

## Escalation engine integration

REQ-008: When an emergency event is published, the system shall fire the `escalation.Engine` for the device's tenant.
REQ-009: When the `chainID` parameter is non-empty at `NewDetector` construction, the system shall use that escalation chain ID for SOS alerts.
REQ-010: While `chainID` is empty, the system shall use the first available chain from the store as the default chain.
REQ-011: If the `escalation.Engine.Fire` call fails, then the system shall log a `slog.Error` event and continue processing subsequent messages.

## Tenant isolation (Article IV)

REQ-012: The system shall always pass the constructor's `tenantID` to escalation + store operations.
REQ-013: The system shall NEVER process messages without an established tenant context.

## Article VII — Emergency Control Plane fail-OPEN

REQ-014: The system shall NEVER block ESP32 field-device emergency operations.
REQ-015: While the `bus.MessageBus` is unavailable at startup, the system shall log + retry rather than panic.

## Code-level constraints

REQ-016: The system shall define the keyword set as a package-level `sosKeywords []string`.
REQ-017: The system shall match keywords case-insensitively over the uppercased text so embedded variants ("HELP SOS NOW") still trigger.
REQ-018: The system shall expose the `moDecodedMsg` struct as a private type (lowercase initial).
REQ-019: The system shall expose `SOSEvent` as a public type (capitalised initial).

## Tests

REQ-020: The system shall include `detector_test.go` covering: keyword-detection (3 keywords × case-variants), explicit field-detection, no-SOS pass-through, escalation-engine fire, store-lookup for chainID resolution.
