// Format a date/timestamp string as UTC 24h format: "2026-03-18 20:42:56 UTC"
export function formatUTC(ts) {
  if (!ts || ts === '0001-01-01T00:00:00Z') return '—'
  const d = new Date(ts)
  if (isNaN(d.getTime())) return '—'
  return d.toISOString().replace('T', ' ').substring(0, 19) + ' UTC'
}

// Format just the time portion: "20:42:56 UTC"
export function formatTimeUTC(ts) {
  if (!ts) return '—'
  const d = new Date(ts)
  if (isNaN(d.getTime())) return '—'
  return d.toISOString().substring(11, 19) + ' UTC'
}

// Format relative time: "2 minutes ago", "1 hour ago", etc.
export function timeAgo(ts) {
  if (!ts || ts === '0001-01-01T00:00:00Z') return '—'
  const d = new Date(ts)
  if (isNaN(d.getTime())) return '—'
  const now = Date.now()
  const diff = Math.floor((now - d.getTime()) / 1000)
  if (diff < 0) return 'just now'
  if (diff < 60) return `${diff}s ago`
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`
  if (diff < 604800) return `${Math.floor(diff / 86400)}d ago`
  return formatUTC(ts)
}

// Format seconds as human-readable uptime: "2d 5h 12m"
export function formatUptime(seconds) {
  if (!seconds || seconds <= 0) return '—'
  const d = Math.floor(seconds / 86400)
  const h = Math.floor((seconds % 86400) / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  if (d > 0) return `${d}d ${h}h ${m}m`
  if (h > 0) return `${h}h ${m}m`
  return `${m}m`
}

// Format just the date: "2026-03-18"
export function formatDateUTC(ts) {
  if (!ts) return '—'
  const d = new Date(ts)
  if (isNaN(d.getTime())) return '—'
  return d.toISOString().substring(0, 10)
}
