<script setup>
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { bridges } from '../api/client'
import { timeAgo, formatUptime, formatUTC } from '../utils/time'
import EmptyState from '../components/EmptyState.vue'

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

// Clipboard feedback
const copied = ref('')

onMounted(async () => {
  await loadBridges()
  pollTimer = setInterval(loadBridges, 30000)
})

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
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

// --- Add Bridge ---
async function addBridge() {
  if (!addForm.value.bridge_id.trim()) {
    addError.value = 'Bridge ID is required'
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
  } catch (e) {
    error.value = 'QR provisioning failed: ' + (e.message || e.toString() || 'Unknown error')
    console.error('QR provisioning error:', e)
  } finally {
    provisionLoading.value = false
  }
}

function dismissProvisionQR() {
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
async function sendCommand(bridgeId, cmd) {
  commandLoading.value = { ...commandLoading.value, [bridgeId + cmd]: true }
  commandResult.value = { ...commandResult.value, [bridgeId]: null }
  try {
    const result = await bridges.sendCommand(bridgeId, { cmd })
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
  if (status === 'online') return 'bg-emerald-400'
  if (status === 'error') return 'bg-red-400'
  if (status === 'binding') return 'bg-amber-400'
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
    meshtastic: 'Meshtastic',
    iridium_sbd: 'Iridium SBD',
    iridium_imt: 'Iridium IMT',
    cellular: 'Cellular',
    astrocast: 'Astrocast',
    zigbee: 'ZigBee',
    aprs: 'APRS',
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
  if (days < 0) return { label: 'Expired', color: 'text-red-400' }
  if (days < 14) return { label: `${days}d left`, color: 'text-amber-400' }
  return { label: `${days}d left`, color: 'text-gray-400' }
}
</script>

<template>
  <div>
    <!-- Header -->
    <div class="flex items-center justify-between mb-4">
      <div>
        <h1 class="text-2xl font-display font-bold">Fleet</h1>
        <p v-if="!loading && bridgeList.length" class="text-sm text-gray-400 mt-0.5">
          {{ totalCount }} bridge{{ totalCount !== 1 ? 's' : '' }}, {{ onlineCount }} online
        </p>
      </div>
      <div class="flex items-center gap-2">
        <button @click="regenerateACL" :disabled="aclLoading"
          class="text-xs px-3 py-1.5 rounded border border-gray-600 text-gray-400 hover:text-gray-200 hover:border-gray-500 transition-colors disabled:opacity-50">
          {{ aclLoading ? 'Regenerating...' : 'Regenerate ACL' }}
        </button>
        <span v-if="aclResult" class="text-xs text-emerald-400">{{ aclResult.bridges_configured }} bridges configured</span>
        <button @click="showAddForm = !showAddForm"
          class="bg-teal-600 hover:bg-teal-500 text-white px-3 py-1.5 rounded text-sm font-medium transition-colors">
          {{ showAddForm ? 'Cancel' : '+ Add Bridge' }}
        </button>
      </div>
    </div>

    <!-- Error -->
    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4 flex items-center justify-between">
      <span>{{ error }}</span>
      <button @click="error = ''" class="text-red-400 hover:text-red-200 text-xs ml-4">dismiss</button>
    </div>

    <!-- Add bridge form -->
    <div v-if="showAddForm" class="bg-tactical-surface rounded-lg border border-tactical-border p-4 mb-4">
      <h3 class="text-sm font-display font-semibold mb-3">Pre-register Bridge</h3>
      <p class="text-xs text-gray-400 mb-3">Create a bridge record before it connects. You'll generate credentials in the next step.</p>
      <div v-if="addError" class="bg-red-900/50 border border-red-700 text-red-200 px-3 py-2 rounded text-xs mb-3">{{ addError }}</div>
      <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 mb-3">
        <div>
          <label class="text-xs text-gray-400 mb-1 block">Bridge ID</label>
          <input v-model="addForm.bridge_id" placeholder="e.g. mule01, bananapi01"
            class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full placeholder-gray-500 focus:outline-none focus:border-teal-500 text-sm font-mono"
            @keydown.enter="addBridge" />
        </div>
        <div>
          <label class="text-xs text-gray-400 mb-1 block">Label (optional)</label>
          <input v-model="addForm.label" placeholder="Human-readable name"
            class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full placeholder-gray-500 focus:outline-none focus:border-teal-500 text-sm"
            @keydown.enter="addBridge" />
        </div>
      </div>
      <div class="flex justify-end">
        <button @click="addBridge"
          class="bg-teal-600 hover:bg-teal-500 text-white px-4 py-2 rounded text-sm transition-colors">
          Create Bridge
        </button>
      </div>
    </div>

    <!-- Onboarding banner -->
    <div v-if="onboardingBridgeId && onboardingStep > 0" class="bg-teal-900/30 border border-teal-700/50 rounded-lg p-4 mb-4">
      <div class="flex items-center justify-between mb-2">
        <h3 class="text-sm font-display font-semibold text-teal-300">Onboarding: {{ onboardingBridgeId }}</h3>
        <button @click="dismissOnboarding" class="text-xs text-gray-400 hover:text-gray-200">dismiss</button>
      </div>
      <div class="flex items-center gap-4 text-xs">
        <div class="flex items-center gap-1.5" :class="onboardingStep >= 1 ? 'text-teal-400' : 'text-gray-500'">
          <span class="w-5 h-5 rounded-full border flex items-center justify-center text-[10px] font-bold"
            :class="onboardingStep > 1 ? 'bg-teal-600 border-teal-600' : onboardingStep === 1 ? 'border-teal-500 text-teal-400' : 'border-gray-600'">
            {{ onboardingStep > 1 ? '\u2713' : '1' }}
          </span>
          MQTT Credentials
        </div>
        <div class="w-8 border-t border-gray-600" />
        <div class="flex items-center gap-1.5" :class="onboardingStep >= 2 ? 'text-teal-400' : 'text-gray-500'">
          <span class="w-5 h-5 rounded-full border flex items-center justify-center text-[10px] font-bold"
            :class="onboardingStep > 2 ? 'bg-teal-600 border-teal-600' : onboardingStep === 2 ? 'border-teal-500 text-teal-400' : 'border-gray-600'">
            {{ onboardingStep > 2 ? '\u2713' : '2' }}
          </span>
          TLS Certificate
        </div>
        <div class="w-8 border-t border-gray-600" />
        <div class="flex items-center gap-1.5" :class="onboardingStep >= 3 ? 'text-teal-400' : 'text-gray-500'">
          <span class="w-5 h-5 rounded-full border flex items-center justify-center text-[10px] font-bold"
            :class="onboardingStep >= 3 ? 'bg-teal-600 border-teal-600' : 'border-gray-600'">
            {{ onboardingStep >= 3 ? '\u2713' : '3' }}
          </span>
          Configure Bridge
        </div>
      </div>
      <div v-if="onboardingStep === 1" class="mt-3 text-xs text-gray-300">
        Expand the bridge card below and click <strong>Generate MQTT Credentials</strong> to get the username and password.
      </div>
      <div v-else-if="onboardingStep === 2" class="mt-3 text-xs text-gray-300">
        Now click <strong>Issue TLS Certificate</strong> to generate the mutual TLS client certificate.
      </div>
      <div v-else-if="onboardingStep === 3" class="mt-3 text-xs text-gray-300">
        Copy the credentials to your bridge's <code class="bg-gray-800 px-1 py-0.5 rounded font-mono text-teal-400">/cubeos/config/secrets.env</code> and restart the bridge service. It will appear as online once it connects via MQTT.
      </div>
    </div>

    <!-- Loading -->
    <div v-if="loading" class="text-center text-gray-500 py-12">Loading...</div>

    <!-- Empty state -->
    <EmptyState v-else-if="!bridgeList.length"
      icon="satellite"
      title="No bridges registered"
      message="Click '+ Add Bridge' to pre-register a bridge, or configure MESHSAT_HUB_URL on your bridge to auto-connect." />

    <!-- Bridge cards grid -->
    <div v-else class="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-4">
      <div v-for="b in bridgeList" :key="b.bridge_id">
        <!-- Card -->
        <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4 cursor-pointer hover:border-gray-500 transition-colors"
          :class="{ 'rounded-b-none': expandedBridge === b.bridge_id }"
          @click="toggleExpand(b.bridge_id)">
          <!-- Card header -->
          <div class="flex items-center justify-between mb-3">
            <div class="flex items-center gap-2 min-w-0">
              <h3 class="font-display font-semibold text-gray-200 truncate">{{ b.label || b.bridge_id }}</h3>
              <span class="flex items-center gap-1.5 text-xs font-medium shrink-0"
                :class="b.online ? 'text-emerald-400' : 'text-red-400'">
                <span class="w-2 h-2 rounded-full" :class="b.online ? 'bg-emerald-400 animate-pulse-dot' : 'bg-red-400'" />
                {{ b.online ? 'Online' : 'Offline' }}
              </span>
            </div>
            <svg class="w-4 h-4 text-gray-500 shrink-0 transition-transform" :class="expandedBridge === b.bridge_id ? 'rotate-180' : ''"
              fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7"/>
            </svg>
          </div>

          <!-- Bridge meta -->
          <div class="grid grid-cols-2 gap-x-4 gap-y-1 text-xs mb-3">
            <div>
              <span class="text-gray-500">ID</span>
              <div class="text-gray-300 font-mono truncate">{{ b.bridge_id }}</div>
            </div>
            <div>
              <span class="text-gray-500">Version</span>
              <div class="text-gray-300 font-mono">{{ b.version || '—' }}</div>
            </div>
            <div>
              <span class="text-gray-500">Last seen</span>
              <div class="text-gray-300">{{ timeAgo(b.last_seen) }}</div>
            </div>
            <div>
              <span class="text-gray-500">Hostname</span>
              <div class="text-gray-300 font-mono truncate">{{ b.hostname || '—' }}</div>
            </div>
          </div>

          <!-- CoT TAK badges -->
          <div v-if="b.cot_callsign || b.cot_type" class="flex flex-wrap gap-1.5">
            <span v-if="b.cot_callsign" class="inline-flex items-center gap-1 text-xs px-1.5 py-0.5 rounded border border-blue-600/30 bg-blue-600/10 text-blue-400 font-mono">&#9670; {{ b.cot_callsign }}</span>
            <span v-if="b.cot_type" class="inline-flex items-center gap-1 text-xs px-1.5 py-0.5 rounded border border-gray-600/30 bg-gray-600/10 text-gray-400">{{ b.cot_type }}</span>
          </div>

          <!-- Interface badges -->
          <div v-if="parseBirth(b)?.interfaces?.length" class="flex flex-wrap gap-1.5">
            <span v-for="iface in parseBirth(b).interfaces" :key="iface.name"
              class="inline-flex items-center gap-1 text-xs px-1.5 py-0.5 rounded border bg-black/20"
              :class="interfaceTypeBadgeColor(iface.type)">
              <span class="w-1.5 h-1.5 rounded-full" :class="interfaceStatusDot(iface.status)" />
              {{ iface.name }}
            </span>
          </div>
        </div>

        <!-- Expanded detail panel -->
        <Transition name="expand">
          <div v-if="expandedBridge === b.bridge_id"
            class="bg-tactical-surface rounded-b-lg border border-t-0 border-tactical-border p-4">

            <!-- Action buttons -->
            <div class="flex flex-wrap gap-2 mb-4 pb-4 border-b border-tactical-border">
              <button @click.stop="openEdit(b)"
                class="text-xs px-3 py-1.5 rounded border border-gray-600 text-gray-300 hover:text-gray-100 hover:border-gray-500 transition-colors">
                Edit
              </button>
              <button @click.stop="confirmDelete(b)"
                class="text-xs px-3 py-1.5 rounded border border-red-800 text-red-400 hover:text-red-300 hover:border-red-700 hover:bg-red-900/30 transition-colors">
                Delete Bridge
              </button>
            </div>

            <!-- Credentials section -->
            <div class="mb-4 pb-4 border-b border-tactical-border">
              <h4 class="text-xs text-gray-500 uppercase tracking-wider font-display mb-2">Credentials</h4>
              <div class="flex flex-wrap items-center gap-3 mb-2">
                <div class="text-xs">
                  <span class="text-gray-500">MQTT:</span>
                  <span :class="hasCredentials(b) ? 'text-emerald-400' : 'text-gray-500'" class="ml-1">
                    {{ hasCredentials(b) ? 'Configured' : 'Not set' }}
                  </span>
                </div>
                <div class="text-xs">
                  <span class="text-gray-500">TLS:</span>
                  <span v-if="hasCertificate(b)" :class="certExpiryStatus(b)?.color" class="ml-1">
                    {{ certExpiryStatus(b)?.label }}
                  </span>
                  <span v-else class="text-gray-500 ml-1">Not issued</span>
                </div>
              </div>
              <div class="flex flex-wrap gap-2">
                <button @click.stop="generateCredentials(b.bridge_id)" :disabled="credentialLoading"
                  class="text-xs px-3 py-1.5 rounded bg-gray-700 hover:bg-gray-600 text-gray-200 transition-colors disabled:opacity-50">
                  {{ credentialLoading ? 'Generating...' : hasCredentials(b) ? 'Rotate MQTT Password' : 'Generate MQTT Credentials' }}
                </button>
                <button @click.stop="issueCertificate(b.bridge_id)" :disabled="certificateLoading"
                  class="text-xs px-3 py-1.5 rounded bg-gray-700 hover:bg-gray-600 text-gray-200 transition-colors disabled:opacity-50">
                  {{ certificateLoading ? 'Issuing...' : hasCertificate(b) ? 'Reissue TLS Certificate' : 'Issue TLS Certificate' }}
                </button>
                <button @click.stop="provisionWithQR(b.bridge_id)" :disabled="provisionLoading"
                  class="text-xs px-3 py-1.5 rounded bg-teal-700 hover:bg-teal-600 text-white transition-colors disabled:opacity-50"
                  title="Generate a QR code with MQTT credentials + TLS certificate for one-step provisioning">
                  {{ provisionLoading ? 'Generating...' : 'Provision QR' }}
                </button>
              </div>

              <!-- One-time MQTT credential display -->
              <div v-if="credentialResult && credentialResult.bridge_id === b.bridge_id"
                class="mt-3 bg-amber-900/20 border border-amber-700/50 rounded-lg p-3">
                <div class="flex items-center justify-between mb-2">
                  <span class="text-xs font-semibold text-amber-300">MQTT Credentials — copy now, shown only once</span>
                  <button @click.stop="dismissCredentials" class="text-xs text-gray-400 hover:text-gray-200">dismiss</button>
                </div>
                <div class="space-y-2 text-xs font-mono">
                  <div class="flex items-center gap-2">
                    <span class="text-gray-400 w-16 shrink-0">URL:</span>
                    <code class="text-gray-200 bg-gray-800 px-2 py-1 rounded flex-1 truncate">{{ credentialResult.mqtt_url }}</code>
                    <button @click.stop="copyToClipboard(credentialResult.mqtt_url, 'url')"
                      class="text-teal-400 hover:text-teal-300 shrink-0 text-xs">{{ copied === 'url' ? 'Copied!' : 'Copy' }}</button>
                  </div>
                  <div class="flex items-center gap-2">
                    <span class="text-gray-400 w-16 shrink-0">User:</span>
                    <code class="text-gray-200 bg-gray-800 px-2 py-1 rounded flex-1 truncate">{{ credentialResult.username }}</code>
                    <button @click.stop="copyToClipboard(credentialResult.username, 'user')"
                      class="text-teal-400 hover:text-teal-300 shrink-0 text-xs">{{ copied === 'user' ? 'Copied!' : 'Copy' }}</button>
                  </div>
                  <div class="flex items-center gap-2">
                    <span class="text-gray-400 w-16 shrink-0">Pass:</span>
                    <code class="text-gray-200 bg-gray-800 px-2 py-1 rounded flex-1 truncate">{{ credentialResult.password }}</code>
                    <button @click.stop="copyToClipboard(credentialResult.password, 'pass')"
                      class="text-teal-400 hover:text-teal-300 shrink-0 text-xs">{{ copied === 'pass' ? 'Copied!' : 'Copy' }}</button>
                  </div>
                </div>
              </div>

              <!-- One-time TLS certificate display -->
              <div v-if="certificateResult && certificateResult.bridge_id === b.bridge_id"
                class="mt-3 bg-amber-900/20 border border-amber-700/50 rounded-lg p-3">
                <div class="flex items-center justify-between mb-2">
                  <span class="text-xs font-semibold text-amber-300">TLS Certificate — private key shown only once</span>
                  <button @click.stop="dismissCertificate" class="text-xs text-gray-400 hover:text-gray-200">dismiss</button>
                </div>
                <div class="text-xs text-gray-400 mb-2">
                  Expires: <span class="text-gray-200">{{ formatUTC(certificateResult.expires) }}</span>
                </div>
                <div class="flex flex-wrap gap-2">
                  <button @click.stop="downloadFile(certificateResult.cert_pem, b.bridge_id + '.crt')"
                    class="text-xs px-2 py-1 rounded bg-gray-700 hover:bg-gray-600 text-gray-200 transition-colors">
                    Download Certificate (.crt)
                  </button>
                  <button @click.stop="downloadFile(certificateResult.key_pem, b.bridge_id + '.key')"
                    class="text-xs px-2 py-1 rounded bg-gray-700 hover:bg-gray-600 text-gray-200 transition-colors">
                    Download Private Key (.key)
                  </button>
                  <button @click.stop="downloadFile(certificateResult.ca_pem, 'meshsat-hub-ca.crt')"
                    class="text-xs px-2 py-1 rounded bg-gray-700 hover:bg-gray-600 text-gray-200 transition-colors">
                    Download CA (.crt)
                  </button>
                  <button @click.stop="copyToClipboard(certificateResult.cert_pem + '\n' + certificateResult.key_pem, 'cert')"
                    class="text-xs text-teal-400 hover:text-teal-300">{{ copied === 'cert' ? 'Copied!' : 'Copy All' }}</button>
                </div>
              </div>
            </div>

            <!-- Commands section (online only) -->
            <div v-if="b.online" class="mb-4 pb-4 border-b border-tactical-border">
              <h4 class="text-xs text-gray-500 uppercase tracking-wider font-display mb-2">Commands</h4>
              <div class="flex flex-wrap gap-2">
                <button @click.stop="sendCommand(b.bridge_id, 'ping')" :disabled="commandLoading[b.bridge_id + 'ping']"
                  class="text-xs px-3 py-1.5 rounded bg-gray-700 hover:bg-gray-600 text-gray-200 transition-colors disabled:opacity-50">
                  {{ commandLoading[b.bridge_id + 'ping'] ? 'Pinging...' : 'Ping' }}
                </button>
                <button @click.stop="sendCommand(b.bridge_id, 'reboot')" :disabled="commandLoading[b.bridge_id + 'reboot']"
                  class="text-xs px-3 py-1.5 rounded bg-gray-700 hover:bg-gray-600 text-amber-300 transition-colors disabled:opacity-50">
                  {{ commandLoading[b.bridge_id + 'reboot'] ? 'Rebooting...' : 'Reboot' }}
                </button>
                <button @click.stop="sendCommand(b.bridge_id, 'flush_burst')" :disabled="commandLoading[b.bridge_id + 'flush_burst']"
                  class="text-xs px-3 py-1.5 rounded bg-gray-700 hover:bg-gray-600 text-gray-200 transition-colors disabled:opacity-50">
                  {{ commandLoading[b.bridge_id + 'flush_burst'] ? 'Flushing...' : 'Flush Burst Queue' }}
                </button>
              </div>
              <div v-if="commandResult[b.bridge_id]" class="mt-2 text-xs">
                <div v-if="commandResult[b.bridge_id].error" class="text-red-400">
                  Error: {{ commandResult[b.bridge_id].error }}
                </div>
                <div v-else class="text-emerald-400">
                  {{ commandResult[b.bridge_id].status }} ({{ commandResult[b.bridge_id].latency_ms }}ms)
                </div>
              </div>
            </div>

            <!-- System metrics -->
            <template v-if="parseHealth(b)">
              <h4 class="text-xs text-gray-500 uppercase tracking-wider font-display mb-2">System</h4>
              <div class="grid grid-cols-3 gap-3 mb-4">
                <div>
                  <div class="flex items-center justify-between text-xs mb-1">
                    <span class="text-gray-400">CPU</span>
                    <span class="text-gray-300">{{ parseHealth(b).cpu_pct?.toFixed(1) || 0 }}%</span>
                  </div>
                  <div class="w-full bg-gray-700 rounded-full h-1.5">
                    <div class="h-1.5 rounded-full transition-all"
                      :class="parseHealth(b).cpu_pct > 80 ? 'bg-red-400' : parseHealth(b).cpu_pct > 50 ? 'bg-amber-400' : 'bg-teal-400'"
                      :style="{ width: Math.min(parseHealth(b).cpu_pct || 0, 100) + '%' }" />
                  </div>
                </div>
                <div>
                  <div class="flex items-center justify-between text-xs mb-1">
                    <span class="text-gray-400">Memory</span>
                    <span class="text-gray-300">{{ parseHealth(b).mem_pct?.toFixed(1) || 0 }}%</span>
                  </div>
                  <div class="w-full bg-gray-700 rounded-full h-1.5">
                    <div class="h-1.5 rounded-full transition-all"
                      :class="parseHealth(b).mem_pct > 80 ? 'bg-red-400' : parseHealth(b).mem_pct > 50 ? 'bg-amber-400' : 'bg-teal-400'"
                      :style="{ width: Math.min(parseHealth(b).mem_pct || 0, 100) + '%' }" />
                  </div>
                </div>
                <div>
                  <div class="flex items-center justify-between text-xs mb-1">
                    <span class="text-gray-400">Disk</span>
                    <span class="text-gray-300">{{ parseHealth(b).disk_pct?.toFixed(1) || 0 }}%</span>
                  </div>
                  <div class="w-full bg-gray-700 rounded-full h-1.5">
                    <div class="h-1.5 rounded-full transition-all"
                      :class="parseHealth(b).disk_pct > 80 ? 'bg-red-400' : parseHealth(b).disk_pct > 50 ? 'bg-amber-400' : 'bg-teal-400'"
                      :style="{ width: Math.min(parseHealth(b).disk_pct || 0, 100) + '%' }" />
                  </div>
                </div>
              </div>

              <!-- Uptime -->
              <div class="text-xs text-gray-400 mb-4">
                Uptime: <span class="text-gray-300">{{ formatUptime(parseHealth(b).uptime_sec) }}</span>
              </div>

              <!-- Interface detail table -->
              <template v-if="parseHealth(b).interfaces?.length">
                <h4 class="text-xs text-gray-500 uppercase tracking-wider font-display mb-2">Interfaces</h4>
                <div class="overflow-x-auto mb-4">
                  <table class="w-full text-xs">
                    <thead>
                      <tr class="text-gray-500 border-b border-tactical-border">
                        <th class="text-left py-1.5 pr-3 font-medium">Interface</th>
                        <th class="text-left py-1.5 pr-3 font-medium">Type</th>
                        <th class="text-left py-1.5 pr-3 font-medium">Status</th>
                        <th class="text-left py-1.5 pr-3 font-medium">Signal</th>
                        <th class="text-left py-1.5 pr-3 font-medium">Health</th>
                        <th class="text-left py-1.5 font-medium">Messages</th>
                      </tr>
                    </thead>
                    <tbody>
                      <tr v-for="iface in parseHealth(b).interfaces" :key="iface.name"
                        class="border-b border-tactical-border/50">
                        <td class="py-1.5 pr-3 font-mono text-gray-300">{{ iface.name }}</td>
                        <td class="py-1.5 pr-3">
                          <span :class="interfaceTypeBadgeColor(birthInterfaceType(b, iface.name))">
                            {{ interfaceTypeLabel(birthInterfaceType(b, iface.name)) }}
                          </span>
                        </td>
                        <td class="py-1.5 pr-3">
                          <span class="flex items-center gap-1">
                            <span class="w-1.5 h-1.5 rounded-full" :class="interfaceStatusDot(iface.status)" />
                            <span :class="iface.status === 'online' ? 'text-emerald-400' : iface.status === 'error' ? 'text-red-400' : 'text-gray-400'">
                              {{ iface.status }}
                            </span>
                          </span>
                        </td>
                        <td class="py-1.5 pr-3 text-gray-300">{{ signalDisplay(iface) }}</td>
                        <td class="py-1.5 pr-3">
                          <span v-if="iface.health_score > 0"
                            :class="iface.health_score >= 80 ? 'text-emerald-400' : iface.health_score >= 50 ? 'text-amber-400' : 'text-red-400'">
                            {{ iface.health_score }}%
                          </span>
                          <span v-else class="text-gray-500">—</span>
                        </td>
                        <td class="py-1.5 text-gray-300">{{ messageDisplay(iface) }}</td>
                      </tr>
                    </tbody>
                  </table>
                </div>
              </template>

              <!-- Reticulum stats -->
              <template v-if="parseHealth(b).reticulum">
                <h4 class="text-xs text-gray-500 uppercase tracking-wider font-display mb-2">Reticulum</h4>
                <div class="grid grid-cols-3 gap-3 text-xs mb-4">
                  <div>
                    <span class="text-gray-500">Routes</span>
                    <div class="text-gray-300 font-mono">{{ parseHealth(b).reticulum.routes }}</div>
                  </div>
                  <div>
                    <span class="text-gray-500">Links</span>
                    <div class="text-gray-300 font-mono">{{ parseHealth(b).reticulum.links }}</div>
                  </div>
                  <div>
                    <span class="text-gray-500">Announces relayed</span>
                    <div class="text-gray-300 font-mono">{{ parseHealth(b).reticulum.announces_relayed }}</div>
                  </div>
                </div>
              </template>

              <!-- HeMB bonding stats -->
              <template v-if="parseHealth(b).hemb">
                <h4 class="text-xs text-gray-500 uppercase tracking-wider font-display mb-2">HeMB Bonding</h4>
                <div class="grid grid-cols-3 gap-3 text-xs mb-4">
                  <div>
                    <span class="text-gray-500">Bond Groups</span>
                    <div class="text-gray-300 font-mono">{{ parseHealth(b).hemb.active_bond_groups }}</div>
                  </div>
                  <div>
                    <span class="text-gray-500">Symbols Tx/Rx</span>
                    <div class="text-gray-300 font-mono">{{ parseHealth(b).hemb.symbols_sent }}/{{ parseHealth(b).hemb.symbols_received }}</div>
                  </div>
                  <div>
                    <span class="text-gray-500">Decoded/Failed</span>
                    <div class="font-mono"><span class="text-emerald-400">{{ parseHealth(b).hemb.generations_decoded }}</span>/<span class="text-red-400">{{ parseHealth(b).hemb.generations_failed }}</span></div>
                  </div>
                </div>
              </template>

              <!-- Burst queue -->
              <template v-if="parseHealth(b).burst_queue">
                <div class="text-xs text-gray-400 mb-4">
                  Burst queue: <span class="text-gray-300 font-mono">{{ parseHealth(b).burst_queue.pending }}</span> pending
                </div>
              </template>
            </template>

            <!-- CoT info -->
            <template v-if="b.cot_callsign || b.cot_type">
              <h4 class="text-xs text-gray-500 uppercase tracking-wider font-display mb-2">CoT</h4>
              <div class="grid grid-cols-2 gap-3 text-xs mb-4">
                <div>
                  <span class="text-gray-500">Callsign</span>
                  <div class="text-gray-300 font-mono">{{ b.cot_callsign || '—' }}</div>
                </div>
                <div>
                  <span class="text-gray-500">Type</span>
                  <div class="text-gray-300 font-mono">{{ b.cot_type || '—' }}</div>
                </div>
              </div>
            </template>

            <!-- Location -->
            <template v-if="b.location_lat && b.location_lon">
              <div class="text-xs text-gray-400 mb-4">
                Location: <span class="text-gray-300 font-mono">{{ b.location_lat.toFixed(6) }}, {{ b.location_lon.toFixed(6) }}</span>
                <span v-if="b.location_alt" class="text-gray-500 ml-1">({{ b.location_alt.toFixed(0) }}m)</span>
              </div>
            </template>

            <!-- Bridge meta -->
            <div class="text-xs text-gray-500 mt-2 pt-2 border-t border-tactical-border/50">
              Created: {{ formatUTC(b.created_at) }}
              <span v-if="b.mode" class="ml-3">Mode: {{ b.mode }}</span>
            </div>

            <!-- No health data -->
            <div v-if="!parseHealth(b)" class="text-xs text-gray-500 italic mt-2">
              No health data received yet — bridge has not connected.
            </div>
          </div>
        </Transition>
      </div>
    </div>

    <!-- Edit modal -->
    <div v-if="showEditModal" class="fixed inset-0 z-50 flex items-center justify-center" @click.self="showEditModal = false">
      <div class="absolute inset-0 bg-black/50" />
      <div class="relative bg-tactical-surface border border-tactical-border rounded-lg p-6 max-w-md mx-4 w-full">
        <h3 class="text-lg font-display font-semibold mb-4">Edit Bridge</h3>
        <div class="space-y-3 mb-4">
          <div>
            <label class="text-xs text-gray-400 mb-1 block">Label</label>
            <input v-model="editForm.label"
              class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full placeholder-gray-500 focus:outline-none focus:border-teal-500 text-sm"
              @keydown.enter="saveEdit" />
          </div>
          <div>
            <label class="text-xs text-gray-400 mb-1 block">CoT Callsign</label>
            <input v-model="editForm.cot_callsign" placeholder="e.g. MESHSAT-01"
              class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full placeholder-gray-500 focus:outline-none focus:border-teal-500 text-sm font-mono"
              @keydown.enter="saveEdit" />
          </div>
        </div>
        <div class="flex justify-end gap-3">
          <button @click="showEditModal = false" class="px-4 py-2 text-sm text-gray-400 hover:text-gray-200">Cancel</button>
          <button @click="saveEdit" class="px-4 py-2 text-sm bg-teal-600 hover:bg-teal-500 text-white rounded transition-colors">Save</button>
        </div>
      </div>
    </div>

    <!-- Delete confirmation modal -->
    <div v-if="showDeleteConfirm" class="fixed inset-0 z-50 flex items-center justify-center" @click.self="showDeleteConfirm = false">
      <div class="absolute inset-0 bg-black/50" />
      <div class="relative bg-tactical-surface border border-tactical-border rounded-lg p-6 max-w-md mx-4">
        <h3 class="text-lg font-display font-semibold mb-2">Delete Bridge</h3>
        <p class="text-gray-400 text-sm mb-2">
          Permanently remove <span class="text-gray-200 font-medium font-mono">{{ bridgeToDelete?.label || bridgeToDelete?.bridge_id }}</span>?
        </p>
        <p class="text-xs text-gray-500 mb-4">
          This will delete the bridge record, disassociate all linked devices, and revoke MQTT credentials. This action cannot be undone.
        </p>
        <div v-if="bridgeToDelete?.online" class="text-amber-400 text-xs mb-4 bg-amber-900/20 border border-amber-700 rounded p-3">
          Warning: This bridge is currently online. Deleting it will disconnect the active MQTT session.
        </div>
        <div class="flex justify-end gap-3">
          <button @click="showDeleteConfirm = false" class="px-4 py-2 text-sm text-gray-400 hover:text-gray-200">Cancel</button>
          <button @click="deleteBridge()" class="px-4 py-2 text-sm bg-red-600 hover:bg-red-500 text-white rounded transition-colors">Delete</button>
        </div>
      </div>
    </div>

    <!-- QR Provision Modal -->
    <div v-if="showProvisionQR" class="fixed inset-0 z-50 flex items-center justify-center" @click.self="dismissProvisionQR">
      <div class="fixed inset-0 bg-black/60"></div>
      <div class="relative bg-tactical-card border border-tactical-border rounded-xl p-6 max-w-lg w-full mx-4 shadow-2xl">
        <h3 class="text-lg font-display font-semibold text-teal-300 mb-1">Provision QR Code</h3>
        <p class="text-xs text-gray-400 mb-4">
          Scan with the MeshSat Android app to auto-configure Hub connection.
          <span class="text-amber-400">Single-use</span> — credentials are regenerated each time.
        </p>
        <div class="flex justify-center bg-white rounded-lg p-4 mb-4">
          <img v-if="provisionQRUrl" :src="provisionQRUrl" :alt="'Provision QR for ' + provisionQRBridgeId"
            class="w-80 h-80 object-contain" />
        </div>
        <div class="text-center text-xs text-gray-500 mb-4">
          Bridge: <span class="text-gray-300 font-mono">{{ provisionQRBridgeId }}</span>
        </div>
        <div class="flex justify-end">
          <button @click="dismissProvisionQR"
            class="px-4 py-2 text-sm bg-gray-700 hover:bg-gray-600 text-gray-200 rounded transition-colors">
            Done
          </button>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.expand-enter-active,
.expand-leave-active {
  transition: all 0.2s ease;
  overflow: hidden;
}
.expand-enter-from,
.expand-leave-to {
  opacity: 0;
  max-height: 0;
}
.expand-enter-to,
.expand-leave-from {
  opacity: 1;
  max-height: 1200px;
}
</style>
