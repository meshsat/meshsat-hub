<script setup>
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { devices, health, credits, ratelimit, messages, escalation, deadman, constellations, reticulum as reticulumApi } from '../api/client'
import { formatUTC } from '../utils/time'

const loading = ref(true)
const lastRefresh = ref(null)
let pollTimer = null

// Data refs
const hubHealth = ref(null)
const deviceList = ref([])
const creditBalance = ref(null)
const budgetList = ref([])
const messageList = ref([])
const alertList = ref([])
const deadmanList = ref([])
const backendList = ref([])
const retIdentity = ref(null)
const retRoutes = ref({ count: 0 })

onMounted(async () => {
  await loadAll()
  pollTimer = setInterval(loadAll, 30000)
})

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

async function loadAll() {
  const results = await Promise.allSettled([
    health.check(),
    devices.list(),
    credits.get(),
    ratelimit.all(),
    messages.list('', 50),
    escalation.listAlerts(true, 10),
    deadman.list(),
    constellations.list(),
    reticulumApi.identity(),
    reticulumApi.routes(),
  ])

  hubHealth.value = results[0].status === 'fulfilled' ? results[0].value : { status: 'error' }
  deviceList.value = results[1].status === 'fulfilled' && Array.isArray(results[1].value) ? results[1].value : []
  creditBalance.value = results[2].status === 'fulfilled' ? (results[2].value?.balance ?? results[2].value) : null
  budgetList.value = results[3].status === 'fulfilled' && Array.isArray(results[3].value) ? results[3].value : []
  messageList.value = results[4].status === 'fulfilled' && Array.isArray(results[4].value) ? results[4].value : []
  alertList.value = results[5].status === 'fulfilled' && Array.isArray(results[5].value) ? results[5].value : []
  deadmanList.value = results[6].status === 'fulfilled' && Array.isArray(results[6].value) ? results[6].value : []
  const consResult = results[7].status === 'fulfilled' ? results[7].value : {}
  backendList.value = consResult.backends || []
  retIdentity.value = results[8].status === 'fulfilled' ? results[8].value : null
  retRoutes.value = results[9].status === 'fulfilled' ? results[9].value : { count: 0 }

  lastRefresh.value = new Date()
  loading.value = false
}

// Computed stats
const onlineDevices = computed(() => deviceList.value.filter(d => {
  if (!d.last_seen || d.last_seen === '0001-01-01T00:00:00Z') return false
  return (Date.now() - new Date(d.last_seen).getTime()) < 3600000
}).length)

const idleDevices = computed(() => deviceList.value.filter(d => {
  if (!d.last_seen || d.last_seen === '0001-01-01T00:00:00Z') return false
  const age = Date.now() - new Date(d.last_seen).getTime()
  return age >= 3600000 && age < 86400000
}).length)

const offlineDevices = computed(() => deviceList.value.length - onlineDevices.value - idleDevices.value)

const moCount = computed(() => messageList.value.filter(m => m.direction === 'mo').length)
const mtCount = computed(() => messageList.value.filter(m => m.direction === 'mt').length)

const activeAlerts = computed(() => alertList.value.filter(a => a.state === 'triggered' || a.state === 'escalating'))
const hasSOS = computed(() => activeAlerts.value.some(a => a.type === 'sos'))

const overdueDevices = computed(() => deadmanList.value.filter(d => {
  if (!d.enabled || !d.last_check_in) return false
  const elapsed = (Date.now() - new Date(d.last_check_in).getTime()) / 1000
  return elapsed > (d.interval_sec + (d.grace_period_sec || 0))
}).length)

const throttledDevices = computed(() => budgetList.value.filter(b => b.throttled).length)

const recentMessages = computed(() => messageList.value.slice(0, 8))

const lastMessageTime = computed(() => {
  if (messageList.value.length === 0) return null
  return messageList.value[0].created_at
})

