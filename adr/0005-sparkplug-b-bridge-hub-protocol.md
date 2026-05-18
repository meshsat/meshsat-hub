# 5. meshsat-uplink/v1 Sparkplug-B-inspired protocol

Date: 2026-05-18

## Status
Accepted

## Context
Bridge ↔ Hub needs durable + idempotent messaging over MQTT. Sparkplug-B is the canonical pattern: BIRTH (retained, QoS 1) → DATA → DEATH (LWT-backed, QoS 1).

## Decision
Implement `meshsat-uplink/v1` per Article XVII. Topic namespace:
- `meshsat/bridge/{bridge_id}/{birth|death|health|cmd|cmd/response}` for bridge-level
- `meshsat/bridge/{bridge_id}/device/{device_id}/{birth|death}` for device-level

Every message includes `"protocol": "meshsat-uplink/v1"` + RFC-3339 `timestamp`. Bridges set MQTT LWT BEFORE publishing birth. Schemas in `internal/protocol/protocol.go` (Hub) + `meshsat/internal/hubreporter/protocol.go` (Bridge) — kept in lockstep via protocol version field.

## Consequences
**Positive:** durable birth/death semantics. Idempotent reconnect. Sparkplug-B literacy in the wider IoT community.
**Negative:** Two impls to keep in lockstep. Mitigated by Article XVII naming the requirement explicitly.

**Enforced by:** Article XVII + Article X (stale-birth detection).
