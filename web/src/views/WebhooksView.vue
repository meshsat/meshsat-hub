<script setup>
import { ref, onMounted } from 'vue'
import { webhooks } from '../api/client'
import { formatUTC } from '../utils/time'

const hookList = ref([])
const logs = ref([])
const error = ref('')
const loading = ref(true)

const form = ref({ url: '', events: 'mo,sos,position', active: true })
const showForm = ref(false)

onMounted(async () => {
  await loadData()
})

async function loadData() {
  loading.value = true
  try {
    const [h, l] = await Promise.all([
      webhooks.list().catch(() => []),
      webhooks.logs().catch(() => []),
    ])
    hookList.value = Array.isArray(h) ? h : []
    logs.value = Array.isArray(l) ? l : []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function createWebhook() {
  if (!form.value.url.trim()) return
  error.value = ''
  try {
    await webhooks.create({
      url: form.value.url.trim(),
      events: form.value.events.split(',').map(e => e.trim()).filter(Boolean),
      active: form.value.active,
    })
    form.value = { url: '', events: 'mo,sos,position', active: true }
    showForm.value = false
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

async function deleteWebhook(id) {
  if (!confirm('Delete this webhook?')) return
  try {
    await webhooks.delete(id)
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

function statusCodeColor(code) {
  if (code >= 200 && code < 300) return 'text-green-400'
  if (code >= 400) return 'text-red-400'
  return 'text-yellow-400'
}
</script>

<template>
  <div>
    <h1 class="text-xl font-bold mb-4">Outbound Webhooks</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">{{ error }}</div>

    <!-- Webhooks -->
    <div class="mb-8">
      <div class="flex items-center justify-between mb-3">
        <h2 class="text-lg font-semibold">Webhooks</h2>
        <button @click="showForm = !showForm"
          class="bg-teal-600 hover:bg-teal-500 text-white px-3 py-1 rounded text-sm transition-colors">
          {{ showForm ? 'Cancel' : '+ New Webhook' }}
        </button>
      </div>

      <div v-if="showForm" class="bg-gray-800 rounded-lg p-4 mb-4">
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 mb-3">
          <div>
            <label class="text-xs text-gray-400">URL</label>
            <input v-model="form.url" placeholder="https://example.com/hook"
              class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full placeholder-gray-500 focus:outline-none focus:border-teal-400" />
          </div>
          <div>
            <label class="text-xs text-gray-400">Events (comma-sep)</label>
            <input v-model="form.events" placeholder="mo,sos,position"
              class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full placeholder-gray-500 focus:outline-none focus:border-teal-400" />
          </div>
        </div>
        <div class="flex items-center justify-between">
          <label class="flex items-center gap-2 text-sm text-gray-400">
            <input type="checkbox" v-model="form.active" class="rounded" /> Active
          </label>
          <button @click="createWebhook"
            class="bg-teal-600 hover:bg-teal-500 text-white px-4 py-2 rounded text-sm transition-colors">Create</button>
        </div>
      </div>

      <div class="overflow-x-auto">
        <table class="w-full border-collapse text-sm">
          <thead>
            <tr class="border-b border-gray-700 text-left text-gray-400">
              <th class="px-3 py-2">URL</th>
              <th class="px-3 py-2">Events</th>
              <th class="px-3 py-2">Status</th>
              <th class="px-3 py-2"></th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="h in hookList" :key="h.id" class="border-b border-gray-800 hover:bg-gray-800/50">
              <td class="px-3 py-2 font-mono text-xs break-all">{{ h.url }}</td>
              <td class="px-3 py-2">
                <span v-for="e in (h.events || [])" :key="e"
                  class="inline-block bg-gray-700 text-gray-300 text-xs px-1.5 py-0.5 rounded mr-1">{{ e }}</span>
              </td>
              <td class="px-3 py-2">
                <span v-if="h.active" class="text-green-400 text-xs">Active</span>
                <span v-else class="text-gray-500 text-xs">Disabled</span>
              </td>
              <td class="px-3 py-2 text-right">
                <button @click="deleteWebhook(h.id)"
                  class="bg-red-900 hover:bg-red-800 text-red-200 px-2 py-1 rounded text-xs transition-colors">Delete</button>
              </td>
            </tr>
            <tr v-if="hookList.length === 0 && !loading">
              <td colspan="4" class="px-3 py-8 text-center text-gray-500">No webhooks configured</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Delivery logs -->
    <div>
      <h2 class="text-lg font-semibold mb-3">Delivery Logs</h2>
      <div class="overflow-x-auto">
        <table class="w-full border-collapse text-sm">
          <thead>
            <tr class="border-b border-gray-700 text-left text-gray-400">
              <th class="px-3 py-2">Time</th>
              <th class="px-3 py-2">Webhook</th>
              <th class="px-3 py-2">Status</th>
              <th class="px-3 py-2">Latency</th>
              <th class="px-3 py-2">Error</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="l in logs" :key="l.timestamp + l.webhook_id" class="border-b border-gray-800 hover:bg-gray-800/50">
              <td class="px-3 py-2 text-gray-400 text-xs">{{ formatUTC(l.timestamp) }}</td>
              <td class="px-3 py-2 font-mono text-xs text-gray-400">{{ l.webhook_id }}</td>
              <td class="px-3 py-2">
                <span :class="statusCodeColor(l.status_code)" class="font-mono text-xs">{{ l.status_code }}</span>
              </td>
              <td class="px-3 py-2 text-gray-400 text-xs">{{ l.latency_ms }}ms</td>
              <td class="px-3 py-2 text-red-400 text-xs">{{ l.error || '—' }}</td>
            </tr>
            <tr v-if="logs.length === 0 && !loading">
              <td colspan="5" class="px-3 py-8 text-center text-gray-500">No delivery logs</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>
  </div>
</template>
