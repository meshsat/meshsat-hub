<script setup>
import { ref, onMounted, onUnmounted, computed } from 'vue'
import { reticulum } from '../api/client'
import EmptyState from '../components/EmptyState.vue'

const topology = ref(null)
const error = ref('')
const loading = ref(true)
let pollTimer = null

const defaultTopology = {
  hub: { dest_hash: '', app_name: '', role: '' },
  routes: [],
  interfaces: [],
  relay_stats: { forwarded: 0, dropped: 0, no_route: 0, rate_limited: 0 },
  path_stats: { requests_received: 0, responses_sent: 0, no_route: 0, deduplicated: 0 },
  hints_published: 0,
}

onMounted(async () => {
  await loadData()
  pollTimer = setInterval(loadData, 15000)
})

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

async function loadData() {
  try {
    topology.value = await reticulum.topology()
    error.value = ''
  } catch (e) {
    error.value = e.message
    if (!topology.value) topology.value = defaultTopology
  } finally {
    loading.value = false
  }
}

const hub = computed(() => topology.value?.hub || defaultTopology.hub)
const routeList = computed(() => topology.value?.routes || [])
const routeCount = computed(() => routeList.value.length)
const interfaces = computed(() => topology.value?.interfaces || [])
const relayStats = computed(() => topology.value?.relay_stats || defaultTopology.relay_stats)
const pathStats = computed(() => topology.value?.path_stats || defaultTopology.path_stats)
const hintsPublished = computed(() => topology.value?.hints_published || 0)

const freeRoutes = computed(() => routeList.value.filter(r => r.cost === 0))
const paidRoutes = computed(() => routeList.value.filter(r => r.cost > 0))
const totalRelayPackets = computed(() => {
  const s = relayStats.value
  return (s.forwarded || 0) + (s.dropped || 0) + (s.no_route || 0) + (s.rate_limited || 0)
})
const totalPathOps = computed(() => {
  const s = pathStats.value
  return (s.requests_received || 0) + (s.responses_sent || 0) + (s.no_route || 0) + (s.deduplicated || 0)
})

// Group routes by interface for topology visualization
const routesByInterface = computed(() => {
  const map = {}
  for (const r of routeList.value) {
    if (!map[r.interface]) map[r.interface] = []
    map[r.interface].push(r)
  }
  return map
})

// A cost is a fact, not a fault: paid paths are how satellite works, so the
// figure takes no alarm colour (ISA-101). Interfaces are named, not hued.
function costColor(cost) {
  return cost === 0 ? 'text-ms-muted' : 'text-ms-text'
}

function ifaceColor() {
  return 'bg-ms-well text-ms-text2 border border-ms-border'
}

