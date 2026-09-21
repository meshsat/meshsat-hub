<script setup>
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { bridges, tenant } from '../api/client'
import { formatUptime, formatUTC } from '../utils/time'
import { FAMILIES, familyStates, ago } from '../utils/paths'
import { useAuthStore } from '../stores/auth'
import UpgradeButton from '../components/UpgradeButton.vue'
import PathCell from '../components/PathCell.vue'
import Icon from '../components/Icon.vue'

const loading = ref(true)
const error = ref('')
const bridgeList = ref([])
const expandedBridge = ref(null)
let pollTimer = null

// Add bridge modal
const showAddForm = ref(false)
const addForm = ref({ bridge_id: '', label: '' })
const addError = ref('')

// Edit bridge modal
const showEditModal = ref(false)
const editForm = ref({ label: '', cot_callsign: '' })
const editBridgeId = ref('')

// Delete confirmation modal
const showDeleteConfirm = ref(false)
const bridgeToDelete = ref(null)

// Credential display (one-time secrets)
const credentialResult = ref(null)
const certificateResult = ref(null)
const credentialLoading = ref(false)
const certificateLoading = ref(false)

// Command state
const commandLoading = ref({})
const commandResult = ref({})
// Which leg to send on, per bridge: '' = let the Hub choose (MQTT while the
// bridge is online, otherwise the out-of-band bearer it is paired for).
const commandVia = ref({})

// ACL regeneration
const aclLoading = ref(false)
const aclResult = ref(null)

// Onboarding flow
const onboardingBridgeId = ref(null)
const onboardingStep = ref(0)

// QR Provision modal
const showProvisionQR = ref(false)
const provisionQRUrl = ref('')
const provisionQRBridgeId = ref('')
const provisionLoading = ref(false)
// Whether the QR's credentials work at the broker yet (MESHSAT-1298). A new
// password reaches the NATS members up to a minute after it is generated, and a
// phone that scanned sooner was refused with the right password. So the QR
// stays blurred until every member accepts it.
const provisionState = ref('')      // pending | live | none | expired | unknown
const provisionAccepted = ref(0)
const provisionMembers = ref(0)
const provisionChecked = ref(false)
const provisionWaited = ref(0)
let provisionTimer = null
const PROVISION_POLL_MS = 2500
const PROVISION_GIVE_UP_S = 180

// Clipboard feedback
const copied = ref('')

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

onMounted(async () => {
  // Overview first-run link: open the add form straight away.
  if (route.query.add === '1') showAddForm.value = true
  if (route.query.kit) expandedBridge.value = String(route.query.kit)
  await loadBridges()
  // Wide screens always show a kit; on a phone the list comes first.
  if (!expandedBridge.value && bridgeList.value.length && window.matchMedia('(min-width: 1280px)').matches) {
    expandedBridge.value = bridgeList.value[0].bridge_id
  }
  loadUsage()
  pollTimer = setInterval(loadBridges, 30000)
})

// The selected kit lives in the URL, so a kit can be linked to and the jump
// search can open one.
watch(() => route.query.kit, (k) => { if (k && k !== expandedBridge.value) expandedBridge.value = String(k) })
watch(expandedBridge, (id) => {
  // Only while this page is the one showing: a slow first load that settles
  // after the user has moved on must not drag the URL back here.
  if (route.name !== 'fleet' || (route.query.kit || null) === (id || null)) return
  const query = { ...route.query }
  if (id) query.kit = id; else delete query.kit
  router.replace({ name: 'fleet', query })
})

const selected = computed(() => bridgeList.value.find(b => b.bridge_id === expandedBridge.value) || null)

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
  stopProvisionWatch()
})

