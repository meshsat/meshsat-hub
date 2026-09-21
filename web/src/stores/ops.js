import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { bridges as bridgesApi, devices as devicesApi, escalation, deadman as deadmanApi, ratelimit as ratelimitApi } from '../api/client'

// The operational picture the shell and the overview share: kits, devices,
// unacknowledged alerts and check-ins. One poll for the whole console instead
// of one per widget, so an open tab costs four requests every 30 s whatever
// page it is on. The websocket only brings the next poll forward; it never
// replaces it, so a replica without the socket still shows the truth.
const POLL_MS = 30000
const WS_DEBOUNCE_MS = 1500

// A kit that has not been heard for this long is history, not news: listing
// it under "needs attention" every day would teach people to ignore the list.
const OFFLINE_NEWS_MS = 7 * 24 * 3600 * 1000

export function parseJSON(v) {
  if (!v) return null
  if (typeof v !== 'string') return v
  try { return JSON.parse(v) } catch { return null }
}

export function isZeroTime(ts) {
  return !ts || String(ts).startsWith('0001-')
}

export const useOpsStore = defineStore('ops', () => {
  const bridges = ref([])
  const devices = ref([])
  const alerts = ref([])
  const checkins = ref([])
  const budgets = ref([])
  const loadedAt = ref(null)
  const loaded = ref(false)
  const failed = ref(false)

  let users = 0
  let timer = null
  let wsTimer = null
  let inflight = null

  async function load() {
    if (inflight) return inflight
    inflight = (async () => {
      const r = await Promise.allSettled([
        bridgesApi.list(),
        devicesApi.list(),
        escalation.listAlerts(true, 50),
        deadmanApi.list(),
        ratelimitApi.all(),
      ])
      const arr = (i) => (r[i].status === 'fulfilled' && Array.isArray(r[i].value) ? r[i].value : null)
      if (arr(0)) bridges.value = arr(0)
      if (arr(1)) devices.value = arr(1)
      if (arr(2)) alerts.value = arr(2)
      if (arr(3)) checkins.value = arr(3)
      if (arr(4)) budgets.value = arr(4)
      failed.value = r.every((x) => x.status === 'rejected')
      loadedAt.value = new Date()
      loaded.value = true
    })()
    try { await inflight } finally { inflight = null }
  }

  // Ref-counted: the shell holds one subscription for as long as someone is
  // signed in; views may hold more without starting a second timer.
  function start() {
    users++
    if (users > 1) return
    load()
    timer = setInterval(load, POLL_MS)
  }

  function stop() {
    users = Math.max(0, users - 1)
    if (users > 0) return
    clearInterval(timer)
    timer = null
  }

  function nudge() {
    if (wsTimer) return
    wsTimer = setTimeout(() => { wsTimer = null; load() }, WS_DEBOUNCE_MS)
  }

  const kits = computed(() => bridges.value.map((b) => {
    const birth = parseJSON(b.last_birth) || {}
    const health = parseJSON(b.last_health) || {}
    const ifaces = (health.interfaces && health.interfaces.length ? health.interfaces : birth.interfaces) || []
    return {
      id: b.bridge_id,
      name: b.cot_callsign || b.label || b.hostname || b.bridge_id,
      online: !!b.online,
      lastSeen: isZeroTime(b.last_seen) ? null : b.last_seen,
      lastReportBearer: b.last_report_bearer || '',
      lastReportAt: isZeroTime(b.last_report_at) ? null : b.last_report_at,
      mode: b.mode || birth.mode || '',
      version: b.version || birth.version || '',
      lat: b.location_lat, lon: b.location_lon,
      battery: health.battery_pct,
      interfaces: ifaces,
      raw: b,
    }
  }))

  // What needs a person, most urgent first (ISA-18.2: priority, then age).
  // Only unacknowledged alerts, overdue check-ins and kits that dropped off
  // recently. A device that is merely quiet is NOT here: silence is not
  // evidence of trouble unless somebody asked for a check-in.
  const attention = computed(() => {
    const items = []
    for (const a of alerts.value) {
      items.push({
        key: 'alert:' + a.id,
        priority: a.type === 'sos' ? 0 : 1,
        kind: a.type === 'sos' ? 'sos' : 'alarm',
        title: a.type === 'sos' ? 'SOS' : (a.type === 'deadman' ? 'Missed check-in' : a.type === 'geofence' ? 'Geofence' : 'Alert'),
        subject: a.device_imei,
        detail: a.detail,
        since: a.created_at,
        alertId: a.id,
        to: { name: 'escalation' },
      })
    }
    for (const c of checkins.value) {
      if (!c.enabled || !c.alerted) continue
      if (alerts.value.some((a) => a.type === 'deadman' && a.device_imei === c.device_imei)) continue
      items.push({
        key: 'checkin:' + c.device_imei,
        priority: 1,
        kind: 'alarm',
        title: 'Missed check-in',
        subject: c.device_imei,
        detail: `Expected every ${Math.round(c.interval_sec / 60)} min`,
        since: c.updated_at,
        to: { name: 'deadman' },
      })
    }
    // A device at its send limit stops sending until the window resets: the
    // tenant's own guard on its carrier bill working as set, but somebody
    // should know, because the next message from the field will not go out.
    for (const b of budgets.value) {
      if (!b.throttled) continue
      items.push({
        key: 'budget:' + b.device_id,
        priority: 2,
        kind: 'caution',
        title: 'Send limit reached',
        subject: b.device_id,
        detail: b.daily_cap > 0 ? `${b.daily_sent} of ${b.daily_cap} today` : (b.monthly_cap > 0 ? `${b.monthly_sent} of ${b.monthly_cap} this month` : 'Sending paused'),
        since: null,
        to: { name: 'deviceDetail', params: { imei: b.device_id } },
      })
    }
    const now = Date.now()
    for (const k of kits.value) {
      if (k.online || !k.lastSeen) continue
      if (now - new Date(k.lastSeen).getTime() > OFFLINE_NEWS_MS) continue
      items.push({
        key: 'kit:' + k.id,
        priority: 2,
        kind: 'caution',
        title: 'Kit offline',
        subject: k.name,
        detail: 'Not connected to the Hub',
        since: k.lastSeen,
        to: { name: 'fleet' },
      })
    }
    return items.sort((a, b) => a.priority - b.priority || new Date(b.since || 0) - new Date(a.since || 0))
  })

  const worst = computed(() => attention.value[0]?.kind || null)

  // The server records the signed-in user as the acknowledger; the body
  // carries nothing else (readJSON refuses unknown fields).
  async function acknowledge(alertId) {
    await escalation.ackAlert(alertId, {})
    await load()
  }

  return { bridges, devices, alerts, checkins, budgets, kits, attention, worst, loadedAt, loaded, failed, load, start, stop, nudge, acknowledge }
})
