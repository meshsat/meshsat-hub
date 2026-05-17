# Design — Routing UI

## Goal

Give operators a click-driven surface for the routing engine they get from spec/006. The Vue view is mostly CRUD against `/api/routes` plus one new helper endpoint for the test-against-sample flow.

## Component layout

```
web/src/views/RoutesView.vue
  ├── <RouteList>           — table of routes with edit/delete/toggle/test actions
  ├── <RouteEditModal>      — form for create + edit (reused, mode prop)
  ├── <RouteDeleteModal>    — confirmation
  ├── <RouteTestModal>      — sample-message tester
  ├── <RouteFlowDiagram>    — small SVG showing source → destination per row
  └── header badge          — total count for caller's tenant
```

State management: Pinia store `useRoutesStore()` holding the list, mutations refresh the store, components subscribe.

## Test-against-sample helper (REQ-708 + REQ-709)

`POST /api/routes/{route_id}/test` is a new backend endpoint. The handler:
1. Loads the route from DB (tenant-filtered).
2. Parses the sample JSON body.
3. Runs the same `routing.filter.Match(route.filter, sampleMessage)` logic the engine uses.
4. Returns `{matched, destination_type, error_if_any}` — does NOT actually invoke the destination handler.

This is a read-only operation; safe for any operator who can read routes.

## Why a flow diagram (REQ-707)

Operators reason about routing visually. A 2-second glance at "satellite → TAK" is faster than parsing a table row. The diagram is intentionally minimal — two color-coded boxes (blue for source, green for destination) with an arrow between them, label below.

## Constitution Article XIII compliance (REQ-712)

- Vue 3 Composition API with `<script setup>` only.
- Tailwind utility classes only; no Bootstrap, no Material, no custom CSS framework.
- Pinia for state (already used elsewhere in the SPA).

## Accessibility (REQ-713)

- All interactive elements (buttons, toggles, modal close) reachable by tab.
- Focus management on modal open (first interactive element gets focus).
- Escape closes the open modal.
- Color-coded source/destination boxes also include text labels (color-only signaling fails WCAG).

## Out of scope

- Per-route metrics dashboards (forward to Grafana, not built into the UI).
- Bulk import/export of routes — deferred.
