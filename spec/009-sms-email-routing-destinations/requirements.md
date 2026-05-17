# Requirements — SMS + Email as Routing Destinations (meshsat-hub v1.3, MESHSAT-181)

Source: `docs/ROADMAP.md` L131–L138, `docs/EXECUTION_PLAN.md` §"Task 15: SMS + Email as routing destinations". Depends on `spec/006-routing-engine-core/` (dispatch.Registry) AND existing internal/sms/ + internal/email/ gateways (already shipped per v1.2 DONE).

The routing engine's `dispatch.Registry` from spec/006 already declares slots for `sms` and `email` destination types. The handlers themselves were not yet wired. This spec wires them.

## Functional requirements

REQ-800: The system shall register an `sms` handler in `dispatch.Registry` that bridges routing dispatch calls to the existing `internal/sms.Gateway.Send(ctx, dest, body)` method.

REQ-801: The system shall register an `email` handler in `dispatch.Registry` that bridges routing dispatch calls to the existing `internal/email.Gateway.Send(ctx, dest, subject, body)` method.

REQ-802: When a route with `destination_type=sms` matches a message, the `sms` handler shall extract the destination phone number from the message's source-device contact list AND shall invoke `sms.Gateway.Send` with the message text body.

REQ-803: When a route with `destination_type=email` matches a message, the `email` handler shall extract the destination email address from the message's source-device contact list AND shall invoke `email.Gateway.Send` with subject "MeshSat: <source_type> from <device_imei>" and the message text body.

REQ-804: The `Route` schema shall gain an optional `destination_address` field for `sms` and `email` destinations, overriding the device-contact-list lookup when provided.

REQ-805: When a route specifies `destination_type=sms` AND `destination_address` is set, the handler shall send to the provided phone number instead of the device-contact lookup.

REQ-806: When a route specifies `destination_type=email` AND `destination_address` is set, the handler shall send to the provided email address instead of the device-contact lookup.

REQ-807: When the SMS gateway returns an error (Twilio API failure, credit exhaustion), the routing engine shall increment `hub_route_dispatch_error_total{reason="handler-error", destination_type="sms"}` per spec/006 REQ-515.

REQ-808: When the email gateway returns an error (SMTP failure, key missing), the routing engine shall increment `hub_route_dispatch_error_total{reason="handler-error", destination_type="email"}` per spec/006 REQ-515.

REQ-809: When the email destination is configured with a PGP key for the recipient (looked up in the existing email keystore from v1.2), the email handler shall PGP-encrypt the body before sending.

REQ-810: When the destination address (phone or email) cannot be resolved from either the route's `destination_address` field or the device contact list, the handler shall NOT invoke the gateway AND shall increment `hub_route_dispatch_error_total{reason="no-address", destination_type=<sms|email>}`.

REQ-811: The route schema validation (existing `route.json` from spec/006) shall accept an optional `destination_address` string field with format `tel` for SMS and `email` for email.

REQ-812: When a route's `destination_type` is changed to `sms` or `email` via PUT /api/routes/{id}, the operator shall be required to provide a `destination_address` if the source device's contact list lacks the relevant entry — the system shall return 400 with an explanatory error.
