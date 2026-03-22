<script setup>
import { ref, computed, onMounted } from 'vue'
import { mptcp, constellations } from '../api/client'
import { formatUTC } from '../utils/time'
import EmptyState from '../components/EmptyState.vue'

const mptcpStatus = ref(null)
const backends = ref([])
const error = ref('')
const loading = ref(true)

const knownConstellations = {
  iridium:    { name: 'Iridium',    mtu: '270 byte MT / 340 byte MO', cost: '$0.05 per msg' },
  astrocast:  { name: 'Astrocast',  mtu: '160 byte MT / 160 byte MO', cost: '$0.01 per msg' },
  globalstar: { name: 'Globalstar', mtu: '9 byte MT / 9 byte MO',     cost: '$0.25 per msg' },
}

const allConstellations = computed(() => {
  const active = new Set(backends.value.map(b => b.toLowerCase()))
  const result = []
  // Add all active backends first (preserving API order)
  for (const b of backends.value) {
    const key = b.toLowerCase()
    const info = knownConstellations[key]
    result.push({
      key,
      name: info ? info.name : b.charAt(0).toUpperCase() + b.slice(1),
      mtu: info ? info.mtu : null,
      cost: info ? info.cost : null,
      active: true,
    })
  }
  // Then add known constellations that are not active
  for (const [key, info] of Object.entries(knownConstellations)) {
    if (!active.has(key)) {
      result.push({
        key,
        name: info.name,
        mtu: info.mtu,
        cost: info.cost,
        active: false,
      })
    }
  }
  return result
})

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
    case 'wifi': return 'text-teal-400'
    case 'ethernet': return 'text-green-400'
    default: return 'text-gray-400'
  }
}

function pathTypeBg(type) {
  switch (type) {
    case 'satellite': return 'bg-purple-900/50'
    case 'cellular': return 'bg-yellow-900/50'
    case 'wifi': return 'bg-teal-900/50'
    case 'ethernet': return 'bg-green-900/50'
    default: return 'bg-gray-800'
  }
}

function subflowStatusColor(status) {
  if (status === 'active') return 'text-green-400'
  if (status === 'backup') return 'text-teal-400'
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
    <h1 class="text-2xl font-display font-bold mb-4">Network</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">{{ error }}</div>

    <!-- Satellite Constellations -->
    <div class="mb-8">
      <h2 class="text-lg font-semibold mb-3 uppercase tracking-wider">Satellite Constellations</h2>
      <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
        <div v-for="c in allConstellations" :key="c.key" class="bg-tactical-surface rounded-lg p-4" :class="{ 'opacity-60': !c.active }">
          <div class="flex items-center justify-between mb-2">
            <div class="flex items-center gap-2">
              <div class="w-2 h-2 rounded-full" :class="c.active ? 'bg-green-400' : 'bg-gray-600'"></div>
              <span class="font-medium">{{ c.name }}</span>
            </div>
            <span class="text-xs px-2 py-0.5 rounded-full" :class="c.active ? 'bg-green-900/50 text-green-400' : 'bg-gray-800 text-gray-500'">
              {{ c.active ? 'Active' : 'Not configured' }}
            </span>
          </div>
          <div class="text-xs text-gray-400 space-y-0.5">
            <div v-if="c.mtu">{{ c.mtu }}</div>
            <div v-if="c.cost">{{ c.cost }}</div>
          </div>
        </div>
        <EmptyState v-if="allConstellations.length === 0 && !loading" icon="satellite" title="No constellations" message="No satellite constellation backends are configured." class="col-span-full" />
      </div>
    </div>

    <!-- MPTCP Concentrator -->
    <div class="mb-8">
      <h2 class="text-lg font-semibold mb-3 uppercase tracking-wider">MPTCP Concentrator</h2>

      <div v-if="mptcpStatus" class="mb-4">
        <!-- Status cards -->
        <div class="grid grid-cols-1 sm:grid-cols-3 gap-4 mb-4">
          <div class="bg-tactical-surface rounded-lg p-4">
            <div class="text-gray-400 text-sm mb-1">Kernel MPTCP</div>
            <div class="text-lg font-bold" :class="mptcpStatus.available ? 'text-green-400' : 'text-red-400'">
              {{ mptcpStatus.available ? 'Available' : 'Not Available' }}
            </div>
          </div>
          <div class="bg-tactical-surface rounded-lg p-4">
            <div class="text-gray-400 text-sm mb-1">Status</div>
            <div class="text-lg font-bold" :class="mptcpStatus.enabled ? 'text-green-400' : 'text-gray-500'">
              {{ mptcpStatus.enabled ? 'Enabled' : 'Disabled' }}
            </div>
          </div>
          <div class="bg-tactical-surface rounded-lg p-4">
            <div class="text-gray-400 text-sm mb-1">Strategy</div>
            <div class="flex items-center gap-2">
              <select :value="mptcpStatus.strategy" @change="setStrategy($event.target.value)"
                class="bg-gray-800 border border-gray-700 px-3 py-1 rounded text-gray-100 text-sm focus:outline-none focus:border-teal-500">
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
