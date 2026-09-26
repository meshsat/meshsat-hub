// The console's navigation, grouped by the job an operator is doing rather
// than by how the Hub is built. The rail and the jump search read this one
// list, so a page is named the same way everywhere.
//
// `owner` items are for a tenant's owners (the API refuses the others too);
// `platformAdmin` items belong to the platform and no customer sees them:
// the Platform group below (account requests, tenants, billing, the Hub's
// own settings), routed under /platform/*.
export const navGroups = [
  { label: 'Now', items: [
    { to: '/', name: 'dashboard', label: 'Overview', icon: 'overview', keywords: 'dashboard home status attention' },
    { to: '/map', name: 'map', label: 'Map', icon: 'map', keywords: 'positions tracks location' },
    { to: '/messages', name: 'messages', label: 'Messages', icon: 'messages', keywords: 'mo mt sms send inbox' },
  ]},
  { label: 'Fleet', items: [
    { to: '/fleet', name: 'fleet', label: 'Kits', icon: 'kits', keywords: 'bridges fleet add bridge provision qr' },
    { to: '/devices', name: 'devices', label: 'Devices', icon: 'devices', keywords: 'imei modem rockblock handheld' },
    { to: '/device-groups', name: 'deviceGroups', label: 'Groups', icon: 'groups', keywords: 'device groups' },
    { to: '/bond-groups', name: 'bondGroups', label: 'Bonding', icon: 'bonding', keywords: 'hemb bond groups rlnc' },
  ]},
  { label: 'Safety', items: [
    { to: '/escalation', name: 'escalation', label: 'Alerts', icon: 'alerts', keywords: 'sos escalation chains acknowledge on call', badge: 'alerts' },
    { to: '/deadman', name: 'deadman', label: 'Check-ins', icon: 'checkins', keywords: 'dead man switch deadman overdue' },
    { to: '/geofences', name: 'geofences', label: 'Geofences', icon: 'geofences', keywords: 'zones areas' },
    { to: '/alert-rules', name: 'alertRules', label: 'Alert rules', icon: 'rules', keywords: 'thresholds battery rules' },
    { to: '/notifications', name: 'notifications', label: 'Notifications', icon: 'bell', keywords: 'apprise ntfy push' },
  ]},
  { label: 'Delivery', items: [
    { to: '/routing', name: 'routing', label: 'Routing', icon: 'routing', keywords: 'routes rules forward relay' },
    { to: '/integrations', name: 'integrations', label: 'Integrations', icon: 'integrations', keywords: 'cloudloop twilio rock7 providers' },
    { to: '/webhooks', name: 'webhooks', label: 'Webhooks', icon: 'webhooks', keywords: 'outbound http' },
    { to: '/email', name: 'email', label: 'Email', icon: 'email', keywords: 'pgp smtp' },
    // Per-tenant hosted TAK (MESHSAT-1037): the customer's own server.
    { to: '/tak', name: 'tak', label: 'TAK', icon: 'tak', keywords: 'atak itak wintak cot enrol' },
    { to: '/costs', name: 'costs', label: 'Costs', icon: 'costs', keywords: 'credits budget spend' },
  ]},
  { label: 'Network', items: [
    { to: '/network', name: 'network', label: 'Network', icon: 'network', keywords: 'wireguard tor onion mptcp' },
    { to: '/topology', name: 'topology', label: 'Topology', icon: 'topology', keywords: 'reticulum graph' },
    { to: '/ota', name: 'ota', label: 'Updates', icon: 'ota', keywords: 'ota firmware hawkbit rollout' },
  ]},
  { label: 'Admin', items: [
    { to: '/users', name: 'users', label: 'Users', icon: 'users', keywords: 'team invite roles', owner: true },
    { to: '/api-keys', name: 'apikeys', label: 'API keys', icon: 'key', keywords: 'tokens', owner: true },
    { to: '/credentials', name: 'credentials', label: 'Credentials', icon: 'credentials', keywords: 'certificates expiry', owner: true },
    { to: '/audit', name: 'audit', label: 'Audit log', icon: 'audit', keywords: 'history chain verify', owner: true },
    { to: '/backup', name: 'backup', label: 'Backup', icon: 'backup', keywords: 'export import restore' },
    { to: '/settings', name: 'settings', label: 'Settings', icon: 'settings', keywords: 'account plan billing tenant limits' },
  ]},
  { label: 'Platform', items: [
    { to: '/platform/requests', name: 'platformRequests', label: 'Requests', icon: 'requests', keywords: 'signups account requests approve reject beta', badge: 'signups', platformAdmin: true },
    { to: '/platform/tenants', name: 'platformTenants', label: 'Tenants', icon: 'tenants', keywords: 'customers accounts support access view as suspend', platformAdmin: true },
    { to: '/platform/billing', name: 'platformBilling', label: 'Billing', icon: 'billing', keywords: 'receipts refunds vat threshold unattributed payments stripe', platformAdmin: true },
    { to: '/platform/system', name: 'platformSystem', label: 'Platform', icon: 'platform', keywords: 'mqtt url plan tiers hub health system', platformAdmin: true },
  ]},
]

export function visibleGroups(auth) {
  return navGroups
    .map((g) => ({ ...g, items: g.items.filter((i) => (!i.owner || auth.isOwner) && (!i.platformAdmin || auth.isPlatformAdmin)) }))
    .filter((g) => g.items.length > 0)
}

// Pages reachable only from inside another page still get a title in the
// jump search and the browser tab.
export const extraTitles = {
  deviceDetail: 'Device',
  deviceConfig: 'Device configuration',
  deviceKeys: 'Device keys',
  help: 'Help',
  login: 'Sign in',
  platformTenant: 'Tenant',
}

export function titleFor(routeName) {
  for (const g of navGroups) for (const i of g.items) if (i.name === routeName) return i.label
  return extraTitles[routeName] || ''
}