async function loadBridges() {
  try {
    const data = await bridges.list()
    bridgeList.value = Array.isArray(data) ? data : []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

const onlineCount = computed(() => bridgeList.value.filter(b => b.online).length)
const totalCount = computed(() => bridgeList.value.length)

// Plan usage (MESHSAT-989). Devices and bridges share one ceiling, so the
// number here counts both. A plan without a ceiling reports -1; an older Hub
// has no endpoint at all, and then the page simply says nothing about plans.
const usage = ref(null)
const atCap = computed(() => usage.value && usage.value.limit !== -1 && usage.value.remaining === 0)

async function loadUsage() {
  try {
    usage.value = await tenant.usage()
  } catch {
    usage.value = null
  }
}

// --- Add Bridge ---
async function addBridge() {
  if (!addForm.value.bridge_id.trim()) {
    addError.value = 'Give the kit an id.'
    return
  }
  addError.value = ''
  try {
    await bridges.create({
      bridge_id: addForm.value.bridge_id.trim(),
      label: addForm.value.label.trim() || addForm.value.bridge_id.trim(),
    })
    const newId = addForm.value.bridge_id.trim()
    addForm.value = { bridge_id: '', label: '' }
    showAddForm.value = false
    await loadBridges()
    // Refresh the ceiling too: a bridge counts against the same allowance as a
    // device, so the one that just filled it has to disable the Add button.
    // Without this the counter and atCap stayed stale until a page reload and
    // the next attempt came back as a server 402. Devices.vue already did this.
    await loadUsage()
    // Start onboarding flow
    onboardingBridgeId.value = newId
    onboardingStep.value = 1
    expandedBridge.value = newId
  } catch (e) {
    addError.value = e.message
  }
}

// --- Edit Bridge ---
function openEdit(b) {
  editBridgeId.value = b.bridge_id
  editForm.value = {
    label: b.label || '',
    cot_callsign: b.cot_callsign || '',
  }
  showEditModal.value = true
}

async function saveEdit() {
  try {
    const updates = {}
    if (editForm.value.label !== undefined) updates.label = editForm.value.label
    if (editForm.value.cot_callsign !== undefined) updates.cot_callsign = editForm.value.cot_callsign
    await bridges.update(editBridgeId.value, updates)
    showEditModal.value = false
    await loadBridges()
  } catch (e) {
    error.value = e.message
  }
}

// --- Delete Bridge ---
function confirmDelete(b) {
  bridgeToDelete.value = b
  showDeleteConfirm.value = true
}

async function deleteBridge() {
  if (!bridgeToDelete.value) return
  const id = bridgeToDelete.value.bridge_id
  showDeleteConfirm.value = false
  bridgeToDelete.value = null
  try {
    await bridges.delete(id)
    if (expandedBridge.value === id) expandedBridge.value = null
    await loadBridges()
  } catch (e) {
    error.value = e.message
  }
}

// --- Credentials ---
async function generateCredentials(bridgeId) {
  credentialLoading.value = true
  credentialResult.value = null
  try {
    const bridge = bridgeList.value.find(b => b.bridge_id === bridgeId)
    const result = bridge && hasCredentials(bridge)
      ? await bridges.rotateCredentials(bridgeId)
      : await bridges.generateCredentials(bridgeId)
    credentialResult.value = result
    if (onboardingBridgeId.value === bridgeId && onboardingStep.value === 1) {
      onboardingStep.value = 2
    }
  } catch (e) {
    error.value = e.message
  } finally {
    credentialLoading.value = false
  }
}

async function issueCertificate(bridgeId) {
  certificateLoading.value = true
  certificateResult.value = null
  try {
    const result = await bridges.issueCertificate(bridgeId)
    certificateResult.value = result
    if (onboardingBridgeId.value === bridgeId && onboardingStep.value === 2) {
      onboardingStep.value = 3
    }
  } catch (e) {
    error.value = e.message
  } finally {
    certificateLoading.value = false
  }
}

function dismissCredentials() {
  credentialResult.value = null
}

function dismissCertificate() {
  certificateResult.value = null
}

// --- QR Provisioning ---
async function provisionWithQR(bridgeId) {
  provisionLoading.value = true
  provisionQRBridgeId.value = bridgeId
  error.value = ''
  try {
    const blob = await bridges.provisionQR(bridgeId, 512)
    if (!blob || blob.size === 0) {
      throw new Error('Empty response from server')
    }
    provisionQRUrl.value = URL.createObjectURL(blob)
    showProvisionQR.value = true
    watchProvisionLive(bridgeId)
  } catch (e) {
    error.value = 'QR provisioning failed: ' + (e.message || e.toString() || 'Unknown error')
    console.error('QR provisioning error:', e)
  } finally {
    provisionLoading.value = false
  }
}

function stopProvisionWatch() {
  if (provisionTimer) {
    clearTimeout(provisionTimer)
    provisionTimer = null
  }
}

function watchProvisionLive(bridgeId) {
  stopProvisionWatch()
  provisionState.value = 'pending'
  provisionAccepted.value = 0
  provisionMembers.value = 0
  provisionChecked.value = false
  const started = Date.now()
  const tick = async () => {
    provisionWaited.value = Math.round((Date.now() - started) / 1000)
    try {
      const s = await bridges.provisionStatus(bridgeId)
      provisionState.value = s.state
      provisionAccepted.value = s.accepted || 0
      provisionMembers.value = s.members || 0
      provisionChecked.value = !!s.checked
    } catch (e) {
      // The status call is a convenience; if it fails, show the QR rather
      // than hold it back on a check that cannot answer.
      provisionState.value = 'unknown'
    }
    if (provisionState.value !== 'pending' || !showProvisionQR.value) return
    if (provisionWaited.value >= PROVISION_GIVE_UP_S) {
      provisionState.value = 'unknown'
      return
    }
    provisionTimer = setTimeout(tick, PROVISION_POLL_MS)
  }
  tick()
}

function dismissProvisionQR() {
  stopProvisionWatch()
  showProvisionQR.value = false
  if (provisionQRUrl.value) {
    URL.revokeObjectURL(provisionQRUrl.value)
    provisionQRUrl.value = ''
  }
  provisionQRBridgeId.value = ''
  // Reload to show updated credentials
  loadBridges()
}

// --- Commands ---
// True when this bridge's command buttons will go over MQTT: either MQTT was
// picked explicitly, or the leg is on Auto and the bridge is online. Anything
// else is an out-of-band bearer, which carries the mgmt_* commands instead.
function isMqttLeg(b) {
  const via = commandVia.value[b.bridge_id] || ''
  return via === 'mqtt' || (via === '' && b.online)
}

// The journal units a kit will return a log for. Mirrors oob.LogUnits in the
// Hub, which mirrors the bridge's own allowlist; the kit refuses anything else.
const LOG_UNITS = [
  'docker', 'meshsat-oob-agent', 'netplan-wpa-wlan0', 'systemd-networkd',
  'x1202-monitor', 'meshsat-mgmt-keepalive', 'meshsat-p2p-link', 'bluetooth',
]
const logUnit = ref({})

const BEARER_LABELS = { mqtt: 'Internet', sms: 'SMS', imt: 'Satellite IMT', sbd: 'Satellite SBD' }
function bearerLabel(bearer) {
  return BEARER_LABELS[bearer] || bearer
}

// How long the page keeps asking for an out-of-band answer: the Hub waits up to
// ten minutes for a satellite pass, so a little longer than that.
const OOB_POLL_MS = 3000
const OOB_GIVE_UP_MS = 11 * 60 * 1000

function newRequestId() {
  return (crypto.randomUUID ? crypto.randomUUID() : String(Date.now()) + Math.random().toString(16).slice(2))
}

// An out-of-band command runs on the Hub as a job: the POST answers at once and
// the page polls. A request held open for a minute does not survive the path to
// the Hub, and what is in front of it re-sends a POST it thinks went unanswered.
async function sendCommand(bridgeId, cmd, payload, outOfBand = false) {
  commandLoading.value = { ...commandLoading.value, [bridgeId + cmd]: true }
  commandResult.value = { ...commandResult.value, [bridgeId]: null }
  try {
    const via = commandVia.value[bridgeId] || ''
    const body = { cmd }
    if (via) body.via = via
    if (payload) body.payload = payload
    if (outOfBand) {
      body.async = true
      body.request_id = newRequestId()
    }
    let result = await bridges.sendCommand(bridgeId, body)
    if (result && result.status === 'pending' && result.request_id) {
      const started = Date.now()
      const requestId = result.request_id
      for (;;) {
        const waited = Date.now() - started
        commandResult.value = { ...commandResult.value, [bridgeId]: { waiting: Math.round(waited / 1000) } }
        if (waited > OOB_GIVE_UP_MS) throw new Error('No answer from the bridge yet. The command was sent; it may still arrive.')
        await new Promise(resolve => setTimeout(resolve, OOB_POLL_MS))
        const job = await bridges.getCommand(bridgeId, requestId)
        if (job.state === 'done') { result = job.response; break }
        if (job.state === 'failed') throw new Error(job.error || 'The command failed')
      }
    }
    commandResult.value = { ...commandResult.value, [bridgeId]: result }
  } catch (e) {
    commandResult.value = { ...commandResult.value, [bridgeId]: { error: e.message } }
  } finally {
    commandLoading.value = { ...commandLoading.value, [bridgeId + cmd]: false }
  }
}

// --- ACL ---
async function regenerateACL() {
  aclLoading.value = true
  aclResult.value = null
  try {
    const result = await bridges.regenerateACL()
    aclResult.value = result
    setTimeout(() => { aclResult.value = null }, 5000)
  } catch (e) {
    error.value = e.message
  } finally {
    aclLoading.value = false
  }
}

// --- Onboarding ---
function dismissOnboarding() {
  onboardingBridgeId.value = null
  onboardingStep.value = 0
}

// --- Helpers ---
function selectKit(bridgeId) {
  if (expandedBridge.value === bridgeId) return
  credentialResult.value = null
  certificateResult.value = null
  expandedBridge.value = bridgeId
}

function confirmReboot(b) {
  if (!confirm(`Reboot ${kitName(b)}? It drops off for a minute or two.`)) return
  sendCommand(b.bridge_id, 'reboot')
}

// CPU, memory and disk as meters that take colour only past a threshold.
// Android keeps its memory nearly full by design, so a phone's memory figure
// is shown but never raised as a caution.
function systemMeters(b) {
  const h = parseHealth(b) || {}
  const level = (v) => (v > 90 ? 'alarm' : v > 80 ? 'caution' : 'normal')
  const phone = (b.mode || '') === 'android'
  return [
    { label: 'CPU', value: h.cpu_pct, level: level(h.cpu_pct) },
    { label: 'Memory', value: h.mem_pct, level: phone ? 'normal' : level(h.mem_pct) },
    { label: 'Disk', value: h.disk_pct, level: level(h.disk_pct) },
  ]
}

function kitName(b) {
  return b.cot_callsign || b.label || b.hostname || b.bridge_id
}

// Health carries the live interface states; the birth frame is the fallback
// for a kit that has announced itself but not yet reported.
function kitInterfaces(b) {
  const h = parseHealth(b)
  if (h?.interfaces?.length) return h.interfaces
  return parseBirth(b)?.interfaces || []
}

function kitStates(b) {
  const st = familyStates(kitInterfaces(b))
  if (b.online) return st
  // An offline kit's last report is history: draw what it has, none working.
  return Object.fromEntries(Object.entries(st).map(([k, v]) => [k, v === 'absent' ? 'absent' : 'down']))
}

function ifaceState(status) {
  const s = String(status || '').toLowerCase()
  if (s === 'online') return 'up'
  if (/bind|connect|start|init|search/.test(s)) return 'coming'
  return 'down'
}

const STATUS_WORDS = { online: 'Working', offline: 'Not working', binding: 'Coming up', error: 'Error', disabled: 'Turned off' }
function statusWords(s) { return STATUS_WORDS[s] || s || 'Unknown' }

function linkWords(b) {
  if (b.online) return 'Live'
  if (b.last_report_at) return `${bearerLabel(b.last_report_bearer || '?')} ${ago(b.last_report_at)}`
  return b.last_seen && !String(b.last_seen).startsWith('0001-') ? `Seen ${ago(b.last_seen)}` : 'Never connected'
}

function toggleExpand(bridgeId) {
  expandedBridge.value = expandedBridge.value === bridgeId ? null : bridgeId
  // Clear per-bridge state when collapsing
  if (expandedBridge.value !== bridgeId) {
    credentialResult.value = null
    certificateResult.value = null
  }
}

async function copyToClipboard(text, label) {
  try {
    await navigator.clipboard.writeText(text)
    copied.value = label
    setTimeout(() => { copied.value = '' }, 2000)
  } catch {
    // Fallback for non-HTTPS contexts
    const ta = document.createElement('textarea')
    ta.value = text
    document.body.appendChild(ta)
    ta.select()
    document.execCommand('copy')
    document.body.removeChild(ta)
    copied.value = label
    setTimeout(() => { copied.value = '' }, 2000)
  }
}

function downloadFile(content, filename) {
  const blob = new Blob([content], { type: 'application/x-pem-file' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}

function parseBirth(b) {
  if (!b.last_birth) return null
  try { return JSON.parse(b.last_birth) } catch { return null }
}

function parseHealth(b) {
  if (!b.last_health) return null
  try { return JSON.parse(b.last_health) } catch { return null }
}

function interfaceStatusDot(status) {
  if (status === 'online') return 'bg-ms-success'
  if (status === 'error') return 'bg-ms-error'
  if (status === 'binding') return 'bg-ms-warning'
  return 'bg-gray-500'
}

function interfaceTypeBadgeColor(type) {
  if (type === 'meshtastic') return 'text-transport-mesh border-transport-mesh/30'
  if (type === 'iridium_sbd' || type === 'iridium_imt') return 'text-transport-iridium border-transport-iridium/30'
  if (type === 'cellular') return 'text-transport-cellular border-transport-cellular/30'
  return 'text-gray-400 border-gray-600'
}

function interfaceTypeLabel(type) {
  const labels = {
    meshtastic: 'LoRa mesh',
    iridium_sbd: 'Iridium SBD',
    iridium_imt: 'Iridium IMT',
    cellular: 'Cellular and SMS',
    zigbee: 'ZigBee',
    aprs: 'APRS radio',
    tcp: 'TCP',
  }
  return labels[type] || type
}

function signalDisplay(iface) {
  if (iface.signal_bars > 0) return `${iface.signal_bars}/5`
  if (iface.signal_dbm && iface.signal_dbm !== 0) return `${iface.signal_dbm} dBm`
  return '—'
}

function birthInterfaceType(b, name) {
  const birth = parseBirth(b)
  if (!birth?.interfaces) return ''
  const match = birth.interfaces.find(i => i.name === name)
  return match?.type || ''
}

function messageDisplay(iface) {
  const parts = []
  if (iface.mo_count > 0) parts.push(`MO: ${iface.mo_count}`)
  if (iface.mt_count > 0) parts.push(`MT: ${iface.mt_count}`)
  if (iface.nodes_seen > 0) parts.push(`${iface.nodes_seen} nodes`)
  return parts.join(', ') || '—'
}

function hasCredentials(b) {
  return !!b.mqtt_username
}

function hasCertificate(b) {
  return !!b.cert_pem
}

function certExpiryStatus(b) {
  if (!b.cert_expiry) return null
  const exp = new Date(b.cert_expiry)
  const now = new Date()
  const days = Math.floor((exp - now) / 86400000)
  if (days < 0) return { label: 'Expired', color: 'text-ms-error' }
  if (days < 14) return { label: `Expires in ${days} days`, color: 'text-ms-warning' }
  return { label: `Valid ${days} more days`, color: 'text-ms-text' }
}
</script>

<template>
  <div class="ms-page">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Kits</h1>
        <p class="ms-lede">
          <template v-if="loading">Loading kits.</template>
          <template v-else-if="!bridgeList.length">A kit is a MeshSat gateway: a Pi in a case, or a phone running the Android app.</template>
          <template v-else>
            {{ totalCount }} kit{{ totalCount !== 1 ? 's' : '' }}, {{ onlineCount === totalCount ? (totalCount === 1 ? 'connected' : 'all connected') : `${onlineCount} connected` }}.<template
              v-if="usage && usage.limit !== -1"> {{ usage.used }} of {{ usage.limit }} devices and kits used on the {{ usage.plan }} plan.</template>
          </template>
        </p>
      </div>
      <div class="flex items-center gap-2">
        <button v-if="auth.isOwner" class="ms-btn-ghost text-xs" :disabled="aclLoading" :title="'Rewrite the broker\'s user list from the kits registered here. Only needed after a broker restore.'"
          @click="regenerateACL">{{ aclLoading ? 'Rewriting' : 'Rewrite broker users' }}</button>
        <span v-if="aclResult" class="text-xs text-ms-muted">{{ aclResult.bridges_configured }} kits written</span>
        <button v-if="!showAddForm" class="ms-btn-primary" :disabled="atCap" data-testid="add-kit"
          :title="atCap ? `The ${usage.plan} plan covers ${usage.limit} devices and kits together. Your kits keep working; upgrade to add more.` : ''"
          @click="showAddForm = true">
          <Icon name="plus" :size="15" />Add kit
        </button>
        <UpgradeButton v-if="atCap" :usage="usage" />
      </div>
    </div>

    <div v-if="error" role="alert" class="mb-4 flex items-start justify-between gap-4 rounded-lg border border-ms-error/50 bg-ms-error/10 px-4 py-3 text-[13px] text-ms-text">
      <span>{{ error }}</span>
      <button class="text-xs text-ms-muted hover:text-ms-text" @click="error = ''">Dismiss</button>
    </div>

    <!-- Add a kit -->
    <section v-if="showAddForm" class="ms-panel p-5 mb-5" aria-labelledby="add-h">
      <h2 id="add-h" class="ms-h2">Add a kit</h2>
      <p class="text-[13px] text-ms-muted mt-1 max-w-[64ch]">Give it a short id; it becomes the kit's name on the broker and cannot be changed later. Next you show a setup QR code, and the kit or the Android app scans it to connect itself.</p>
      <div v-if="addError" class="mt-3 rounded-md border border-ms-error/50 bg-ms-error/10 px-3 py-2 text-xs text-ms-text">{{ addError }}</div>
      <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 mt-4 max-w-2xl">
        <label class="block">
          <span class="ms-label">Kit id</span>
          <input v-model="addForm.bridge_id" placeholder="e.g. field-kit-01" class="ms-input w-full mt-1 font-mono" @keydown.enter="addBridge" />
        </label>
        <label class="block">
          <span class="ms-label">Name (optional)</span>
          <input v-model="addForm.label" placeholder="What your team calls it" class="ms-input w-full mt-1" @keydown.enter="addBridge" />
        </label>
      </div>
      <div class="flex gap-2 mt-4">
        <button class="ms-btn-primary" @click="addBridge">Create kit</button>
        <button class="ms-btn" @click="showAddForm = false">Cancel</button>
      </div>
    </section>

    <div v-if="loading" class="text-sm text-ms-muted py-10">Loading kits.</div>

    <div v-else-if="!bridgeList.length" class="ms-panel px-6 py-12 text-center">
      <Icon name="kits" :size="28" class="mx-auto text-ms-muted" />
      <h2 class="ms-h2 mt-3">No kits yet</h2>
      <p class="text-[13px] text-ms-muted mt-1 max-w-[52ch] mx-auto">Add a kit, show its setup QR code, and scan it with the kit or the MeshSat Android app. It appears here the moment it connects.</p>
      <button class="ms-btn-primary mt-4" @click="showAddForm = true">Add your first kit</button>
    </div>

    <div v-else class="xl:grid xl:grid-cols-[minmax(0,5fr)_minmax(0,7fr)] xl:gap-5 items-start">
      <!-- Kit list -->
      <section class="ms-panel overflow-hidden" :class="selected ? 'hidden xl:block' : ''" aria-label="Kits">
        <ul role="listbox" aria-label="Kits" class="divide-y divide-ms-border">
          <li v-for="b in bridgeList" :key="b.bridge_id" role="option" :aria-selected="expandedBridge === b.bridge_id" tabindex="0"
            class="relative px-4 py-3 cursor-pointer transition-colors"
            :class="expandedBridge === b.bridge_id ? 'bg-ms-well' : 'hover:bg-ms-well/50'"
            @click="selectKit(b.bridge_id)" @keydown.enter="selectKit(b.bridge_id)">
            <span v-if="expandedBridge === b.bridge_id" class="absolute left-0 top-2 bottom-2 w-0.5 rounded-full bg-ms-primary" aria-hidden="true" />
            <div class="flex items-center gap-2.5 min-w-0">
              <span class="w-2 h-2 rounded-full shrink-0" :class="b.online ? 'bg-ms-success' : 'border border-ms-warning'" :title="b.online ? 'Connected' : 'Offline'" />
              <span class="text-[13px] font-medium text-ms-text truncate">{{ kitName(b) }}</span>
              <span class="ml-auto text-xs whitespace-nowrap" :class="b.online ? 'text-ms-muted' : 'text-ms-warning'">{{ linkWords(b) }}</span>
            </div>
            <div class="flex items-center gap-2 mt-1.5 pl-[18px] min-w-0">
              <span class="ms-id text-ms-muted truncate">{{ b.bridge_id }}</span>
              <span class="ml-auto flex items-center -mr-1">
                <PathCell v-for="f in FAMILIES" :key="f.key" :state="kitStates(b)[f.key]" :label="f.long" />
              </span>
            </div>
          </li>
        </ul>
      </section>

      <!-- Selected kit -->
      <section v-if="selected" class="ms-panel overflow-hidden mt-5 xl:mt-0" aria-labelledby="kit-h" data-testid="kit-detail">
        <div class="px-5 pt-4 pb-4 border-b border-ms-border">
          <button class="xl:hidden ms-btn-ghost -ml-2 mb-2 text-xs" @click="expandedBridge = null"><Icon name="collapse" :size="14" />All kits</button>
          <div class="flex flex-wrap items-start gap-3">
            <div class="min-w-0 flex-1">
              <h2 id="kit-h" class="text-lg font-semibold text-ms-text truncate">{{ kitName(selected) }}</h2>
              <div class="flex flex-wrap items-center gap-x-3 gap-y-1 mt-0.5 text-[13px]">
                <span class="ms-id text-ms-muted">{{ selected.bridge_id }}</span>
                <span :class="selected.online ? 'text-ms-text2' : 'text-ms-warning'">{{ selected.online ? 'Connected to the Hub' : `Offline, ${linkWords(selected).toLowerCase()}` }}</span>
              </div>
            </div>
            <div class="flex gap-2">
              <button class="ms-btn" @click="openEdit(selected)">Edit</button>
              <button class="ms-btn hover:!border-ms-error hover:!text-ms-error" @click="confirmDelete(selected)">Delete</button>
            </div>
          </div>
        </div>

        <!-- Paths -->
        <div class="px-5 py-4 border-b border-ms-border">
          <h3 class="ms-h2">Paths</h3>
          <div v-if="kitInterfaces(selected).length" class="mt-2 -mx-5 overflow-x-auto">
            <table class="ms-table">
              <thead><tr><th>Interface</th><th>State</th><th class="hidden sm:table-cell">Signal</th><th class="hidden sm:table-cell">Traffic</th></tr></thead>
              <tbody>
                <tr v-for="iface in kitInterfaces(selected)" :key="iface.name">
                  <td>
                    <div class="text-[13px] text-ms-text">{{ interfaceTypeLabel(iface.type || birthInterfaceType(selected, iface.name) || iface.name) }}</div>
                    <div class="ms-id text-ms-muted">{{ iface.name }}</div>
                  </td>
                  <td>
                    <span class="inline-flex items-center gap-2">
                      <PathCell :state="ifaceState(iface.status)" :label="iface.name" />
                      <span class="text-[13px]" :class="iface.status === 'error' ? 'text-ms-error' : iface.status === 'online' ? 'text-ms-text' : 'text-ms-muted'">{{ statusWords(iface.status) }}</span>
                    </span>
                  </td>
                  <td class="hidden sm:table-cell text-ms-text2">{{ signalDisplay(iface) }}</td>
                  <td class="hidden sm:table-cell text-ms-muted">{{ messageDisplay(iface) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else class="text-[13px] text-ms-muted mt-1">The kit has not reported its interfaces yet. They appear after it first connects.</p>
        </div>

        <!-- Commands. Present whether or not the kit is on MQTT: a kit that has
             lost its internet is exactly the one worth commanding, over a
             sealed out-of-band frame (MESHSAT-964). -->
        <div class="px-5 py-4 border-b border-ms-border">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <h3 class="ms-h2">Commands</h3>
            <label class="flex items-center gap-2 text-xs text-ms-muted">
              Send over
              <select :value="commandVia[selected.bridge_id] || ''" class="ms-input h-7 text-xs"
                @change="commandVia[selected.bridge_id] = $event.target.value">
                <option value="">Best available</option>
                <option value="mqtt">Internet (MQTT)</option>
                <option value="sms">SMS</option>
                <option value="imt">Satellite (IMT)</option>
                <option value="sbd">Satellite (SBD)</option>
              </select>
            </label>
          </div>
          <p v-if="!isMqttLeg(selected)" class="text-xs text-ms-muted mt-2 max-w-[64ch]">
            Out of band: the command travels as a sealed frame and the kit answers on the same bearer. About a minute over SMS, several over satellite. It expires if the kit cannot act on it in time.
          </p>
          <div class="flex flex-wrap gap-2 mt-3">
            <template v-if="isMqttLeg(selected)">
              <button class="ms-btn" :disabled="commandLoading[selected.bridge_id + 'ping']" @click="sendCommand(selected.bridge_id, 'ping')">
                {{ commandLoading[selected.bridge_id + 'ping'] ? 'Pinging' : 'Ping' }}</button>
              <button class="ms-btn" :disabled="commandLoading[selected.bridge_id + 'flush_burst']" @click="sendCommand(selected.bridge_id, 'flush_burst')">
                {{ commandLoading[selected.bridge_id + 'flush_burst'] ? 'Flushing' : 'Flush burst queue' }}</button>
              <button class="ms-btn text-ms-warning" :disabled="commandLoading[selected.bridge_id + 'reboot']" @click="confirmReboot(selected)">
                {{ commandLoading[selected.bridge_id + 'reboot'] ? 'Rebooting' : 'Reboot' }}</button>
            </template>
            <template v-else>
              <button class="ms-btn" :disabled="commandLoading[selected.bridge_id + 'mgmt_ping']" @click="sendCommand(selected.bridge_id, 'mgmt_ping', null, true)">
                {{ commandLoading[selected.bridge_id + 'mgmt_ping'] ? 'Pinging' : 'Ping' }}</button>
              <button class="ms-btn" :disabled="commandLoading[selected.bridge_id + 'mgmt_status']" @click="sendCommand(selected.bridge_id, 'mgmt_status', null, true)">
                {{ commandLoading[selected.bridge_id + 'mgmt_status'] ? 'Asking' : 'Status' }}</button>
              <!-- mgmt_log needs a journal unit: without one the Hub refuses it. -->
              <span class="inline-flex">
                <select :value="logUnit[selected.bridge_id] || LOG_UNITS[0]" aria-label="Log unit" class="ms-input rounded-r-none border-r-0"
                  @change="logUnit[selected.bridge_id] = $event.target.value">
                  <option v-for="u in LOG_UNITS" :key="u" :value="u">{{ u }}</option>
                </select>
                <button class="ms-btn rounded-l-none" :disabled="commandLoading[selected.bridge_id + 'mgmt_log']"
                  @click="sendCommand(selected.bridge_id, 'mgmt_log', { unit: logUnit[selected.bridge_id] || LOG_UNITS[0] }, true)">
                  {{ commandLoading[selected.bridge_id + 'mgmt_log'] ? 'Fetching' : 'Log' }}</button>
              </span>
            </template>
          </div>
          <div v-if="commandResult[selected.bridge_id]" class="mt-3 text-[13px]" aria-live="polite">
            <div v-if="commandResult[selected.bridge_id].error" class="text-ms-error">{{ commandResult[selected.bridge_id].error }}</div>
            <div v-else-if="commandResult[selected.bridge_id].waiting !== undefined" class="text-ms-muted">
              Sent. Waiting for the kit to answer, {{ commandResult[selected.bridge_id].waiting }} s.
            </div>
            <div v-else class="text-ms-text">
              {{ commandResult[selected.bridge_id].status === 'ok' ? 'Answered' : commandResult[selected.bridge_id].status }}<span
                v-if="commandResult[selected.bridge_id].bearer" class="text-ms-muted"> over {{ bearerLabel(commandResult[selected.bridge_id].bearer) }}</span><span
                class="text-ms-muted"> in {{ (commandResult[selected.bridge_id].latency_ms / 1000).toFixed(1) }} s</span>
            </div>
            <pre v-if="commandResult[selected.bridge_id].result"
              class="mt-2 p-3 rounded-md bg-ms-bg border border-ms-border text-xs text-ms-text2 font-mono overflow-x-auto whitespace-pre-wrap">{{ commandResult[selected.bridge_id].result }}</pre>
          </div>
        </div>

        <!-- Connection -->
        <div class="px-5 py-4 border-b border-ms-border">
          <h3 class="ms-h2">Connection</h3>
          <dl class="grid grid-cols-2 sm:grid-cols-3 gap-x-6 gap-y-3 mt-3 text-[13px]">
            <div><dt class="ms-label">Broker login</dt><dd class="mt-0.5" :class="hasCredentials(selected) ? 'text-ms-text' : 'text-ms-muted'">{{ hasCredentials(selected) ? 'Issued' : 'Not issued yet' }}</dd></div>
            <div><dt class="ms-label">Client certificate</dt>
              <dd class="mt-0.5" :class="hasCertificate(selected) ? certExpiryStatus(selected)?.color : 'text-ms-muted'">{{ hasCertificate(selected) ? certExpiryStatus(selected)?.label : 'Not issued yet' }}</dd></div>
            <div><dt class="ms-label">Last report</dt><dd class="mt-0.5 text-ms-text">{{ selected.last_report_at ? `${bearerLabel(selected.last_report_bearer || '?')}, ${ago(selected.last_report_at)}` : 'None yet' }}</dd></div>
          </dl>
          <div class="flex flex-wrap gap-2 mt-4">
            <button class="ms-btn-primary" :disabled="provisionLoading" @click="provisionWithQR(selected.bridge_id)"
              title="A single-use QR code carrying the broker login and certificate. The kit or the Android app scans it and connects.">
              {{ provisionLoading ? 'Preparing' : 'Show setup QR' }}</button>
            <button class="ms-btn" :disabled="credentialLoading" @click="generateCredentials(selected.bridge_id)">
              {{ credentialLoading ? 'Issuing' : hasCredentials(selected) ? 'Rotate broker password' : 'Issue broker login' }}</button>
            <button class="ms-btn" :disabled="certificateLoading" @click="issueCertificate(selected.bridge_id)">
              {{ certificateLoading ? 'Issuing' : hasCertificate(selected) ? 'Reissue certificate' : 'Issue certificate' }}</button>
          </div>
          <p v-if="onboardingBridgeId === selected.bridge_id && onboardingStep > 0 && !hasCredentials(selected)" class="text-xs text-ms-muted mt-3 max-w-[64ch]">
            New kit: press <span class="text-ms-text">Show setup QR</span> and scan it with the kit or the Android app. For a kit you set up by hand, issue the login and the certificate instead.
          </p>

          <!-- One-time broker credentials -->
          <div v-if="credentialResult && credentialResult.bridge_id === selected.bridge_id" class="mt-4 rounded-lg border border-ms-warning/50 bg-ms-warning/5 p-4">
            <div class="flex items-center justify-between gap-3 mb-3">
              <span class="text-[13px] font-medium text-ms-text">Copy these now. The password is shown only once.</span>
              <button class="text-xs text-ms-muted hover:text-ms-text" @click="dismissCredentials">Dismiss</button>
            </div>
            <div class="space-y-2">
              <div v-for="row in [['URL', credentialResult.mqtt_url, 'url'], ['User', credentialResult.username, 'user'], ['Password', credentialResult.password, 'pass']]" :key="row[2]"
                class="flex items-center gap-2">
                <span class="ms-label w-16 shrink-0">{{ row[0] }}</span>
                <code class="flex-1 min-w-0 truncate rounded-md bg-ms-bg border border-ms-border px-2 py-1 text-xs font-mono text-ms-text">{{ row[1] }}</code>
                <button class="ms-btn h-7 text-xs" @click="copyToClipboard(row[1], row[2])">{{ copied === row[2] ? 'Copied' : 'Copy' }}</button>
              </div>
            </div>
          </div>

          <!-- One-time certificate -->
          <div v-if="certificateResult && certificateResult.bridge_id === selected.bridge_id" class="mt-4 rounded-lg border border-ms-warning/50 bg-ms-warning/5 p-4">
            <div class="flex items-center justify-between gap-3 mb-1">
              <span class="text-[13px] font-medium text-ms-text">Download the private key now. It is shown only once.</span>
              <button class="text-xs text-ms-muted hover:text-ms-text" @click="dismissCertificate">Dismiss</button>
            </div>
            <p class="text-xs text-ms-muted mb-3">Valid until {{ formatUTC(certificateResult.expires) }}</p>
            <div class="flex flex-wrap gap-2">
              <button class="ms-btn h-7 text-xs" @click="downloadFile(certificateResult.cert_pem, selected.bridge_id + '.crt')">Certificate (.crt)</button>
              <button class="ms-btn h-7 text-xs" @click="downloadFile(certificateResult.key_pem, selected.bridge_id + '.key')">Private key (.key)</button>
              <button class="ms-btn h-7 text-xs" @click="downloadFile(certificateResult.ca_pem, 'meshsat-hub-ca.crt')">Hub CA (.crt)</button>
              <button class="ms-btn-ghost h-7 text-xs" @click="copyToClipboard(certificateResult.cert_pem + '\n' + certificateResult.key_pem, 'cert')">{{ copied === 'cert' ? 'Copied' : 'Copy both' }}</button>
            </div>
          </div>
        </div>

        <!-- System -->
        <div v-if="parseHealth(selected)" class="px-5 py-4 border-b border-ms-border">
          <div class="flex items-baseline justify-between">
            <h3 class="ms-h2">System</h3>
            <span class="text-xs text-ms-muted">Up {{ formatUptime(parseHealth(selected).uptime_sec) }}</span>
          </div>
          <div class="grid grid-cols-3 gap-5 mt-3">
            <div v-for="m in systemMeters(selected)" :key="m.label">
              <div class="flex items-baseline justify-between text-xs">
                <span class="text-ms-muted">{{ m.label }}</span>
                <span class="ms-num" :class="m.level === 'alarm' ? 'text-ms-error' : m.level === 'caution' ? 'text-ms-warning' : 'text-ms-text2'">{{ Math.round(m.value || 0) }}%</span>
              </div>
              <div class="h-1 rounded-full bg-ms-border mt-1.5 overflow-hidden">
                <div class="h-full rounded-full" :class="m.level === 'alarm' ? 'bg-ms-error' : m.level === 'caution' ? 'bg-ms-warning' : 'bg-ms-text2/60'" :style="{ width: Math.min(m.value || 0, 100) + '%' }" />
              </div>
            </div>
          </div>
          <dl class="grid grid-cols-2 sm:grid-cols-3 gap-x-6 gap-y-3 mt-4 text-[13px]">
            <div v-if="parseHealth(selected).battery_pct !== undefined"><dt class="ms-label">Battery</dt><dd class="mt-0.5 ms-num" :class="parseHealth(selected).battery_pct < 15 ? 'text-ms-warning' : 'text-ms-text'">{{ Math.round(parseHealth(selected).battery_pct) }}%</dd></div>
            <div v-if="parseHealth(selected).burst_queue"><dt class="ms-label">Burst queue</dt><dd class="mt-0.5 ms-num text-ms-text">{{ parseHealth(selected).burst_queue.pending }} waiting</dd></div>
            <div v-if="parseHealth(selected).reticulum"><dt class="ms-label">Reticulum</dt><dd class="mt-0.5 ms-num text-ms-text">{{ parseHealth(selected).reticulum.routes }} routes, {{ parseHealth(selected).reticulum.links }} links</dd></div>
            <div v-if="parseHealth(selected).hemb"><dt class="ms-label">Bonding</dt><dd class="mt-0.5 ms-num text-ms-text">{{ parseHealth(selected).hemb.active_bond_groups }} groups, {{ parseHealth(selected).hemb.generations_decoded }} decoded, {{ parseHealth(selected).hemb.generations_failed }} failed</dd></div>
          </dl>
        </div>

        <!-- Details -->
        <div class="px-5 py-4">
          <h3 class="ms-h2">Details</h3>
          <dl class="grid grid-cols-2 sm:grid-cols-3 gap-x-6 gap-y-3 mt-3 text-[13px]">
            <div><dt class="ms-label">Software</dt><dd class="mt-0.5 text-ms-text">{{ selected.version || 'Unknown' }}<span v-if="selected.mode" class="text-ms-muted">, {{ selected.mode }}</span></dd></div>
            <div><dt class="ms-label">Host</dt><dd class="mt-0.5 text-ms-text truncate">{{ selected.hostname || 'Unknown' }}</dd></div>
            <div v-if="selected.cot_callsign || selected.cot_type"><dt class="ms-label">TAK callsign</dt><dd class="mt-0.5 text-ms-text truncate">{{ selected.cot_callsign || 'None' }} <span v-if="selected.cot_type" class="ms-id text-ms-muted">{{ selected.cot_type }}</span></dd></div>
            <div v-if="selected.location_lat && selected.location_lon"><dt class="ms-label">Position</dt>
              <dd class="mt-0.5"><router-link :to="{ name: 'map', query: { focus: selected.bridge_id } }" class="ms-id text-ms-text hover:text-ms-primary">{{ selected.location_lat.toFixed(5) }}, {{ selected.location_lon.toFixed(5) }}</router-link></dd></div>
            <div><dt class="ms-label">Added</dt><dd class="mt-0.5 text-ms-text">{{ formatUTC(selected.created_at) }}</dd></div>
          </dl>
          <p v-if="!parseHealth(selected)" class="text-[13px] text-ms-muted mt-4">No health report yet: the kit has not connected.</p>
        </div>
      </section>
    </div>

    <!-- Edit -->
    <div v-if="showEditModal" class="fixed inset-0 z-50 flex items-center justify-center p-4" role="dialog" aria-modal="true" aria-labelledby="edit-h" @click.self="showEditModal = false">
      <div class="absolute inset-0 bg-black/60" @click="showEditModal = false" />
      <div class="relative ms-panel p-6 w-full max-w-md">
        <h3 id="edit-h" class="ms-h2 text-base">Edit kit</h3>
        <div class="space-y-3 mt-4">
          <label class="block"><span class="ms-label">Name</span>
            <input v-model="editForm.label" class="ms-input w-full mt-1" @keydown.enter="saveEdit" /></label>
          <label class="block"><span class="ms-label">TAK callsign</span>
            <input v-model="editForm.cot_callsign" placeholder="e.g. MESHSAT-01" class="ms-input w-full mt-1 font-mono" @keydown.enter="saveEdit" /></label>
        </div>
        <div class="flex justify-end gap-2 mt-6">
          <button class="ms-btn" @click="showEditModal = false">Cancel</button>
          <button class="ms-btn-primary" @click="saveEdit">Save</button>
        </div>
      </div>
    </div>

    <!-- Delete -->
    <div v-if="showDeleteConfirm" class="fixed inset-0 z-50 flex items-center justify-center p-4" role="dialog" aria-modal="true" aria-labelledby="del-h" @click.self="showDeleteConfirm = false">
      <div class="absolute inset-0 bg-black/60" @click="showDeleteConfirm = false" />
      <div class="relative ms-panel p-6 w-full max-w-md">
        <h3 id="del-h" class="ms-h2 text-base">Delete {{ bridgeToDelete ? kitName(bridgeToDelete) : 'kit' }}?</h3>
        <p class="text-[13px] text-ms-muted mt-2">The kit's record goes, its broker login and certificate stop working, and devices linked to it are unlinked. This cannot be undone.</p>
        <p v-if="bridgeToDelete?.online" class="mt-3 rounded-md border border-ms-warning/50 bg-ms-warning/5 px-3 py-2 text-xs text-ms-text">It is connected right now and will be disconnected.</p>
        <div class="flex justify-end gap-2 mt-6">
          <button class="ms-btn" @click="showDeleteConfirm = false">Cancel</button>
          <button class="inline-flex items-center h-8 px-3 rounded-md text-[13px] font-semibold bg-ms-error text-ms-on-primary hover:opacity-90" @click="deleteBridge()">Delete kit</button>
        </div>
      </div>
    </div>

    <!-- Setup QR -->
    <div v-if="showProvisionQR" class="fixed inset-0 z-50 flex items-center justify-center p-4" role="dialog" aria-modal="true" aria-labelledby="qr-h" @click.self="dismissProvisionQR">
      <div class="absolute inset-0 bg-black/70" @click="dismissProvisionQR" />
      <div class="relative ms-panel p-6 w-full max-w-md">
        <h3 id="qr-h" class="ms-h2 text-base">Set up {{ provisionQRBridgeId }}</h3>
        <p class="text-[13px] text-ms-muted mt-1">Scan with the kit or the MeshSat Android app. The code works once; showing it again issues a new login.</p>
        <div class="relative flex justify-center bg-white rounded-lg p-4 mt-4">
          <img v-if="provisionQRUrl" :src="provisionQRUrl" :alt="'Setup QR code for ' + provisionQRBridgeId"
            class="w-72 h-72 object-contain transition"
            :class="provisionState === 'pending' ? 'blur-md opacity-40 pointer-events-none select-none' : ''" />
          <div v-if="provisionState === 'pending'" class="absolute inset-0 flex items-center justify-center p-6">
            <p class="text-sm text-center text-black font-medium">
              Waiting for the broker to accept the new login<br />
              <span class="font-mono">{{ provisionAccepted }} of {{ provisionMembers || '?' }}</span> ready, {{ provisionWaited }} s
            </p>
          </div>
        </div>
        <p class="text-[13px] text-center mt-3 min-h-[1.25rem]" aria-live="polite">
          <span v-if="provisionState === 'pending'" class="text-ms-warning">Don't scan yet: a scan now would be refused. Up to a minute.</span>
          <span v-else-if="provisionState === 'live' && provisionChecked" class="text-ms-text">Ready. All {{ provisionMembers }} broker members accept it. Scan now.</span>
          <span v-else-if="provisionState === 'live'" class="text-ms-text">Ready. Scan now.</span>
          <span v-else-if="provisionState === 'none'" class="text-ms-muted">Scanned. The kit is connecting.</span>
          <span v-else-if="provisionState === 'expired'" class="text-ms-error">This code has expired. Close it and show a new one.</span>
          <span v-else-if="provisionState === 'unknown'" class="text-ms-warning">Could not confirm the broker has it yet. If the app is refused, it retries by itself.</span>
        </p>
        <div class="flex justify-end mt-4">
          <button class="ms-btn" @click="dismissProvisionQR">Done</button>
        </div>
      </div>
    </div>
  </div>
</template>
