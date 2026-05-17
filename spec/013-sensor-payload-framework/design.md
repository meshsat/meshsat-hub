# Design — Sensor Payload Framework

## Goal

Devices in the field send opaque binary payloads that downstream tools can't reason about. The sensor payload framework lets tenants declare "this byte-stream is a ZCL frame; decode it" or "...is Cayenne LPP; decode it" and the routing engine enriches outbound messages with decoded fields BEFORE dispatch to TAK/webhooks/etc.

## Wire diagram

```
                                ┌───────────────────────────┐
                                │ inbound MO (decoded text  │
                                │ OR base64 binary)         │
                                └────────────┬──────────────┘
                                             │
                                             ▼
                                ┌───────────────────────────┐
                                │ routing.Engine.Dispatch   │
                                │                           │
                                │ for each matching route:  │
                                │   decoded := decoder.     │
                                │     Decode(msg)           │ ← REQ-1209
                                │   if err != nil:          │ ← REQ-1211
                                │     log WARN              │
                                │     counter error++       │
                                │     dispatch msg as-is    │
                                │   else:                   │ ← REQ-1210
                                │     msg.decoded = decoded │
                                │     dispatch enriched msg │
                                └───────────────────────────┘
```

## Decoder kinds (REQ-1201)

| Kind | Implementation | Use case |
|---|---|---|
| `zigbee_cluster` | Built-in. Parses ZCL frame headers + cluster/attribute IDs against ZCL spec (REQ-1212). | ZigBee 3.0 devices via the existing internal/zigbee/ paths |
| `lora_cayenne_lpp` | Built-in. Parses Cayenne LPP channel/data-type pairs (REQ-1213). | LoRa devices using the popular Cayenne spec |
| `protobuf` | Built-in. Accepts a tenant-uploaded `.proto` file + dynamically decodes via descriptor (REQ-1214). | Operator-defined custom binary formats |
| `json_passthrough` | Trivial. Parses JSON + emits as `decoded` (REQ-1215). | Debugging + non-binary devices |
| `wasm` | DEFERRED (REQ-1218) — would let operators upload `.wasm` modules for fully custom decoders. Out of v2.0 scope. |

## Why a decoder-per-tenant model

Two tenants could legitimately decode the same raw bytes differently (different device firmware versions, different cluster IDs). Tenant-scoped registration prevents cross-tenant decoder leakage and lets operators iterate on their decoder config without coordinating with the platform.

## Tables touched

- `sensor_decoder` (new) per REQ-1207. Both stores per Article XII.
- `audit_log` (existing) — new event types `sensor_decoder.{created,updated,deleted}`.

## `config` field semantics

The `config TEXT` column (REQ-1207) is decoder-kind-dependent JSON:
- `zigbee_cluster`: `{enabled_clusters: ["0x0000", "0x0402", ...]}`
- `lora_cayenne_lpp`: `{}` (no config needed)
- `protobuf`: `{proto_file_id: <uuid>}` — references a separately-uploaded .proto file
- `json_passthrough`: `{}`

## Routing-engine integration

The decoder runs as a transform stage in `routing.Engine.Dispatch` (spec/006). It runs BEFORE the destination handler is invoked, so TAK / APRS / webhook handlers see the enriched message. The decoder lookup is `(tenant_id, message.device_class)` — operators tag their devices with a class string in the existing device registry.

## Out of scope (deferred to v2.1)

- WASM-kind decoders (REQ-1218).
- Decoder marketplace / sharing across tenants.
- Decoder versioning (multiple coexisting versions per kind).
- Tenant-uploadable `.proto` file storage UI — v2.0 ships REST-only; UI is deferred.
