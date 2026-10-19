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

### TAK Delivery Topics (Hub 2026-10, MESHSAT-1461)

| Topic | QoS | Retained | Publisher | Description |
|-------|-----|----------|-----------|-------------|
| `meshsat/broadcast/tak/cot/in` | 0 | No | Hub | One event from one of the tenant's TAK servers |
| `meshsat/broadcast/tak/cot/in/{sender_bridge_id}` | 0 | No | Hub | One event another bridge of the tenant exported |

What the Hub hands back to a tenant's bridges. Subscribe to both: the exact
topic and one level below it (`.../in/+`).

- **From a TAK server.** While a tenant has a bridge online, the Hub holds a
  connection to each of the tenant's TAK servers and delivers what they send:
  the phones on the tenant's TAK map, their chat, their markers.
- **From the tenant's other bridges.** Every event a bridge exports is also
  delivered to the tenant's other bridges, whether or not the tenant has a TAK
  server. The sender's bridge id is the last topic segment (percent-encoded
  like every id). **Drop the message whose last segment is your own bridge
  id.** Compare the topic, not the event's `uid`: a kit exports events for
  mesh nodes other than itself.
- **Payload:** exactly one `<event>` on one line, re-written by the Hub, at
  most 64 KiB from a server and 16 KiB from a bridge.
- **QoS 0 and never retained.** A position is true for a minute or two;
  nothing is queued for a bridge that was offline and nothing is kept for the
  next one to subscribe.
- **Do not export what you receive here.** The Hub has already sent it
  everywhere it goes.
- **Not delivered:** keepalives and protocol negotiation, the Hub's own
  markers (`meshsat-device-...`, `meshsat-bridge-...`, `meshsat-hub-...`), an
  echo of what the Hub itself wrote in the last minute, a second copy of an
  event already delivered, and anything `tak.Sanitize` refuses.
- **The Hub on a TAK server.** On each connection the Hub first sends an event
  with `uid="meshsat-hub-{tenant}"` and callsign "MeshSat Hub". A TAK server
  sends a connection nothing until it has said who it is; the event is not a
  position type, so TAK clients do not draw it.

For a tenant other than the default one, both topics sit under the tenant's
root: `meshsat/{tenant_id}/broadcast/tak/cot/in` and `.../in/{sender}`.

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
meshsat/bridge/+/msg/out          (only with HUB_MSG_ROUTE_ENABLED)
meshsat/bridge/+/msg/ack          (only with HUB_MSG_ROUTE_ENABLED)
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

## Messaging v1 (`hub_msg`, Hub 2026-10, MESHSAT-1584)

One bridge sends one text to another bridge of its tenant through the Hub, and
the sender learns what is known about it: forwarded by the Hub, stored by the
recipient's MeshSat app, refused, or expired. This is the Hub's side of
HeMB-NG stage 2 (decisions C3 to C6a, C13, C4.1). The test vectors are
`docs/fixtures/v1` at release `fixtures-v1.1`; `internal/hubmsg` runs every
one of them as production code.

**State:** the Hub implements it behind `HUB_MSG_ROUTE_ENABLED` (default
`false`; with it off nothing subscribes to these topics; on in the production
configmap since 19 Oct 2026). The Bridge speaks it behind `MESHSAT_HUB_MSG_ROUTE`
(default `false`, meshsat `6a28669f`, durable since `ace79651`, `64a3990a` and `fb7d4d29`): a contact's
MQTT address `bridge:<id>` is a delivery-intent route, and what arrives on
`msg/in` is committed, answered and handed to the access rules as interface
`hub_0`. MeshSat Android speaks it behind the setting "Messages through the
Hub" (default off, meshsat-android `b0a84f5`); MeshSat iOS does not speak it
yet. Recipients are `bridge:<id>` only; a phone is a bridge principal (C4.1),
so phones use the same topics.

### Topics

| Topic | QoS | Retained | Publisher | Payload |
|-------|-----|----------|-----------|---------|
| `{root}/bridge/{id}/msg/out` | 1 | No | the bridge `{id}` | a submission |
| `{root}/bridge/{id}/msg/in` | 1 | No | the Hub only | a delivery to `{id}` |
| `{root}/bridge/{id}/msg/ack` | 1 | No | the bridge `{id}` | its answer to a delivery |
| `{root}/bridge/{id}/msg/status` | 1 | No | the Hub only | events and refusals for `{id}`'s operations |

