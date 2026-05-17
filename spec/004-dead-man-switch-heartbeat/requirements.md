# Requirements — Wire Dead Man's Switch Heartbeats (meshsat-hub v1.1, MESHSAT-171)

Source: `docs/ROADMAP.md` L77 ("Dead man's switch triggers: Monitor runs but integration with actual device heartbeats unclear"), `docs/EXECUTION_PLAN.md` §"Task 4: Dead man's switch heartbeat wiring (MESHSAT-171)" — P1 ("monitor runs but doesn't see actual device activity").

Constitution invariants in scope: Article II (security #1), Article VIII (audit log hash chain), Article IX (webhook authenticity verified BEFORE processing — heartbeats arrive via the same RockBLOCK/Cloudloop ingress as MO messages), Article XII (single-source schema parity for any new tables).

The `internal/deadman/` package already implements `Monitor` (`Monitor` struct at `deadman.go:27`, `NewMonitor(store, escalationEngine)` at `deadman.go:39`, `CheckIn(deviceIMEI string)` at `deadman.go:97`). The monitor runs in-process but no caller invokes `CheckIn` on inbound device activity — so devices never "check in" and the dead-man timeout never has anything to reset against.

**Cross-spec dependency:** the SOS trigger spec (`spec/001-sos-trigger/`) already invokes `escalation.Engine.Trigger` on SOS events; this spec wires the dead-man path to the SAME escalation engine, sharing the chain configuration.

## Functional requirements

REQ-300: When a MO message is decoded successfully by the RockBLOCK ingress handler, the handler shall call `deadmanMonitor.CheckIn(deviceIMEI)` before returning.

REQ-301: When a MO message is decoded successfully by the Cloudloop ingress handler, the handler shall call `deadmanMonitor.CheckIn(deviceIMEI)` against the SAME monitor instance.

REQ-302: When a position update is received by the position subscriber for a device, the subscriber shall call `deadmanMonitor.CheckIn(deviceIMEI)`.

REQ-303: When the dead-man monitor detects a device that has not checked in within its configured timeout window, the system shall invoke `escalation.Engine.Trigger(deviceIMEI, deadmanEvent)`.

REQ-304: The system shall write an audit log entry of type `deadman.triggered` containing `device_imei`, `last_seen_at`, `timeout_seconds` when the dead-man condition fires.

REQ-305: The system shall publish a MQTT event to `meshsat/{device_id}/deadman/triggered` per the `deadman-event.json` schema when the dead-man condition fires.

REQ-306: The system shall expose `GET /api/devices/{imei}/deadman` returning the device's current dead-man state with fields `enabled`, `timeout_seconds`, `last_seen_at`, `triggered_at`.

REQ-307: The system shall expose `POST /api/devices/{imei}/deadman` accepting a body `{enabled: bool, timeout_seconds: int}` to configure the dead-man window for a device.

REQ-308: When `POST /api/devices/{imei}/deadman` is called with `timeout_seconds < 60`, the system shall return 400 with an error describing the minimum.

REQ-309: When `POST /api/devices/{imei}/deadman` is called with `timeout_seconds > 86400` (24 hours), the system shall return 400 with an error describing the maximum.

REQ-310: When an operator calls any `/api/devices/{imei}/deadman` endpoint, the system shall filter by the operator's `tenant_id` JWT claim and return 403 on cross-tenant access.

REQ-311: When the dead-man monitor's tick goroutine runs, the system shall consult the `device_deadman_config` table to enumerate enabled devices and their per-device timeouts.

REQ-312: The `device_deadman_config` table shall have columns `device_imei TEXT PRIMARY KEY`, `tenant_id TEXT NOT NULL`, `enabled INTEGER NOT NULL DEFAULT 0`, `timeout_seconds INTEGER NOT NULL DEFAULT 3600`, `updated_at INTEGER NOT NULL`, `triggered_at INTEGER`.

REQ-313: The schema migration adding `device_deadman_config` shall land in BOTH `internal/store/sqlite/` AND `internal/store/mariadb/mariadb.go` per Constitution Article XII.

REQ-314: When the dead-man condition fires for a device, the system shall set `device_deadman_config.triggered_at = now()` to prevent re-firing within the same timeout window.

REQ-315: When a device whose `triggered_at` is set checks in again, the system shall clear `triggered_at` and shall write an audit entry of type `deadman.recovered`.

REQ-316: When the dead-man monitor fires, the system shall increment the `hub_deadman_triggered_total` Prometheus counter labelled by `device_imei`.

REQ-317: When a device check-in resets a previously-triggered dead-man state, the system shall increment `hub_deadman_recovered_total` labelled by `device_imei`.

REQ-318: The dead-man monitor tick interval shall be at most 30 seconds, so the worst-case detection latency is `timeout_seconds + 30s`.
