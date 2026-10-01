# MeshSat Bridge-to-Hub Uplink Protocol

**Protocol Version:** `meshsat-uplink/v1`
**Status:** Draft
**Date:** 2026-03-23
**Issue:** MESHSAT-280

## Overview

This document specifies the wire-level protocol for MeshSat field nodes (bridges, Android apps) to register with and report to a MeshSat Hub instance. The protocol is inspired by Eclipse Sparkplug B (IIoT edge-to-SCADA standard) and is CoT-native for TAK integration.

## Design Principles

1. **Sparkplug B lifecycle** — BIRTH/DEATH/DATA pattern for edge nodes and devices
2. **CoT-native** — every entity maps to a MIL-STD-2525 CoT type
3. **Offline-first** — local queue with store-and-forward on reconnect
4. **Transport-agnostic** — MQTT primary, satellite binary fallback
5. **Multi-tenant** — bridge registration is tenant-scoped
6. **Backwards-compatible** — device telemetry uses existing `meshsat/{device_id}/*` topics

## Transport

**Primary:** MQTT 3.1.1/5.0 over TLS (port 8883) or plain (port 1883/6071)
**Fallback:** Iridium SBD/IMT compressed binary (see Satellite Fallback section)

## MQTT Topic Namespace

### Bridge Lifecycle Topics (NEW)

| Topic | QoS | Retained | Publisher | Description |
|-------|-----|----------|-----------|-------------|
| `meshsat/bridge/{bridge_id}/birth` | 1 | Yes | Bridge | Bridge registration / online |
| `meshsat/bridge/{bridge_id}/death` | 1 | No | Bridge/LWT | Bridge offline |
| `meshsat/bridge/{bridge_id}/health` | 0 | No | Bridge | Periodic health (30s) |
| `meshsat/bridge/{bridge_id}/cmd` | 1 | No | Hub | Commands to bridge |
| `meshsat/bridge/{bridge_id}/cmd/response` | 1 | No | Bridge | Command responses |

### Device Lifecycle Topics (NEW)

| Topic | QoS | Retained | Publisher | Description |
|-------|-----|----------|-----------|-------------|
| `meshsat/bridge/{bridge_id}/device/{device_id}/birth` | 1 | No | Bridge | Device online |
| `meshsat/bridge/{bridge_id}/device/{device_id}/death` | 1 | No | Bridge | Device offline |

### Device Telemetry Topics (EXISTING — backwards compatible)

| Topic | QoS | Publisher | Description |
|-------|-----|-----------|-------------|
| `meshsat/{device_id}/position` | 0 | Bridge | GPS position |
| `meshsat/{device_id}/telemetry` | 0 | Bridge | Sensor data |
| `meshsat/{device_id}/sos` | 1 | Bridge | Emergency event |
| `meshsat/{device_id}/mo/decoded` | 0 | Bridge | Decoded messages |

### TAK Export Topic (Hub 2026-10, MESHSAT-1458)

| Topic | QoS | Retained | Publisher | Description |
|-------|-----|----------|-----------|-------------|
| `meshsat/bridge/{bridge_id}/tak/cot/out` | 1 | No | Bridge | One Cursor-on-Target event the bridge built, as XML |

A bridge (a kit, or the Android or iOS app) that builds CoT for what it knows
publishes each event here, and the Hub forwards it to every TAK server the
bridge's tenant has: the hosted one, the tenant's own, or both. The bridge
needs no TAK server of its own and no address for one.

- **Payload:** exactly one `<event>` element as UTF-8 XML, at most 16 KiB. No
  DOCTYPE, comment, CDATA section or processing instruction (one leading
  `<?xml?>` declaration is tolerated), no namespace prefixes. The event needs
  `uid`, `type`, `time`, `start`, `stale` and one `<point>` with `lat` and
  `lon`; `<point>` and `<detail>` are the only direct children. Anything else
  is dropped and counted, never forwarded in part.
