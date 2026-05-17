# Design — Wire MO Fragment Reassembly

## Goal

Close the v1.1 P0 silent-failure in `docs/ROADMAP.md` L74. The `fragment.Reassembler` type already exists (`internal/fragment/fragment.go:110`); the RockBLOCK + Cloudloop handlers already expose `SetReassembler`. The wiring is missing — `main.go` never calls either setter, never spawns the expiry goroutine. As a result, multi-fragment SBD payloads pass through ingress as broken partial messages.

## Wire diagram

```
                              ┌─────────────────────────────┐
RockBLOCK MO webhook          │   main.go startup           │
  POST /api/webhook/rockblock │                             │
  ────────►                   │ reassembler :=              │
                              │   fragment.NewReassembler(  │
Cloudloop LingoMO webhook     │     5 * time.Minute)        │ ← REQ-001
  POST /api/webhook/cloudloop │                             │
  ────────►                   │ rbHandler.SetReassembler(   │ ← REQ-002
                              │     reassembler)            │
                              │ clHandler.SetReassembler(   │ ← REQ-003
                              │     reassembler)            │
                              │                             │
                              │ go func() {                 │ ← REQ-004
                              │   t := time.NewTicker(60*s) │
                              │   for range t.C {           │
                              │     reassembler.Expire()    │
                              │   }                         │
                              │ }()                         │
                              └─────────────┬───────────────┘
                                            │
                                            ▼
              ┌─────────────────────────────────────────────┐
              │  ingress handler (rockblock OR cloudloop)   │
              │                                             │
              │  if fragment.IsFragment(rawBytes) {         │ ← REQ-005
              │      reassembled, err :=                    │
              │          reassembler.AddFragment(           │
              │              imei, rawBytes)                │
              │      if err != nil → audit + return         │ ← REQ-008
              │      if reassembled == nil → return         │ ← REQ-007
              │      rawBytes = reassembled                 │ ← REQ-006
              │  }                                          │
              │                                             │
              │  publish mo/raw + mo/decoded                │
              │  increment hub_fragment_received_total      │ ← REQ-013
              └─────────────────────────────────────────────┘
                                            │
                                            ▼
                            ┌─────────────────────────────┐
                            │  /api/fragments/inflight    │ ← REQ-010
                            │  filtered by tenant_id JWT  │ ← REQ-011
                            └─────────────────────────────┘
```

## Why a shared reassembler instance

REQ-003 binds the same `*fragment.Reassembler` to BOTH the RockBLOCK and Cloudloop handlers. A device sending fragments via RockBLOCK then failing-over to Cloudloop (or any other satellite reseller routing the same SBD bundle through a different ingress) sees its fragments converge in one table by `(imei, message_id)`. Per-handler reassemblers would silently drop the cross-provider case.

## State + lifetime

- **In-memory only.** The reassembler intentionally does NOT persist to SQLite. On Hub restart, in-flight fragments are lost. The carrier (Iridium / Cloudloop) will retransmit per the SBD reliability model. Operational tradeoff: simpler code + zero state-migration risk vs occasional retransmit. Documented in REQ-012.
- **5-minute timeout** matches the SBD store-and-forward window. Tunable via a future config, default is enough for the production satellite path.
- **Expiry goroutine** runs every 60s (REQ-004). Time-vs-wakeup tradeoff: short enough to keep memory bounded, long enough to not burn CPU.

## Tables touched

- `audit_log` (existing, append-only SHA-256 hash chain, Constitution Article VIII) — new event types `fragment.error`, `fragment.timeout`.
- No new tables. (The `/api/fragments/inflight` endpoint reads in-memory state from the reassembler; no DB round-trip.)

## Prometheus counter

`hub_fragment_received_total{state="buffered|reassembled|error|timeout|duplicate"}` (REQ-013, REQ-014). Wires into the existing `internal/metrics/` registry.

## Out of scope

- No retransmit logic at the Hub layer — that lives at the carrier (Iridium reliability model).
- No persistent fragment table — explicit no-state design (REQ-012).
- No UI for in-flight fragments — REST API only in v1.1; UI deferred.
