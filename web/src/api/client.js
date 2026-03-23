import { useAuthStore } from '../stores/auth'

const BASE = '/api'

async function fetchJSON(url, opts = {}) {
  const auth = useAuthStore()
  const headers = { 'Content-Type': 'application/json', ...opts.headers }
  if (auth.token) {
    headers['Authorization'] = `Bearer ${auth.token}`
  }
  let res = await fetch(BASE + url, { ...opts, headers })
  if (res.status === 401) {
    // Try silent token refresh before logging out
    const refreshed = await auth.refreshToken()
    if (refreshed) {
      headers['Authorization'] = `Bearer ${auth.token}`
      res = await fetch(BASE + url, { ...opts, headers })
    }
    if (res.status === 401) {
      auth.logout()
      window.location.hash = '#/login'
      throw new Error('Unauthorized')
    }
  }
  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }))
    throw new Error(err.error || res.statusText)
  }
  if (res.status === 204) return null
  return res.json()
}

export const devices = {
  list: () => fetchJSON('/devices'),
  get: (imei) => fetchJSON(`/devices/${imei}`),
  create: (data) => fetchJSON('/devices', { method: 'POST', body: JSON.stringify(data) }),
  update: (imei, data) => fetchJSON(`/devices/${imei}`, { method: 'PUT', body: JSON.stringify(data) }),
  delete: (imei) => fetchJSON(`/devices/${imei}`, { method: 'DELETE' }),
}

export const deviceKeys = {
  list: (imei) => fetchJSON(`/devices/${imei}/keys`),
  create: (imei, data) => fetchJSON(`/devices/${imei}/keys`, { method: 'POST', body: JSON.stringify(data) }),
  delete: (imei, id) => fetchJSON(`/devices/${imei}/keys/${id}`, { method: 'DELETE' }),
}

export const deviceConfig = {
  getLatest: (imei) => fetchJSON(`/devices/${imei}/config`),
  createVersion: (imei, data) => fetchJSON(`/devices/${imei}/config`, { method: 'PUT', body: JSON.stringify(data) }),
  listVersions: (imei, limit = 50) => fetchJSON(`/devices/${imei}/config/history?limit=${limit}`),
  getVersion: (imei, version) => fetchJSON(`/devices/${imei}/config/${version}`),
}

export const messages = {
  list: (device = '', limit = 100) => {
    const params = new URLSearchParams()
    if (device) params.set('device', device)
    if (limit) params.set('limit', limit)
    return fetchJSON(`/messages?${params}`)
  },
  get: (id) => fetchJSON(`/messages/${id}`),
  send: (imei, text, compress = true, encrypt = true) => fetchJSON(`/devices/${imei}/send`, {
    method: 'POST', body: JSON.stringify({ text, compress, encrypt })
  }),
  sendSMS: (to, text, compress = false, encrypt = false) => fetchJSON('/sms/send', {
    method: 'POST', body: JSON.stringify({ to, text, compress, encrypt })
  }),
}

export const positions = {
  allLatest: () => fetchJSON('/positions/latest'),
  latest: (imei) => fetchJSON(`/devices/${imei}/position`),
  history: (imei, limit = 100) => fetchJSON(`/devices/${imei}/positions?limit=${limit}`),
}

export const credits = {
  get: () => fetchJSON('/credits'),
}

export const constellations = {
  list: () => fetchJSON('/constellations'),
}

export const authApi = {
  me: () => fetchJSON('/auth/me'),
  listKeys: () => fetchJSON('/auth/keys'),
  createKey: (data) => fetchJSON('/auth/keys', { method: 'POST', body: JSON.stringify(data) }),
  deleteKey: (id) => fetchJSON(`/auth/keys/${id}`, { method: 'DELETE' }),
}

export const ratelimit = {
  all: () => fetchJSON('/ratelimit'),
  get: (deviceID) => fetchJSON(`/ratelimit/${deviceID}`),
  override: (deviceID, data) => fetchJSON(`/ratelimit/${deviceID}/override`, { method: 'POST', body: JSON.stringify(data) }),
  deleteOverride: (deviceID) => fetchJSON(`/ratelimit/${deviceID}/override`, { method: 'DELETE' }),
}

export const auditLog = {
  list: (limit = 100) => fetchJSON(`/audit?limit=${limit}`),
  verify: () => fetchJSON('/audit/verify'),
}

export const escalation = {
  listChains: () => fetchJSON('/escalation/chains'),
  getChain: (id) => fetchJSON(`/escalation/chains/${id}`),
  createChain: (data) => fetchJSON('/escalation/chains', { method: 'POST', body: JSON.stringify(data) }),
  deleteChain: (id) => fetchJSON(`/escalation/chains/${id}`, { method: 'DELETE' }),
  listAlerts: (active = true, limit = 50) => fetchJSON(`/alerts?active=${active}&limit=${limit}`),
  getAlert: (id) => fetchJSON(`/alerts/${id}`),
  triggerAlert: (data) => fetchJSON('/alerts', { method: 'POST', body: JSON.stringify(data) }),
  ackAlert: (id, data = {}) => fetchJSON(`/alerts/${id}/ack`, { method: 'POST', body: JSON.stringify(data) }),
}

export const deadman = {
  list: () => fetchJSON('/deadman'),
  configure: (imei, data) => fetchJSON(`/deadman/${imei}`, { method: 'PUT', body: JSON.stringify(data) }),
  delete: (imei) => fetchJSON(`/deadman/${imei}`, { method: 'DELETE' }),
  snooze: (imei, data) => fetchJSON(`/deadman/${imei}/snooze`, { method: 'POST', body: JSON.stringify(data) }),
}

