# Requirements — Routing Engine Core (meshsat-hub v1.3, MESHSAT-178)

Source: `docs/ROADMAP.md` L131–L138 (v1.3 Routing Engine), `docs/EXECUTION_PLAN.md` §"Task 12: Routing engine core (MESHSAT-178)" — "the killer feature: configurable any-to-any message routing with zero-config defaults".

Constitution invariants in scope: Article III (httpjson.ReadJSON), Article IV (triple-layer tenant isolation), Article VIII (audit hash chain), Article XII (sqlite+mariadb schema parity).

The Hub currently performs hard-coded MQTT-subscribe fan-out (RockBLOCK MO → MQTT topics, no per-tenant routing logic). v1.3 introduces a `Route` model letting operators define `(source → destinations)` rules per-tenant with filters, enable/disable, and per-destination dispatch.

## Functional requirements

REQ-500: The system shall introduce a `Route` model with fields `route_id`, `tenant_id`, `name`, `source_type`, `destination_type`, `filter`, `enabled`, `created_at`, `updated_at`.

REQ-501: The system shall implement a `routing.Engine` that subscribes to every inbound MQTT topic shape (`meshsat/+/mo/decoded`, `meshsat/+/position`, `meshsat/+/sos`, `meshsat/+/telemetry`) on startup.

REQ-502: When an inbound message arrives, the routing engine shall enumerate `enabled=1` routes whose `tenant_id` matches the message's tenant AND whose `source_type` matches the message's source type AND whose `filter` matches the message payload.

REQ-503: When the routing engine iterates a matching route, the routing engine shall dispatch the message to the route's `destination_type` handler.

REQ-504: The system shall support destination types: `tak`, `aprs_is`, `webhook`, `notification` (Apprise/ntfy), `mqtt`, `sms`, `email`.

REQ-505: The system shall expose `GET /api/routes` returning all routes for the caller's tenant.

REQ-506: The system shall expose `POST /api/routes` accepting a `Route` body, validating it against the `route.json` schema, and inserting it.

REQ-507: The system shall expose `PUT /api/routes/{route_id}` accepting a partial `Route` body and updating only the supplied fields.

REQ-508: The system shall expose `DELETE /api/routes/{route_id}` removing the route from the table.

REQ-509: When an operator calls any `/api/routes*` endpoint, the system shall filter by the operator's `tenant_id` JWT claim and shall return 403 on cross-tenant access.

REQ-510: When a route is created, updated, or deleted, the system shall write an audit log entry of type `route.created`, `route.updated`, or `route.deleted` respectively, containing `route_id`, `tenant_id`, `actor_session_id`.

REQ-511: The `route` table shall have columns `route_id TEXT PRIMARY KEY`, `tenant_id TEXT NOT NULL`, `name TEXT NOT NULL`, `source_type TEXT NOT NULL`, `destination_type TEXT NOT NULL`, `filter TEXT`, `enabled INTEGER NOT NULL DEFAULT 1`, `created_at INTEGER NOT NULL`, `updated_at INTEGER NOT NULL`.

REQ-512: The schema migration for the `route` table shall land in BOTH `internal/store/sqlite/` AND `internal/store/mariadb/mariadb.go` per Constitution Article XII.

REQ-513: The route `filter` field shall accept JSON of the form `{keyword: string?, portnum: int?, device_imei: string?}`, where any subset of fields may be present; absent fields match all.

REQ-514: When a route's `destination_type` is unknown, the routing engine shall NOT dispatch and shall increment the `hub_route_dispatch_error_total{reason="unknown-destination"}` Prometheus counter.

REQ-515: When a destination handler returns an error during dispatch, the system shall increment `hub_route_dispatch_error_total{reason="handler-error"}` AND shall write an audit entry of type `route.dispatch_error`.

REQ-516: When a route dispatches successfully, the system shall increment the `hub_route_dispatch_success_total` Prometheus counter labelled by `destination_type`.

REQ-517: The routing engine shall be reload-safe: on `POST /api/routes`, `PUT`, or `DELETE`, the engine shall refresh its in-memory route cache within 1 second without restarting subscriptions.

REQ-518: When the `enabled` flag is toggled, the routing engine shall stop or start dispatching for that route within 1 second of the change committing to DB.
