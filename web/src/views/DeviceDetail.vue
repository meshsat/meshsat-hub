<script setup>
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useRoute } from 'vue-router'
import { devices, messages, positions, deviceKeys, deviceConfig, ratelimit, deadman, wireguard as wgApi } from '../api/client'
import { formatUTC } from '../utils/time'

const route = useRoute()
const imei = computed(() => route.params.imei)

const loading = ref(true)
const device = ref(null)
const msgList = ref([])
const posList = ref([])
const keyList = ref([])
const configLatest = ref(null)
const budget = ref(null)
const dmsConfig = ref(null)
const wgConfig = ref(null)
const error = ref('')
const activeTab = ref('messages')
let pollTimer = null

onMounted(async () => {
  await loadAll()
  pollTimer = setInterval(loadAll, 30000)
})

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

async function loadAll() {
  const results = await Promise.allSettled([
    devices.get(imei.value),
    messages.list(imei.value, 50),
    positions.history(imei.value, 20),
    deviceKeys.list(imei.value),
    deviceConfig.getLatest(imei.value),
    ratelimit.get(imei.value),
    deadman.list(),
    wgApi.getDeviceWG(imei.value),
  ])

  device.value = results[0].status === 'fulfilled' ? results[0].value : null
  msgList.value = results[1].status === 'fulfilled' && Array.isArray(results[1].value) ? results[1].value : []
  posList.value = results[2].status === 'fulfilled' && Array.isArray(results[2].value) ? results[2].value : []
  keyList.value = results[3].status === 'fulfilled' && Array.isArray(results[3].value) ? results[3].value : []
  configLatest.value = results[4].status === 'fulfilled' ? results[4].value : null
  budget.value = results[5].status === 'fulfilled' ? results[5].value : null
  const dmsList = results[6].status === 'fulfilled' && Array.isArray(results[6].value) ? results[6].value : []
  dmsConfig.value = dmsList.find(d => d.device_imei === imei.value) || null
  wgConfig.value = results[7].status === 'fulfilled' ? results[7].value : null

  if (!device.value) error.value = 'Device not found'
  loading.value = false
}

