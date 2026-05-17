# Design — Default Zero-Config Routes

## Goal

A brand-new tenant gains immediate cross-channel reach: any satellite message lands in TAK, APRS-IS, webhooks, and notifications without setting up a single route. This is the "killer feature" promise per `docs/ROADMAP.md` L131 — zero-config out of the box.

## Trigger points

1. **Tenant creation hook**: the existing tenant-creation handler in `internal/auth/` (or wherever tenants are minted) gains a call to `routing.SeedDefaults(ctx, tenantID, "system:tenant-onboarding")`.
2. **Operator-initiated re-seed**: `POST /api/routes/seed-defaults` lets the owner restore the canonical default set without manual route creation.

## Default route set (REQ-601)

| Source | Destination | Rationale |
|---|---|---|
| mo_decoded | tak | Forward all decoded MO to TAK CoT |
| mo_decoded | aprs_is | Push MO content to APRS-IS for radio operators |
| mo_decoded | webhook | Default outbound webhook fanout |
| mo_decoded | notification | Apprise/ntfy notify on every MO |
| position | tak | TAK PLI updates |
| position | aprs_is | APRS position beacons |
| sos | notification | Emergency notify |
| sos | tak | TAK emergency rendering |

`sos` destinations specifically duplicate the SOS detector path from spec/001-sos-trigger; this is intentional defense-in-depth — operator-disabled escalation chains should not silently suppress SOS-to-TAK/notification flow.

## Idempotency (REQ-605)

Re-seeding must not blow away operator overrides. The lookup key is `(tenant_id, name)` where `name` is `default:<source>-<destination>` (stable). Existing rows are left untouched. The `enabled` flag is preserved — an operator who disabled `default:mo_decoded-webhook` will NOT see it silently re-enable on next seed.

## Audit + counter

`route.created` audit entry (REQ-603) with `actor_session_id=system:tenant-onboarding` lets operators see which routes came from the system vs human action. `hub_default_routes_seeded_total{tenant_id}` (REQ-609) lets Grafana dashboards count how many tenants have been onboarded.

## Out of scope

- Tenant-template support (different default sets for different tenant tiers) — deferred.
- Operator-configurable default sets — deferred.
