Feature: Default Zero-Config Routes (meshsat-hub v1.3, MESHSAT-179)

  Parent epic: MESHSAT-179. Killer-feature delivery per docs/ROADMAP.md L131.

  Background:
    Given the Hub is running with the routing.Engine started

  # REQ-600 + REQ-601 + REQ-602 — tenant creation seeds defaults
  Scenario: New tenant gets all 8 default routes
    When a new tenant "tenant-new" is created
    Then the route table contains exactly 8 default routes for tenant-new
    And every default route has name starting with "default:"
    And every default route has enabled=true and filter={}

  # REQ-603 — audit entry uses system actor
  Scenario: Default-route seeding writes audit entries with system actor
    When a new tenant "tenant-X" is created
    Then 8 audit_log entries of type "route.created" exist with tenant_id="tenant-X"
    And every audit entry has actor_session_id="system:tenant-onboarding"

  # REQ-604 + REQ-607 — owner can re-seed
  Scenario: Owner re-seeds defaults via POST /api/routes/seed-defaults
    Given the operator authenticates with JWT containing tenant_id="tenant-A" role="owner"
    When POST /api/routes/seed-defaults is called
    Then the response status is 200
    And the response body contains seeded_count and skipped_count

  # REQ-605 — re-seed is idempotent
  Scenario: Re-seeding does not touch existing default routes
    Given tenant-A has all 8 default routes
    And the operator disabled "default:mo_decoded-webhook" by setting enabled=false
    When POST /api/routes/seed-defaults is called
    Then the response body has seeded_count=0 and skipped_count=8
    And the "default:mo_decoded-webhook" route is still disabled

  # REQ-606 — non-owner cannot re-seed
  Scenario: Operator role cannot re-seed defaults
    Given the operator authenticates with JWT containing tenant_id="tenant-A" role="operator"
    When POST /api/routes/seed-defaults is called
    Then the response status is 403

  # REQ-608 — deleted defaults stay deleted
  Scenario: Deleted default route does not auto-reappear
    Given tenant-A has the "default:position-aprs_is" route
    When DELETE /api/routes/{route_id} is called for that route
    And a new MO position message arrives for tenant-A
    Then the APRS-IS handler is NOT invoked for the position message

  # REQ-609 — Prometheus counter
  Scenario: Seeding emits hub_default_routes_seeded_total
    When a new tenant "tenant-Y" is created
    Then the hub_default_routes_seeded_total{tenant_id="tenant-Y"} counter is incremented by 1
