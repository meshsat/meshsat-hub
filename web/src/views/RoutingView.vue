<script setup>
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
const formEnabled = ref(true)

// Test state
const showTest = ref(false)
const testChannel = ref('iridium')
const testDeviceID = ref('')
const testText = ref('')
const testResults = ref(null)
const testLoading = ref(false)

const sourceTypes = ['*', 'iridium', 'astrocast', 'sms', 'email']
const destTypes = ['tak', 'aprs', 'sms', 'email', 'webhook', 'notification', 'mqtt']

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
  formEnabled.value = true
  showForm.value = true
}

function openEditForm(route) {
  editingId.value = route.id
  formName.value = route.name
  formSource.value = route.source_type
  formDest.value = route.destination_type
  formFilter.value = route.filter || ''
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
      enabled: !route.enabled,
    })
    await loadRoutes()
  } catch (e) {
    error.value = e.message
  }
}

async function deleteRoute(route) {
  if (!confirm(`Delete route "${route.name}"?`)) return
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
    const key = r.source_type === '*' ? 'All Sources' : r.source_type
    if (!sources.has(key)) sources.set(key, new Set())
    sources.get(key).add(r.destination_type)
  }
  return Array.from(sources.entries()).map(([src, dests]) => ({
    source: src,
    destinations: Array.from(dests),
  }))
})

function sourceBadgeClass(type) {
  if (type === '*' || type === 'All Sources') return 'bg-gray-700 text-gray-300'
  if (type === 'iridium') return 'bg-blue-900/50 text-blue-300'
  if (type === 'astrocast') return 'bg-indigo-900/50 text-indigo-300'
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
  if (type === 'mqtt') return 'bg-cyan-900/50 text-cyan-300'
  return 'bg-gray-700 text-gray-300'
}
</script>

