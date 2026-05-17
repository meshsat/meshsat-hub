# Requirements — Default Zero-Config Routes (meshsat-hub v1.3, MESHSAT-179)

Source: `docs/ROADMAP.md` L131–L138, `docs/EXECUTION_PLAN.md` §"Task 13: Default zero-config routes (MESHSAT-179)". Depends on `spec/006-routing-engine-core/`.

A new tenant should receive sensible default routes so messages flow out of the box without manual route configuration. Operators may disable or modify any default route afterwards.

## Functional requirements

REQ-600: When a new tenant is created, the system shall seed a default set of routes into the `route` table.

REQ-601: The default route set shall include: `mo_decoded → tak`, `mo_decoded → aprs_is`, `mo_decoded → webhook`, `mo_decoded → notification`, `position → tak`, `position → aprs_is`, `sos → notification`, `sos → tak`.

REQ-602: The system shall seed every default route with `enabled=true`, an empty `filter`, and a stable `name` prefixed with `default:`.

REQ-603: When a default route is seeded, the system shall write an audit log entry of type `route.created` with `actor_session_id` set to the literal string `system:tenant-onboarding`.

REQ-604: The system shall expose `POST /api/routes/seed-defaults` accepting an empty body to re-seed the default route set for the caller's tenant.

REQ-605: When `POST /api/routes/seed-defaults` is called and a default route with the same name already exists, the system shall skip the existing route AND shall NOT modify its `enabled` flag (so operator-disabled defaults stay disabled across re-seed).

REQ-606: When `POST /api/routes/seed-defaults` is called by an operator, the system shall verify the operator's `tenant_id` JWT claim AND shall verify the operator role is `owner` (per existing RBAC roles per `docs/SECURITY_AUDIT.md` §4).

REQ-607: The response body of `POST /api/routes/seed-defaults` shall contain `seeded_count` (number of routes inserted) and `skipped_count` (number of routes skipped because they already exist).

REQ-608: When an operator deletes a default route via `DELETE /api/routes/{route_id}` (per `spec/006-routing-engine-core/` REQ-508), the system shall NOT recreate the deleted default outside of an explicit `POST /api/routes/seed-defaults` call.

REQ-609: The system shall increment the `hub_default_routes_seeded_total` Prometheus counter labelled by `tenant_id` when a tenant receives default routes.
