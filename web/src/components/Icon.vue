<script setup>
// Monochrome line icons for the console, drawn on a 24 px grid with a 1.6
// stroke in currentColor so they take the text colour of wherever they sit.
// Geometry only, no fills, no brand marks: the MeshSat mark is BrandLockup's.
defineProps({
  name: { type: String, required: true },
  size: { type: [Number, String], default: 18 },
})

const P = {
  overview: ['M4 4h7v7H4z', 'M13 4h7v4h-7z', 'M13 10h7v10h-7z', 'M4 13h7v7H4z'],
  map: ['M9 4 3 6.5v13L9 17l6 3 6-2.5v-13L15 7 9 4z', 'M9 4v13', 'M15 7v13'],
  messages: ['M4 5h16v11H9l-5 4V5z', 'M8 9.5h8', 'M8 12.5h5'],
  kits: ['M4 9h16v10H4z', 'M8 9V5', 'M16 9V6', 'M7.5 14h.01', 'M11 14h6'],
  devices: ['M8 3h8v18H8z', 'M11 17.5h2', 'M8 7h8'],
  groups: ['M4 6h7v5H4z', 'M13 6h7v5h-7z', 'M8.5 13h7v5h-7z'],
  bonding: ['M10 14a4 4 0 0 1 0-5.6l2-2a4 4 0 0 1 5.6 5.6l-1 1', 'M14 10a4 4 0 0 1 0 5.6l-2 2a4 4 0 0 1-5.6-5.6l1-1'],
  alerts: ['M12 3 2.5 20h19L12 3z', 'M12 10v4.5', 'M12 17.2h.01'],
  checkins: ['M12 21a8 8 0 1 0 0-16 8 8 0 0 0 0 16z', 'M12 9v4l2.5 2', 'M9 2.5h6'],
  geofences: ['M5 7 12 3l7 4v6l-7 8-7-8V7z', 'M12 10.5h.01'],
  rules: ['M4 7h10', 'M18 7h2', 'M4 17h2', 'M10 17h10', 'M16 5v4', 'M8 15v4'],
  bell: ['M6 16V11a6 6 0 1 1 12 0v5l1.5 2h-15L6 16z', 'M10 20.5h4'],
  routing: ['M4 12h6l4-6h6', 'M10 12l4 6h6', 'M17 3l3 3-3 3', 'M17 15l3 3-3 3'],
  integrations: ['M9 3v5', 'M15 3v5', 'M6 8h12v3a6 6 0 0 1-12 0V8z', 'M12 17v4'],
  webhooks: ['M9 8a3 3 0 1 1 5 2.2L11 16', 'M6 15.5a3 3 0 1 0 4.5 2.6h6', 'M18 12.5a3 3 0 1 1-1.5 5.6'],
  email: ['M3 6h18v12H3z', 'M3 7l9 6 9-6'],
  tak: ['M12 20a8 8 0 1 0 0-16 8 8 0 0 0 0 16z', 'M12 2v5', 'M12 17v5', 'M2 12h5', 'M17 12h5'],
  network: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18z', 'M3 12h18', 'M12 3c2.5 2.7 3.7 5.7 3.7 9s-1.2 6.3-3.7 9c-2.5-2.7-3.7-5.7-3.7-9S9.5 5.7 12 3z'],
  topology: ['M6 6.5a2.5 2.5 0 1 0 0-.01z', 'M18 6.5a2.5 2.5 0 1 0 0-.01z', 'M12 18.5a2.5 2.5 0 1 0 0-.01z', 'M8.2 7.8l2.6 8.4', 'M15.8 7.8l-2.6 8.4', 'M8.5 6.5h7'],
  ota: ['M12 4v10', 'M8 10l4 4 4-4', 'M5 19h14'],
  backup: ['M4 5h16v4H4z', 'M5 9h14v10H5z', 'M10 13h4'],
  users: ['M9 11a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7z', 'M3 20c.7-3.4 3-5.5 6-5.5s5.3 2.1 6 5.5', 'M16 4.5a3.5 3.5 0 0 1 0 6.5', 'M18 14.8c1.6.8 2.6 2.6 3 5.2'],
  key: ['M8 15a4 4 0 1 0 0-.01z', 'M11 12l9-9', 'M17 6l3 3', 'M15 8l2 2'],
  credentials: ['M12 3l7 3v6c0 4.2-3 7.6-7 9-4-1.4-7-4.8-7-9V6l7-3z', 'M9 12l2 2 4-4'],
  audit: ['M6 3h12v18H6z', 'M9 8h6', 'M9 12h6', 'M9 16h3'],
  settings: ['M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z', 'M19 12a7 7 0 0 0-.1-1.2l2-1.6-2-3.4-2.4 1a7 7 0 0 0-2-1.2L14 3h-4l-.5 2.6a7 7 0 0 0-2 1.2l-2.4-1-2 3.4 2 1.6a7 7 0 0 0 0 2.4l-2 1.6 2 3.4 2.4-1a7 7 0 0 0 2 1.2L10 21h4l.5-2.6a7 7 0 0 0 2-1.2l2.4 1 2-3.4-2-1.6c.1-.4.1-.8.1-1.2z'],
  help: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18z', 'M9.5 9.5a2.5 2.5 0 1 1 3.5 2.3c-.6.3-1 .9-1 1.6v.6', 'M12 17h.01'],
  costs: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18z', 'M14.5 9c-.4-1-1.3-1.5-2.5-1.5-1.5 0-2.5.8-2.5 2 0 2.8 5 1.5 5 4.3 0 1.2-1 2.2-2.5 2.2-1.3 0-2.3-.6-2.7-1.7', 'M12 6v1.5', 'M12 16.5V18'],
  search: ['M11 18a7 7 0 1 0 0-14 7 7 0 0 0 0 14z', 'M20 20l-4-4'],
  sun: ['M12 16a4 4 0 1 0 0-8 4 4 0 0 0 0 8z', 'M12 2v2', 'M12 20v2', 'M2 12h2', 'M20 12h2', 'M4.9 4.9l1.4 1.4', 'M17.7 17.7l1.4 1.4', 'M4.9 19.1l1.4-1.4', 'M17.7 6.3l1.4-1.4'],
  moon: ['M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z'],
  menu: ['M4 7h16', 'M4 12h16', 'M4 17h16'],
  close: ['M6 6l12 12', 'M18 6 6 18'],
  chevron: ['M9 6l6 6-6 6'],
  chevronDown: ['M6 9l6 6 6-6'],
  collapse: ['M15 6l-6 6 6 6', 'M20 4v16'],
  expand: ['M9 6l6 6-6 6', 'M4 4v16'],
  logout: ['M15 4h4v16h-4', 'M10 8l-4 4 4 4', 'M6 12h10'],
  external: ['M14 4h6v6', 'M20 4l-9 9', 'M18 14v6H4V6h6'],
  refresh: ['M20 11a8 8 0 1 0-2.3 5.7', 'M20 5v6h-6'],
  plus: ['M12 5v14', 'M5 12h14'],
  check: ['M5 12.5l4.5 4.5L19 7'],
  satellite: ['M7 7l4 4', 'M4.5 9.5l5-5 3 3-5 5z', 'M11.5 16.5l5-5 3 3-5 5z', 'M13 11l2 2', 'M5 19a4 4 0 0 0 4-4'],
  mesh: ['M5 6.5a1.5 1.5 0 1 0 0-.01z', 'M19 6.5a1.5 1.5 0 1 0 0-.01z', 'M12 18.5a1.5 1.5 0 1 0 0-.01z', 'M6.5 6.5h11', 'M6 8l5 9', 'M18 8l-5 9'],
  signal: ['M4 20v-3', 'M9 20v-7', 'M14 20V9', 'M19 20V4'],
  pin: ['M12 21s7-6.2 7-12a7 7 0 1 0-14 0c0 5.8 7 12 7 12z', 'M12 11.5a2.5 2.5 0 1 0 0-5 2.5 2.5 0 0 0 0 5z'],
  lock: ['M6 11h12v10H6z', 'M8.5 11V8a3.5 3.5 0 0 1 7 0v3'],
  filter: ['M4 5h16l-6 7.5V19l-4 2v-8.5L4 5z'],
  layers: ['M12 3 3 8l9 5 9-5-9-5z', 'M3 13l9 5 9-5'],
  target: ['M12 20a8 8 0 1 0 0-16 8 8 0 0 0 0 16z', 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z'],
  // Platform group: an inbox with a waiting card, a row of buildings, a
  // document with a total line, and a server with its status lamp.
  requests: ['M4 13h4l1.5 2.5h5L16 13h4', 'M4 13V19h16v-6', 'M7 9h10', 'M9 5h6'],
  tenants: ['M3 20h18', 'M4 20V8l5-3v15', 'M9 20V11l6-2v11', 'M15 20V13l5-1.5V20', 'M6 11h.01', 'M6 14h.01', 'M12 14h.01'],
  billing: ['M6 3h9l4 4v14H6z', 'M15 3v4h4', 'M9 11h6', 'M9 14h6', 'M9 17h3'],
  platform: ['M4 5h16v6H4z', 'M4 13h16v6H4z', 'M7 8h.01', 'M7 16h.01', 'M11 8h6', 'M11 16h6'],
}
</script>

<template>
  <svg :width="size" :height="size" viewBox="0 0 24 24" fill="none" stroke="currentColor"
    stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"
    class="shrink-0">
    <path v-for="(d, i) in (P[name] || P.help)" :key="i" :d="d" />
  </svg>
</template>
