# Design — SOS Trigger Logic

## Goal

Close the v1.1 P0 silent-failure documented in `docs/ROADMAP.md` L99: SOS detection logic is coded under `internal/escalation/` and `internal/sos/` but `main.go` never wires it up, so SOS messages arriving in production are silently dropped.

## Wire diagram

```
RockBLOCK MO webhook  →  internal/rockblock/handler.go
                                |
                                v  (publish on meshsat/{device_id}/mo/decoded)
                         MQTT broker
                                |
                                v  (REQ-001 subscribe)
                       internal/sos/detector.go
                       ├── keyword match  (REQ-002/003/004)
                       ├── sos_flagged device  (REQ-005)
                       ├── JSON sos:true  (REQ-006)
                       └── malformed → audit (REQ-016)
                                |
                                v  (flag = true)
              ┌─────────────────┼─────────────────────────┐
              v                 v                         v
   publish meshsat/        invoke              write audit
   {dev}/sos (REQ-007)     escalation.Engine    "sos.detected"
   schema-valid (REQ-011)  .Trigger() (REQ-008) (REQ-010)
                          rate-limit bypass    increment
                          (REQ-009)            hub_sos_events_total
                          fall-back default    (REQ-017)
                          (REQ-014/015)
                          retry 3× exp (REQ-013)
```

## Tables touched

- `audit_log` — append-only SHA-256 hash chain (Constitution Article VIII). New event types: `sos.detected`, `sos.malformed`, `sos.no-chain`.
- `sos_events` (new) — per-tenant rolling table for `/api/sos/recent`. Fields: `event_id UUID PK`, `device_id`, `tenant_id`, `detected_at`, `source_topic`, `detection_reason ENUM`, `keyword_matched NULL`.
- `sos_detection_rules` (new) — tenant-scoped rule store. Fields: `rule_id`, `tenant_id`, `keywords JSON`, `enabled BOOL`.

Schema parity (Constitution Article XII): both new tables MUST appear in `internal/store/sqlite/` AND `internal/store/mariadb/mariadb.go` in the same migration step.

## Rate-limit bypass mechanism

The existing per-device rate limiter in `internal/ratelimit/` already supports a bypass-list per call site. The SOS path adds the device to the bypass-list before calling the sender; this satisfies REQ-009 without per-device configuration.

## Why event-driven via MQTT, not direct call

The MO handler in `internal/rockblock/handler.go` publishes to MQTT after decoding. The detector subscribes. This indirection means:
- The detector is testable in isolation (publish a fixture to MQTT, observe detector behavior).
- Future MO sources (Cloudloop direct, Globalstar webhook) reuse the same detector by publishing to the same topic.
- A bug in the detector cannot block the MO acknowledgment path back to the satellite carrier.

## What is NOT in scope

- New escalation transports — uses the existing `escalation.Engine` interface.
- Operator UI for SOS rules — REST API only in v1.1; UI follows in v1.3 routing-engine work.
- SOS over Cloudloop (the Cloudloop webhook path also writes to `meshsat/{device_id}/mo/decoded` already, so the detector picks it up without code changes).
