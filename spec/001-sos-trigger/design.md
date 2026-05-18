# Design — SOS trigger logic (spec/001 — RETROSPECTIVE)

CGC-grounded against `internal/sos/detector.go` + `detector_test.go` 2026-05-18.

## Real shape (CGC-verified)

```go
// Package sos implements SOS detection on incoming MO messages.
// It subscribes to meshsat/+/mo/decoded, checks for SOS indicators
// (keywords, explicit field), publishes to the sos topic, and fires
// the escalation engine.
package sos

var sosKeywords = []string{"SOS", "MAYDAY", "EMERGENCY"}

type moDecodedMsg struct {
  IMEI string `json:"imei"`
  Text string `json:"text"`
  SOS  *bool  `json:"sos,omitempty"`
}

type SOSEvent struct {
  IMEI    string `json:"imei"`
  Text    string `json:"text"`
  Keyword string `json:"keyword,omitempty"`
  Source  string `json:"source"` // "keyword" or "field"
}

type Detector struct {
  bus       bus.MessageBus
  engine    *escalation.Engine
  tenantID  string
  chainID   string       // default escalation chain
  dataStore store.Store
}

func NewDetector(b bus.MessageBus, engine *escalation.Engine, dataStore store.Store,
                 tenantID, chainID string) *Detector
```

## Wire diagram

```
ESP32 field device → MO uplink → bridge → Hub MQTT (meshsat/{IMEI}/mo/decoded)
                                                  │
                                                  ▼
                                          sos.Detector.handleMO()
                                                  │
                          ┌──────────────────────┼──────────────────────┐
                          ▼                       ▼                       ▼
              keyword match              explicit sos: true        no SOS indicator
              (Source="keyword")         (Source="field")            ↓ pass-through
                          │                       │
                          └───────┬───────────────┘
                                  ▼
                          publish SOSEvent → meshsat/{IMEI}/sos
                                  │
                                  ▼
                          escalation.Engine.Fire(tenantID, chainID, event)
                                  │
                                  ▼
                          (notification fan-out: Apprise / ntfy / Email / SMS)
```

## Real file paths (CGC-verified)

| File | Status |
|---|---|
| `internal/sos/detector.go` | EXISTS — Detector type + sosKeywords + moDecodedMsg + SOSEvent |
| `internal/sos/detector_test.go` | EXISTS — covers all REQ-020 cases |
| `internal/escalation/engine.go` | dependency — Detector takes `*escalation.Engine` |
| `internal/bus/` | dependency — `bus.MessageBus` interface |
| `internal/store/store.go` | dependency — `store.Store` interface |

## NOT present (CGC-confirmed absences)

- No separate `dispatcher.go` — escalation dispatch lives entirely inside `escalation.Engine`
- No `/api/sos/*` REST endpoints on Hub side — the surface is MQTT-only

## Out of scope

- Escalation chain definitions (`escalation.Engine` is its own concern)
- Cross-tenant SOS aggregation
- Replay protection (handled by the `dedup` package for the wider MQTT flow)