function timeSince(ts) {
  if (!ts) return 'never'
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

function budgetPercent(sent, cap) {
  if (!cap || cap <= 0) return 0
  return Math.min(100, Math.round((sent / cap) * 100))
}

function budgetBarColor(pct) {
  if (pct >= 90) return 'bg-red-500'
  if (pct >= 70) return 'bg-amber-500'
  return 'bg-teal-500'
}

function channelBadge(ch) {
  const colors = {
    iridium: 'bg-orange-600',
    astrocast: 'bg-amber-700',
    globalstar: 'bg-yellow-700',
    sms: 'bg-sky-700',
    email: 'bg-purple-700',
    mqtt: 'bg-green-700',
  }
  return colors[ch] || 'bg-gray-600'
}

function directionLabel(d) {
  return d === 'mo' ? 'MO' : 'MT'
}

function directionColor(d) {
  return d === 'mo' ? 'text-emerald-400' : 'text-sky-400'
}
</script>

<template>
  <div class="p-4 lg:p-6 max-w-7xl mx-auto">
    <!-- Header -->
    <div class="flex items-center justify-between mb-6">
      <h1 class="text-2xl font-bold">Operations Dashboard</h1>
      <div class="flex items-center gap-3 text-xs text-gray-500">
        <span v-if="lastRefresh">Updated {{ timeSince(lastRefresh) }}</span>
        <button @click="loadAll" class="text-teal-400 hover:text-teal-300">Refresh</button>
      </div>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-16">Loading operational data...</div>

    <template v-else>
      <!-- SOS Banner -->
      <div v-if="hasSOS" class="bg-red-900/60 border border-red-600 rounded-lg p-4 mb-6 flex items-center gap-3">
        <span class="text-red-400 text-2xl font-bold">SOS</span>
        <div>
          <p class="text-red-200 font-semibold">Active SOS Alert</p>
          <p class="text-red-300 text-sm">{{ activeAlerts.filter(a => a.type === 'sos').length }} device(s) in distress — immediate action required</p>
        </div>
      </div>

      <!-- Primary KPI Row -->
      <div class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-6 gap-3 mb-6">
        <!-- Hub Status -->
        <div class="bg-gray-800 rounded-lg p-4 border border-gray-700">
          <div class="text-gray-400 text-xs uppercase tracking-wider mb-1">Hub</div>
          <div class="text-xl font-bold" :class="hubHealth?.status === 'ok' ? 'text-emerald-400' : 'text-red-400'">
            {{ hubHealth?.status || '?' }}
          </div>
        </div>

        <!-- Devices Online/Idle/Offline -->
        <div class="bg-gray-800 rounded-lg p-4 border border-gray-700">
          <div class="text-gray-400 text-xs uppercase tracking-wider mb-1">Devices</div>
          <div class="flex items-baseline gap-1">
            <span class="text-xl font-bold text-emerald-400">{{ onlineDevices }}</span>
            <span class="text-gray-500 text-xs">/</span>
            <span class="text-sm text-amber-400">{{ idleDevices }}</span>
            <span class="text-gray-500 text-xs">/</span>
            <span class="text-sm text-red-400">{{ offlineDevices }}</span>
          </div>
          <div class="text-gray-500 text-[10px] mt-0.5">on / idle / off</div>
        </div>

        <!-- Messages -->
        <div class="bg-gray-800 rounded-lg p-4 border border-gray-700">
          <div class="text-gray-400 text-xs uppercase tracking-wider mb-1">Messages</div>
          <div class="flex items-baseline gap-2">
            <span class="text-xl font-bold text-emerald-400">{{ moCount }}</span>
            <span class="text-gray-500 text-xs">MO</span>
            <span class="text-lg text-sky-400">{{ mtCount }}</span>
            <span class="text-gray-500 text-xs">MT</span>
          </div>
        </div>

        <!-- Credits -->
        <div class="bg-gray-800 rounded-lg p-4 border border-gray-700">
          <div class="text-gray-400 text-xs uppercase tracking-wider mb-1">Credits</div>
          <div class="text-xl font-bold" :class="creditBalance !== null && creditBalance < 100 ? 'text-amber-400' : 'text-teal-400'">
            {{ creditBalance !== null ? creditBalance.toLocaleString() : '---' }}
          </div>
        </div>

        <!-- Active Alerts -->
        <div class="bg-gray-800 rounded-lg p-4 border border-gray-700" :class="activeAlerts.length > 0 ? 'border-red-700' : ''">
          <div class="text-gray-400 text-xs uppercase tracking-wider mb-1">Alerts</div>
          <div class="text-xl font-bold" :class="activeAlerts.length > 0 ? 'text-red-400' : 'text-emerald-400'">
            {{ activeAlerts.length }}
          </div>
        </div>

        <!-- Mesh Nodes -->
        <div class="bg-gray-800 rounded-lg p-4 border border-gray-700">
          <div class="text-gray-400 text-xs uppercase tracking-wider mb-1">Mesh Nodes</div>
          <div class="text-xl font-bold text-teal-400">{{ retRoutes.count }}</div>
        </div>
      </div>

      <!-- Secondary Info Row -->
      <div class="grid grid-cols-1 lg:grid-cols-3 gap-4 mb-6">
        <!-- Constellation Status -->
        <div class="bg-gray-800 rounded-lg border border-gray-700 p-4">
          <h2 class="text-sm font-semibold text-gray-300 uppercase tracking-wider mb-3">Constellations</h2>
          <div v-if="backendList.length === 0" class="text-gray-500 text-sm">No backends configured</div>
          <div v-else class="space-y-2">
            <div v-for="b in backendList" :key="b" class="flex items-center justify-between">
              <div class="flex items-center gap-2">
                <span class="w-2 h-2 rounded-full bg-emerald-400"></span>
                <span class="text-sm capitalize">{{ b }}</span>
              </div>
              <span class="text-xs text-gray-500">active</span>
            </div>
          </div>
        </div>

        <!-- Safety Status -->
        <div class="bg-gray-800 rounded-lg border border-gray-700 p-4">
          <h2 class="text-sm font-semibold text-gray-300 uppercase tracking-wider mb-3">Safety</h2>
          <div class="space-y-2 text-sm">
            <div class="flex items-center justify-between">
              <span class="text-gray-400">Active Alerts</span>
              <span :class="activeAlerts.length > 0 ? 'text-red-400 font-semibold' : 'text-emerald-400'">
                {{ activeAlerts.length }}
              </span>
            </div>
            <div class="flex items-center justify-between">
              <span class="text-gray-400">DMS Overdue</span>
              <span :class="overdueDevices > 0 ? 'text-amber-400 font-semibold' : 'text-emerald-400'">
                {{ overdueDevices }}
              </span>
            </div>
            <div class="flex items-center justify-between">
              <span class="text-gray-400">Throttled</span>
              <span :class="throttledDevices > 0 ? 'text-amber-400' : 'text-gray-500'">
                {{ throttledDevices }}
              </span>
            </div>
            <div class="flex items-center justify-between">
              <span class="text-gray-400">DMS Monitors</span>
              <span class="text-gray-300">{{ deadmanList.filter(d => d.enabled).length }}</span>
            </div>
          </div>
        </div>

        <!-- Network Identity -->
        <div class="bg-gray-800 rounded-lg border border-gray-700 p-4">
          <h2 class="text-sm font-semibold text-gray-300 uppercase tracking-wider mb-3">Network</h2>
          <div v-if="retIdentity" class="space-y-2 text-sm">
            <div>
              <span class="text-gray-400 text-xs">Hub DestHash</span>
              <p class="font-mono text-xs text-teal-400 truncate">{{ retIdentity.dest_hash }}</p>
            </div>
            <div class="flex items-center justify-between">
              <span class="text-gray-400">Known Routes</span>
              <span class="text-gray-300">{{ retRoutes.count }}</span>
            </div>
            <div class="flex items-center justify-between">
              <span class="text-gray-400">Last Message</span>
              <span class="text-gray-300">{{ timeSince(lastMessageTime) }}</span>
            </div>
          </div>
          <div v-else class="text-gray-500 text-sm">Identity loading...</div>
        </div>
      </div>

      <!-- Main Content Row -->
      <div class="grid grid-cols-1 lg:grid-cols-2 gap-4 mb-6">
        <!-- Recent Activity -->
        <div class="bg-gray-800 rounded-lg border border-gray-700 overflow-hidden">
          <div class="px-4 py-3 border-b border-gray-700">
            <h2 class="text-sm font-semibold text-gray-300 uppercase tracking-wider">Recent Messages</h2>
          </div>
          <div v-if="recentMessages.length === 0" class="p-6 text-center text-gray-500 text-sm">
            No messages yet
          </div>
          <div v-else class="divide-y divide-gray-700/50">
            <div v-for="msg in recentMessages" :key="msg.id" class="px-4 py-2.5 flex items-center gap-3 text-sm">
              <span class="font-semibold text-xs w-6" :class="directionColor(msg.direction)">
                {{ directionLabel(msg.direction) }}
              </span>
              <span :class="[channelBadge(msg.channel), 'px-1.5 py-0.5 rounded text-[10px] font-medium min-w-[52px] text-center']">
                {{ msg.channel }}
              </span>
              <span class="font-mono text-xs text-gray-400 w-20 shrink-0 truncate">{{ msg.device_imei }}</span>
              <span class="text-gray-300 truncate flex-1">{{ msg.text || '(binary)' }}</span>
              <span class="text-gray-500 text-xs shrink-0">{{ timeSince(msg.created_at) }}</span>
            </div>
          </div>
        </div>

        <!-- Device Fleet -->
        <div class="bg-gray-800 rounded-lg border border-gray-700 overflow-hidden">
          <div class="px-4 py-3 border-b border-gray-700">
            <h2 class="text-sm font-semibold text-gray-300 uppercase tracking-wider">Device Fleet</h2>
          </div>
          <div v-if="deviceList.length === 0" class="p-6 text-center text-gray-500 text-sm">
            No devices registered
          </div>
          <div v-else class="divide-y divide-gray-700/50 max-h-80 overflow-y-auto">
            <div v-for="dev in deviceList" :key="dev.imei" class="px-4 py-2.5 flex items-center gap-3 text-sm">
              <span class="w-2 h-2 rounded-full shrink-0" :class="{
                'bg-emerald-400': dev.last_seen && (Date.now() - new Date(dev.last_seen).getTime()) < 3600000,
                'bg-amber-400': dev.last_seen && (Date.now() - new Date(dev.last_seen).getTime()) >= 3600000 && (Date.now() - new Date(dev.last_seen).getTime()) < 86400000,
                'bg-red-400': !dev.last_seen || dev.last_seen === '0001-01-01T00:00:00Z' || (Date.now() - new Date(dev.last_seen).getTime()) >= 86400000,
              }"></span>
              <span class="font-mono text-xs text-gray-400 w-28 shrink-0 truncate">{{ dev.imei }}</span>
              <span class="text-gray-300 truncate flex-1">{{ dev.label || dev.type }}</span>
              <span class="text-gray-500 text-xs shrink-0">{{ timeSince(dev.last_seen) }}</span>
            </div>
          </div>
        </div>
      </div>

      <!-- Budget Section -->
      <div v-if="budgetList.length > 0">
        <h2 class="text-sm font-semibold text-gray-300 uppercase tracking-wider mb-3">Budget Usage</h2>
        <div class="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-3">
          <div v-for="b in budgetList" :key="b.device_id"
               class="bg-gray-800 rounded-lg border border-gray-700 p-4"
               :class="b.throttled ? 'border-red-700' : ''">
            <div class="flex items-center justify-between mb-2">
              <span class="font-mono text-xs text-gray-300">{{ b.device_id }}</span>
              <span v-if="b.throttled" class="text-red-400 text-[10px] font-bold uppercase px-1.5 py-0.5 bg-red-900/50 rounded">Throttled</span>
            </div>
            <div v-if="b.daily_cap > 0" class="mb-2">
              <div class="flex justify-between text-[10px] text-gray-500 mb-1">
                <span>Daily</span>
                <span>{{ b.daily_sent }} / {{ b.daily_cap }}</span>
              </div>
              <div class="w-full bg-gray-700 rounded-full h-1.5">
                <div :class="budgetBarColor(budgetPercent(b.daily_sent, b.daily_cap))"
                     class="h-1.5 rounded-full transition-all"
                     :style="{ width: budgetPercent(b.daily_sent, b.daily_cap) + '%' }"></div>
              </div>
            </div>
            <div v-if="b.monthly_cap > 0">
              <div class="flex justify-between text-[10px] text-gray-500 mb-1">
                <span>Monthly</span>
                <span>{{ b.monthly_sent }} / {{ b.monthly_cap }}</span>
              </div>
              <div class="w-full bg-gray-700 rounded-full h-1.5">
                <div :class="budgetBarColor(budgetPercent(b.monthly_sent, b.monthly_cap))"
                     class="h-1.5 rounded-full transition-all"
                     :style="{ width: budgetPercent(b.monthly_sent, b.monthly_cap) + '%' }"></div>
              </div>
            </div>
            <div v-if="b.daily_cap <= 0 && b.monthly_cap <= 0" class="text-[10px] text-gray-600">
              No limits
            </div>
          </div>
        </div>
      </div>

      <p class="text-gray-600 text-[10px] mt-4">Auto-refreshes every 30 seconds.</p>
    </template>
  </div>
</template>
