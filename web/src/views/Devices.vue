<script setup>
import { ref, onMounted, computed } from 'vue'
import { devices, messages, tenant } from '../api/client'
import { formatUTC } from '../utils/time'
import EmptyState from '../components/EmptyState.vue'
import UpgradeButton from '../components/UpgradeButton.vue'

const deviceList = ref([])
const messageCounts = ref({})
const newIMEI = ref('')
const newLabel = ref('')
const newType = ref('rockblock')
const error = ref('')
const loading = ref(false)

// Plan usage (MESHSAT-989). Devices and bridges share one ceiling. An older
// Hub has no such endpoint, and then the page says nothing about plans.
const usage = ref(null)
const atCap = computed(() => usage.value && usage.value.limit !== -1 && usage.value.remaining === 0)

async function loadUsage() {
  try {
    usage.value = await tenant.usage()
  } catch {
    usage.value = null
  }
}

onMounted(async () => {
  await loadDevices()
  loadUsage()
})

async function loadDevices() {
  loading.value = true
  try {
    deviceList.value = await devices.list() || []
    // Load message counts per device in background
    for (const d of deviceList.value) {
      messages.list(d.imei, 1).then(msgs => {
        messageCounts.value[d.imei] = Array.isArray(msgs) ? msgs.length : 0
      }).catch(() => {})
    }
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function addDevice() {
  if (!newIMEI.value.trim()) return
  error.value = ''
  try {
    await devices.create({ imei: newIMEI.value.trim(), label: newLabel.value.trim() || newIMEI.value.trim(), type: newType.value })
    loadUsage()
    newIMEI.value = ''
    newLabel.value = ''
    await loadDevices()
  } catch (e) {
    error.value = e.message
  }
}

async function removeDevice(imei) {
  if (!confirm(`Delete device ${imei}?`)) return
  try {
    await devices.delete(imei)
    await loadDevices()
  } catch (e) {
    error.value = e.message
  }
}

function deviceStatus(d) {
  if (!d.last_seen || d.last_seen === '0001-01-01T00:00:00Z') return 'unknown'
  const diff = Date.now() - new Date(d.last_seen).getTime()
  if (diff < 3600000) return 'online'    // < 1 hour
  if (diff < 86400000) return 'idle'     // < 24 hours
  return 'offline'
}

function statusColor(status) {
  if (status === 'online') return 'text-ms-success'
  if (status === 'idle') return 'text-ms-warning'
  if (status === 'offline') return 'text-ms-error'
  return 'text-gray-500'
}

function formatLastSeen(d) {
  return formatUTC(d.last_seen)
}
</script>

<template>
  <div>
    <div class="flex flex-wrap items-baseline gap-x-3 mb-4">
      <h1 class="text-2xl font-display font-bold">Devices</h1>
      <p v-if="usage" class="text-sm text-gray-400">
        <template v-if="usage.limit === -1">{{ usage.used }} registered</template>
        <template v-else>{{ usage.used }} / {{ usage.limit }} on the {{ usage.plan }} plan</template>
        <span v-if="usage.bridges"> ({{ usage.devices }} device<span v-if="usage.devices !== 1">s</span>,
          {{ usage.bridges }} bridge<span v-if="usage.bridges !== 1">s</span>)</span>
      </p>
      <UpgradeButton v-if="atCap" :usage="usage" class="ml-auto" />
    </div>

    <!-- At the ceiling: say so before somebody fills in a form for a 402. The
         devices they already have are untouched, and that is worth saying. -->
    <div v-if="atCap" class="bg-amber-900/50 border border-amber-700/50 text-amber-200 px-4 py-3 rounded mb-4 text-sm">
      The {{ usage.plan }} plan covers {{ usage.limit }} devices and bridges together, and you have {{ usage.used }}.
      Everything already registered keeps working and keeps reporting. Remove one, or move up a plan, to add another.
      <UpgradeButton :usage="usage" variant="button" class="mt-2" />
    </div>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">
      {{ error }}
    </div>

    <!-- Add device form -->
    <div class="flex flex-wrap gap-2 mb-4">
      <input v-model="newIMEI" placeholder="IMEI"
        class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 placeholder-gray-500 focus:outline-none focus:border-brand-primary flex-1 min-w-[180px]" />
      <input v-model="newLabel" placeholder="Label (optional)"
        class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 placeholder-gray-500 focus:outline-none focus:border-brand-primary flex-1 min-w-[140px]" />
      <select v-model="newType"
        class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 focus:outline-none focus:border-brand-primary">
        <option value="rockblock">RockBLOCK</option>
        <option value="android">Android</option>
        <option value="other">Other</option>
      </select>
      <button @click="addDevice" :disabled="atCap"
        class="bg-brand-accent hover:bg-brand-primary disabled:opacity-50 disabled:cursor-not-allowed text-ms-on-primary px-4 py-2 rounded-lg font-medium transition-colors">
        Add
      </button>
    </div>

    <!-- Device table -->
    <div v-if="deviceList.length > 0 || loading" class="overflow-x-auto">
      <table class="w-full border-collapse text-sm">
        <thead>
          <tr class="border-b border-tactical-border text-left text-gray-500">
            <th class="px-3 py-2">Status</th>
            <th class="px-3 py-2">IMEI</th>
            <th class="px-3 py-2">Label</th>
            <th class="px-3 py-2">Type</th>
            <th class="px-3 py-2">Last Seen</th>
            <th class="px-3 py-2 text-right">Msgs</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="d in deviceList" :key="d.imei" class="border-b border-tactical-border/30 hover:bg-white/[0.02]">
            <td class="px-3 py-2">
              <span :class="statusColor(deviceStatus(d))" class="font-medium text-xs uppercase">
                {{ deviceStatus(d) }}
              </span>
            </td>
            <td class="px-3 py-2 font-mono text-xs">
              <router-link :to="`/devices/${d.imei}`" class="text-brand-primary hover:text-brand-primary hover:underline">{{ d.imei }}</router-link>
            </td>
            <td class="px-3 py-2">{{ d.label }}</td>
            <td class="px-3 py-2 text-gray-400">{{ d.type }}</td>
            <td class="px-3 py-2 text-gray-400">{{ formatLastSeen(d) }}</td>
            <td class="px-3 py-2 text-right text-gray-400">{{ messageCounts[d.imei] ?? '...' }}</td>
            <td class="px-3 py-2 text-right">
              <button @click="removeDevice(d.imei)"
                class="bg-red-900 hover:bg-red-800 text-red-200 px-2 py-1 rounded-lg text-xs transition-colors">
                Delete
              </button>
            </td>
          </tr>
          <tr v-if="loading">
            <td colspan="7" class="px-3 py-8 text-center text-gray-500">Loading...</td>
          </tr>
        </tbody>
      </table>
    </div>
    <EmptyState v-else icon="device" title="No devices registered" message="Register your first device to start tracking satellite messages and positions." />
  </div>
</template>
