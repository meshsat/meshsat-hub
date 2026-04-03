<script setup>
import { ref, computed, onMounted } from 'vue'
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
const fedPeers = ref({ enabled: false, peers: [], total_in: 0, total_out: 0, peer_count: 0 })
const missions = ref([])
const bridgeList = ref([])
const intList = ref([])
const chatMessages = ref([])
const chatInput = ref('')

const takInt = computed(() => intList.value.find(i => i.name && i.name.includes('TAK') && !i.name.includes('Federation')))
const fedInt = computed(() => intList.value.find(i => i.name && i.name.includes('Federation')))
const takBridges = computed(() => bridgeList.value.filter(b => b.cot_callsign || b.cot_type))

async function loadAll() {
  loading.value = true
  const results = await Promise.allSettled([
    tak.federationPeers(),
    tak.missions(),
    bridges.list(),
    integrations.list(),
  ])
  fedPeers.value = results[0].status === 'fulfilled' ? results[0].value : fedPeers.value
  missions.value = results[1].status === 'fulfilled' && Array.isArray(results[1].value) ? results[1].value : []
  bridgeList.value = results[2].status === 'fulfilled' && Array.isArray(results[2].value) ? results[2].value : []
  intList.value = results[3].status === 'fulfilled' && Array.isArray(results[3].value) ? results[3].value : []
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

onMounted(loadAll)
</script>

<template>
<div class="p-4 lg:p-6 max-w-6xl mx-auto space-y-4">
  <div class="flex items-center justify-between">
    <h1 class="text-2xl font-display font-bold">TAK Operations</h1>
    <button @click="loadAll" class="px-3 py-1 rounded text-xs bg-gray-700 text-gray-300 hover:bg-gray-600">Refresh</button>
  </div>

  <div class="flex gap-1 mb-4">
    <button v-for="tab in tabs" :key="tab.id" @click="activeTab = tab.id"
      class="px-4 py-2 rounded-lg text-xs font-medium whitespace-nowrap transition-colors"
      :class="activeTab === tab.id ? 'bg-blue-600 text-white' : 'bg-gray-800 text-gray-400 hover:text-gray-200'">
      {{ tab.label }}
    </button>
  </div>

  <!-- OVERVIEW -->
  <div v-if="activeTab === 'overview'" class="space-y-4">
    <div class="grid grid-cols-2 md:grid-cols-4 gap-3">
      <div class="bg-gray-800 rounded-lg border border-gray-700 p-3 text-center">
        <div class="text-lg font-bold" :class="takInt?.enabled ? 'text-emerald-400' : 'text-gray-500'">{{ takInt?.enabled ? 'Connected' : 'Offline' }}</div>
        <div class="text-xs text-gray-500">TAK Server</div>
      </div>
      <div class="bg-gray-800 rounded-lg border border-gray-700 p-3 text-center">
        <div class="text-2xl font-bold text-purple-400">{{ fedPeers.peer_count }}</div>
        <div class="text-xs text-gray-500">Federation Peers</div>
      </div>
      <div class="bg-gray-800 rounded-lg border border-gray-700 p-3 text-center">
        <div class="text-2xl font-bold text-cyan-400">{{ missions.length }}</div>
        <div class="text-xs text-gray-500">Active Missions</div>
      </div>
      <div class="bg-gray-800 rounded-lg border border-gray-700 p-3 text-center">
        <div class="text-2xl font-bold text-blue-400">{{ takBridges.length }}</div>
        <div class="text-xs text-gray-500">TAK Bridges</div>
      </div>
    </div>

    <!-- TAK Server config info -->
    <div class="bg-gray-800 rounded-lg border border-gray-700 p-4">
      <h3 class="text-sm font-medium text-gray-300 mb-2">TAK Server</h3>
      <div class="grid grid-cols-2 md:grid-cols-3 gap-2 text-xs" v-if="takInt?.config">
        <div v-for="(v, k) in takInt.config" :key="k" class="flex justify-between">
          <span class="text-gray-500">{{ k }}</span>
          <span :class="v === 'configured' ? 'text-emerald-400' : v === 'not set' ? 'text-amber-400' : 'text-gray-300'">{{ v }}</span>
        </div>
      </div>
      <p v-else class="text-xs text-gray-500">TAK not configured. Set HUB_TAK_ENABLED=true.</p>
    </div>

    <!-- Federation throughput -->
    <div class="bg-gray-800 rounded-lg border border-gray-700 p-4" v-if="fedPeers.enabled">
      <h3 class="text-sm font-medium text-gray-300 mb-2">Federation Throughput</h3>
      <div class="grid grid-cols-3 gap-3 text-center">
        <div><div class="text-lg font-bold text-emerald-400">{{ fedPeers.total_in }}</div><div class="text-xs text-gray-500">CoT In</div></div>
        <div><div class="text-lg font-bold text-amber-400">{{ fedPeers.total_out }}</div><div class="text-xs text-gray-500">CoT Out</div></div>
        <div><div class="text-lg font-bold text-purple-400">{{ fedPeers.peer_count }}</div><div class="text-xs text-gray-500">Peers</div></div>
      </div>
    </div>

    <!-- CoT Type Legend -->
    <div class="bg-gray-800 rounded-lg border border-gray-700 p-4">
      <h3 class="text-sm font-medium text-gray-400 mb-2">CoT Type Legend</h3>
      <div class="grid grid-cols-3 gap-1 text-xs">
        <span class="text-blue-400">&#9670; PLI — Friendly Ground (a-f-G)</span>
        <span class="text-cyan-400">&#9679; Chat — GeoChat (b-t-f)</span>
        <span class="text-red-500">&#10006; SOS — Emergency (b-a)</span>
        <span class="text-orange-400">&#9679; Waypoint (b-m-p)</span>
        <span class="text-purple-400">&#9679; Sensor (t-x-d-d)</span>
        <span class="text-red-400">&#9670; Hostile (a-h-G)</span>
      </div>
    </div>
  </div>

  <!-- MISSIONS -->
  <div v-if="activeTab === 'missions'" class="space-y-4">
    <div class="bg-gray-800 rounded-lg border border-gray-700 p-4">
      <h3 class="text-sm font-medium text-gray-300 mb-3">TAK Server Missions</h3>
      <table v-if="missions.length" class="w-full text-sm">
        <thead><tr class="text-xs text-gray-500 border-b border-gray-700"><th class="px-3 py-2 text-left">Name</th><th class="px-3 py-2 text-left">Description</th><th class="px-3 py-2 text-left">Tool</th><th class="px-3 py-2 text-left">Created</th></tr></thead>
        <tbody><tr v-for="m in missions" :key="m.name" class="border-b border-gray-700/50">
          <td class="px-3 py-2 text-gray-200 font-mono">{{ m.name }}</td>
          <td class="px-3 py-2 text-gray-400 text-xs">{{ m.description || '—' }}</td>
          <td class="px-3 py-2 text-gray-400 text-xs">{{ m.tool || '—' }}</td>
          <td class="px-3 py-2 text-gray-500 text-xs">{{ m.createTime || '—' }}</td>
        </tr></tbody>
      </table>
      <p v-else class="text-sm text-gray-500">No missions found. Connect to a TAK Server first.</p>
    </div>
  </div>

  <!-- FLEET COT STATUS -->
  <div v-if="activeTab === 'fleet'" class="space-y-4">
    <div class="bg-gray-800 rounded-lg border border-gray-700 p-4">
      <h3 class="text-sm font-medium text-gray-300 mb-3">Fleet TAK/CoT Status</h3>
      <table v-if="bridgeList.length" class="w-full text-sm">
        <thead><tr class="text-xs text-gray-500 border-b border-gray-700"><th class="px-3 py-2 text-left">Bridge</th><th class="px-3 py-2 text-left">Callsign</th><th class="px-3 py-2 text-left">CoT Type</th><th class="px-3 py-2 text-left">Status</th></tr></thead>
        <tbody><tr v-for="b in bridgeList" :key="b.id" class="border-b border-gray-700/50">
          <td class="px-3 py-2 text-gray-200">{{ b.hostname || b.id }}</td>
          <td class="px-3 py-2"><span v-if="b.cot_callsign" class="px-1.5 py-0.5 rounded text-xs bg-blue-600/20 text-blue-400 font-mono">{{ b.cot_callsign }}</span><span v-else class="text-gray-600 text-xs">—</span></td>
          <td class="px-3 py-2"><span v-if="b.cot_type" class="text-xs text-gray-400">{{ cotTypeName(b.cot_type) }}</span><span v-else class="text-gray-600 text-xs">—</span></td>
          <td class="px-3 py-2"><span class="w-2 h-2 rounded-full inline-block" :class="b.online ? 'bg-emerald-400' : 'bg-gray-600'"></span> <span class="text-xs" :class="b.online ? 'text-emerald-400' : 'text-gray-500'">{{ b.online ? 'Online' : 'Offline' }}</span></td>
        </tr></tbody>
      </table>
      <p v-else class="text-sm text-gray-500">No bridges registered.</p>
    </div>
  </div>

  <!-- FEDERATION -->
  <div v-if="activeTab === 'federation'" class="space-y-4">
    <div class="bg-gray-800 rounded-lg border border-gray-700 p-4">
      <h3 class="text-sm font-medium text-gray-300 mb-3">Federation Peers</h3>
      <div v-if="!fedPeers.enabled" class="text-sm text-gray-500">Federation not enabled. Set HUB_TAK_FEDERATION_ENABLED=true.</div>
      <div v-else>
        <div class="grid grid-cols-3 gap-3 text-center mb-4">
          <div><div class="text-lg font-bold text-emerald-400">{{ fedPeers.total_in }}</div><div class="text-xs text-gray-500">Messages In</div></div>
          <div><div class="text-lg font-bold text-amber-400">{{ fedPeers.total_out }}</div><div class="text-xs text-gray-500">Messages Out</div></div>
          <div><div class="text-lg font-bold text-purple-400">{{ fedPeers.peer_count }}</div><div class="text-xs text-gray-500">Connected</div></div>
        </div>
        <table v-if="fedPeers.peers.length" class="w-full text-sm">
          <thead><tr class="text-xs text-gray-500 border-b border-gray-700"><th class="px-3 py-2 text-left">Address</th><th class="px-3 py-2 text-left">Status</th></tr></thead>
          <tbody><tr v-for="p in fedPeers.peers" :key="p.address" class="border-b border-gray-700/50">
            <td class="px-3 py-2 text-gray-200 font-mono">{{ p.address }}</td>
            <td class="px-3 py-2"><span class="w-2 h-2 rounded-full inline-block" :class="p.connected ? 'bg-emerald-400' : 'bg-gray-600'"></span> <span class="text-xs" :class="p.connected ? 'text-emerald-400' : 'text-gray-500'">{{ p.connected ? 'Connected' : 'Disconnected' }}</span></td>
          </tr></tbody>
        </table>
        <p v-else class="text-sm text-gray-500">No peers connected.</p>
      </div>
    </div>
    <div class="bg-gray-800 rounded-lg border border-gray-700 p-4" v-if="fedInt?.config">
      <h3 class="text-sm font-medium text-gray-300 mb-2">Federation Config</h3>
      <div class="grid grid-cols-2 gap-2 text-xs">
        <div v-for="(v, k) in fedInt.config" :key="k" class="flex justify-between">
          <span class="text-gray-500">{{ k }}</span>
          <span :class="v === 'configured' ? 'text-emerald-400' : v === 'not set' || v === 'missing' ? 'text-amber-400' : 'text-gray-300'">{{ v }}</span>
        </div>
      </div>
    </div>
  </div>

  <!-- GEOCHAT -->
  <div v-if="activeTab === 'chat'" class="space-y-4">
    <div class="bg-gray-800 rounded-lg border border-gray-700 p-4">
      <h3 class="text-sm font-medium text-gray-300 mb-3">Fleet GeoChat</h3>
      <div class="space-y-2 max-h-80 overflow-y-auto mb-3">
        <div v-for="(m, i) in chatMessages" :key="i" class="flex gap-2">
          <span class="text-xs text-cyan-400 font-mono whitespace-nowrap">{{ m.callsign || '?' }}</span>
          <span class="text-xs text-gray-300">{{ m.text }}</span>
        </div>
        <p v-if="!chatMessages.length" class="text-xs text-gray-500">No fleet chat messages. GeoChat events from TAK clients will appear here.</p>
      </div>
      <div class="flex gap-2">
        <input v-model="chatInput" placeholder="Type a message..." class="flex-1 px-3 py-2 rounded bg-gray-900 border border-gray-700 text-sm text-gray-200">
        <button class="px-4 py-2 rounded text-xs bg-cyan-600 text-white hover:bg-cyan-500">Send</button>
      </div>
    </div>
  </div>
</div>
</template>
