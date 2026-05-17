Feature: Routing UI (meshsat-hub v1.3, MESHSAT-180)

  Parent epic: MESHSAT-180. Vue surface for the routing engine per docs/ROADMAP.md L131.

  Background:
    Given the operator has authenticated as tenant-A with role="operator"
    And tenant-A has the 8 default routes seeded

  # REQ-700 + REQ-701 — list view
  Scenario: /routes renders the route table
    When the operator navigates to /routes
    Then the table displays 8 rows
    And every row has columns name, source_type, destination_type, filter summary, enabled toggle, actions

  # REQ-702 + REQ-703 — create
  Scenario: + New Route adds a route
    When the operator clicks "+ New Route"
    Then a modal form opens
    When the operator fills in name="my-route", source_type="mo_decoded", destination_type="webhook" and submits
    Then a POST /api/routes is sent
    And on success the route list refreshes
    And the new row "my-route" appears in the table

  # REQ-704 — toggle enabled
  Scenario: Toggling enabled commits via PUT
    Given the row "default:mo_decoded-tak" exists in the table
    When the operator clicks the row's enabled toggle
    Then a PUT /api/routes/{route_id} is sent with body {"enabled": false}
    And the row's enabled state visually updates within 1 second

  # REQ-706 — delete with confirmation
  Scenario: Delete action requires confirmation
    When the operator clicks the Delete action on a row
    Then a confirmation modal appears
    When the operator confirms
    Then a DELETE /api/routes/{route_id} is sent
    And the row disappears from the table

  # REQ-707 — flow diagram
  Scenario: Every row shows a source→destination SVG
    When the operator visits /routes
    Then every row contains an SVG diagram with a blue source box, an arrow, and a green destination box

  # REQ-708 + REQ-709 — test against sample
  Scenario: Test Route action evaluates filter
    Given a route exists with filter={"keyword":"SOS"}
    When the operator clicks "Test Route" on that row
    And submits sample message body {"text":"this is SOS"}
    Then POST /api/routes/{route_id}/test is sent
    And the response shows matched=true
    And the destination handler was NOT actually invoked

  # REQ-710 — invalid sample is caught client-side
  Scenario: Malformed sample shows inline validation
    When the operator opens Test Route
    And enters invalid JSON in the sample body
    Then the form displays an inline validation error
    And NO POST request is made

  # REQ-711 — header badge updates
  Scenario: Route count badge updates after create
    Given the badge shows "8 routes"
    When the operator creates a new route
    Then the badge updates to "9 routes" within 1 second

  # REQ-713 — keyboard navigation
  Scenario: Tab order visits every interactive control
    When the operator presses Tab repeatedly starting from page focus
    Then focus visits "+ New Route" button, then each row's toggle, edit, delete, test buttons in DOM order
    When the operator presses Escape with a modal open
    Then the modal closes