`{root}` is the tenant root (`meshsat`, or `meshsat/{tenant_id}`, C13). The
sender's principal is the topic's `{id}`: the bridge's NATS user may publish
`msg/out` and `msg/ack` under its own id and is denied `msg/in` and
`msg/status` (`natsauth.NATSPublishDeny`), so no payload field names the
sender.

### Submission (`msg/out`)

```json
{"v":1,"op_id":"<UUIDv7>","mid":"<UUIDv7>","to":"bridge:kit-b",
 "content_type":"text/plain;charset=utf-8","mid_expires_at":1792400000000,
 "expires_at":1792396400000,"content_sha256":"<hex>","body":"Water at the north gate"}
```

- `mid` is the message, `op_id` this attempt to deliver it through the Hub.
  Both are UUIDv7 (C1e). Times are Unix milliseconds.
- `body` is a JSON string; its UTF-8 bytes are what the recipient stores and
  what `content_sha256` covers (C1d: tag `meshsat.content/1`, then `mid`,
  `to`, `content_type`, `mid_expires_at`, `body`, each length-prefixed).
- `expires_at` is when the Hub stops trying, at most `mid_expires_at`.
- At most 16 KiB of body.

**The Hub's checks, in order.** The first that fails is the answer:

1. `bad_id`: `op_id` or `mid` is not a UUIDv7.
2. `digest_mismatch`: `content_sha256` is not the digest of the fields as sent.
3. `bad_expiry`: `expires_at` is later than `mid_expires_at`.
4. `bad_version`, `bad_recipient` (not `bridge:<id>`), `bad_content_type`, `too_large`.
5. The key `(tenant, sender, op_id)`. If it is already stored:
   - the same `to`, `mid`, `content_sha256` and `expires_at`: **every** event
     of the operation is published again (`replay_all_events`), even after its
     deadline, because the sender may have lost any of them;
   - anything else: `op_conflict`.
6. For a new key only:
   - `expired_at_hub` if `expires_at` has passed;
   - `refused_unknown_recipient` if `to` is not a bridge of the tenant;
   - `mid_conflict` if the sender has this `mid` under another digest.
7. Otherwise the operation is stored, admitted and delivered by the outbox.

A refusal is published on the sender's `msg/status` as a refusal record, not
an event. It concerns the submission, not a stored operation, so no reducer
takes it in:

```json
{"v":1,"type":"refusal","op_id":"...","mid":"...","reason":"op_conflict"}
```

A payload that is not JSON is refused `bad_json`. A database error decides
nothing and publishes nothing; the sender's next repeat is decided then.

### Delivery (`msg/in`)

The submission's fields plus `from`, the sender's principal. `from` and
`op_id` are the recipient's commit key (C3):

```json
{"v":1,"op_id":"...","mid":"...","from":"bridge:kit-a","to":"bridge:kit-b",
 "content_type":"text/plain;charset=utf-8","mid_expires_at":...,"expires_at":...,
 "content_sha256":"...","body":"Water at the north gate"}
```

The outbox runs on the leader replica. It publishes a due operation, records
`hub_forwarded` once the broker acknowledged the publish, and publishes again
after 15 s, 30 s, 60 s, 120 s and then every 5 min until the recipient answers
or `expires_at` passes. After `expires_at` it publishes nothing more and
records `hub_expired`. A delivery the broker had already queued can still
arrive late; the recipient refuses it at commit (`expired_at_recipient`) or
stores it, and a late `stored` still completes the operation.

A recipient taken out of the tenant after admission gets nothing; the
operation ends `hub_refused` with reason `refused_unknown_recipient`.

### Answer (`msg/ack`)

```json
{"v":1,"op_id":"...","mid":"...","from":"bridge:kit-a","result":"stored","reason":""}
```

- Sent after the recipient's durable commit (C3), never before.
- `from` is the delivery's `from`: the origin the recipient committed under.
  The Hub takes the answer only for an operation of that origin whose
  recipient is the answering bridge and whose `mid` matches, so `from`
  selects among the answering bridge's own deliveries and cannot reach anyone
  else's. Anything else is ignored.
- `result` is `stored` or `refused`. A refusal's reasons (C5): `op_conflict`,
  `mid_conflict`, `digest_mismatch`, `bad_expiry`, `bad_id`,
  `expired_at_recipient`, `receiver_busy`.

| Answer | Event recorded | Outbox |
|--------|----------------|--------|
| `stored` | `recipient_app_stored`, class `completed` | stops |
| `refused` `receiver_busy` | `recipient_busy`, class `unknown` | keeps trying until `expires_at` |
| `refused` `expired_at_recipient` | `recipient_refused`, class `expired` | stops |
| `refused`, any other C5 reason | `recipient_refused`, class `failed` | stops |

