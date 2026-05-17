# Requirements — Routing UI (meshsat-hub v1.3, MESHSAT-180)

Source: `docs/ROADMAP.md` L131–L138, `docs/EXECUTION_PLAN.md` §"Task 14: Routing UI (MESHSAT-180)". Depends on `spec/006-routing-engine-core/`. Vue 3 + Tailwind per Constitution Article XIII.

The operator-facing Vue SPA gains a Routes view: list active routes, create/edit/delete them, visualize the (source → destination) flow, and test a route against a sample message before committing.

## Functional requirements

REQ-700: The system shall add a `RoutesView.vue` view under `web/src/views/` accessible from the navigation as a top-level route at `/routes`.

REQ-701: When the operator visits `/routes`, the view shall call `GET /api/routes` and render the response as a table with columns `name`, `source_type`, `destination_type`, `filter summary`, `enabled toggle`, `actions`.

REQ-702: The view shall provide a `+ New Route` button opening a modal form for creating a route.

REQ-703: When the operator submits the new-route form, the view shall POST to `/api/routes` and refresh the list on success.

REQ-704: When the operator toggles a route's `enabled` switch, the view shall PUT to `/api/routes/{route_id}` with body `{enabled: bool}` and shall show inline success/failure feedback.

REQ-705: When the operator clicks the `Edit` action, the view shall open the new-route form pre-populated with the existing route's fields and shall PUT on submit.

REQ-706: When the operator clicks the `Delete` action, the view shall display a confirmation modal AND on confirm shall DELETE `/api/routes/{route_id}`.

REQ-707: The view shall include a visual `(source → destination)` flow diagram per route, rendered as a labeled SVG arrow between two color-coded boxes.

REQ-708: The view shall include a `Test Route` action that opens a modal accepting a sample message body, invokes a new `POST /api/routes/{route_id}/test` endpoint, and displays the dispatch outcome (matched=true/false, destination invoked, latency_ms).

REQ-709: The system shall expose `POST /api/routes/{route_id}/test` accepting a sample message JSON, evaluating the route's filter against it without invoking real destinations, and returning `{matched, destination_type, error_if_any}`.

REQ-710: When the operator submits the test form with a malformed sample message, the view shall display the validation error inline AND shall NOT make the POST request.

REQ-711: The view shall display the tenant-scoped route count in a header badge that updates after every mutation.

REQ-712: The view shall use Vue 3 Composition API + Tailwind only per Constitution Article XIII; no Options API; no other CSS framework.

REQ-713: The view shall be keyboard-accessible: tab navigation visits every interactive control in DOM order; Enter activates focused buttons; Escape closes open modals.
