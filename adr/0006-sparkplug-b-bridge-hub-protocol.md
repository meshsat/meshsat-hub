# ADR-0006 — Sparkplug-B-inspired Bridge⇄Hub wire protocol

* Status: Accepted — codified after the fact 2026-05-17. The decision is shipped at protocol version `meshsat-uplink/v1`.
* Date: Originally drafted 2026-03-23 (MESHSAT-280); recorded as ADR 2026-05-17 during the deep-spec audit.
* Deciders: `ufwtqkgz@meshsat.net`
* Source document: `docs/bridge-hub-protocol.md`

## Context

A bridge fleet (each running on a Raspberry Pi or BananaPi in the field) needs a well-defined lifecycle for registering with Hub, reporting health, sending device telemetry, and surfacing emergency events. The wire protocol has to satisfy several constraints simultaneously:

1. **Offline-first** — bridges drop connectivity routinely; local outbox + reconnect replay must Just Work.
2. **Transport-agnostic** — MQTT is primary, but Iridium SBD/IMT (compressed binary) is a fallback when MQTT is unreachable for >5 min.
3. **CoT-native** — operators using TAK want every entity (bridge, device, SOS event) to map to a MIL-STD-2525 CoT type without translation in the gateway.
4. **Multi-tenant** — bridge registration is tenant-scoped; topic ACLs enforce.
5. **Backwards-compatible** — legacy device-telemetry topics (`meshsat/{device_id}/position`, etc.) must continue to work for pre-protocol bridges.

A custom hand-rolled message protocol would re-invent state-machine semantics for every event type. An off-the-shelf IIoT protocol gives us tested semantics + a community reference.

## Decision

The bridge ⇄ Hub uplink follows the **Eclipse Sparkplug B** edge-to-SCADA pattern, namespaced as `meshsat-uplink/v1`. Every bridge and every device it owns has a deterministic **BIRTH → DATA → DEATH** lifecycle, anchored by MQTT retained messages and LWT (Last Will and Testament).

Topic namespace (per `docs/bridge-hub-protocol.md` L29–L63):

| Lifecycle event | Topic | Retained | QoS |
|---|---|---|---|
| Bridge registration | `meshsat/bridge/{bridge_id}/birth` | Yes | 1 |
| Bridge offline (graceful) | `meshsat/bridge/{bridge_id}/death` | No | 1 |
| Bridge offline (ungraceful, via LWT) | (same `death` topic, set as MQTT LWT) | No | 1 |
| Bridge health (every 30s) | `meshsat/bridge/{bridge_id}/health` | No | 0 |
| Device under bridge online | `meshsat/bridge/{bridge_id}/device/{device_id}/birth` | No | 1 |
| Device under bridge offline | `meshsat/bridge/{bridge_id}/device/{device_id}/death` | No | 1 |
| Hub → bridge command | `meshsat/bridge/{bridge_id}/cmd` | No | 1 |
| Bridge → Hub command response | `meshsat/bridge/{bridge_id}/cmd/response` | No | 1 |

All messages are JSON, all include `"protocol": "meshsat-uplink/v1"`, all include an RFC-3339 `timestamp` field. The full schema for each message type (`BridgeBirth`, `BridgeDeath`, `BridgeHealth`, `DeviceBirth`, `DeviceDeath`, `Command`, `CommandResponse`) is in `docs/bridge-hub-protocol.md` L73–L237.

## Lifecycle invariants

1. On connect, the bridge sets its MQTT **LWT** to `BridgeDeath` on `meshsat/bridge/{id}/death`. This guarantees ungraceful disconnects surface as a `DEATH` event without bridge cooperation.
2. The bridge then publishes `BridgeBirth` on `meshsat/bridge/{id}/birth` with the **retained** flag set. New Hub instances joining the broker see the current state of every bridge by reading retained births.
3. On reconnect after a network drop, the bridge **re-publishes** `BridgeBirth` (overwrites the stale retained message), then replays its **local outbox** FIFO at ≤10 msg/s. `BridgeBirth` itself is never queued — always re-emitted fresh.
4. On graceful shutdown, the bridge publishes `DeviceDeath` for every device it owns, then publishes its own `BridgeDeath` (reason: `shutdown`), then disconnects.
5. Hub's bridge reaper marks bridges + their devices offline both via timeout AND via stale-birth detection (see Constitution Article X — both required).

## CoT mapping

Every entity emits a MIL-STD-2525 CoT type so a downstream TAK server can render it without per-entity logic:

| Entity | CoT type | TAK rendering |
|---|---|---|
| Bridge (Pi/BananaPi) | `a-f-G-U-C-I` | Blue diamond + antenna (friendly ground unit infrastructure) |
| Meshtastic node | `a-f-G-U-C` | Blue diamond (friendly ground unit) |
| Iridium SBD/IMT modem | `a-f-G-E-S` | Blue rectangle (friendly ground equipment sensor) |
| Cellular modem | `a-f-G-E-C` | Blue rectangle (friendly ground equipment comms) |
| Hub server | `a-f-G-I-H` | Blue tent (friendly ground installation HQ) |
| SOS / Emergency | `b-a` | Red exclamation (alarm) |
| Chat message | `b-t-f` | Chat bubble (GeoChat) |

Source: `docs/bridge-hub-protocol.md` L239–L251.

## Consequences

**Positive**
- Bridge implementation is straightforward — Sparkplug B is well-documented and has reference implementations.
- Hub can render fleet state by replaying retained births alone, no separate "current state" snapshot needed.
- TAK integration is zero-config — bridges and devices already emit CoT types.
- Authentication is progressive (Phase 1 username/pw → Phase 2 mTLS → Phase 3 ACLs per `docs/bridge-hub-protocol.md` L306–L320) without breaking existing bridges at each upgrade.

**Negative**
- MQTT retained messages can become stale if a bridge is permanently retired without graceful shutdown — Hub UI's "purge bridge" action MUST publish an empty retained message on `birth` topic to clear.
- Sparkplug B's group/edge/device hierarchy is larger than what we use — we only adopt birth/death/data + state machine semantics, not the full numeric `bdSeq` / `seqNum` ordering. Acceptable simplification given our QoS-1 + retained-birth pattern catches the same misordering classes.

**Forward direction**
- A `meshsat-uplink/v2` would be required for any breaking topic-shape change. The `protocol` version field in every message is the signal.
- Adding non-bridge entity types (e.g. shared infrastructure devices not owned by a bridge) would go on a new `meshsat/{tenant}/{entity_type}/{entity_id}/...` namespace, NOT under `meshsat/bridge/...`.

## Alternatives considered

- **Custom hand-rolled protocol**: rejected — re-invents Sparkplug B's lifecycle state machine, with worse documentation.
- **Bare MQTT pub/sub without lifecycle**: rejected — no way to recover bridge state after Hub restart without a separate snapshot mechanism.
- **gRPC stream**: rejected — adds HTTP/2 to bridge dependencies, breaks the offline-first + Iridium-fallback story (gRPC doesn't degrade gracefully to satellite).

## Compliance

- Schemas in both `meshsat/internal/hubreporter/protocol.go` AND `meshsat-hub/internal/protocol/protocol.go` MUST stay in sync via the `protocol` version field — they are NOT a shared module today (`docs/bridge-hub-protocol.md` L324–L327).
- Hub reaper MUST handle stale retained births (per Constitution Article X).
- LWT MUST be set BEFORE birth publish on every bridge connect.
- Hub MUST honor `cmd_response` ordering via `request_id` correlation (per L210–L233 of the protocol doc).