<template>
  <div>
    <div class="flex items-center justify-between mb-4">
      <h1 class="text-xl font-bold">Routing Rules</h1>
      <div class="flex gap-2">
        <button @click="showTest = !showTest"
          class="bg-gray-700 hover:bg-gray-600 text-gray-300 px-3 py-2 rounded font-medium transition-colors text-sm">
          {{ showTest ? 'Hide Test' : 'Test Route' }}
        </button>
        <button v-if="canModify && !showForm" @click="openCreateForm"
          class="bg-cyan-600 hover:bg-cyan-500 text-white px-4 py-2 rounded font-medium transition-colors text-sm">
          Create Route
        </button>
      </div>
    </div>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">
      {{ error }}
    </div>

    <!-- Visual flow diagram -->
    <div v-if="flowGroups.length > 0" class="bg-gray-800 rounded-lg p-4 mb-6">
      <h2 class="text-sm font-semibold text-gray-400 mb-3">Active Route Flow</h2>
      <div class="space-y-3">
        <div v-for="group in flowGroups" :key="group.source" class="flex items-center gap-3 flex-wrap">
          <span :class="sourceBadgeClass(group.source)"
            class="text-xs px-2.5 py-1 rounded font-medium whitespace-nowrap">
            {{ group.source === '*' ? 'All Sources' : group.source }}
          </span>
          <svg class="w-6 h-4 text-gray-600 flex-shrink-0" viewBox="0 0 24 16">
            <path d="M2 8h16M14 3l5 5-5 5" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"/>
          </svg>
          <div class="flex flex-wrap gap-1.5">
            <span v-for="dest in group.destinations" :key="dest"
              :class="destBadgeClass(dest)" class="text-xs px-2.5 py-1 rounded font-medium">
              {{ dest }}
            </span>
          </div>
        </div>
      </div>
    </div>

    <!-- Test route panel -->
    <div v-if="showTest" class="bg-gray-800 rounded-lg p-4 mb-6">
      <h2 class="text-sm font-semibold text-gray-300 mb-3">Test Sample Message</h2>
      <div class="grid grid-cols-1 sm:grid-cols-3 gap-3 mb-3">
        <div>
          <label class="text-xs text-gray-400">Source Channel</label>
          <select v-model="testChannel"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full focus:outline-none focus:border-cyan-400">
            <option v-for="s in sourceTypes.filter(s => s !== '*')" :key="s" :value="s">{{ s }}</option>
          </select>
        </div>
        <div>
          <label class="text-xs text-gray-400">Device IMEI (optional)</label>
          <input v-model="testDeviceID" placeholder="300234065123456"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full placeholder-gray-500 focus:outline-none focus:border-cyan-400" />
        </div>
        <div>
          <label class="text-xs text-gray-400">Message Text</label>
          <input v-model="testText" placeholder="Sample message text"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full placeholder-gray-500 focus:outline-none focus:border-cyan-400" />
        </div>
      </div>
      <button @click="runTest" :disabled="testLoading"
        class="bg-cyan-600 hover:bg-cyan-500 disabled:bg-gray-600 text-white px-4 py-2 rounded text-sm font-medium transition-colors">
        {{ testLoading ? 'Testing...' : 'Run Test' }}
      </button>

      <div v-if="testResults" class="mt-4">
        <div class="text-xs text-gray-400 mb-2">
          {{ testResults.filter(r => r.matched).length }} of {{ testResults.length }} routes matched
        </div>
        <div class="space-y-1">
          <div v-for="r in testResults" :key="r.route_id"
            :class="r.matched ? 'bg-green-900/20 border-green-800' : 'bg-gray-900/50 border-gray-800'"
            class="flex items-center justify-between border rounded px-3 py-2 text-sm">
            <div class="flex items-center gap-2">
              <span :class="r.matched ? 'text-green-400' : 'text-gray-600'" class="text-xs font-mono">
                {{ r.matched ? 'MATCH' : 'SKIP' }}
              </span>
              <span class="text-gray-300">{{ r.route_name }}</span>
            </div>
            <span :class="destBadgeClass(r.destination_type)" class="text-xs px-2 py-0.5 rounded font-medium">
              {{ r.destination_type }}
            </span>
          </div>
        </div>
      </div>
    </div>

    <!-- Create / Edit form -->
    <div v-if="showForm" class="bg-gray-800 rounded-lg p-4 mb-6">
      <h2 class="text-sm font-semibold text-gray-300 mb-3">
        {{ editingId ? 'Edit Route' : 'Create New Route' }}
      </h2>
      <div class="flex flex-wrap gap-2">
        <input v-model="formName" placeholder="Route name"
          class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 placeholder-gray-500 focus:outline-none focus:border-cyan-400 flex-1 min-w-[160px]" />
        <select v-model="formSource"
          class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 focus:outline-none focus:border-cyan-400">
          <option v-for="s in sourceTypes" :key="s" :value="s">Source: {{ s === '*' ? 'All' : s }}</option>
        </select>
        <select v-model="formDest"
          class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 focus:outline-none focus:border-cyan-400">
          <option v-for="d in destTypes" :key="d" :value="d">Dest: {{ d }}</option>
        </select>
        <input v-model="formFilter" placeholder="Filter (IMEI or keyword)"
          class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 placeholder-gray-500 focus:outline-none focus:border-cyan-400 flex-1 min-w-[140px]" />
        <label class="flex items-center gap-2 text-sm text-gray-300 px-2">
          <input type="checkbox" v-model="formEnabled" class="rounded" />
          Enabled
        </label>
      </div>
      <div class="flex gap-2 mt-3">
        <button @click="submitForm"
          class="bg-cyan-600 hover:bg-cyan-500 text-white px-4 py-2 rounded font-medium transition-colors text-sm">
          {{ editingId ? 'Update' : 'Create' }}
        </button>
        <button @click="cancelForm"
          class="bg-gray-700 hover:bg-gray-600 text-gray-300 px-4 py-2 rounded font-medium transition-colors text-sm">
          Cancel
        </button>
      </div>
    </div>

    <!-- Routes table -->
    <div class="overflow-x-auto">
      <table class="w-full border-collapse text-sm">
        <thead>
          <tr class="border-b border-gray-700 text-left text-gray-400">
            <th class="px-3 py-2">Name</th>
            <th class="px-3 py-2">Source</th>
            <th class="px-3 py-2">Destination</th>
            <th class="px-3 py-2">Filter</th>
            <th class="px-3 py-2">Enabled</th>
            <th v-if="canModify" class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in routeList" :key="r.id" class="border-b border-gray-800 hover:bg-gray-800/50">
            <td class="px-3 py-2 font-medium">{{ r.name }}</td>
            <td class="px-3 py-2">
              <span :class="sourceBadgeClass(r.source_type)" class="text-xs px-2 py-0.5 rounded font-medium">
                {{ r.source_type === '*' ? 'all' : r.source_type }}
              </span>
            </td>
            <td class="px-3 py-2">
              <span :class="destBadgeClass(r.destination_type)" class="text-xs px-2 py-0.5 rounded font-medium">
                {{ r.destination_type }}
              </span>
            </td>
            <td class="px-3 py-2 text-gray-400 font-mono text-xs">{{ r.filter || '—' }}</td>
            <td class="px-3 py-2">
              <button v-if="canModify" @click="toggleEnabled(r)"
                :class="r.enabled ? 'bg-green-900/50 text-green-300' : 'bg-gray-700 text-gray-500'"
                class="text-xs px-2 py-0.5 rounded font-medium transition-colors">
                {{ r.enabled ? 'On' : 'Off' }}
              </button>
              <span v-else
                :class="r.enabled ? 'bg-green-900/50 text-green-300' : 'bg-gray-700 text-gray-500'"
                class="text-xs px-2 py-0.5 rounded font-medium">
                {{ r.enabled ? 'On' : 'Off' }}
              </span>
            </td>
            <td v-if="canModify" class="px-3 py-2 text-right">
              <button @click="openEditForm(r)"
                class="bg-gray-700 hover:bg-gray-600 text-gray-300 px-2 py-1 rounded text-xs transition-colors mr-1">
                Edit
              </button>
              <button @click="deleteRoute(r)"
                class="bg-red-900 hover:bg-red-800 text-red-200 px-2 py-1 rounded text-xs transition-colors">
                Delete
              </button>
            </td>
          </tr>
          <tr v-if="routeList.length === 0 && !loading">
            <td :colspan="canModify ? 6 : 5" class="px-3 py-8 text-center text-gray-500">No routing rules configured</td>
          </tr>
          <tr v-if="loading">
            <td :colspan="canModify ? 6 : 5" class="px-3 py-8 text-center text-gray-500">Loading...</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
