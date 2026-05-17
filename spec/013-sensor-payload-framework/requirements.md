# Requirements — Sensor Payload Framework (meshsat-hub v2.0, MESHSAT-185)

Source: `docs/ROADMAP.md` L152, `docs/EXECUTION_PLAN.md` §"Task 19: Sensor payload framework (MESHSAT-185)" — "pluggable decoders (ZigBee, LoRa, Protobuf) with registration API".

Constitution invariants in scope: Article III (httpjson.ReadJSON), Article IV (tenant isolation), Article XII (sqlite+mariadb parity).

A plugin architecture where each tenant can register a payload decoder by name + apply it to MO messages from specific device classes. Out-of-the-box decoders: ZigBee cluster library, LoRa custom binary, generic Protobuf. New decoders are registered via REST API + applied via the routing engine's filter chain (composes with spec/006).

## Functional requirements

REQ-1200: The system shall introduce a `SensorDecoder` model with fields `decoder_id`, `tenant_id`, `name`, `kind` (built-in or wasm), `config`, `created_at`, `updated_at`.

REQ-1201: The system shall ship built-in decoders for kinds `zigbee_cluster`, `lora_cayenne_lpp`, `protobuf`, `json_passthrough`.

REQ-1202: The system shall expose `GET /api/sensor-decoders` returning the list of decoders for the caller's tenant.

REQ-1203: The system shall expose `POST /api/sensor-decoders` accepting a `SensorDecoder` body for registering a new tenant-scoped decoder.

REQ-1204: The system shall expose `PUT /api/sensor-decoders/{decoder_id}` for updating decoder config.

REQ-1205: The system shall expose `DELETE /api/sensor-decoders/{decoder_id}` for removing a decoder.

REQ-1206: When an operator calls any `/api/sensor-decoders*` endpoint, the system shall filter by `tenant_id` JWT claim AND return 403 on cross-tenant access.

REQ-1207: The `sensor_decoder` table shall have columns `decoder_id TEXT PRIMARY KEY`, `tenant_id TEXT NOT NULL`, `name TEXT NOT NULL`, `kind TEXT NOT NULL`, `config TEXT`, `created_at INTEGER NOT NULL`, `updated_at INTEGER NOT NULL`.

REQ-1208: The schema migration adding `sensor_decoder` shall land in BOTH `internal/store/sqlite/` AND `internal/store/mariadb/mariadb.go` per Constitution Article XII.

REQ-1209: When the routing engine dispatches a message to a destination, the engine shall apply any tenant-scoped sensor decoder whose `kind` matches the message's declared device class.

REQ-1210: When a sensor decoder is invoked, the decoder shall enrich the outbound message with decoded fields (e.g. `temperature_celsius`, `humidity_pct`, `battery_voltage`) merged into the message payload as a `decoded` subobject.

REQ-1211: When a sensor decoder fails (malformed input, unknown kind), the routing engine shall log a WARN AND shall increment `hub_sensor_decode_error_total{kind, tenant_id}` AND shall pass the ORIGINAL message through without enrichment.

REQ-1212: The built-in `zigbee_cluster` decoder shall handle ZCL frames per the Zigbee Cluster Library spec for clusters Basic (0x0000), Temperature (0x0402), Humidity (0x0405), Battery (0x0001).

REQ-1213: The built-in `lora_cayenne_lpp` decoder shall handle Cayenne LPP frames per the Cayenne specification for channels 0-99.

REQ-1214: The built-in `protobuf` decoder shall accept a tenant-uploaded `.proto` descriptor file and shall dynamically decode messages against it.

REQ-1215: The built-in `json_passthrough` decoder shall accept JSON input + emit it as the `decoded` subobject unchanged — useful as a debugging decoder.

REQ-1216: When the system loads decoders at startup, the system shall increment `hub_sensor_decoders_loaded_total{kind}` Prometheus counter per kind.

REQ-1217: When a decoder is registered, updated, or deleted, the system shall write an audit log entry of type `sensor_decoder.created`, `sensor_decoder.updated`, or `sensor_decoder.deleted` respectively, with `decoder_id` and `tenant_id`.

REQ-1218: If a POST or PUT to `/api/sensor-decoders` specifies `kind=wasm`, then the system shall return 400 with an error noting that WASM-kind decoders are deferred to v2.1 and the v2.0 surface accepts only the 4 built-in kinds from REQ-1201.
