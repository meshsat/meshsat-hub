<script setup>
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { tak, bridges, integrations } from '../api/client'

const activeTab = ref('overview')
const tabs = [
  { id: 'overview', label: 'Overview' },
  { id: 'missions', label: 'Missions' },
  { id: 'fleet', label: 'Fleet CoT' },
  { id: 'federation', label: 'Federation' },
  { id: 'chat', label: 'GeoChat' },
]

const loading = ref(true)
const fleetStatus = ref(null)
const fedPeers = ref({ enabled: false, peers: [], total_in: 0, total_out: 0, peer_count: 0 })
const missions = ref([])
const bridgeList = ref([])
const intList = ref([])
const chatMessages = ref([])
const chatInput = ref('')
let pollTimer = null

const takInt = computed(() => intList.value.find(i => i.name && i.name.includes('TAK') && !i.name.includes('Federation')))
const fedInt = computed(() => intList.value.find(i => i.name && i.name.includes('Federation')))

onMounted(async () => {
  await loadAll()
  pollTimer = setInterval(loadAll, 30000)
})

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

async function loadAll() {
  const results = await Promise.allSettled([
    tak.fleetStatus(),
    tak.federationPeers(),
    tak.missions(),
    bridges.list(),
    integrations.list(),
  ])
  fleetStatus.value = results[0].status === 'fulfilled' ? results[0].value : null
  fedPeers.value = results[1].status === 'fulfilled' ? results[1].value : fedPeers.value
  missions.value = results[2].status === 'fulfilled' && Array.isArray(results[2].value) ? results[2].value : []
  bridgeList.value = results[3].status === 'fulfilled' && Array.isArray(results[3].value) ? results[3].value : []
  intList.value = results[4].status === 'fulfilled' && Array.isArray(results[4].value) ? results[4].value : []
  loading.value = false
}

function cotTypeName(t) {
  if (!t) return '—'
  if (t.includes('U-C-I')) return 'Infrastructure'
  if (t.includes('E-S')) return 'Sensor'
  if (t.includes('E-C')) return 'Comms'
  if (t.includes('U-C')) return 'Ground Unit'
  return t
}
</script>