function ifaceBorderColor() {
  return 'border-ms-border'
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
  <div class="ms-page max-w-7xl">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Topology</h1>
        <p class="ms-lede">The Hub's Reticulum node, its interfaces, and the nodes it can reach.</p>
      </div>
      <button @click="loadData" class="ms-btn-ghost text-xs">Refresh</button>
    </div>

    <div v-if="error" role="alert" class="ms-alert mb-4">{{ error }}</div>

    <div v-if="loading" class="text-gray-400 py-16 text-center">Loading network topology...</div>

    <template v-else>
      <!-- Hub Identity Card -->
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4 mb-6">
        <div class="flex items-center gap-3 mb-3">
          <div class="w-2 h-2 rounded-full bg-ms-success"></div>
          <h2 class="ms-h2 text-[15px]">Hub node</h2>
          <span class="text-xs text-gray-500 capitalize">{{ hub.role.replace(/_/g, ' ') || 'Transport node' }}</span>
        </div>
        <div v-if="hub.dest_hash" class="grid grid-cols-1 md:grid-cols-2 gap-4 text-sm">
          <div>
            <span class="text-gray-400 text-xs">Destination hash</span>
            <p class="font-mono text-ms-text break-all">{{ hub.dest_hash }}</p>
          </div>
          <div>
            <span class="text-gray-400 text-xs">App name</span>
            <p class="font-mono">{{ hub.app_name }}</p>
          </div>
        </div>
        <p v-else class="text-gray-500">Identity not loaded</p>
      </div>

      <!-- Stats Row -->
      <div class="grid grid-cols-2 sm:grid-cols-4 lg:grid-cols-8 gap-3 mb-6">
        <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4 text-center">
          <p class="text-2xl font-sans font-semibold ms-num">{{ routeCount }}</p>
          <p class="text-gray-400 text-xs">Known nodes</p>
        </div>
        <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4 text-center">
          <p class="text-2xl font-sans font-semibold ms-num">{{ freeRoutes.length }}</p>
          <p class="text-gray-400 text-xs">Free paths</p>
        </div>
        <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4 text-center">
          <p class="text-2xl font-sans font-semibold ms-num">{{ paidRoutes.length }}</p>
          <p class="text-gray-400 text-xs">Paid paths</p>
        </div>
        <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4 text-center">
          <p class="text-2xl font-sans font-semibold ms-num">{{ interfaces.length }}</p>
          <p class="text-gray-400 text-xs">Interfaces</p>
        </div>
        <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4 text-center">
          <p class="text-2xl font-sans font-semibold ms-num">{{ relayStats.forwarded || 0 }}</p>
          <p class="text-gray-400 text-xs">Forwarded</p>
        </div>
        <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4 text-center">
          <p class="text-2xl font-sans font-semibold ms-num" :class="(relayStats.dropped || 0) > 0 ? 'text-ms-warning' : 'text-ms-text'">
            {{ relayStats.dropped || 0 }}
          </p>
          <p class="text-gray-400 text-xs">Dropped</p>
        </div>
        <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4 text-center">
          <p class="text-2xl font-sans font-semibold ms-num">{{ pathStats.responses_sent || 0 }}</p>
          <p class="text-gray-400 text-xs">Path replies</p>
        </div>
        <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4 text-center">
          <p class="text-2xl font-sans font-semibold ms-num">{{ hintsPublished }}</p>
          <p class="text-gray-400 text-xs">Hints sent</p>
        </div>
      </div>

      <!-- Transport Interfaces -->
      <div class="mb-6">
        <h2 class="text-sm font-sans font-semibold text-gray-200 mb-3">Transport interfaces</h2>
        <EmptyState v-if="interfaces.length === 0" icon="globe" title="No interfaces"
          message="Reticulum transport interfaces will appear here when registered." />
        <div v-else class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-3">
          <div v-for="iface in interfaces" :key="iface.name"
            class="bg-tactical-surface rounded-lg border p-4" :class="ifaceBorderColor(iface.name)">
            <div class="flex items-center justify-between mb-2">
              <div class="flex items-center gap-2">
                <span :class="[ifaceColor(iface.name), 'px-2 py-0.5 rounded text-xs font-medium']">
                  {{ iface.name }}
                </span>
              </div>
              <span class="w-2 h-2 rounded-full" :class="iface.available ? 'bg-ms-success' : 'bg-ms-error'"></span>
            </div>
            <div class="grid grid-cols-3 gap-2 text-xs text-gray-400">
              <div>
                <span class="block text-gray-500">Cost</span>
                <span :class="costColor(iface.cost)">{{ iface.cost === 0 ? 'Free' : `$${iface.cost.toFixed(2)}` }}</span>
              </div>
              <div>
                <span class="block text-gray-500">MTU</span>
                <span>{{ iface.mtu }}B</span>
              </div>
              <div>
                <span class="block text-gray-500">Routes</span>
                <span>{{ (routesByInterface[iface.name] || []).length }}</span>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- Network Topology Visualization -->
      <div v-if="routeList.length > 0" class="mb-6">
        <h2 class="text-sm font-sans font-semibold text-gray-200 mb-3">Network map</h2>
        <div class="bg-tactical-surface rounded-lg border border-tactical-border p-6">
          <div class="flex items-center justify-center gap-8 flex-wrap">
            <!-- Hub node (center) -->
            <div class="flex flex-col items-center">
              <div class="w-16 h-16 rounded-full bg-ms-primary/10 border-2 border-ms-primary flex items-center justify-center text-ms-text font-semibold text-xs">
                HUB
              </div>
              <span class="text-xs text-gray-500 mt-1">Transport node</span>
            </div>

            <!-- Interface groups radiating from hub -->
            <div v-for="(routes, ifaceName) in routesByInterface" :key="ifaceName" class="flex flex-col items-center gap-2">
              <!-- Interface label -->
              <div class="flex items-center gap-1">
                <div class="w-8 border-t" :class="ifaceBorderColor(ifaceName).replace('border-', 'border-t-')"></div>
                <span :class="[ifaceColor(ifaceName), 'px-2 py-0.5 rounded text-[10px] font-medium']">{{ ifaceName }}</span>
              </div>

              <!-- Remote nodes -->
              <div class="flex flex-wrap gap-2 max-w-xs justify-center">
                <div v-for="route in routes.slice(0, 8)" :key="route.dest_hash"
                  class="group relative w-10 h-10 rounded-full bg-gray-700 border flex items-center justify-center text-[9px] font-mono text-gray-400 cursor-default"
                  :class="ifaceBorderColor(ifaceName)" :title="route.dest_hash">
                  {{ route.dest_hash.substring(0, 4) }}
                  <!-- Tooltip -->
                  <div class="absolute bottom-full mb-2 hidden group-hover:block bg-tactical-surface border border-gray-700 rounded px-2 py-1 text-xs whitespace-nowrap z-10">
                    {{ route.dest_hash.substring(0, 16) }}...
                    <br/>hops: {{ route.hops }} · {{ timeSince(route.last_seen) }}
                  </div>
                </div>
                <div v-if="routes.length > 8" class="w-10 h-10 rounded-full bg-gray-800/50 flex items-center justify-center text-[10px] text-gray-500">
                  +{{ routes.length - 8 }}
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- Routing Table -->
      <div class="bg-tactical-surface rounded-lg border border-tactical-border overflow-hidden">
        <div class="px-4 py-3 border-b border-tactical-border flex items-center justify-between">
          <h2 class="text-sm font-sans font-semibold text-gray-200">Routing table</h2>
          <span class="text-xs text-gray-500">{{ routeCount }} entries</span>
        </div>

        <EmptyState v-if="routeList.length === 0" icon="globe" title="No routes learned"
          message="Waiting for announces from field devices. Routes appear when Reticulum nodes announce their identity." />

        <div v-else class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead class="text-gray-400 text-left border-b border-tactical-border">
              <tr>
                <th class="px-4 py-2">Destination</th>
                <th class="px-4 py-2">Interface</th>
                <th class="px-4 py-2">Cost</th>
                <th class="px-4 py-2">Hops</th>
                <th class="px-4 py-2">Last seen</th>
                <th class="px-4 py-2">App data</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="route in routeList" :key="route.dest_hash"
                  class="border-b border-tactical-border/50 hover:bg-white/[0.02]">
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
      </div>

      <!-- Relay & Path Stats -->
      <div class="grid grid-cols-1 lg:grid-cols-2 gap-4 mt-4">
        <!-- Relay Stats -->
        <div v-if="totalRelayPackets > 0" class="bg-tactical-surface rounded-lg border border-tactical-border p-4">
          <h2 class="text-sm font-sans font-semibold text-gray-200 mb-3">Relay statistics</h2>
          <div class="grid grid-cols-2 gap-4 text-sm">
            <div class="flex items-center justify-between">
              <span class="text-gray-400">Forwarded</span>
              <span class="text-ms-success font-medium">{{ relayStats.forwarded || 0 }}</span>
            </div>
            <div class="flex items-center justify-between">
              <span class="text-gray-400">Dropped</span>
              <span class="text-ms-error font-medium">{{ relayStats.dropped || 0 }}</span>
            </div>
            <div class="flex items-center justify-between">
              <span class="text-gray-400">No route</span>
              <span class="text-ms-warning font-medium">{{ relayStats.no_route || 0 }}</span>
            </div>
            <div class="flex items-center justify-between">
              <span class="text-gray-400">Rate limited</span>
              <span class="text-gray-300 font-medium">{{ relayStats.rate_limited || 0 }}</span>
            </div>
          </div>
        </div>

        <!-- Path Discovery Stats -->
        <div v-if="totalPathOps > 0 || hintsPublished > 0" class="bg-tactical-surface rounded-lg border border-tactical-border p-4">
          <h2 class="text-sm font-sans font-semibold text-gray-200 mb-3">Path discovery</h2>
          <div class="grid grid-cols-2 gap-4 text-sm">
            <div class="flex items-center justify-between">
              <span class="text-gray-400">Requests</span>
              <span class="text-sky-400 font-medium">{{ pathStats.requests_received || 0 }}</span>
            </div>
            <div class="flex items-center justify-between">
              <span class="text-gray-400">Responses</span>
              <span class="text-ms-success font-medium">{{ pathStats.responses_sent || 0 }}</span>
            </div>
            <div class="flex items-center justify-between">
              <span class="text-gray-400">Deduplicated</span>
              <span class="text-gray-300 font-medium">{{ pathStats.deduplicated || 0 }}</span>
            </div>
            <div class="flex items-center justify-between">
              <span class="text-gray-400">Hints published</span>
              <span class="text-purple-400 font-medium">{{ hintsPublished }}</span>
            </div>
          </div>
        </div>
      </div>

      <p class="text-gray-500 text-xs mt-3">Auto-refreshes every 15 seconds. Routes expire after 30 minutes without announce refresh.</p>
    </template>
  </div>
</template>