### Events (`msg/status`)

```json
{"v":1,"op_id":"...","mid":"...","recipient":"bridge:kit-b","event_id":"<UUIDv7>","rev":2,
 "class":"completed","kind":"recipient_app_stored","reason":"","authority":"recipient",
 "scope":"the recipient's MeshSat app","detail":"","at":"2026-10-19T03:10:00Z"}
```

- The kinds the Hub writes: `hub_forwarded` (`forwarded`), `hub_expired`
  (`expired`, reason `expired`), `hub_refused` (`failed`, reason
  `refused_unknown_recipient`), and the recipient's kinds above. The sender's
  own `hub_broker_puback` (`accepted`) never comes from the Hub.
- `event_id` is a UUIDv7 derived from the operation's key and the evidence
  (kind, class, reason), so every replica that records the same evidence
  makes the same id and it is stored once.
- The reducer is C6's: the union of the valid events by `event_id`, state =
  the strongest class (`completed` > `failed` > `expired` > `forwarded` >
  `unknown` > `accepted` > `pending`), `completed` and `failed` together mark
  a conflict, `rev` orders the history and decides nothing. The Hub keeps the
  same projection on its own row.
- An event is published until the broker acknowledged it; a failed publish is
  tried again on the next pass, in order.

**What the sender does with them** (the Bridge; a phone does the same):

- It keeps each event as received, once per `event_id`, before its delivery
  ledger takes it. An event the ledger did not take is applied again, from what
  was kept, after a restart too, and the ledger records each `event_id` once.
- An event it could not even keep makes it repeat the operation's submission,
  unchanged, and the Hub answers with every event again under the same ids.
- It keeps asking until it keeps the event it lost, or a valid `completed`
  event (C6's strongest class: nothing a replay could bring raises it). A
  weaker outcome kept (`failed`, `expired`) ends nothing, because a completion
  may follow it: the Hub records a stored answer that comes after its expiry. A
  repeat going out does not end it either.
- A process start asks once for every operation without a completion, a
  weaker outcome kept included, since a lost event may have left no mark; it
  walks them all in batches. A Hub that does not answer is asked less often (30
  s, doubling to 30 min).
- The Bridge's MQTT session is persistent (clean session off, subscriptions
  resumed), so the broker keeps its QoS 1 `msg/status` while it is away. A
  phone's session is clean: what the Hub publishes while the phone is off the
  Hub is gone, so the phone asks once at **every connect**, not only at a
  process start, whatever an operation's wait.
- On a phone the chat row is the ledger: an event is kept and the row's status
  recomputed in one transaction. A phone's operation lives 24 h
  (`expires_at` = `mid_expires_at`).
- It stops 12 h past `expires_at`, inside the Hub's 24 h. For an operation with
  an event still missing, or no outcome at all, it records that the outcome is
  unknown (`hub_outcome_unknown`): given up, not recovered.
- A refusal record is kept the same way, once per reason. The message was not
  carried, so the sender's ledger books the attempt not sent
  (`dropped:hub_refusal:<reason>`) and its next route takes over.

**What the recipient does with a delivery** after its answer: it hands the
stored message to its access rules as `hub_0`, once. The work the rules make is
listed under the stored message, rule, target and destination, in the
transaction that queues it. A hand-off that could not make all of it stays
pending, and when tried again it makes only what is missing.

### Replicas, restarts and retention

- Every replica subscribes to `msg/out` and `msg/ack` (the bus gives each
  replica every message). Admission and events are transactions on the
  shared store, so the second copy changes nothing. The other replica's copy
  of a submission reads as a repeat, and nothing tells it apart from a sender
  asking again, so it is answered as one: the replay marks the operation's
  events for publication again. The drainer publishes them once, however many
  marks came first. A copy that arrives before the events went out changes
  nothing, and one that arrives after sends them once more, under the same ids.
- The outbox leases an operation to one drainer for 30 s; a drainer that dies
  holding it loses the lease. Due times and leases are judged by the
  drainer's clock.
- An operation and its events are kept until 24 h and 120 s after
  `expires_at`, so a late repeat is answered from the stored row.
- Metrics: `meshsat_hub_msg_submissions_total{result,reason}`,
  `meshsat_hub_msg_deliveries_total{result}`, `meshsat_hub_msg_acks_total{kind}`.

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
