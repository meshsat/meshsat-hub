# Design — Wire Dead Man's Switch Heartbeats

## Goal

Close the v1.1 P1 silent-failure in `docs/ROADMAP.md` L77. The `deadman.Monitor` already exists with `CheckIn(deviceIMEI)`. Nobody calls it — so the monitor's internal timeout map is permanently empty and no devices ever "fail" the dead-man test.

## Wire diagram

```
                              ┌─────────────────────────────┐
                              │  main.go (existing)         │
                              │  deadmanMonitor :=          │
                              │    deadman.NewMonitor(      │
                              │      store, escalation)     │
                              └─────────────┬───────────────┘
                                            │ (already exists)
                                            ▼
            ┌──────────────────────────────────────────────────┐
            │  rockblock.Handler.ServeHTTP        ← REQ-300    │
            │    after successful MO decode:                   │
            │       deadmanMonitor.CheckIn(imei)               │
            │                                                  │
            │  cloudloop.WebhookHandler.processLingoMO ← REQ-301│
            │    after successful MO decode:                   │
            │       deadmanMonitor.CheckIn(imei)               │
            │                                                  │
            │  position.Subscriber.onPositionUpdate ← REQ-302  │
            │    after position parse:                         │
            │       deadmanMonitor.CheckIn(imei)               │
            └─────────────────┬────────────────────────────────┘
                              │
                              ▼
        ┌────────────────────────────────────────────────────────┐
        │  deadmanMonitor.tick() (every ≤30s)                    │
        │    SELECT device_imei FROM device_deadman_config       │
        │      WHERE enabled=1 AND triggered_at IS NULL          │
        │      AND last_seen_at + timeout_seconds < now()        │
        │                                                        │
        │    for each: ← REQ-303                                 │
        │      escalation.Engine.Trigger(imei, deadmanEvent)     │
        │      audit.Log("deadman.triggered", ...) ← REQ-304     │
        │      publish meshsat/{imei}/deadman/triggered ← REQ-305│
        │      UPDATE device_deadman_config                      │
        │        SET triggered_at=now() ← REQ-314                │
        │      hub_deadman_triggered_total{imei}++  ← REQ-316    │
        └────────────────────────────────────────────────────────┘
                              │
                              ▼
        ┌────────────────────────────────────────────────────────┐
        │  operator-facing REST API                              │
        │    GET  /api/devices/{imei}/deadman  ← REQ-306         │
        │    POST /api/devices/{imei}/deadman  ← REQ-307         │
        │      tenant-filtered ← REQ-310                          │
        │      timeout bounds 60..86400  ← REQ-308 + REQ-309      │
        └────────────────────────────────────────────────────────┘
```

## Why a per-device config table (not per-tenant defaults)

Field operators set device-specific dead-man windows depending on mission profile — a stationary base station may have a 24h timeout, an active SAR responder may have a 1h timeout. Per-device config beats per-tenant defaults for this use case (REQ-312).

## Tables touched

- `audit_log` (existing) — new event types `deadman.triggered`, `deadman.recovered`.
- `device_deadman_config` (new) — schema per REQ-312. Both stores per Article XII.

## Re-fire prevention (REQ-314 + REQ-315)

Without `triggered_at`, the monitor would fire on every tick for any timed-out device, spamming the escalation chain. Setting `triggered_at = now()` once causes the WHERE clause to exclude the device until a check-in clears it (REQ-315).

Operator semantics: once a dead-man fires, the operator MUST verify the device is OK + the device sends a check-in (any MO or position) to clear the triggered state. If the device is genuinely down and the operator can't reach it, the escalation chain has already done its job.

## Cross-spec composition

- `spec/001-sos-trigger/` (already shipped): triggers escalation on SOS-keyword MO. This spec triggers escalation on dead-man-timeout. SAME `escalation.Engine` instance from main.go — no double-wiring.
- `spec/002-mo-fragment-reassembly/`: CheckIn happens AFTER fragment reassembly completes — partial fragments don't count as a check-in (REQ-300 says "decoded successfully").
- `spec/003-e2e-encryption-wire/`: CheckIn happens AFTER decryption (if applicable) — undecryptable payloads still count as MO arrivals since the device IS alive enough to transmit.

## Out of scope

- No tenant-level dead-man defaults — per-device config only in v1.1.
- No dead-man "snooze" UI — operator must POST to re-enable.
- No retry of escalation chain — that's already handled by SOS spec REQ-013 (exponential backoff 1/2/4s).
