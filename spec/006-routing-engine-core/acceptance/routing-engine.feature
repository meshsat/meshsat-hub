Feature: Routing Engine Core (meshsat-hub v1.3, MESHSAT-178)

  Parent epic: MESHSAT-178. v1.3 killer feature per docs/ROADMAP.md L131.

  Background:
    Given the Hub is running with the routing.Engine started
    And the engine has subscribed to meshsat/+/mo/decoded, meshsat/+/position, meshsat/+/sos, meshsat/+/telemetry

  # REQ-502 + REQ-503 + REQ-516 — happy path dispatch
  Scenario: Matching route fans out to one destination
    Given a route exists: tenant=tenant-A, source=mo_decoded, destination=tak, enabled=true, filter={}
    When an MO decoded message arrives for tenant-A
    Then the TAK destination handler is invoked exactly once
    And the hub_route_dispatch_success_total{destination_type="tak"} counter is incremented by 1

  # REQ-502 — tenant scoping
  Scenario: Tenant-mismatched route is NOT invoked
    Given a route exists: tenant=tenant-A, source=mo_decoded, destination=tak, enabled=true
    When an MO decoded message arrives for tenant-B
    Then NO TAK destination handler is invoked

  # REQ-513 — filter semantics
  Scenario: Keyword filter narrows the dispatch
    Given a route exists: tenant=tenant-A, source=mo_decoded, destination=tak, enabled=true, filter={"keyword":"SOS"}
    When an MO decoded message with text "this is SOS" arrives for tenant-A
    Then the TAK destination handler is invoked
    When an MO decoded message with text "weather report" arrives for tenant-A
    Then the TAK destination handler is NOT invoked a second time

  # REQ-514 — unknown destination is recorded, not crashed
  Scenario: Unknown destination_type increments the unknown-destination counter
    Given a route exists with destination_type="unknown_kind"
    When a message arrives that would match the route
    Then the hub_route_dispatch_error_total{reason="unknown-destination"} counter is incremented by 1
    And NO handler is invoked
    And the Hub process does not crash

  # REQ-515 — handler error is captured + audited
  Scenario: Destination handler returning an error writes audit + increments counter
    Given a route exists: tenant=tenant-A, source=mo_decoded, destination=webhook, enabled=true
    And the webhook dispatcher will return an error on the next call
    When an MO decoded message arrives for tenant-A
    Then the hub_route_dispatch_error_total{reason="handler-error"} counter is incremented by 1
    And an audit_log entry of type "route.dispatch_error" exists

  # REQ-505 + REQ-509 — list scoped to tenant
  Scenario: GET /api/routes returns only the caller's tenant routes
    Given 2 routes exist for tenant-A and 1 route exists for tenant-B
    And the operator authenticates with JWT containing tenant_id="tenant-A"
    When GET /api/routes is called
    Then the response status is 200
    And the response body contains exactly 2 routes
    And every route has tenant_id "tenant-A"

  # REQ-506 + REQ-510 — create writes audit
  Scenario: POST /api/routes creates a route and writes audit
    Given the operator authenticates with JWT containing tenant_id="tenant-A"
    When POST /api/routes is called with a valid Route body
    Then the response status is 201
    And the response body contains a route_id assigned by the server
    And an audit_log entry of type "route.created" exists

  # REQ-507 + REQ-518 — patch + engine reload
  Scenario: PUT /api/routes/{id} toggling enabled false stops dispatch within 1 second
    Given a route exists: route_id="r-1", enabled=true
    When PUT /api/routes/r-1 is called with body {"enabled": false}
    Then the response status is 200
    And within 1 second the route is excluded from dispatch
    When a matching message arrives
    Then the destination handler is NOT invoked

  # REQ-508 + REQ-510 — delete + audit
  Scenario: DELETE /api/routes/{id} removes the route
    Given a route exists: route_id="r-2"
    When DELETE /api/routes/r-2 is called
    Then the response status is 204
    And no route with route_id="r-2" exists in the DB
    And an audit_log entry of type "route.deleted" exists

  # REQ-509 — cross-tenant access forbidden
  Scenario: Operator from tenant-A cannot patch a tenant-B route
    Given a route exists: route_id="r-3", tenant_id="tenant-B"
    And the operator authenticates with JWT containing tenant_id="tenant-A"
    When PUT /api/routes/r-3 is called
    Then the response status is 403
