<script setup>
import { ref, onMounted } from 'vue'
import { mptcp, constellations } from '../api/client'
import { formatUTC } from '../utils/time'

const mptcpStatus = ref(null)
const backends = ref([])
const error = ref('')
const loading = ref(true)

onMounted(async () => {
  await loadData()
})

async function loadData() {
  loading.value = true
  try {
    const [ms, cs] = await Promise.all([
      mptcp.status().catch(() => null),
      constellations.list().catch(() => ({ backends: [] })),
    ])
    mptcpStatus.value = ms
    backends.value = cs.backends || []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function setStrategy(strategy) {
  error.value = ''
  try {
    await mptcp.setStrategy(strategy)
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

function pathTypeColor(type) {
  switch (type) {
    case 'satellite': return 'text-purple-400'
    case 'cellular': return 'text-yellow-400'
    case 'wifi': return 'text-cyan-400'
    case 'ethernet': return 'text-green-400'
    default: return 'text-gray-400'
  }
}

function pathTypeBg(type) {
  switch (type) {
    case 'satellite': return 'bg-purple-900/50'
    case 'cellular': return 'bg-yellow-900/50'
    case 'wifi': return 'bg-cyan-900/50'
    case 'ethernet': return 'bg-green-900/50'
    default: return 'bg-gray-800'
  }
}

function subflowStatusColor(status) {
  if (status === 'active') return 'text-green-400'
  if (status === 'backup') return 'text-cyan-400'
  if (status === 'degraded') return 'text-yellow-400'
  if (status === 'down') return 'text-red-400'
  return 'text-gray-400'
}

function formatBytes(bytes) {
  if (!bytes || bytes < 1024) return (bytes || 0) + ' B'
  if (bytes < 1048576) return (bytes / 1024).toFixed(1) + ' KB'
  if (bytes < 1073741824) return (bytes / 1048576).toFixed(1) + ' MB'
  return (bytes / 1073741824).toFixed(1) + ' GB'
}
</script>

<template>
  <div>
    <h1 class="text-xl font-bold mb-4">Network</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">{{ error }}</div>

    <!-- Satellite Constellations -->
    <div class="mb-8">
      <h2 class="text-lg font-semibold mb-3">Satellite Constellations</h2>
      <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
        <div v-for="b in backends" :key="b" class="bg-gray-800 rounded-lg p-4">
          <div class="flex items-center gap-2 mb-1">
            <div class="w-2 h-2 rounded-full bg-green-400"></div>
            <span class="font-medium capitalize">{{ b }}</span>
          </div>
          <div class="text-xs text-gray-400">
            <span v-if="b === 'iridium'">270 byte MT / $0.05 per msg</span>
            <span v-else-if="b === 'astrocast'">160 byte MT / $0.01 per msg</span>
            <span v-else>Available</span>
          </div>
        </div>
        <div v-if="backends.length === 0 && !loading" class="text-gray-500 text-sm py-4 col-span-full">No constellation backends registered</div>
      </div>
    </div>

    <!-- MPTCP Concentrator -->
    <div class="mb-8">
      <h2 class="text-lg font-semibold mb-3">MPTCP Concentrator</h2>

      <div v-if="mptcpStatus" class="mb-4">
        <!-- Status cards -->
        <div class="grid grid-cols-1 sm:grid-cols-3 gap-4 mb-4">
          <div class="bg-gray-800 rounded-lg p-4">
            <div class="text-gray-400 text-sm mb-1">Kernel MPTCP</div>
            <div class="text-lg font-bold" :class="mptcpStatus.available ? 'text-green-400' : 'text-red-400'">
              {{ mptcpStatus.available ? 'Available' : 'Not Available' }}
            </div>
          </div>
          <div class="bg-gray-800 rounded-lg p-4">
            <div class="text-gray-400 text-sm mb-1">Status</div>
            <div class="text-lg font-bold" :class="mptcpStatus.enabled ? 'text-green-400' : 'text-gray-500'">
              {{ mptcpStatus.enabled ? 'Enabled' : 'Disabled' }}
            </div>
          </div>
          <div class="bg-gray-800 rounded-lg p-4">
            <div class="text-gray-400 text-sm mb-1">Strategy</div>
            <div class="flex items-center gap-2">
              <select :value="mptcpStatus.strategy" @change="setStrategy($event.target.value)"
                class="bg-gray-700 border border-gray-600 px-3 py-1 rounded text-gray-100 text-sm focus:outline-none focus:border-cyan-400">
                <option value="failover">Failover</option>
                <option value="aggregate">Aggregate</option>
                <option value="redundant">Redundant</option>
              </select>
            </div>
          </div>
        </div>

        <!-- Subflows -->
        <div v-if="mptcpStatus.subflows && mptcpStatus.subflows.length > 0">
          <h3 class="text-sm font-semibold text-gray-400 mb-2">Subflows</h3>
          <div class="grid grid-cols-1 md:grid-cols-2 gap-3">
            <div v-for="sf in mptcpStatus.subflows" :key="sf.id" class="rounded-lg p-4 border border-gray-700" :class="pathTypeBg(sf.path_type)">
              <div class="flex items-center justify-between mb-2">
                <div class="flex items-center gap-2">
                  <span :class="pathTypeColor(sf.path_type)" class="text-sm font-medium capitalize">{{ sf.path_type }}</span>
                  <span class="text-gray-400 text-xs font-mono">{{ sf.interface }}</span>
                </div>
                <span :class="subflowStatusColor(sf.status)" class="text-xs uppercase font-medium">{{ sf.status }}</span>
              </div>
              <div class="grid grid-cols-3 gap-2 text-xs text-gray-400">
                <div>
                  <div class="text-gray-500">Address</div>
                  <div class="font-mono">{{ sf.local_addr }}</div>
                </div>
                <div>
                  <div class="text-gray-500">TX / RX</div>
                  <div>{{ formatBytes(sf.bytes_sent) }} / {{ formatBytes(sf.bytes_recv) }}</div>
                </div>
                <div>
                  <div class="text-gray-500">RTT</div>
                  <div>{{ sf.rtt_ms || '—' }}ms</div>
                </div>
              </div>
            </div>
          </div>
        </div>
        <div v-else class="text-gray-500 text-sm">No active subflows</div>

        <div v-if="mptcpStatus.updated_at" class="text-xs text-gray-500 mt-3">
          Last updated: {{ formatUTC(mptcpStatus.updated_at) }}
        </div>
      </div>

      <div v-else-if="!loading" class="text-gray-500 text-sm">MPTCP status unavailable</div>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>
  </div>
</template>
