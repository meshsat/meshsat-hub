const BASE = '/api'

async function fetchJSON(url, opts = {}) {
  const res = await fetch(BASE + url, {
    headers: { 'Content-Type': 'application/json', ...opts.headers },
    ...opts,
  })
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

export const messages = {
  list: (device = '', limit = 100) => {
    const params = new URLSearchParams()
    if (device) params.set('device', device)
    if (limit) params.set('limit', limit)
    return fetchJSON(`/messages?${params}`)
  },
  get: (id) => fetchJSON(`/messages/${id}`),
}

export const positions = {
  allLatest: () => fetchJSON('/positions/latest'),
  latest: (imei) => fetchJSON(`/devices/${imei}/position`),
  history: (imei, limit = 100) => fetchJSON(`/devices/${imei}/positions?limit=${limit}`),
}

export const credits = {
  get: () => fetchJSON('/credits'),
}

export const health = {
  check: () => fetchJSON('/healthz').catch(() => ({ status: 'error' })),
}