function timeSince(ts) {
  if (!ts || ts === '0001-01-01T00:00:00Z') return 'never'
  const ms = Date.now() - new Date(ts).getTime()
  if (ms < 0) return 'now'
  const sec = Math.floor(ms / 1000)
  if (sec < 60) return `${sec}s ago`
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min}m ago`
  const hr = Math.floor(min / 60)
  if (hr < 24) return `${hr}h ago`
  return `${Math.floor(hr / 24)}d ago`
}

function onlineStatus(ts) {
  if (!ts || ts === '0001-01-01T00:00:00Z') return { label: 'offline', color: 'text-red-400', dot: 'bg-red-400' }
  const age = Date.now() - new Date(ts).getTime()
  if (age < 3600000) return { label: 'online', color: 'text-emerald-400', dot: 'bg-emerald-400' }
  if (age < 86400000) return { label: 'idle', color: 'text-amber-400', dot: 'bg-amber-400' }
  return { label: 'offline', color: 'text-red-400', dot: 'bg-red-400' }
}

function budgetPercent(sent, cap) {
  if (!cap || cap <= 0) return 0
  return Math.min(100, Math.round((sent / cap) * 100))
}

function budgetBarColor(pct) {
  if (pct >= 90) return 'bg-red-500'
  if (pct >= 70) return 'bg-amber-500'
  return 'bg-teal-500'
}

const tabs = [
  { id: 'messages', label: 'Messages' },
  { id: 'positions', label: 'Positions' },
  { id: 'keys', label: 'Keys' },
  { id: 'config', label: 'Config' },
]
</script>

<template>
  <div class="p-4 lg:p-6 max-w-6xl mx-auto">
    <div v-if="loading" class="text-center text-gray-500 py-16">Loading device...</div>
    <div v-else-if="error" class="text-center text-red-400 py-16">{{ error }}</div>

    <template v-else-if="device">
      <!-- Header -->
      <div class="flex items-center gap-4 mb-6">
        <router-link to="/devices" class="text-gray-500 hover:text-gray-300 text-sm">&larr; Devices</router-link>
        <div class="flex items-center gap-3">
          <span class="w-3 h-3 rounded-full" :class="onlineStatus(device.last_seen).dot"></span>
          <h1 class="text-2xl font-display font-bold">{{ device.label || device.imei }}</h1>
        </div>
        <span class="text-sm" :class="onlineStatus(device.last_seen).color">{{ onlineStatus(device.last_seen).label }}</span>
      </div>

      <!-- Info Cards -->
      <div class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-6 gap-3 mb-6">
        <div class="bg-gray-900 rounded-xl border border-gray-800 p-3">
          <div class="text-gray-500 text-[10px] uppercase">IMEI</div>
          <div class="font-mono text-xs text-gray-300 truncate">{{ device.imei }}</div>
        </div>
        <div class="bg-gray-900 rounded-xl border border-gray-800 p-3">
          <div class="text-gray-500 text-[10px] uppercase">Type</div>
          <div class="text-sm text-gray-300">{{ device.type || 'unknown' }}</div>
        </div>
        <div class="bg-gray-900 rounded-xl border border-gray-800 p-3">
          <div class="text-gray-500 text-[10px] uppercase">Last Seen</div>
          <div class="text-sm text-gray-300">{{ timeSince(device.last_seen) }}</div>
        </div>
        <div class="bg-gray-900 rounded-xl border border-gray-800 p-3">
          <div class="text-gray-500 text-[10px] uppercase">Messages</div>
          <div class="text-sm text-gray-300">{{ msgList.length }}</div>
        </div>
        <div class="bg-gray-900 rounded-xl border border-gray-800 p-3">
          <div class="text-gray-500 text-[10px] uppercase">Enc Keys</div>
          <div class="text-sm text-gray-300">{{ keyList.length }}</div>
        </div>
        <div class="bg-gray-900 rounded-xl border border-gray-800 p-3">
          <div class="text-gray-500 text-[10px] uppercase">DMS</div>
          <div class="text-sm" :class="dmsConfig?.enabled ? 'text-emerald-400' : 'text-gray-500'">
            {{ dmsConfig?.enabled ? 'active' : 'off' }}
          </div>
        </div>
      </div>

      <!-- Budget Bar -->
      <div v-if="budget && (budget.daily_cap > 0 || budget.monthly_cap > 0)" class="bg-gray-900 rounded-xl border border-gray-800 p-4 mb-6">
        <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">Budget Usage</h2>
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <div v-if="budget.daily_cap > 0">
            <div class="flex justify-between text-xs text-gray-500 mb-1">
              <span>Daily</span>
              <span>{{ budget.daily_sent }} / {{ budget.daily_cap }}</span>
            </div>
            <div class="w-full bg-gray-700 rounded-full h-2">
              <div :class="budgetBarColor(budgetPercent(budget.daily_sent, budget.daily_cap))"
                   class="h-2 rounded-full transition-all"
                   :style="{ width: budgetPercent(budget.daily_sent, budget.daily_cap) + '%' }"></div>
            </div>
          </div>
          <div v-if="budget.monthly_cap > 0">
            <div class="flex justify-between text-xs text-gray-500 mb-1">
              <span>Monthly</span>
              <span>{{ budget.monthly_sent }} / {{ budget.monthly_cap }}</span>
            </div>
            <div class="w-full bg-gray-700 rounded-full h-2">
              <div :class="budgetBarColor(budgetPercent(budget.monthly_sent, budget.monthly_cap))"
                   class="h-2 rounded-full transition-all"
                   :style="{ width: budgetPercent(budget.monthly_sent, budget.monthly_cap) + '%' }"></div>
            </div>
          </div>
        </div>
      </div>

      <!-- WireGuard -->
      <div v-if="wgConfig" class="bg-gray-900 rounded-xl border border-gray-800 p-4 mb-6">
        <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-2">WireGuard VPN</h2>
        <div class="flex items-center gap-4 text-sm">
          <span class="text-gray-400">Address:</span>
          <span class="font-mono text-teal-400">{{ wgConfig.vpn_address || wgConfig.address || 'assigned' }}</span>
        </div>
      </div>

      <!-- Tabbed Content -->
      <div class="bg-gray-900 rounded-xl border border-gray-800 overflow-hidden">
        <div class="flex border-b border-gray-800">
          <button v-for="tab in tabs" :key="tab.id"
            @click="activeTab = tab.id"
            class="px-4 py-2.5 text-sm font-medium transition-colors"
            :class="activeTab === tab.id ? 'text-teal-400 border-b-2 border-teal-400' : 'text-gray-500 hover:text-gray-300'">
            {{ tab.label }}
          </button>
        </div>

        <!-- Messages Tab -->
        <div v-if="activeTab === 'messages'">
          <div v-if="msgList.length === 0" class="p-8 text-center text-gray-500 text-sm">No messages</div>
          <div v-else class="divide-y divide-gray-800/50 max-h-96 overflow-y-auto">
            <div v-for="m in msgList" :key="m.id" class="px-4 py-2.5 flex items-center gap-3 text-sm">
              <span class="font-semibold text-xs w-6" :class="m.direction === 'mo' ? 'text-emerald-400' : 'text-sky-400'">
                {{ m.direction?.toUpperCase() }}
              </span>
              <span class="text-xs text-gray-500 w-16">{{ m.channel }}</span>
              <span class="text-gray-300 truncate flex-1">{{ m.text || '(binary)' }}</span>
              <span class="text-gray-500 text-xs shrink-0">{{ timeSince(m.created_at) }}</span>
            </div>
          </div>
        </div>

        <!-- Positions Tab -->
        <div v-if="activeTab === 'positions'">
          <div v-if="posList.length === 0" class="p-8 text-center text-gray-500 text-sm">No positions</div>
          <table v-else class="w-full text-sm">
            <thead class="text-gray-400 text-left border-b border-gray-800">
              <tr>
                <th class="px-4 py-2">Time</th>
                <th class="px-4 py-2">Lat</th>
                <th class="px-4 py-2">Lon</th>
                <th class="px-4 py-2">Alt</th>
                <th class="px-4 py-2">Source</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-800/50">
              <tr v-for="p in posList" :key="p.id" class="hover:bg-white/[0.02]">
                <td class="px-4 py-2 text-gray-400">{{ timeSince(p.created_at) }}</td>
                <td class="px-4 py-2 font-mono text-xs">{{ p.lat?.toFixed(6) }}</td>
                <td class="px-4 py-2 font-mono text-xs">{{ p.lon?.toFixed(6) }}</td>
                <td class="px-4 py-2 text-gray-400">{{ p.alt ? p.alt.toFixed(1) + 'm' : '—' }}</td>
                <td class="px-4 py-2 text-gray-400">{{ p.source }}</td>
              </tr>
            </tbody>
          </table>
        </div>

        <!-- Keys Tab -->
        <div v-if="activeTab === 'keys'">
          <div v-if="keyList.length === 0" class="p-8 text-center text-gray-500 text-sm">No encryption keys</div>
          <div v-else class="divide-y divide-gray-800/50">
            <div v-for="k in keyList" :key="k.id" class="px-4 py-3 flex items-center justify-between text-sm">
              <div>
                <span class="text-gray-300">v{{ k.version }}</span>
                <span class="text-gray-500 text-xs ml-2">{{ k.mode }}</span>
                <span v-if="k.key_hash" class="font-mono text-[10px] text-gray-600 ml-2">{{ k.key_hash?.substring(0, 12) }}...</span>
              </div>
              <span class="text-gray-500 text-xs">{{ formatUTC(k.created_at) }}</span>
            </div>
          </div>
        </div>

        <!-- Config Tab -->
        <div v-if="activeTab === 'config'">
          <div v-if="!configLatest" class="p-8 text-center text-gray-500 text-sm">No configuration versions</div>
          <div v-else class="p-4">
            <div class="flex items-center justify-between mb-3">
              <span class="text-sm text-gray-400">Version {{ configLatest.version }}</span>
              <span class="text-xs text-gray-500">{{ formatUTC(configLatest.created_at) }}</span>
            </div>
            <pre class="bg-gray-900 rounded p-3 text-xs text-gray-300 overflow-x-auto max-h-64">{{ JSON.stringify(configLatest.config || configLatest, null, 2) }}</pre>
          </div>
        </div>
      </div>
    </template>
  </div>
</template>
