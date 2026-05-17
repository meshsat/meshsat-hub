# Design — Routing Engine Core

## Goal

Replace hard-coded MQTT fan-out with operator-configurable per-tenant routing. The `Route` table is the source of truth; the in-memory `routing.Engine` is a 1-second-refresh cache subscribing to every inbound MQTT topic shape and dispatching to per-destination handlers.

## Wire diagram

```
                          ┌────────────────────────────────────┐
                          │   inbound MQTT (subscribed at      │
                          │   engine startup, REQ-501):        │
                          │   meshsat/+/mo/decoded             │
                          │   meshsat/+/position               │
                          │   meshsat/+/sos                    │
                          │   meshsat/+/telemetry              │
                          └─────────────┬──────────────────────┘
                                        │
                                        ▼
                          ┌────────────────────────────────────┐
                          │   routing.Engine.OnMessage         │
                          │   (REQ-502)                        │
                          │   tenant_id := extractTenant(msg)  │
                          │   source_type := extractSrc(topic) │
                          │   matchingRoutes := cache.Filter(  │
                          │     tenant_id, source_type, msg)   │
                          └─────────────┬──────────────────────┘
                                        │ (REQ-503)
                                        ▼
              ┌──────────────────────────────────────────────────────┐
              │   for each matching route:                           │
              │     handler := dispatch.Lookup(route.destination_type)│
              │     if handler == nil:                               │ ← REQ-514
              │       counter unknown-destination++                  │
              │       continue                                       │
              │     err := handler.Dispatch(ctx, route, msg)         │
              │     if err != nil:                                   │ ← REQ-515
              │       counter handler-error{dest}++                  │
              │       audit "route.dispatch_error"                   │
              │     else:                                            │ ← REQ-516
              │       counter success{dest}++                        │
              └──────────────────────────────────────────────────────┘
                                        │
                                        ▼
                          ┌────────────────────────────────────┐
                          │   /api/routes CRUD (REQ-505..510)  │
                          │   tenant-filtered (REQ-509)        │
                          │   audit on every mutation (REQ-510)│
                          │                                    │
                          │   On any DB mutation:              │ ← REQ-517
                          │     engine.Reload() within 1s      │ ← REQ-518
                          └────────────────────────────────────┘
```

## Tables touched

- `route` (new) per REQ-511. Both stores per Article XII.
- `audit_log` (existing) — new event types `route.created/updated/deleted/dispatch_error`.

## Why an in-memory cache (vs DB-on-every-message)

MO ingress is high-throughput (typical ~1000 msgs/min/tenant peak). Querying `route` for every inbound message would be a per-message DB hit. In-memory cache refreshed within 1 second of any mutation (REQ-517) costs at most 1 second of stale routing — acceptable for a non-safety-critical routing layer (SOS path is separate, has its own escalation chain).

## Dispatch handler registry

`dispatch.Registry` is a `map[string]Handler` keyed by `destination_type`. Each handler is bound at engine startup (TAK gateway, APRS-IS, webhook dispatcher, notification engine, MQTT republish, SMS gateway, email gateway). Adding a new destination type = registering a new handler — no engine code change.

Spec 009 (SMS + email destinations) and the existing channel gateways register against THIS registry.

## Filter semantics (REQ-513)

`filter` JSON is operator-friendly: any subset of `{keyword, portnum, device_imei}` may be present. AND-semantics across present fields. Absent fields match all. Examples:
- `{}` → match every message in the source_type
- `{"keyword": "SOS"}` → only SOS-keyword text payloads
- `{"device_imei": "300258060902280"}` → only messages from one device
- `{"keyword": "weather", "portnum": 5}` → text matching "weather" AND portnum 5

Complex boolean filters (OR, NOT, regex) deferred to a v1.3.1 spec — keeps the v1.3 surface bounded.

## Out of scope (deferred to subsequent specs)

- Default zero-config routes — `spec/007-default-zero-config-routes/` covers tenant-creation seeding.
- Routing UI — `spec/008-routing-ui/` covers the Vue surface.
- SMS + email handler bindings — `spec/009-sms-email-routing-destinations/` extends the dispatch registry.
- Boolean / regex filters — deferred to v1.3.1.
