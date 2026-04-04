<script setup>
import { ref, onMounted, watch } from 'vue'
import { costs, devices } from '../api/client'
import EmptyState from '../components/EmptyState.vue'

const entries = ref([])
const summary = ref([])
const deviceList = ref([])
const loading = ref(true)
const error = ref('')

const filterDevice = ref('')
const filterFrom = ref('')
const filterTo = ref('')
const groupBy = ref('device')
const showSummary = ref(true)

onMounted(async () => {
  try {
    deviceList.value = await devices.list().catch(() => [])
  } catch { /* ignore */ }
  await loadData()
})

watch([filterDevice, filterFrom, filterTo, groupBy], loadData)

async function loadData() {
  loading.value = true
  error.value = ''
  try {
    const [e, s] = await Promise.all([
      costs.list(filterDevice.value, filterFrom.value, filterTo.value, 200).catch(() => []),
      costs.summary(groupBy.value, filterFrom.value, filterTo.value).catch(() => []),
    ])
    entries.value = Array.isArray(e) ? e : []
    summary.value = Array.isArray(s) ? s : []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function totalCost() {
  return summary.value.reduce((sum, s) => sum + (s.total_usd || 0), 0)
}

function totalMessages() {
  return summary.value.reduce((sum, s) => sum + (s.count || 0), 0)
}

function directionClass(dir) {
  return dir === 'mo' ? 'bg-teal-900/50 text-teal-300' : 'bg-amber-900/50 text-amber-300'
}

function formatDate(d) {
  if (!d) return '—'
  return new Date(d).toLocaleString()
}
</script>

<template>
  <div class="p-4 lg:p-6 max-w-6xl mx-auto">
    <h1 class="text-2xl font-display font-bold mb-6">Cost Tracking</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4 text-sm">{{ error }}</div>

    <!-- Summary cards -->
    <div class="grid grid-cols-2 sm:grid-cols-3 gap-4 mb-6">
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4">
        <div class="text-xs text-gray-500 uppercase tracking-wider mb-1">Total Cost</div>
        <div class="text-2xl font-display font-bold text-gray-100">${{ totalCost().toFixed(2) }}</div>
      </div>
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4">
        <div class="text-xs text-gray-500 uppercase tracking-wider mb-1">Messages</div>
        <div class="text-2xl font-display font-bold text-gray-100">{{ totalMessages() }}</div>
      </div>
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4">
        <div class="text-xs text-gray-500 uppercase tracking-wider mb-1">Avg / Message</div>
        <div class="text-2xl font-display font-bold text-gray-100">
          ${{ totalMessages() > 0 ? (totalCost() / totalMessages()).toFixed(3) : '0.00' }}
        </div>
      </div>
    </div>

    <!-- Filters -->
    <div class="flex flex-wrap items-end gap-3 mb-6">
      <div>
        <label class="block text-xs text-gray-500 mb-1">Device</label>
        <select v-model="filterDevice" class="bg-tactical-surface border border-gray-700 rounded px-3 py-2 text-sm">
          <option value="">All devices</option>
          <option v-for="d in deviceList" :key="d.imei" :value="d.imei">{{ d.label || d.imei }}</option>
        </select>
      </div>
      <div>
        <label class="block text-xs text-gray-500 mb-1">From</label>
        <input v-model="filterFrom" type="date" class="bg-tactical-surface border border-gray-700 rounded px-3 py-2 text-sm">
      </div>
      <div>
        <label class="block text-xs text-gray-500 mb-1">To</label>
        <input v-model="filterTo" type="date" class="bg-tactical-surface border border-gray-700 rounded px-3 py-2 text-sm">
      </div>
      <div>
        <label class="block text-xs text-gray-500 mb-1">Group by</label>
        <select v-model="groupBy" class="bg-tactical-surface border border-gray-700 rounded px-3 py-2 text-sm">
          <option value="device">Device</option>
          <option value="month">Month</option>
        </select>
      </div>
      <div class="flex items-center gap-2">
        <button @click="showSummary = !showSummary"
          class="text-xs px-3 py-2 rounded"
          :class="showSummary ? 'bg-teal-700 text-white' : 'bg-gray-700 text-gray-400'">
          {{ showSummary ? 'Summary' : 'Details' }}
        </button>
      </div>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>

    <template v-else>
      <!-- Summary table -->
      <div v-if="showSummary" class="bg-tactical-surface rounded-lg border border-tactical-border overflow-hidden mb-6">
        <div class="px-4 py-3 border-b border-tactical-border">
          <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider">
            Summary by {{ groupBy === 'device' ? 'Device' : 'Month' }} ({{ summary.length }})
          </h2>
        </div>
        <EmptyState v-if="summary.length === 0" icon="chart" title="No cost data" message="Cost entries are created when satellite messages are sent." />
        <table v-else class="w-full text-sm">
          <thead>
            <tr class="text-left text-xs text-gray-500 border-b border-tactical-border">
              <th class="px-4 py-2">{{ groupBy === 'device' ? 'Device' : 'Month' }}</th>
              <th class="px-4 py-2 text-right">Messages</th>
              <th class="px-4 py-2 text-right">Total Cost</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="s in summary" :key="s.group_key" class="border-b border-tactical-border/50 hover:bg-white/[0.02]">
              <td class="px-4 py-2 font-mono text-xs text-gray-300">{{ s.group_key }}</td>
              <td class="px-4 py-2 text-right text-gray-400">{{ s.count }}</td>
              <td class="px-4 py-2 text-right text-gray-200 font-medium">${{ (s.total_usd || 0).toFixed(2) }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- Detail table -->
      <div v-if="!showSummary" class="bg-tactical-surface rounded-lg border border-tactical-border overflow-hidden">
        <div class="px-4 py-3 border-b border-tactical-border">
          <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider">Entries ({{ entries.length }})</h2>
        </div>
        <EmptyState v-if="entries.length === 0" icon="chart" title="No cost entries" message="Cost entries are created when satellite messages are sent." />
        <table v-else class="w-full text-sm">
          <thead>
            <tr class="text-left text-xs text-gray-500 border-b border-tactical-border">
              <th class="px-4 py-2">Device</th>
              <th class="px-4 py-2">Interface</th>
              <th class="px-4 py-2">Direction</th>
              <th class="px-4 py-2 text-right">Cost</th>
              <th class="px-4 py-2">Date</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="e in entries" :key="e.id" class="border-b border-tactical-border/50 hover:bg-white/[0.02]">
              <td class="px-4 py-2 font-mono text-xs text-gray-300">{{ e.device_imei }}</td>
              <td class="px-4 py-2 text-xs text-gray-400">{{ e.interface_type }}</td>
              <td class="px-4 py-2">
                <span class="text-xs px-1.5 py-0.5 rounded uppercase" :class="directionClass(e.direction)">{{ e.direction }}</span>
              </td>
              <td class="px-4 py-2 text-right text-gray-200">${{ (e.cost_usd || 0).toFixed(3) }}</td>
              <td class="px-4 py-2 text-gray-500 text-xs">{{ formatDate(e.created_at) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
  </div>
</template>
