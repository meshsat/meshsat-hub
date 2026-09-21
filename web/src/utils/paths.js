// How the console talks about bearers. A kit reports its interfaces by name
// and type in its birth and health frames; the console groups them into the
// families an operator thinks in, and draws each family as one state.

export const FAMILIES = [
  { key: 'mesh', label: 'Mesh', long: 'LoRa mesh' },
  { key: 'sat', label: 'Satellite', long: 'Iridium satellite' },
  { key: 'cell', label: 'Cellular', long: 'Cellular and SMS' },
  { key: 'aprs', label: 'APRS', long: 'APRS radio' },
  { key: 'zigbee', label: 'ZigBee', long: 'ZigBee sensors' },
]

export function familyOf(iface) {
  const s = `${iface?.type || ''} ${iface?.name || ''}`.toLowerCase()
  if (/mesh|meshtastic|lora/.test(s)) return 'mesh'
  if (/iridium|sbd|imt|globalstar|astrocast|satellite/.test(s)) return 'sat'
  if (/cellular|sms|lte|gsm|modem/.test(s)) return 'cell'
  if (/aprs/.test(s)) return 'aprs'
  if (/zigbee/.test(s)) return 'zigbee'
  return null
}

// up: at least one interface in the family is working.
// coming: one is still binding or connecting.
// down: the kit has the hardware but none of it is working.
// absent: the kit has no such hardware.
export function familyStates(interfaces) {
  const out = Object.fromEntries(FAMILIES.map((f) => [f.key, 'absent']))
  const rank = { absent: 0, down: 1, coming: 2, up: 3 }
  for (const i of interfaces || []) {
    const f = familyOf(i)
    if (!f) continue
    const st = String(i.status || '').toLowerCase()
    const s = st === 'online' || st === 'up' || st === 'connected' ? 'up'
      : /bind|connect|start|init|search/.test(st) ? 'coming' : 'down'
    if (rank[s] > rank[out[f]]) out[f] = s
  }
  return out
}

export const STATE_WORDS = { up: 'working', coming: 'coming up', down: 'fitted, not working', absent: 'not fitted' }

// The bearer a stored message came over. The Hub records a channel for
// satellite and SMS; mesh traffic arrives through a kit with no channel and a
// Meshtastic node id ("!a1b2c3d4") as its sender.
export function bearerOf(m) {
  const ch = String(m?.channel || '').toLowerCase()
  const id = String(m?.device_imei || '')
  if (ch === 'iridium' || ch === 'globalstar' || /^\d{15}$/.test(id)) return { key: 'sat', label: ch === 'globalstar' ? 'Globalstar' : 'Satellite' }
  if (ch === 'sms' || id.startsWith('+')) return { key: 'cell', label: 'SMS' }
  if (id.startsWith('!')) return { key: 'mesh', label: 'Mesh' }
  if (ch === 'mqtt') return { key: 'ip', label: 'Internet' }
  if (ch === 'email') return { key: 'ip', label: 'Email' }
  return { key: 'other', label: ch || 'Kit' }
}

// A message body the Hub could not read (sealed end to end between kits, or
// binary) is shown as what it is instead of as base64 noise.
export function bodyOf(m) {
  const t = m?.text || ''
  if (!t) return { kind: 'empty', text: m?.raw_hex ? `Binary, ${Math.round(m.raw_hex.length / 2)} bytes` : 'No text' }
  if (/^[A-Za-z0-9+/]{16,}={0,2}$/.test(t) && !/\s/.test(t)) {
    return { kind: 'sealed', text: `Encrypted, ${Math.floor((t.length * 3) / 4)} bytes` }
  }
  return { kind: 'text', text: t }
}

export function ago(ts) {
  if (!ts || String(ts).startsWith('0001-')) return 'never'
  const s = Math.floor((Date.now() - new Date(ts).getTime()) / 1000)
  if (s < 0 || s < 45) return 'just now'
  if (s < 3600) return `${Math.round(s / 60)} min ago`
  if (s < 86400) return `${Math.round(s / 3600)} h ago`
  const d = Math.round(s / 86400)
  return d === 1 ? 'yesterday' : `${d} days ago`
}

export function hhmm(ts) {
  const d = new Date(ts)
  return isNaN(d) ? '' : d.toISOString().slice(11, 16)
}