<template>
<div class="p-4 lg:p-6 max-w-6xl mx-auto">
  <div class="flex items-center justify-between mb-6">
    <div>
      <h1 class="text-2xl font-display font-bold">TAK Operations Center</h1>
      <p class="text-gray-400 text-sm mt-1">Cursor on Target gateway, missions, and federation</p>
    </div>
    <button @click="loadAll" class="px-3 py-1.5 rounded text-xs bg-tactical-surface border border-tactical-border text-gray-300 hover:text-gray-100 transition-colors">Refresh</button>
  </div>

  <div v-if="loading" class="text-center text-gray-500 py-16">Loading TAK status...</div>

  <template v-else>
    <!-- Status Cards -->
    <div class="grid grid-cols-2 sm:grid-cols-4 gap-3 mb-6">
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-3">
        <div class="text-gray-500 text-[10px] uppercase">CoT Gateway</div>
        <div class="flex items-center gap-1.5 mt-1">
          <span class="w-2 h-2 rounded-full" :class="fleetStatus?.tak_enabled ? 'bg-emerald-400' : 'bg-red-400'"></span>
          <span class="text-sm" :class="fleetStatus?.tak_enabled ? 'text-emerald-400' : 'text-red-400'">
            {{ fleetStatus?.tak_enabled ? 'active' : 'disabled' }}
          </span>
        </div>
      </div>
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-3">
        <div class="text-gray-500 text-[10px] uppercase">Federation Peers</div>
        <div class="text-xl font-display font-bold text-gray-200 mt-1">{{ fedPeers.peer_count }}</div>
      </div>
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-3">
        <div class="text-gray-500 text-[10px] uppercase">Missions</div>
        <div class="text-xl font-display font-bold text-gray-200 mt-1">{{ missions.length }}</div>
      </div>
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-3">
        <div class="text-gray-500 text-[10px] uppercase">TAK Host</div>
        <div class="text-sm text-gray-300 font-mono truncate mt-1">{{ fleetStatus?.tak_host || '—' }}</div>
      </div>
    </div>

    <!-- Tabbed Content -->
    <div class="bg-tactical-surface rounded-lg border border-tactical-border overflow-hidden">
      <div class="flex border-b border-tactical-border">
        <button v-for="tab in tabs" :key="tab.id" @click="activeTab = tab.id"
          class="px-4 py-2.5 text-sm font-medium transition-colors"
          :class="activeTab === tab.id ? 'text-teal-400 border-b-2 border-teal-400' : 'text-gray-500 hover:text-gray-300'">
          {{ tab.label }}
        </button>
      </div>

      <!-- OVERVIEW -->
      <div v-if="activeTab === 'overview'" class="p-4 space-y-4">
        <!-- TAK Server Connection -->
        <div>
          <h3 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">TAK Server</h3>
          <div v-if="takInt?.config" class="grid grid-cols-2 md:grid-cols-3 gap-x-6 gap-y-1.5">
            <div v-for="(v, k) in takInt.config" :key="k" class="flex items-center justify-between text-xs">
              <span class="text-gray-500">{{ k.replace(/_/g, ' ') }}</span>
              <span class="font-mono" :class="v === 'configured' ? 'text-emerald-400' : v === 'not set' ? 'text-amber-400' : 'text-gray-300'">{{ v }}</span>
            </div>
          </div>
          <p v-else class="text-xs text-gray-500">TAK not configured. Set <code class="text-teal-400">HUB_TAK_ENABLED=true</code>.</p>
        </div>

        <!-- Federation Throughput -->
        <div v-if="fleetStatus?.federation_enabled" class="border-t border-tactical-border/50 pt-4">
          <h3 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">Federation Throughput</h3>
          <div class="grid grid-cols-3 gap-3">
            <div class="text-center">
              <div class="text-lg font-display font-bold text-teal-400">{{ fleetStatus?.federation_in ?? 0 }}</div>
              <div class="text-[10px] text-gray-500 uppercase">CoT In</div>
            </div>
            <div class="text-center">
              <div class="text-lg font-display font-bold text-sky-400">{{ fleetStatus?.federation_out ?? 0 }}</div>
              <div class="text-[10px] text-gray-500 uppercase">CoT Out</div>
            </div>
            <div class="text-center">
              <div class="text-lg font-display font-bold text-gray-200">{{ fleetStatus?.federation_peers ?? 0 }}</div>
              <div class="text-[10px] text-gray-500 uppercase">Peers</div>
            </div>
          </div>
        </div>

        <!-- CoT Type Legend -->
        <div class="border-t border-tactical-border/50 pt-4">
          <h3 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">CoT Type Legend</h3>
          <div class="grid grid-cols-2 sm:grid-cols-3 gap-1.5 text-xs">
            <span class="text-sky-400">&#9670; PLI — Friendly Ground (a-f-G)</span>
            <span class="text-teal-400">&#9679; Chat — GeoChat (b-t-f)</span>
            <span class="text-red-400">&#10006; SOS — Emergency (b-a)</span>
            <span class="text-amber-400">&#9679; Waypoint (b-m-p)</span>
            <span class="text-purple-400">&#9679; Sensor (t-x-d-d)</span>
            <span class="text-red-400">&#9670; Hostile (a-h-G)</span>
          </div>
        </div>
      </div>

      <!-- MISSIONS -->
      <div v-if="activeTab === 'missions'">
        <div v-if="!fleetStatus?.tak_enabled" class="p-8 text-center text-gray-500 text-sm">
          TAK gateway disabled — enable to view missions
        </div>
        <div v-else-if="missions.length === 0" class="p-8 text-center text-gray-500 text-sm">No missions on TAK server</div>
        <table v-else class="w-full text-sm">
          <thead class="text-gray-400 text-left border-b border-tactical-border">
            <tr>
              <th class="px-4 py-2">Name</th>
              <th class="px-4 py-2">Description</th>
              <th class="px-4 py-2">Tool</th>
              <th class="px-4 py-2">Created</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-tactical-border/50">
            <tr v-for="m in missions" :key="m.name" class="hover:bg-white/[0.02]">
              <td class="px-4 py-2 text-gray-200 font-mono">{{ m.name }}</td>
              <td class="px-4 py-2 text-gray-400 text-xs">{{ m.description || '—' }}</td>
              <td class="px-4 py-2 text-gray-400 text-xs">{{ m.tool || '—' }}</td>
              <td class="px-4 py-2 text-gray-500 text-xs">{{ m.createTime || '—' }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- FLEET COT -->
      <div v-if="activeTab === 'fleet'">
        <div v-if="!fleetStatus?.tak_enabled" class="p-8 text-center text-gray-500 text-sm">
          TAK gateway disabled — devices not forwarded to TAK
        </div>
        <div v-else-if="bridgeList.length === 0" class="p-8 text-center text-gray-500 text-sm">No bridges registered</div>
        <table v-else class="w-full text-sm">
          <thead class="text-gray-400 text-left border-b border-tactical-border">
            <tr>
              <th class="px-4 py-2">Bridge</th>
              <th class="px-4 py-2">Callsign</th>
              <th class="px-4 py-2">CoT Type</th>
              <th class="px-4 py-2">Status</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-tactical-border/50">
            <tr v-for="b in bridgeList" :key="b.id" class="hover:bg-white/[0.02]">
              <td class="px-4 py-2 text-gray-200">{{ b.hostname || b.id }}</td>
              <td class="px-4 py-2">
                <span v-if="b.cot_callsign" class="px-1.5 py-0.5 rounded text-xs bg-sky-900/50 text-sky-300 border border-sky-700/50 font-mono">{{ b.cot_callsign }}</span>
                <span v-else class="text-gray-600 text-xs">—</span>
              </td>
              <td class="px-4 py-2 text-gray-400 text-xs">{{ cotTypeName(b.cot_type) }}</td>
              <td class="px-4 py-2">
                <span class="flex items-center gap-1.5">
                  <span class="w-2 h-2 rounded-full" :class="b.online ? 'bg-emerald-400' : 'bg-gray-600'"></span>
                  <span class="text-xs" :class="b.online ? 'text-emerald-400' : 'text-gray-500'">{{ b.online ? 'Online' : 'Offline' }}</span>
                </span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- FEDERATION -->
      <div v-if="activeTab === 'federation'" class="p-4">
        <div v-if="!fleetStatus?.federation_enabled" class="text-center text-gray-500 py-8 text-sm">
          Federation v2 disabled. Set <code class="text-teal-400">HUB_TAK_FEDERATION_ENABLED=true</code> and configure peers.
        </div>
        <div v-else class="space-y-4">
          <!-- Summary -->
          <div class="grid grid-cols-3 gap-3">
            <div class="bg-gray-800/30 rounded-lg border border-tactical-border/50 p-3 text-center">
              <div class="text-gray-500 text-[10px] uppercase">Connected Peers</div>
              <div class="text-xl font-display font-bold text-gray-200 mt-1">{{ fedPeers.peer_count }}</div>
            </div>
            <div class="bg-gray-800/30 rounded-lg border border-tactical-border/50 p-3 text-center">
              <div class="text-gray-500 text-[10px] uppercase">Messages In</div>
              <div class="text-xl font-display font-bold text-teal-400 mt-1">{{ fedPeers.total_in }}</div>
            </div>
            <div class="bg-gray-800/30 rounded-lg border border-tactical-border/50 p-3 text-center">
              <div class="text-gray-500 text-[10px] uppercase">Messages Out</div>
              <div class="text-xl font-display font-bold text-sky-400 mt-1">{{ fedPeers.total_out }}</div>
            </div>
          </div>

          <!-- Peer List -->
          <h3 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider">Peer Connections</h3>
          <div v-if="!fedPeers.peers || fedPeers.peers.length === 0" class="text-center text-gray-500 py-4 text-sm">No peers connected</div>
          <div v-else class="space-y-2">
            <div v-for="p in fedPeers.peers" :key="p.address"
              class="bg-gray-800/30 rounded-lg border border-tactical-border/50 p-3 flex items-center justify-between">
              <div class="flex items-center gap-2">
                <span class="w-2 h-2 rounded-full" :class="p.connected ? 'bg-emerald-400' : 'bg-red-400'"></span>
                <span class="font-mono text-sm text-gray-300">{{ p.address }}</span>
              </div>
              <div class="flex items-center gap-4 text-xs text-gray-500">
                <span v-if="p.msgs_in !== undefined">in: {{ p.msgs_in }}</span>
                <span v-if="p.msgs_out !== undefined">out: {{ p.msgs_out }}</span>
              </div>
            </div>
          </div>

          <!-- Federation Config -->
          <div v-if="fedInt?.config" class="border-t border-tactical-border/50 pt-4">
            <h3 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">Federation Config</h3>
            <div class="grid grid-cols-2 gap-x-6 gap-y-1.5">
              <div v-for="(v, k) in fedInt.config" :key="k" class="flex items-center justify-between text-xs">
                <span class="text-gray-500">{{ k.replace(/_/g, ' ') }}</span>
                <span class="font-mono" :class="v === 'configured' ? 'text-emerald-400' : (v === 'not set' || (typeof v === 'string' && v.startsWith('missing'))) ? 'text-amber-400' : 'text-gray-300'">{{ v }}</span>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- GEOCHAT -->
      <div v-if="activeTab === 'chat'" class="p-4">
        <div class="space-y-2 max-h-80 overflow-y-auto mb-3">
          <div v-for="(m, i) in chatMessages" :key="i" class="flex gap-2">
            <span class="text-xs text-teal-400 font-mono whitespace-nowrap">{{ m.callsign || '?' }}</span>
            <span class="text-xs text-gray-300">{{ m.text }}</span>
          </div>
          <p v-if="!chatMessages.length" class="text-center text-gray-500 py-8 text-sm">
            No fleet chat messages. GeoChat events (<code class="text-teal-400">b-t-f</code>) from TAK clients will appear here.
          </p>
        </div>
        <div class="flex gap-2 border-t border-tactical-border/50 pt-3">
          <input v-model="chatInput" placeholder="Type a message..."
            class="flex-1 px-3 py-2 rounded bg-tactical-bg border border-tactical-border text-sm text-gray-200 placeholder-gray-600 focus:outline-none focus:border-teal-500">
          <button class="px-4 py-2 rounded text-xs font-medium bg-teal-600 text-white hover:bg-teal-500 transition-colors">Send</button>
        </div>
      </div>
    </div>
  </template>
</div>
</template>
