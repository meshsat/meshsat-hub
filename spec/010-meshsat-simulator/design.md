# Design — MeshSat Simulator

## Goal

Per `docs/ROADMAP.md` L149: "Can't develop or demo without hardware." The simulator removes that blocker: contributors run `make dev`, see synthetic MO traffic flow through the full pipeline, and can validate every downstream handler (audit, MQTT publish, fragment reassembly, decryption, dead-man, SOS detector, routing engine) without ever touching a satellite modem.

## Wire diagram

```
        ┌──────────────────────────┐
        │  POST /api/simulator/   │
        │  start {                 │
        │    device_imei,          │
        │    message_rate_per_min, │
        │    payload_pattern       │
        │  }                       │
        └────────────┬─────────────┘
                     │  (REQ-900)
                     ▼
        ┌──────────────────────────┐
        │  internal/simulator/     │
        │  ─ goroutine per device  │
        │  ─ ticker @ rate/60s     │
        │  ─ payload generator     │
        │  ─ POST to same          │ ← REQ-908
        │    /api/webhook/rockblock│
        │    endpoint as real wh   │
        └────────────┬─────────────┘
                     │
                     ▼
        ┌──────────────────────────┐
        │ EXISTING rockblock       │
        │ handler — full pipeline  │
        │ (HMAC verify, decode,    │
        │  fragment reassembly,    │
        │  decryption, audit,      │
        │  MQTT publish, dedup,    │
        │  dead-man check-in)      │
        └──────────────────────────┘
```

## Why route through the real webhook (REQ-908)

Two options for delivery:
- (a) Simulator directly enqueues into `bus.MessageBus` — bypasses HMAC, fragment, decryption layers.
- (b) Simulator POSTs to `/api/webhook/rockblock` with the configured HMAC secret — exercises the full pipeline.

Option (b) is strictly better for development confidence. Operators get genuine end-to-end traffic; contributor onboarding includes "see your audit-log SHA-256 hash chain advance with every simulated message". HMAC overhead is microseconds; not a concern.

## Mode gating (REQ-910 + REQ-913)

Default-off in cluster mode for two reasons:
1. Cluster mode is production-shaped; synthetic traffic would pollute real audit logs + MQTT topics consumers downstream are sharing.
2. Operator-explicit opt-in via `HUB_SIMULATOR_ENABLED=true` keeps the surface intentional.

In standalone mode (the dev mode), it's auto-on so `make dev` works.

## Payload patterns (REQ-904..907)

| Pattern | Generator | Use case |
|---|---|---|
| `text` | Markov-ish sentence picker from a corpus of operator-style phrases | Smoke-test compression, decryption, audit log |
| `position` | Random walk from configurable lat/lon start | Validate map view + position subscriber + dead-man |
| `telemetry` | Synthetic battery/voltage/temp values | Validate telemetry handlers |
| `sos` | Literal "SOS" text once per interval | Validate SOS detector (spec/001) end-to-end |
| `binary_random` | Random bytes (sized to fit MO 340-byte limit) | Smoke-test fragment reassembly (when message exceeds limit) |

## Out of scope

- No GUI for the simulator — REST API only.
- No replay of recorded real-traffic files — deferred.
- No multi-tenant simulation (cross-tenant load testing) — single-tenant-scoped per call.
