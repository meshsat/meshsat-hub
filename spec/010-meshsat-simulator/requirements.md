# Requirements — MeshSat Simulator (meshsat-hub v2.0, MESHSAT-182)

Source: `docs/ROADMAP.md` L149–L156 (v2.0 Developer Experience), `docs/EXECUTION_PLAN.md` §"Task 16: MeshSat Simulator (MESHSAT-182)" — "virtual Iridium modem, synthetic MO generator. Can't develop or demo without hardware".

Constitution invariants in scope: Article III (httpjson.ReadJSON), Article IV (tenant isolation), Article XII (sqlite+mariadb parity for any new persistent state).

A virtual Iridium modem that synthesizes MO messages without real RockBLOCK hardware. Enables `make dev` to spin up a fully functional Hub with simulated traffic, unblocks contributor onboarding, lets demos run on a laptop.

## Functional requirements

REQ-900: The system shall expose `POST /api/simulator/start` accepting a body `{device_imei: string, message_rate_per_minute: int, payload_pattern: enum}` to start a synthetic MO stream from a virtual device.

REQ-901: When `POST /api/simulator/start` is called for an already-running simulator, the system shall return 409 with an explanatory error.

REQ-902: The system shall expose `POST /api/simulator/stop/{device_imei}` to terminate a running simulator.

REQ-903: The system shall expose `GET /api/simulator/active` returning the list of currently running simulators with `device_imei`, `started_at`, `message_count_so_far`, `message_rate_per_minute`.

REQ-904: The system shall support payload patterns `text`, `position`, `telemetry`, `sos`, `binary_random`.

REQ-905: When `payload_pattern=text`, the simulator shall generate plain text messages following a sentence-generator producing short operator-style content.

REQ-906: When `payload_pattern=position`, the simulator shall generate GPS positions drifting from a starting lat/lon by a configurable random-walk delta per message.

REQ-907: When `payload_pattern=sos`, the simulator shall generate one message every `message_rate_per_minute` interval with the literal keyword "SOS" so the SOS detector (spec/001) fires.

REQ-908: The simulator shall route generated messages through the same `internal/rockblock/` ingress path as real webhooks, so all downstream handlers (audit, MQTT publish, dedup, fragment, decryption, dead-man) exercise identically.

REQ-909: When an operator calls any `/api/simulator/*` endpoint, the system shall verify the operator's role is `owner` and return 403 otherwise.

REQ-910: When the simulator is started in `HUB_MODE=standalone` or with env `HUB_SIMULATOR_ENABLED=true`, the simulator endpoints shall be available; otherwise the endpoints shall return 404.

REQ-911: The system shall increment the `hub_simulator_messages_total{device_imei, payload_pattern}` Prometheus counter on every synthetic message generated.

REQ-912: When the Hub shuts down gracefully, the system shall stop all running simulators and shall write an audit log entry of type `simulator.stopped` for each.

REQ-913: The simulator shall NOT be available in cluster mode by default; an operator who explicitly sets `HUB_SIMULATOR_ENABLED=true` in cluster mode shall see a startup-time WARN log entry.