- **The Hub re-writes it.** Every element and attribute is kept, in order; the
  bytes are the Hub's own, on one line. Do not rely on formatting surviving.
- **Not retained, not queued while offline.** CoT is perishable: an event that
  could not be sent when it was true is not worth sending later.
- **Not forwarded:** keepalives (`t-x-c-t...`), protocol negotiation
  (`t-x-takp-...`), a `uid` starting `meshsat-device-` or `meshsat-bridge-`
  (those are the Hub's own markers), an event already stale by more than five
  minutes, and a repeat of an event forwarded in the last minute.
- **The bridge must be registered** to the tenant whose topic root it
  publishes under. The Hub takes the tenant from the topic and never from the
  bridge id.
- **Budget:** 10 events a second per bridge and 50 per tenant, with bursts of
  50 and 200. Emergencies (`type` starting `b-a`, or a `<emergency>` detail)
  have a budget of their own and go ahead of everything else.
- **One node, one marker.** When a bridge exports a position event (`type`
  starting `a-`) for a node, the Hub stops drawing that node from its
  `position` reports until the event goes stale, so the node is on the map
  once, under the name the bridge gave it.

The topic clients first shipped with, `meshsat/{id}/tak/cot/out`, was never
accepted by the broker and is not consumed.

### Tenant Namespaces (Hub 2026-09, MESHSAT-864)

The tables above show the **default tenant** shape. Every other tenant owns the
root `meshsat/{tenant_id}`, so its bridges publish and subscribe under
`meshsat/{tenant_id}/bridge/{bridge_id}/...` and
`meshsat/{tenant_id}/{device_id}/...`. A bridge learns its root from the
provisioning bundle field `mqtt_topic_prefix` (`"meshsat"` for the default
tenant, `"meshsat/{tenant_id}"` otherwise) and must build every topic as
`{mqtt_topic_prefix}/...`. The Hub subscribes to both shapes and answers on
the shape of the bridge's tenant. A registered bridge or device cannot change
tenant by publishing under another prefix: the Hub trusts its own records, and
an unregistered device published under an unknown tenant lands in the default
tenant.

### Hub Subscription Wildcards

```
meshsat/bridge/+/birth
meshsat/bridge/+/death
meshsat/bridge/+/health
meshsat/bridge/+/cmd/response
meshsat/bridge/+/device/+/birth
meshsat/bridge/+/device/+/death
meshsat/bridge/+/tak/cot/out
```
plus the tenant-prefixed twin of each (`meshsat/+/bridge/+/birth`, `meshsat/+/+/position`, ...).

## Message Formats

All messages are JSON. All include a `protocol` field set to `meshsat-uplink/v1`. All include a `timestamp` field in RFC 3339 format.

### BridgeBirth

Published on connect (retained). Re-published on reconnect.

```json
{
  "protocol": "meshsat-uplink/v1",
  "bridge_id": "mule01",
  "version": "0.18.0",
  "hostname": "nllei01mule01",
  "mode": "direct",
  "tenant_id": "default",
  "location": {
    "lat": 52.16,
    "lon": 4.51,
    "alt": 0,
    "source": "fixed"
  },
  "interfaces": [
    {"name": "mesh_0", "type": "meshtastic", "status": "online", "port": "/dev/ttyACM0"},
    {"name": "iridium_0", "type": "iridium_sbd", "status": "online", "imei": "300234065123456"},
    {"name": "cellular_0", "type": "cellular", "status": "online", "imsi": "204080123456789"}
  ],
  "capabilities": ["meshtastic", "iridium_sbd", "cellular", "reticulum", "tak", "aprs"],
  "reticulum": {
    "identity_hash": "a1b2c3d4e5f6",
    "public_key": "base64...",
    "transport_enabled": true
  },
  "cot_type": "a-f-G-U-C-I",
  "cot_callsign": "MESHSAT-MULE01",
  "uptime_sec": 86400,
  "timestamp": "2026-03-23T12:00:00Z"
}
```

### BridgeDeath

Published explicitly on graceful shutdown. Set as LWT for ungraceful disconnect.

```json
{
  "protocol": "meshsat-uplink/v1",
  "bridge_id": "mule01",
  "reason": "shutdown",
  "timestamp": "2026-03-23T12:00:00Z"
}
```

Reason values: `shutdown` (graceful), `lwt` (ungraceful), `error` (crash).

### BridgeHealth

Published every 30 seconds (configurable).

```json
{
  "protocol": "meshsat-uplink/v1",
  "bridge_id": "mule01",
  "uptime_sec": 86430,
  "cpu_pct": 12.5,
  "mem_pct": 45.2,
  "disk_pct": 23.1,
  "interfaces": [
    {"name": "mesh_0", "status": "online", "health_score": 92, "nodes_seen": 3},
    {"name": "iridium_0", "status": "online", "signal_bars": 3, "mo_count": 42},
    {"name": "cellular_0", "status": "online", "signal_dbm": -67, "operator": "KPN"}
  ],
  "burst_queue": {"pending": 2, "next_window": "2026-03-23T12:05:00Z"},
  "reticulum": {"routes": 5, "links": 1, "announces_relayed": 12},
  "outbox": {"pending": 0, "replayed": 0},
  "timestamp": "2026-03-23T12:00:30Z"
}
```

### DeviceBirth

Published when a device comes online under a bridge.

```json
{
  "protocol": "meshsat-uplink/v1",
  "device_id": "!aabbccdd",
  "bridge_id": "mule01",
  "type": "meshtastic_node",
  "label": "T-Deck Alpha",
  "hardware": "LILYGO_TDECK",
  "firmware": "2.5.6",
  "position": {"lat": 52.17, "lon": 4.52, "alt": 5, "source": "gps"},
  "cot_type": "a-f-G-U-C",
  "cot_callsign": "TDECK-ALPHA",
  "capabilities": ["position", "telemetry", "text"],
  "timestamp": "2026-03-23T12:00:00Z"
}
```

### DeviceDeath

```json
{
  "protocol": "meshsat-uplink/v1",
  "device_id": "!aabbccdd",
  "bridge_id": "mule01",
  "reason": "offline",
  "timestamp": "2026-03-23T12:10:00Z"
}
```

### DevicePosition (legacy topic format)

```json
{
  "lat": 52.17,
  "lon": 4.52,
  "alt": 5.0,
  "speed": 1.2,
  "course": 180.0,
  "source": "gps",
  "cep": 10.0,
  "bridge_id": "mule01",
  "timestamp": "2026-03-23T12:00:15Z"
}
```

### DeviceTelemetry (legacy topic format)

```json
{
  "battery_level": 85.0,
  "voltage": 4.1,
  "temperature": 22.5,
  "humidity": 45.0,
  "channel_util": 12.3,
  "air_util_tx": 3.2,
  "uptime_sec": 7200,
  "bridge_id": "mule01",
  "timestamp": "2026-03-23T12:00:15Z"
}
```

### Command (Hub to Bridge)

```json
{
  "protocol": "meshsat-uplink/v1",
  "cmd": "send_text",
  "request_id": "550e8400-e29b-41d4-a716-446655440000",
  "target_device": "!aabbccdd",
  "payload": {"text": "check in please"},
  "timestamp": "2026-03-23T12:01:00Z"
}
```

Supported commands: `send_mt`, `send_text`, `config_update`, `flush_burst`, `ping`, `reboot`.

### CommandResponse (Bridge to Hub)

```json
{
  "protocol": "meshsat-uplink/v1",
  "request_id": "550e8400-e29b-41d4-a716-446655440000",
  "cmd": "send_text",
  "status": "ok",
  "result": {"message_id": 12345},
  "timestamp": "2026-03-23T12:01:01Z"
}
```

Status values: `ok`, `error`.

## CoT Type Mapping

| Entity | CoT Type | MIL-STD-2525 | TAK Appearance |
|--------|----------|--------------|----------------|
| Bridge (Pi/BananaPi) | `a-f-G-U-C-I` | Friendly Ground Unit Infrastructure | Blue diamond + antenna |
| Meshtastic node | `a-f-G-U-C` | Friendly Ground Unit | Blue diamond |
| Iridium modem (SBD/IMT) | `a-f-G-E-S` | Friendly Ground Equipment Sensor | Blue rectangle |
| Cellular modem | `a-f-G-E-C` | Friendly Ground Equipment Comms | Blue rectangle |
| ZigBee device | `a-f-G-U-C` | Friendly Ground Unit | Blue diamond |
| Android phone | `a-f-G-U-C` | Friendly Ground Unit (mobile) | Blue diamond |
| Hub server | `a-f-G-I-H` | Friendly Ground Installation HQ | Blue tent |
| SOS/Emergency | `b-a` | Alarm | Red exclamation |
| Chat message | `b-t-f` | GeoChat | Chat bubble |

## Device Type Identifiers

| Identifier | Description |
|------------|-------------|
| `meshtastic_node` | LoRa mesh node (T-Deck, Heltec, etc.) |
| `iridium_sbd` | Iridium 9603N SBD modem |
| `iridium_imt` | RockBLOCK 9704 IMT modem |
| `cellular` | Cellular modem (LTE/4G/5G) |
| `zigbee` | ZigBee coordinator or end device |
| `aprs` | APRS/AX.25 station |

## Connection Lifecycle

### Normal Operation
1. Bridge connects to Hub MQTT broker
2. Sets LWT: BridgeDeath on `meshsat/bridge/{id}/death`
3. Publishes BridgeBirth on `meshsat/bridge/{id}/birth` (retained)
4. Publishes DeviceBirth for each connected device
5. Starts health reporter (every 30s)
6. Streams device positions/telemetry to legacy topics
7. Listens for commands on `meshsat/bridge/{id}/cmd`

### Graceful Shutdown
1. Publishes DeviceDeath for all devices (reason: `bridge_shutdown`)
2. Publishes BridgeDeath (reason: `shutdown`)
3. Disconnects MQTT

### Ungraceful Disconnect
1. MQTT broker fires LWT: BridgeDeath (reason: `lwt`)
2. Hub marks bridge and all devices offline

### Reconnect
1. Bridge reconnects to MQTT broker
2. Re-publishes BridgeBirth (retained, overwrites stale birth)
3. Replays offline outbox (FIFO, throttled to 10 msg/s)
4. Resumes normal health/telemetry reporting

## Offline Resilience

### Local Outbox
- SQLite table `hub_outbox` stores unsent messages
- FIFO replay on reconnect
- 7-day retention or 10,000 messages (configurable)
- Bridge birth is never queued (always sent fresh)

### Satellite Fallback
When MQTT is unreachable for >5 minutes and satellite is available:
- Magic byte prefix `0x4D53` identifies bridge satellite messages
- Compressed binary encoding (protobuf + zstd)
- Position every 15 min, SOS immediately, health every hour
- Hub webhook handler decodes and processes as MQTT equivalent
- Satellite mode stops when MQTT reconnects

## Authentication (Progressive)

### Phase 1: Username/Password
- Bridge uses MQTT username (bridge_id) and password (shared secret)
- Hub API generates credentials: `POST /api/bridges/{id}/credentials`

### Phase 2: TLS Client Certificates
- Hub acts as mini-CA, issues per-bridge certificates
- Certificate CN contains bridge_id
- 90-day expiry with auto-renewal

### Phase 3: ACLs
- Bridge can only publish/subscribe to its own topics
- Device topics restricted to devices owned by the bridge

## Go Implementation

Types are defined in two independent packages (not a shared module):
- **Bridge:** `meshsat/internal/hubreporter/protocol.go`
- **Hub:** `github.com/meshsat/meshsat-hub/internal/protocol/protocol.go`

Keep in sync via the `protocol` version field in all messages.
