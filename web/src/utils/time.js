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

// Format just the date: "2026-03-18"
export function formatDateUTC(ts) {
  if (!ts) return '—'
  const d = new Date(ts)
  if (isNaN(d.getTime())) return '—'
  return d.toISOString().substring(0, 10)
}
