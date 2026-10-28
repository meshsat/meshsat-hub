<script setup>
// The official public warnings relayed to this workspace's kits
// (MESHSAT-1734, docs/warning-relay.md): what matched, which kits got it and
// when, and an owner's test injection. The list is the API's, newest match
// first; each kit line carries the time it was last handed the digest.
import { ref, computed, onMounted } from 'vue'
import { warnings, bridges, tenant } from '../api/client'
import { formatUTC } from '../utils/time'
import { ago } from '../utils/paths'
import { useAuthStore } from '../stores/auth'
import { useToastStore } from '../stores/toast'
import EmptyState from '../components/EmptyState.vue'

const auth = useAuthStore()
const toast = useToastStore()

const rows = ref([])
const key = ref(null)
const account = ref(null)
const kits = ref([])
const loading = ref(true)
const error = ref('')
const sending = ref(false)
const testResult = ref(null)

const canTest = computed(() => auth.isOwner || auth.isPlatformAdmin)
const relayOff = computed(() => key.value && key.value.enabled === false)
const optedIn = computed(() => !account.value || /^(true|1|yes|on)$/i.test(account.value.values?.enabled || ''))
const kitLabel = computed(() => Object.fromEntries(kits.value.map((k) => [k.bridge_id, k.label || k.bridge_id])))

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [list, k, accounts, b] = await Promise.all([
      warnings.list(200),
      warnings.key().catch(() => null),
      tenant.integrations().catch(() => []),
      bridges.list().catch(() => []),
    ])
    rows.value = Array.isArray(list) ? list : []
    key.value = k
    account.value = (Array.isArray(accounts) ? accounts : []).find((a) => a.provider === 'warnings') || null
    kits.value = Array.isArray(b) ? b : []
  } catch (e) {
    error.value = e.message || 'Could not load the warnings'
  } finally {
    loading.value = false
  }
}

function parse(json, fallback) {
  try { return json ? JSON.parse(json) : fallback } catch { return fallback }
}

// The areas column: each area's description and its geocodes, as the feed
// named them.
function areas(w) {
  return parse(w.areas, []).map((a) => {
    const codes = (a.geocodes || []).join(', ')
    return [a.desc, codes].filter(Boolean).join(' ')
  }).filter(Boolean)
}

// The kits column: who was handed the digest, on what, when, and whether
// that was a repeat (slot n = the n-th repeat_hours after the warning was
// sent).
function handed(w) {
  const detail = parse(w.detail, {})
  const ids = parse(w.bridges, [])
  return ids.map((id) => {
    const h = detail.handed?.[id]
    return {
      id,
      label: kitLabel.value[id] || id,
      bearers: (h?.bearers || detail.bearers?.[id] || []).join(' + '),
      at: h?.at || '',
      slot: h?.slot || 0,
    }
  })
}

function inForce(w) {
  if (w.msg_type === 'Cancel' || w.superseded_by) return false
  return !!w.expires && new Date(w.expires) > new Date()
}

function kind(w) {
  if (w.msg_type === 'Cancel') return 'Cancelled'
  if (w.msg_type === 'Update') return 'Update'
  return ''
}

const SOURCE_WORDS = { dwd: 'DWD', 'meteoalarm-nl': 'KNMI via MeteoAlarm', test: 'Test' }

function esc(s) {
  return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;')
}

// The first area of the workspace's settings, as a CAP area: a circle line
// becomes <circle>, a geocode line a <geocode> under its name. Without any,
// a 25 km circle around the first kit that has reported a position.
function firstArea() {
  const line = (account.value?.values?.areas || '').split('\n').map((l) => l.trim()).find(Boolean)
  if (line) {
    const circle = line.match(/^(-?[\d.]+)\s*,\s*(-?[\d.]+)\s*,\s*([\d.]+)$/)
    if (circle) return { desc: `Circle ${circle[1]},${circle[2]} ${circle[3]} km`, xml: `<circle>${circle[1]},${circle[2]} ${circle[3]}</circle>` }
    const [name, value] = line.includes(':') ? line.split(':', 2) : ['AREA', line]
    return { desc: line, xml: `<geocode><valueName>${esc(name.trim().toUpperCase())}</valueName><value>${esc(value.trim())}</value></geocode>` }
  }
  const kit = kits.value.find((k) => k.location_lat || k.location_lon)
  if (kit) {
    return { desc: `25 km around ${kit.label || kit.bridge_id}`, xml: `<circle>${kit.location_lat},${kit.location_lon} 25</circle>` }
  }
  return null
}