export const notifications = {
  listPrefs: () => fetchJSON('/notifications/prefs'),
  getPref: (imei) => fetchJSON(`/notifications/prefs/${imei}`),
  savePref: (imei, data) => fetchJSON(`/notifications/prefs/${imei}`, { method: 'PUT', body: JSON.stringify(data) }),
  deletePref: (imei) => fetchJSON(`/notifications/prefs/${imei}`, { method: 'DELETE' }),
}

export const webhooks = {
  list: () => fetchJSON('/webhooks'),
  create: (data) => fetchJSON('/webhooks', { method: 'POST', body: JSON.stringify(data) }),
  delete: (id) => fetchJSON(`/webhooks/${id}`, { method: 'DELETE' }),
  logs: () => fetchJSON('/webhooks/logs'),
}

export const ota = {
  listTargets: () => fetchJSON('/ota/targets'),
  getTarget: (id) => fetchJSON(`/ota/targets/${id}`),
  createTarget: (data) => fetchJSON('/ota/targets', { method: 'POST', body: JSON.stringify(data) }),
  deleteTarget: (id) => fetchJSON(`/ota/targets/${id}`, { method: 'DELETE' }),
  getTargetActions: (id) => fetchJSON(`/ota/targets/${id}/actions`),
  cancelAction: (controllerId, actionId) => fetchJSON(`/ota/targets/${controllerId}/actions/${actionId}`, { method: 'DELETE' }),
  createRollout: (data) => fetchJSON('/ota/rollouts', { method: 'POST', body: JSON.stringify(data) }),
  getRollout: (id) => fetchJSON(`/ota/rollouts/${id}`),
  startRollout: (id) => fetchJSON(`/ota/rollouts/${id}/start`, { method: 'POST' }),
  pauseRollout: (id) => fetchJSON(`/ota/rollouts/${id}/pause`, { method: 'POST' }),
}

export const mptcp = {
  status: () => fetchJSON('/mptcp/status'),
  setStrategy: (strategy) => fetchJSON('/mptcp/strategy', { method: 'PUT', body: JSON.stringify({ strategy }) }),
  listEndpoints: () => fetchJSON('/mptcp/endpoints'),
  addEndpoint: (data) => fetchJSON('/mptcp/endpoints', { method: 'POST', body: JSON.stringify(data) }),
  removeEndpoint: (id) => fetchJSON(`/mptcp/endpoints/${id}`, { method: 'DELETE' }),
}

export const users = {
  list: () => fetchJSON('/users'),
  get: (id) => fetchJSON(`/users/${id}`),
  create: (data) => fetchJSON('/users', { method: 'POST', body: JSON.stringify(data) }),
  update: (id, data) => fetchJSON(`/users/${id}`, { method: 'PUT', body: JSON.stringify(data) }),
  delete: (id) => fetchJSON(`/users/${id}`, { method: 'DELETE' }),
}

export const routes = {
  list: () => fetchJSON('/routes'),
  create: (data) => fetchJSON('/routes', { method: 'POST', body: JSON.stringify(data) }),
  update: (id, data) => fetchJSON(`/routes/${id}`, { method: 'PUT', body: JSON.stringify(data) }),
  delete: (id) => fetchJSON(`/routes/${id}`, { method: 'DELETE' }),
  test: (data) => fetchJSON('/routes/test', { method: 'POST', body: JSON.stringify(data) }),
}

export const reticulum = {
  identity: () => fetchJSON('/reticulum/identity'),
  routes: () => fetchJSON('/reticulum/routes'),
  relay: () => fetchJSON('/reticulum/relay'),
}

export const email = {
  publicKey: () => fetchJSON('/email/keys/public'),
  listContacts: () => fetchJSON('/email/keys'),
  addContact: (data) => fetchJSON('/email/keys', { method: 'POST', body: JSON.stringify(data) }),
  deleteContact: (addr) => fetchJSON(`/email/keys/${encodeURIComponent(addr)}`, { method: 'DELETE' }),
  testSend: (data) => fetchJSON('/email/test', { method: 'POST', body: JSON.stringify(data) }),
}

export const wireguard = {
  listPeers: () => fetchJSON('/wireguard/peers'),
  createPeer: (data) => fetchJSON('/wireguard/peers', { method: 'POST', body: JSON.stringify(data) }),
  getPeerConfig: (id) => fetchJSON(`/wireguard/peers/${id}/config`),
  deletePeer: (id) => fetchJSON(`/wireguard/peers/${id}`, { method: 'DELETE' }),
  getDeviceWG: (imei) => fetchJSON(`/devices/${imei}/wireguard`),
}

export const backup = {
  exportData: () => fetchJSON('/backup/export'),
  diff: (data) => fetchJSON('/backup/diff', { method: 'POST', body: JSON.stringify(data) }),
  importData: (data) => fetchJSON('/backup/import', { method: 'POST', body: JSON.stringify(data) }),
}

export const codecs = {
  list: () => fetchJSON('/codecs'),
}

export const tor = {
  onion: () => fetchJSON('/tor/onion'),
}

export const ipougrs = {
  status: () => fetchJSON('/ipougrs/status'),
}

export const geofences = {
  list: () => fetchJSON('/geofences'),
  create: (data) => fetchJSON('/geofences', { method: 'POST', body: JSON.stringify(data) }),
  delete: (id) => fetchJSON(`/geofences/${id}`, { method: 'DELETE' }),
}

export const bridges = {
  list: () => fetchJSON('/bridges'),
  get: (id) => fetchJSON(`/bridges/${id}`),
}

export const integrations = {
  list: () => fetchJSON('/integrations'),
}

export const health = {
  check: () => fetch('/healthz').then(r => r.json()).catch(() => ({ status: 'error' })),
  readyz: () => fetch('/readyz').then(r => r.json()).catch(() => ({ status: 'error' })),
}
