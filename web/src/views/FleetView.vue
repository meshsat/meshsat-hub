<script setup>
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { bridges } from '../api/client'
import { timeAgo, formatUptime } from '../utils/time'
import EmptyState from '../components/EmptyState.vue'

const loading = ref(true)
const error = ref('')
const bridgeList = ref([])
const expandedBridge = ref(null)
let pollTimer = null

onMounted(async () => {
  await loadBridges()
  pollTimer = setInterval(loadBridges, 30000)
})

onUnmounted(() => {
  if (pollTimer) clearInterval(pollTimer)
})

async function loadBridges() {
  try {
    const data = await bridges.list()
    bridgeList.value = Array.isArray(data) ? data : []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function toggleExpand(bridgeId) {
  expandedBridge.value = expandedBridge.value === bridgeId ? null : bridgeId
}

const onlineCount = computed(() => bridgeList.value.filter(b => b.online).length)
const totalCount = computed(() => bridgeList.value.length)

function parseBirth(b) {
  if (!b.last_birth) return null
  try { return JSON.parse(b.last_birth) } catch { return null }
}

function parseHealth(b) {
  if (!b.last_health) return null
  try { return JSON.parse(b.last_health) } catch { return null }
}

function interfaceStatusDot(status) {
  if (status === 'online') return 'bg-green-400'
  if (status === 'error') return 'bg-red-400'
  if (status === 'binding') return 'bg-yellow-400'
  return 'bg-gray-500'
}

function interfaceTypeBadgeColor(type) {
  if (type === 'meshtastic') return 'text-transport-mesh border-transport-mesh/30'
  if (type === 'iridium_sbd' || type === 'iridium_imt') return 'text-transport-iridium border-transport-iridium/30'
  if (type === 'cellular') return 'text-transport-cellular border-transport-cellular/30'
  return 'text-gray-400 border-gray-600'
}

function interfaceTypeLabel(type) {
  const labels = {
    meshtastic: 'Meshtastic',
    iridium_sbd: 'Iridium SBD',
    iridium_imt: 'Iridium IMT',
    cellular: 'Cellular',
    astrocast: 'Astrocast',
    zigbee: 'ZigBee',
    aprs: 'APRS',
    tcp: 'TCP',
  }
  return labels[type] || type
}

function signalDisplay(iface) {
  if (iface.signal_bars > 0) return `${iface.signal_bars}/5`
  if (iface.signal_dbm && iface.signal_dbm !== 0) return `${iface.signal_dbm} dBm`
  return '—'
}

function birthInterfaceType(b, name) {
  const birth = parseBirth(b)
  if (!birth?.interfaces) return ''
  const match = birth.interfaces.find(i => i.name === name)
  return match?.type || ''
}

function messageDisplay(iface) {
  const parts = []
  if (iface.mo_count > 0) parts.push(`MO: ${iface.mo_count}`)
  if (iface.mt_count > 0) parts.push(`MT: ${iface.mt_count}`)
  if (iface.nodes_seen > 0) parts.push(`${iface.nodes_seen} nodes`)
  return parts.join(', ') || '—'
}
</script>

<template>
  <div>
    <!-- Header -->
    <div class="flex items-center justify-between mb-4">
      <div>
        <h1 class="text-2xl font-display font-bold">Fleet</h1>
        <p v-if="!loading && bridgeList.length" class="text-sm text-gray-400 mt-0.5">
          {{ totalCount }} bridge{{ totalCount !== 1 ? 's' : '' }}, {{ onlineCount }} online
        </p>
      </div>
    </div>

    <!-- Error -->
    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">{{ error }}</div>

    <!-- Loading -->
    <div v-if="loading" class="text-center text-gray-500 py-12">Loading...</div>

    <!-- Empty state -->
    <EmptyState v-else-if="!bridgeList.length"
      icon="satellite"
      title="No bridges connected"
      message="Configure MESHSAT_HUB_URL on your bridge to connect it to this Hub" />

    <!-- Bridge cards grid -->
    <div v-else class="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-4">
      <div v-for="b in bridgeList" :key="b.bridge_id">
        <!-- Card -->
        <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4 cursor-pointer hover:border-gray-500 transition-colors"
          @click="toggleExpand(b.bridge_id)">
          <!-- Card header -->
          <div class="flex items-center justify-between mb-3">
            <div class="flex items-center gap-2 min-w-0">
              <h3 class="font-display font-semibold text-gray-200 truncate">{{ b.label || b.bridge_id }}</h3>
              <span class="flex items-center gap-1.5 text-xs font-medium shrink-0"
                :class="b.online ? 'text-green-400' : 'text-red-400'">
                <span class="w-2 h-2 rounded-full" :class="b.online ? 'bg-green-400 animate-pulse-dot' : 'bg-red-400'" />
                {{ b.online ? 'Online' : 'Offline' }}
              </span>
            </div>
            <svg class="w-4 h-4 text-gray-500 shrink-0 transition-transform" :class="expandedBridge === b.bridge_id ? 'rotate-180' : ''"
              fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7"/>
            </svg>
          </div>

          <!-- Bridge meta -->
          <div class="grid grid-cols-2 gap-x-4 gap-y-1 text-xs mb-3">
            <div>
              <span class="text-gray-500">Hostname</span>
              <div class="text-gray-300 font-mono truncate">{{ b.hostname || '—' }}</div>
            </div>
            <div>
              <span class="text-gray-500">Version</span>
              <div class="text-gray-300 font-mono">{{ b.version || '—' }}</div>
            </div>
            <div>
              <span class="text-gray-500">Last seen</span>
              <div class="text-gray-300">{{ timeAgo(b.last_seen) }}</div>
            </div>
            <div>
              <span class="text-gray-500">Mode</span>
              <div class="text-gray-300">{{ b.mode || '—' }}</div>
            </div>
          </div>

          <!-- Interface badges -->
          <div v-if="parseBirth(b)?.interfaces?.length" class="flex flex-wrap gap-1.5">
            <span v-for="iface in parseBirth(b).interfaces" :key="iface.name"
              class="inline-flex items-center gap-1 text-xs px-1.5 py-0.5 rounded border bg-black/20"
              :class="interfaceTypeBadgeColor(iface.type)">
              <span class="w-1.5 h-1.5 rounded-full" :class="interfaceStatusDot(iface.status)" />
              {{ iface.name }}
            </span>
          </div>
        </div>

        <!-- Expanded detail panel -->
        <Transition name="expand">
          <div v-if="expandedBridge === b.bridge_id"
            class="bg-tactical-surface rounded-b-lg border border-t-0 border-tactical-border p-4 -mt-1">

            <!-- System metrics -->
            <template v-if="parseHealth(b)">
              <h4 class="text-xs text-gray-500 uppercase tracking-wider font-display mb-2">System</h4>
              <div class="grid grid-cols-3 gap-3 mb-4">
                <div>
                  <div class="flex items-center justify-between text-xs mb-1">
                    <span class="text-gray-400">CPU</span>
                    <span class="text-gray-300">{{ parseHealth(b).cpu_pct?.toFixed(1) || 0 }}%</span>
                  </div>
                  <div class="w-full bg-gray-700 rounded-full h-1.5">
                    <div class="h-1.5 rounded-full transition-all"
                      :class="parseHealth(b).cpu_pct > 80 ? 'bg-red-400' : parseHealth(b).cpu_pct > 50 ? 'bg-yellow-400' : 'bg-teal-400'"
                      :style="{ width: Math.min(parseHealth(b).cpu_pct || 0, 100) + '%' }" />
                  </div>
                </div>
                <div>
                  <div class="flex items-center justify-between text-xs mb-1">
                    <span class="text-gray-400">Memory</span>
                    <span class="text-gray-300">{{ parseHealth(b).mem_pct?.toFixed(1) || 0 }}%</span>
                  </div>
                  <div class="w-full bg-gray-700 rounded-full h-1.5">
                    <div class="h-1.5 rounded-full transition-all"
                      :class="parseHealth(b).mem_pct > 80 ? 'bg-red-400' : parseHealth(b).mem_pct > 50 ? 'bg-yellow-400' : 'bg-teal-400'"
                      :style="{ width: Math.min(parseHealth(b).mem_pct || 0, 100) + '%' }" />
                  </div>
                </div>
                <div>
                  <div class="flex items-center justify-between text-xs mb-1">
                    <span class="text-gray-400">Disk</span>
                    <span class="text-gray-300">{{ parseHealth(b).disk_pct?.toFixed(1) || 0 }}%</span>
                  </div>
                  <div class="w-full bg-gray-700 rounded-full h-1.5">
                    <div class="h-1.5 rounded-full transition-all"
                      :class="parseHealth(b).disk_pct > 80 ? 'bg-red-400' : parseHealth(b).disk_pct > 50 ? 'bg-yellow-400' : 'bg-teal-400'"
                      :style="{ width: Math.min(parseHealth(b).disk_pct || 0, 100) + '%' }" />
                  </div>
                </div>
              </div>

              <!-- Uptime -->
              <div class="text-xs text-gray-400 mb-4">
                Uptime: <span class="text-gray-300">{{ formatUptime(parseHealth(b).uptime_sec) }}</span>
              </div>

              <!-- Interface detail table -->
              <template v-if="parseHealth(b).interfaces?.length">
                <h4 class="text-xs text-gray-500 uppercase tracking-wider font-display mb-2">Interfaces</h4>
                <div class="overflow-x-auto mb-4">
                  <table class="w-full text-xs">
                    <thead>
                      <tr class="text-gray-500 border-b border-tactical-border">
                        <th class="text-left py-1.5 pr-3 font-medium">Interface</th>
                        <th class="text-left py-1.5 pr-3 font-medium">Type</th>
                        <th class="text-left py-1.5 pr-3 font-medium">Status</th>
                        <th class="text-left py-1.5 pr-3 font-medium">Signal</th>
                        <th class="text-left py-1.5 pr-3 font-medium">Health</th>
                        <th class="text-left py-1.5 font-medium">Messages</th>
                      </tr>
                    </thead>
                    <tbody>
                      <tr v-for="iface in parseHealth(b).interfaces" :key="iface.name"
                        class="border-b border-tactical-border/50">
                        <td class="py-1.5 pr-3 font-mono text-gray-300">{{ iface.name }}</td>
                        <td class="py-1.5 pr-3">
                          <span :class="interfaceTypeBadgeColor(birthInterfaceType(b, iface.name))">
                            {{ interfaceTypeLabel(birthInterfaceType(b, iface.name)) }}
                          </span>
                        </td>
                        <td class="py-1.5 pr-3">
                          <span class="flex items-center gap-1">
                            <span class="w-1.5 h-1.5 rounded-full" :class="interfaceStatusDot(iface.status)" />
                            <span :class="iface.status === 'online' ? 'text-green-400' : iface.status === 'error' ? 'text-red-400' : 'text-gray-400'">
                              {{ iface.status }}
                            </span>
                          </span>
                        </td>
                        <td class="py-1.5 pr-3 text-gray-300">{{ signalDisplay(iface) }}</td>
                        <td class="py-1.5 pr-3">
                          <span v-if="iface.health_score > 0"
                            :class="iface.health_score >= 80 ? 'text-green-400' : iface.health_score >= 50 ? 'text-yellow-400' : 'text-red-400'">
                            {{ iface.health_score }}%
                          </span>
                          <span v-else class="text-gray-500">—</span>
                        </td>
                        <td class="py-1.5 text-gray-300">{{ messageDisplay(iface) }}</td>
                      </tr>
                    </tbody>
                  </table>
                </div>
              </template>

              <!-- Reticulum stats -->
              <template v-if="parseHealth(b).reticulum">
                <h4 class="text-xs text-gray-500 uppercase tracking-wider font-display mb-2">Reticulum</h4>
                <div class="grid grid-cols-3 gap-3 text-xs mb-4">
                  <div>
                    <span class="text-gray-500">Routes</span>
                    <div class="text-gray-300 font-mono">{{ parseHealth(b).reticulum.routes }}</div>
                  </div>
                  <div>
                    <span class="text-gray-500">Links</span>
                    <div class="text-gray-300 font-mono">{{ parseHealth(b).reticulum.links }}</div>
                  </div>
                  <div>
                    <span class="text-gray-500">Announces relayed</span>
                    <div class="text-gray-300 font-mono">{{ parseHealth(b).reticulum.announces_relayed }}</div>
                  </div>
                </div>
              </template>

              <!-- Burst queue -->
              <template v-if="parseHealth(b).burst_queue">
                <div class="text-xs text-gray-400 mb-4">
                  Burst queue: <span class="text-gray-300 font-mono">{{ parseHealth(b).burst_queue.pending }}</span> pending
                </div>
              </template>
            </template>

            <!-- CoT info -->
            <template v-if="b.cot_callsign || b.cot_type">
              <h4 class="text-xs text-gray-500 uppercase tracking-wider font-display mb-2">CoT</h4>
              <div class="grid grid-cols-2 gap-3 text-xs mb-4">
                <div>
                  <span class="text-gray-500">Callsign</span>
                  <div class="text-gray-300 font-mono">{{ b.cot_callsign || '—' }}</div>
                </div>
                <div>
                  <span class="text-gray-500">Type</span>
                  <div class="text-gray-300 font-mono">{{ b.cot_type || '—' }}</div>
                </div>
              </div>
            </template>

            <!-- Location -->
            <template v-if="b.location_lat && b.location_lon">
              <div class="text-xs text-gray-400">
                Location: <span class="text-gray-300 font-mono">{{ b.location_lat.toFixed(6) }}, {{ b.location_lon.toFixed(6) }}</span>
              </div>
            </template>

            <!-- No health data -->
            <div v-if="!parseHealth(b)" class="text-xs text-gray-500 italic">
              No health data received yet
            </div>
          </div>
        </Transition>
      </div>
    </div>
  </div>
</template>

<style scoped>
.expand-enter-active,
.expand-leave-active {
  transition: all 0.2s ease;
  overflow: hidden;
}
.expand-enter-from,
.expand-leave-to {
  opacity: 0;
  max-height: 0;
}
.expand-enter-to,
.expand-leave-from {
  opacity: 1;
  max-height: 600px;
}
</style>