function testCAP(area) {
  const iso = (d) => d.toISOString().replace(/\.\d{3}Z$/, '+00:00')
  const now = new Date()
  const expires = new Date(now.getTime() + 2 * 3600 * 1000)
  const sender = auth.user?.email || 'console@hub'
  return `<?xml version="1.0" encoding="UTF-8"?>
<alert xmlns="urn:oasis:names:tc:emergency:cap:1.2">
  <identifier>meshsat-test-${now.getTime()}</identifier>
  <sender>${esc(sender)}</sender>
  <sent>${iso(now)}</sent>
  <status>Test</status>
  <msgType>Alert</msgType>
  <scope>Public</scope>
  <info>
    <language>en</language>
    <category>Safety</category>
    <event>Relay test</event>
    <urgency>Immediate</urgency>
    <severity>Minor</severity>
    <certainty>Observed</certainty>
    <expires>${iso(expires)}</expires>
    <senderName>MeshSat Hub</senderName>
    <headline>Relay test from the Hub, no action needed</headline>
    <area><areaDesc>${esc(area.desc)}</areaDesc>${area.xml}</area>
  </info>
</alert>
`
}

async function sendTest() {
  const area = firstArea()
  if (!area) {
    toast.error('Name an area under Integrations, Public warnings, or let a kit report its position, before sending a test.')
    return
  }
  const satellite = /^(true|1|yes|on)$/i.test(account.value?.values?.satellite || '')
  const where = satellite ? ' and over satellite on your own account' : ''
  if (!confirm(`Send a test warning for "${area.desc}" to your kits now? It goes out on their radios marked TEST${where}.`)) return
  sending.value = true
  testResult.value = null
  try {
    testResult.value = await warnings.inject(testCAP(area))
    const kitsHanded = (testResult.value.deliveries || []).reduce((n, d) => n + (d.bridges || []).length, 0)
    if (kitsHanded) toast.success(`Test warning handed to ${kitsHanded} kit${kitsHanded === 1 ? '' : 's'}`)
    else toast.warning('The test matched no kit')
    await load()
  } catch (e) {
    toast.error(e.message || 'The test could not be sent')
  } finally {
    sending.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="ms-page">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Warnings</h1>
        <p class="ms-lede">
          Official public warnings for the areas you named and where your kits are, handed to your kits
          as a signed digest. Each kit puts a warning on its mesh and its APRS bulletin channel, marked
          as a relay. Which areas, feeds and languages: Integrations, Public warnings.
        </p>
      </div>
      <div class="flex gap-2">
        <button v-if="canTest && !relayOff" class="ms-btn-primary" :disabled="sending || loading" @click="sendTest">
          {{ sending ? 'Sending...' : 'Send a test warning' }}
        </button>
        <button class="ms-btn-ghost" :disabled="loading" @click="load">Refresh</button>
      </div>
    </div>

    <div v-if="error" role="alert" class="ms-alert mb-4">{{ error }}</div>

    <div v-if="!loading && !relayOff && !optedIn" class="ms-note mb-4">
      This workspace has not turned the relay on. Turn it on under
      <router-link to="/integrations" class="text-brand-primary">Integrations</router-link>, Public warnings,
      and name the areas or let your kits report their position.
    </div>

    <div v-if="testResult" class="ms-panel mb-5">
      <div class="ms-panel-head">
        <h2 class="ms-h2">Test warning {{ testResult.identifier }}</h2>
        <button class="ms-btn-ghost" @click="testResult = null">Close</button>
      </div>
      <div class="p-4 text-[13px] space-y-2">
        <div v-for="d in testResult.deliveries" :key="d.tenant_id">
          <div v-if="d.error" class="text-ms-text">{{ d.error }}</div>
          <div v-else class="text-ms-text">
            Handed to
            <span v-for="(b, i) in d.bridges" :key="b">{{ i ? ', ' : '' }}<span class="ms-id">{{ kitLabel[b] || b }}</span>
              <span class="text-ms-muted">({{ (d.bearers?.[b] || []).join(' + ') || 'not sent' }})</span></span>;
            digest {{ (d.digest_hex || '').length / 2 }} bytes.
          </div>
          <div v-if="d.why?.length" class="text-ms-muted">Matched: {{ d.why.join('; ') }}</div>
          <div v-if="d.skipped?.length" class="text-ms-muted">Left out: {{ d.skipped.join('; ') }}</div>
        </div>
      </div>
    </div>

    <EmptyState v-if="!loading && relayOff" unavailable title="The warning relay is off on this Hub"
      message="No feed is read and nothing is handed to the kits until the operator turns it on." />

    <div v-else class="ms-panel overflow-x-auto">
      <table class="ms-table">
        <thead>
          <tr>
            <th>Warning</th>
            <th>Severity</th>
            <th>Sent / expires</th>
            <th>Areas</th>
            <th>Kits</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="w in rows" :key="w.source + '/' + w.identifier" :class="{ 'text-ms-muted': !inForce(w) }">
            <td class="align-top py-3 min-w-[16rem]">
              <div class="flex flex-wrap items-center gap-1.5">
                <span class="text-ms-text">{{ w.event || w.headline }}</span>
                <span v-if="kind(w)" class="ms-chip">{{ kind(w) }}</span>
                <span v-if="w.test" class="ms-chip">Test</span>
                <span v-if="inForce(w)" class="ms-chip">In force</span>
              </div>
              <div v-if="w.headline && w.headline !== w.event" class="text-xs text-ms-muted2 max-w-[40ch]">{{ w.headline }}</div>
              <div class="text-xs text-ms-muted">{{ SOURCE_WORDS[w.source] || w.source }}<span v-if="w.sender_name">, {{ w.sender_name }}</span></div>
              <div v-if="w.superseded_by" class="text-xs text-ms-muted">Replaced by a later message</div>
            </td>
            <td class="align-top py-3 whitespace-nowrap">
              <div :class="{ 'text-ms-error': inForce(w) && w.severity === 'Extreme', 'text-ms-warning': inForce(w) && w.severity === 'Severe' }">{{ w.severity || '' }}</div>
              <div class="text-xs text-ms-muted">{{ [w.urgency, w.certainty].filter(Boolean).join(', ') }}</div>
            </td>
            <td class="align-top py-3 whitespace-nowrap">
              <div>{{ formatUTC(w.sent) }}</div>
              <div class="text-xs text-ms-muted">{{ w.expires ? 'until ' + formatUTC(w.expires) : 'no expiry given' }}</div>
            </td>
            <td class="align-top py-3 text-xs">
              <div v-for="a in areas(w)" :key="a" class="max-w-[22ch] truncate" :title="a">{{ a }}</div>
            </td>
            <td class="align-top py-3 text-xs">
              <div v-for="h in handed(w)" :key="h.id">
                <span class="ms-id">{{ h.label }}</span>
                <span v-if="h.at" class="text-ms-muted"> {{ h.bearers || 'not sent' }}, {{ ago(h.at) }}<span v-if="h.slot"> (repeat {{ h.slot }})</span></span>
                <span v-else class="text-ms-muted"> {{ h.bearers || 'not sent' }}</span>
              </div>
            </td>
          </tr>
          <tr v-if="!loading && rows.length === 0">
            <td colspan="5" class="text-center text-ms-muted py-8">
              No warning has matched this workspace yet. A warning is listed here when one of your areas, or a kit's position, lies in it.
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
