<script setup>
import Icon from '../components/Icon.vue'
import { ref, computed, onMounted } from 'vue'
import { routes as routesApi } from '../api/client'
import { useAuthStore } from '../stores/auth'

const auth = useAuthStore()
const routeList = ref([])
const error = ref('')
const loading = ref(false)

// Form state
const showForm = ref(false)
const editingId = ref(null)
const formName = ref('')
const formSource = ref('*')
const formDest = ref('mqtt')
const formFilter = ref('')
const formSenders = ref('')
const formEnabled = ref(true)

// Test state
const showTest = ref(false)
const testChannel = ref('iridium')
const testDeviceID = ref('')
const testText = ref('')
const testResults = ref(null)
const testLoading = ref(false)

const sourceTypes = ['*', 'satellite', 'iridium', 'sms', 'email']
const destTypes = ['tak', 'aprs', 'sms', 'email', 'satellite', 'satellite_relay', 'webhook', 'notification', 'mqtt']

const showDeleteConfirm = ref(false)
const routeToDelete = ref(null)

const canModify = computed(() => auth.isOwner)

onMounted(async () => {
  await loadRoutes()
})

async function loadRoutes() {
  loading.value = true
  try {
    routeList.value = await routesApi.list() || []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function openCreateForm() {
  editingId.value = null
  formName.value = ''
  formSource.value = '*'
  formDest.value = 'mqtt'
  formFilter.value = ''
  formSenders.value = ''
  formEnabled.value = true
  showForm.value = true
}

function openEditForm(route) {
  editingId.value = route.id
  formName.value = route.name
  formSource.value = route.source_type
  formDest.value = route.destination_type
  formFilter.value = route.filter || ''
  formSenders.value = route.senders || ''
  formEnabled.value = route.enabled
  showForm.value = true
}

function cancelForm() {
  showForm.value = false
  editingId.value = null
}

async function submitForm() {
  if (!formName.value.trim()) {
    error.value = 'Route name is required'
    return
  }
  error.value = ''
  const data = {
    name: formName.value.trim(),
    source_type: formSource.value,
    destination_type: formDest.value,
    filter: formFilter.value.trim(),
    senders: formSenders.value.trim(),
    enabled: formEnabled.value,
  }
  try {
    if (editingId.value) {
      await routesApi.update(editingId.value, data)
    } else {
      await routesApi.create(data)
    }
    showForm.value = false
    editingId.value = null
    await loadRoutes()
  } catch (e) {
    error.value = e.message
  }
}

async function toggleEnabled(route) {
  error.value = ''
  try {
    await routesApi.update(route.id, {
      name: route.name,
      source_type: route.source_type,
      destination_type: route.destination_type,
      filter: route.filter || '',
      senders: route.senders || '',
      enabled: !route.enabled,
    })
    await loadRoutes()
  } catch (e) {
    error.value = e.message
  }
}

function confirmDeleteRoute(route) {
  routeToDelete.value = route
  showDeleteConfirm.value = true
}

async function deleteRoute() {
  const route = routeToDelete.value
  if (!route) return
  showDeleteConfirm.value = false
  routeToDelete.value = null
  try {
    await routesApi.delete(route.id)
    await loadRoutes()
  } catch (e) {
    error.value = e.message
  }
}

async function runTest() {
  testLoading.value = true
  testResults.value = null
  error.value = ''
  try {
    testResults.value = await routesApi.test({
      channel: testChannel.value,
      device_id: testDeviceID.value.trim(),
      text: testText.value.trim(),
    })
  } catch (e) {
    error.value = e.message
  } finally {
    testLoading.value = false
  }
}

// Flow diagram: group enabled routes by source → destination
const flowGroups = computed(() => {
  const enabled = routeList.value.filter(r => r.enabled)
  const sources = new Map()
  for (const r of enabled) {
    const key = r.source_type === '*' ? 'All sources' : r.source_type
    if (!sources.has(key)) sources.set(key, new Set())
    sources.get(key).add(r.destination_type)
  }
  return Array.from(sources.entries()).map(([src, dests]) => ({
    source: src,
    destinations: Array.from(dests),
  }))
})

// Routes are drawn in words, not colours: a destination is not abnormal, so
// it takes no hue (ISA-101). The raw type stays visible where it is typed.
const WORDS = {
  '*': 'Any source', satellite: 'Satellite', iridium: 'Iridium', sms: 'SMS', email: 'Email',
  tak: 'TAK', aprs: 'APRS', webhook: 'Webhook', notification: 'Notifications', mqtt: 'MQTT',
  satellite_relay: 'Relay to a kit by satellite',
}
function words(type) { return WORDS[type] || type }

// The one "filter" field means two things, as the engine reads it
// (isRecipientDestination): for SMS, email and satellite destinations it is
// WHO receives the message; for the others it is a condition on the message
// (the sender's id, or a word in the text).
const RECIPIENT_DESTS = new Set(['sms', 'email', 'satellite', 'satellite_relay'])
function isRecipient(dest) { return RECIPIENT_DESTS.has(dest) }

function sourceBadgeClass(type) {
  if (type === '*' || type === 'All sources') return 'bg-gray-700 text-gray-300'
  if (type === 'iridium' || type === 'satellite' || type === 'satellite_relay') return 'bg-blue-900/50 text-blue-300'
  if (type === 'sms') return 'bg-green-900/50 text-green-300'
  if (type === 'email') return 'bg-yellow-900/50 text-yellow-300'
  return 'bg-gray-700 text-gray-300'
}

function destBadgeClass(type) {
  if (type === 'tak') return 'bg-orange-900/50 text-orange-300'
  if (type === 'aprs') return 'bg-emerald-900/50 text-emerald-300'
  if (type === 'sms') return 'bg-green-900/50 text-green-300'
  if (type === 'email') return 'bg-yellow-900/50 text-yellow-300'
  if (type === 'webhook') return 'bg-purple-900/50 text-purple-300'
  if (type === 'notification') return 'bg-pink-900/50 text-pink-300'
  if (type === 'mqtt') return 'bg-brand-primary/15 text-brand-primary'
  return 'bg-gray-700 text-gray-300'
}
</script>

<template>
  <div class="ms-page">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Routing</h1>
        <p class="ms-lede">Where each incoming message goes. Every rule that matches a message fires; none stops the others.</p>
      </div>
      <div class="flex gap-2">
        <button class="ms-btn" @click="showTest = !showTest">{{ showTest ? 'Hide test' : 'Test a route' }}</button>
        <button v-if="canModify && !showForm" class="ms-btn-primary" @click="openCreateForm"><Icon name="plus" :size="15" />New route</button>
      </div>
    </div>

    <div v-if="error" role="alert" class="ms-alert mb-4">{{ error }}</div>

    <!-- Flow: sources on the left, where their messages go on the right -->
    <section v-if="flowGroups.length > 0" class="ms-panel p-5 mb-5" aria-labelledby="flow-h">
      <h2 id="flow-h" class="ms-h2">What is switched on</h2>
      <div class="mt-4 space-y-4">
        <div v-for="group in flowGroups" :key="group.source" class="grid grid-cols-[8.5rem_1.5rem_1fr] items-start gap-2">
          <span class="text-[13px] font-medium text-ms-text pt-0.5">{{ words(group.source) }}</span>
          <svg class="w-5 h-5 text-ms-muted mt-0.5" viewBox="0 0 24 24" aria-hidden="true"><path d="M3 12h15M13 6l6 6-6 6" stroke="currentColor" stroke-width="1.6" fill="none" stroke-linecap="round" stroke-linejoin="round" /></svg>
          <div class="flex flex-wrap gap-1.5">
            <span v-for="dest in group.destinations" :key="dest" class="inline-flex items-center h-6 px-2 rounded-md border border-ms-border bg-ms-well text-xs text-ms-text2">{{ words(dest) }}</span>
          </div>
        </div>
      </div>
    </section>

    <!-- Test -->
    <section v-if="showTest" class="ms-panel p-5 mb-5" aria-labelledby="test-h">
      <h2 id="test-h" class="ms-h2">Test a sample message</h2>
      <p class="text-xs text-ms-muted mt-1">Nothing is sent: this only shows which rules would fire.</p>
      <div class="grid grid-cols-1 sm:grid-cols-3 gap-3 mt-3">
        <label class="block"><span class="ms-label">Arrives over</span>
          <select v-model="testChannel" class="ms-input w-full mt-1">
            <option v-for="src in sourceTypes.filter(x => x !== '*')" :key="src" :value="src">{{ words(src) }}</option>
          </select></label>
        <label class="block"><span class="ms-label">From (IMEI or number, optional)</span>
          <input v-model="testDeviceID" placeholder="300234065123456" class="ms-input w-full mt-1 font-mono" /></label>
        <label class="block"><span class="ms-label">Text</span>
          <input v-model="testText" placeholder="Sample message text" class="ms-input w-full mt-1" /></label>
      </div>
      <button class="ms-btn-primary mt-3" :disabled="testLoading" @click="runTest">{{ testLoading ? 'Testing' : 'Run test' }}</button>
      <div v-if="testResults" class="mt-4">
        <p class="text-[13px] text-ms-text">{{ testResults.filter(r => r.matched).length }} of {{ testResults.length }} rules would fire.</p>
        <ul class="mt-2 divide-y divide-ms-border rounded-lg border border-ms-border overflow-hidden">
          <li v-for="r in testResults" :key="r.route_id" class="flex items-center justify-between gap-3 px-3 h-10 text-[13px]"
            :class="r.matched ? 'bg-ms-well' : ''">
            <span class="flex items-center gap-2 min-w-0">
              <Icon :name="r.matched ? 'check' : 'close'" :size="14" :class="r.matched ? 'text-ms-text' : 'text-ms-muted'" />
              <span class="truncate" :class="r.matched ? 'text-ms-text' : 'text-ms-muted'">{{ r.route_name }}</span>
            </span>
            <span class="text-xs text-ms-muted whitespace-nowrap">{{ words(r.destination_type) }}</span>
          </li>
        </ul>
      </div>
    </section>

    <!-- Create / edit -->
    <section v-if="showForm" class="ms-panel p-5 mb-5" aria-labelledby="form-h">
      <h2 id="form-h" class="ms-h2">{{ editingId ? 'Edit route' : 'New route' }}</h2>
      <div class="grid gap-3 mt-3 sm:grid-cols-2 lg:grid-cols-3">
        <label class="block"><span class="ms-label">Name</span>
          <input v-model="formName" placeholder="Route name" class="ms-input w-full mt-1" /></label>
        <label class="block"><span class="ms-label">When a message arrives over</span>
          <select v-model="formSource" class="ms-input w-full mt-1">
            <option v-for="src in sourceTypes" :key="src" :value="src">{{ words(src) }}</option>
          </select></label>
        <label class="block"><span class="ms-label">Send it to</span>
          <select v-model="formDest" class="ms-input w-full mt-1">
            <option v-for="d in destTypes" :key="d" :value="d">{{ words(d) }}</option>
          </select></label>
        <label class="block lg:col-span-1"><span class="ms-label">{{ isRecipient(formDest) ? 'Recipients (numbers, addresses or IMEIs, comma separated)' : 'Only if the sender id is, or the text contains' }}</span>
          <input v-model="formFilter" :placeholder="isRecipient(formDest) ? '+31612345678' : 'Filter (IMEI or keyword)'" class="ms-input w-full mt-1 font-mono" /></label>
        <label class="block lg:col-span-2"><span class="ms-label">Only from these senders (comma separated; empty means anyone)</span>
          <input v-model="formSenders" placeholder="IMEI or number" class="ms-input w-full mt-1 font-mono" /></label>
      </div>
      <label class="inline-flex items-center gap-2 mt-3 text-[13px] text-ms-text2"><input v-model="formEnabled" type="checkbox" class="accent-brand-primary" /> Switched on</label>
      <div class="flex gap-2 mt-4">
        <button class="ms-btn-primary" @click="submitForm">{{ editingId ? 'Save route' : 'Create route' }}</button>
        <button class="ms-btn" @click="cancelForm">Cancel</button>
      </div>
    </section>

    <!-- Rules -->
    <section class="ms-panel overflow-hidden" aria-label="Routes">
      <div class="overflow-x-auto">
        <table class="ms-table">
          <thead>
            <tr><th>Rule</th><th>From</th><th>To</th><th class="hidden lg:table-cell">Conditions</th><th class="text-center">On</th><th v-if="canModify"></th></tr>
          </thead>
          <tbody>
            <tr v-if="loading && !routeList.length"><td :colspan="canModify ? 6 : 5" class="text-ms-muted">Loading routes.</td></tr>
            <tr v-else-if="!routeList.length"><td :colspan="canModify ? 6 : 5" class="text-ms-muted !h-20">No rules yet, so incoming messages are stored and shown but go nowhere else.</td></tr>
            <tr v-for="r in routeList" :key="r.id">
              <td class="text-[13px] font-medium" :class="r.enabled ? 'text-ms-text' : 'text-ms-muted'">{{ r.name }}<span v-if="!r.enabled" class="font-normal"> (off)</span></td>
              <td class="whitespace-nowrap" :class="r.enabled ? 'text-ms-text2' : 'text-ms-muted'">{{ words(r.source_type) }}</td>
              <td class="whitespace-nowrap" :class="r.enabled ? 'text-ms-text2' : 'text-ms-muted'">{{ words(r.destination_type) }}</td>
              <td class="hidden lg:table-cell">
                <div v-if="r.filter" class="text-xs text-ms-muted">{{ isRecipient(r.destination_type) ? 'To' : 'Matching' }} <span class="ms-id text-ms-text2">{{ r.filter }}</span></div>
                <div v-if="r.senders" class="text-xs text-ms-muted">From <span class="ms-id text-ms-text2">{{ r.senders }}</span></div>
                <div v-if="!r.filter && !r.senders" class="text-xs text-ms-muted">Every message</div>
              </td>
              <td class="text-center">
                <button v-if="canModify" role="switch" :aria-checked="r.enabled" :aria-label="`${r.enabled ? 'Switch off' : 'Switch on'} ${r.name}`"
                  class="relative inline-flex w-9 h-5 rounded-full transition-colors align-middle"
                  :class="r.enabled ? 'bg-ms-primary' : 'bg-ms-border-light'" @click="toggleEnabled(r)">
                  <span class="absolute top-0.5 w-4 h-4 rounded-full bg-ms-bg transition-all" :class="r.enabled ? 'left-[18px]' : 'left-0.5'" />
                </button>
                <span v-else class="text-xs" :class="r.enabled ? 'text-ms-text' : 'text-ms-muted'">{{ r.enabled ? 'On' : 'Off' }}</span>
              </td>
              <td v-if="canModify" class="text-right whitespace-nowrap">
                <button class="ms-btn-ghost h-7 text-xs" @click="openEditForm(r)">Edit</button>
                <button class="ms-btn-danger" @click="confirmDeleteRoute(r)">Delete</button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>

    <div v-if="showDeleteConfirm" class="fixed inset-0 z-50 flex items-center justify-center p-4" role="dialog" aria-modal="true" @click.self="showDeleteConfirm = false">
      <div class="absolute inset-0 bg-black/60" @click="showDeleteConfirm = false" />
      <div class="relative ms-panel p-6 w-full max-w-md">
        <h3 class="ms-h2 text-base">Delete {{ routeToDelete?.name }}?</h3>
        <p class="text-[13px] text-ms-muted mt-2">Messages it carries stop going there at once. To pause it instead, switch it off.</p>
        <div class="flex justify-end gap-2 mt-6">
          <button class="ms-btn" @click="showDeleteConfirm = false">Cancel</button>
          <button class="ms-btn-destroy" @click="deleteRoute()">Delete route</button>
        </div>
      </div>
    </div>
  </div>
</template>
