<script setup>
import { ref, onMounted, onUnmounted, computed } from 'vue'
import { reticulum } from '../api/client'

const identity = ref(null)
const routeData = ref({ count: 0, routes: [] })
const error = ref('')
const loading = ref(true)
let pollTimer = null

onMounted(async () => {
  await loadData()
  pollTimer = setInterval(loadData, 30000)
})

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

async function loadData() {
  try {
    const [id, rt] = await Promise.all([
      reticulum.identity().catch(() => null),
      reticulum.routes().catch(() => ({ count: 0, routes: [] })),
    ])
    identity.value = id
    routeData.value = rt
    error.value = ''
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

const freeRoutes = computed(() => routeData.value.routes.filter(r => r.cost === 0))
const paidRoutes = computed(() => routeData.value.routes.filter(r => r.cost > 0))

function costColor(cost) {
  if (cost === 0) return 'text-green-400'
  if (cost <= 0.02) return 'text-yellow-400'
  return 'text-red-400'
}

function ifaceColor(iface) {
  const colors = {
    mqtt: 'bg-green-700',
    tor: 'bg-purple-700',
    wireguard: 'bg-blue-700',
    iridium: 'bg-red-700',
    astrocast: 'bg-orange-700',
    globalstar: 'bg-yellow-700',
  }
  return colors[iface] || 'bg-gray-700'
}

function timeSince(iso) {
  if (!iso) return '—'
  const ms = Date.now() - new Date(iso).getTime()
  const sec = Math.floor(ms / 1000)
  if (sec < 60) return `${sec}s ago`
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min}m ago`
  return `${Math.floor(min / 60)}h ago`
}
</script>

<template>
  <div class="p-6 max-w-6xl mx-auto">
    <h1 class="text-2xl font-bold mb-6">Reticulum Topology</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 rounded p-3 mb-4">{{ error }}</div>

    <div v-if="loading" class="text-gray-400">Loading...</div>

    <template v-else>
      <!-- Hub Identity Card -->
      <div class="bg-gray-800 rounded-lg border border-gray-700 p-4 mb-6">
        <h2 class="text-lg font-semibold mb-3">Hub Identity</h2>
        <div v-if="identity" class="grid grid-cols-1 md:grid-cols-3 gap-4 text-sm">
          <div>
            <span class="text-gray-400">Dest Hash</span>
            <p class="font-mono text-green-400 break-all">{{ identity.dest_hash }}</p>
          </div>
          <div>
            <span class="text-gray-400">App Name</span>
            <p class="font-mono">{{ identity.app_name }}</p>
          </div>
          <div>
            <span class="text-gray-400">Public Key</span>
            <p class="font-mono text-xs text-gray-300 break-all truncate" :title="identity.public_key_hex">
              {{ identity.public_key_hex?.substring(0, 32) }}...
            </p>
          </div>
        </div>
        <p v-else class="text-gray-500">Identity not loaded</p>
      </div>

      <!-- Stats Row -->
      <div class="grid grid-cols-2 md:grid-cols-4 gap-4 mb-6">
        <div class="bg-gray-800 rounded-lg border border-gray-700 p-4 text-center">
          <p class="text-3xl font-bold">{{ routeData.count }}</p>
          <p class="text-gray-400 text-sm">Known Nodes</p>
        </div>
        <div class="bg-gray-800 rounded-lg border border-gray-700 p-4 text-center">
          <p class="text-3xl font-bold text-green-400">{{ freeRoutes.length }}</p>
          <p class="text-gray-400 text-sm">Free Paths</p>
        </div>
        <div class="bg-gray-800 rounded-lg border border-gray-700 p-4 text-center">
          <p class="text-3xl font-bold text-red-400">{{ paidRoutes.length }}</p>
          <p class="text-gray-400 text-sm">Paid Paths</p>
        </div>
        <div class="bg-gray-800 rounded-lg border border-gray-700 p-4 text-center">
          <p class="text-3xl font-bold text-teal-400">
            {{ new Set(routeData.routes.map(r => r.interface)).size }}
          </p>
          <p class="text-gray-400 text-sm">Active Interfaces</p>
        </div>
      </div>

      <!-- Route Table -->
      <div class="bg-gray-800 rounded-lg border border-gray-700 overflow-hidden">
        <div class="px-4 py-3 border-b border-gray-700 flex items-center justify-between">
          <h2 class="text-lg font-semibold">Routing Table</h2>
          <button @click="loadData" class="text-sm text-teal-400 hover:text-teal-300">Refresh</button>
        </div>

        <div v-if="routeData.routes.length === 0" class="p-8 text-center text-gray-500">
          No routes learned yet. Waiting for announces from field devices.
        </div>

        <table v-else class="w-full text-sm">
          <thead class="text-gray-400 text-left border-b border-gray-700">
            <tr>
              <th class="px-4 py-2">Destination</th>
              <th class="px-4 py-2">Interface</th>
              <th class="px-4 py-2">Cost</th>
              <th class="px-4 py-2">Hops</th>
              <th class="px-4 py-2">Last Seen</th>
              <th class="px-4 py-2">App Data</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="route in routeData.routes" :key="route.dest_hash"
                class="border-b border-gray-700/50 hover:bg-gray-700/30">
              <td class="px-4 py-2 font-mono text-xs">{{ route.dest_hash }}</td>
              <td class="px-4 py-2">
                <span :class="[ifaceColor(route.interface), 'px-2 py-0.5 rounded text-xs font-medium']">
                  {{ route.interface }}
                </span>
              </td>
              <td class="px-4 py-2" :class="costColor(route.cost)">
                {{ route.cost === 0 ? 'Free' : `$${route.cost.toFixed(2)}` }}
              </td>
              <td class="px-4 py-2">{{ route.hops }}</td>
              <td class="px-4 py-2 text-gray-400">{{ timeSince(route.last_seen) }}</td>
              <td class="px-4 py-2 text-gray-400 text-xs truncate max-w-[200px]">{{ route.app_data || '—' }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <p class="text-gray-500 text-xs mt-3">Auto-refreshes every 30 seconds. Routes expire after 30 minutes without announce refresh.</p>
    </template>
  </div>
</template>
