<script setup>
import { ref, onMounted, computed } from 'vue'
import { devices, tenant } from '../api/client'
import { formatUTC } from '../utils/time'
import { ago } from '../utils/paths'
import { useOpsStore } from '../stores/ops'
import Icon from '../components/Icon.vue'
import UpgradeButton from '../components/UpgradeButton.vue'

const deviceList = ref([])
const ops = useOpsStore()
const q = ref('')
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

// A satellite device that is quiet is not failing: it reports when it has
// something to say. So "last heard" is plain text, and only a check-in that
// was asked for and missed is shown as an alarm (from the shared ops poll).
const TYPE_WORDS = { rockblock: 'RockBLOCK', iridium_imt: 'Iridium 9704', iridium_sbd: 'Iridium SBD', android: 'Android phone', other: 'Other' }
const missed = computed(() => new Set(ops.checkins.filter((c) => c.enabled && c.alerted).map((c) => c.device_imei)))
const watched = computed(() => Object.fromEntries(ops.checkins.filter((c) => c.enabled).map((c) => [c.device_imei, c])))
const shown = computed(() => {
  const t = q.value.trim().toLowerCase()
  return [...deviceList.value]
    .filter((d) => !t || `${d.imei} ${d.label || ''} ${d.type || ''}`.toLowerCase().includes(t))
    .sort((a, b) => new Date(b.last_seen || 0) - new Date(a.last_seen || 0))
})
</script>

<template>
  <div class="ms-page">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Devices</h1>
        <p class="ms-lede">Satellite modems and handhelds the Hub hears from directly.<template v-if="usage">
          <template v-if="usage.limit === -1"> {{ usage.used }} devices and kits registered.</template>
          <template v-else> {{ usage.used }} of {{ usage.limit }} devices and kits used on the {{ usage.plan }} plan.</template></template>
        </p>
      </div>
      <UpgradeButton v-if="atCap" :usage="usage" />
    </div>

    <!-- At the ceiling: say so before somebody fills in a form for a 402. The
         devices they already have are untouched, and that is worth saying. -->
    <div v-if="atCap" class="ms-note mb-4">
      The {{ usage.plan }} plan covers {{ usage.limit }} devices and kits together, and you have {{ usage.used }}.
      Everything already registered keeps working and keeps reporting. Remove one, or move up a plan, to add another.
      <UpgradeButton :usage="usage" variant="button" class="mt-2" />
    </div>

    <div v-if="error" role="alert" class="ms-alert mb-4">{{ error }}</div>

    <section class="ms-panel p-4 mb-5" aria-labelledby="reg-h">
      <h2 id="reg-h" class="ms-h2">Register a device</h2>
      <div class="flex flex-wrap items-end gap-2 mt-3">
        <label class="block flex-1 min-w-[180px]"><span class="ms-label">IMEI</span>
          <input v-model="newIMEI" placeholder="IMEI" inputmode="numeric" class="ms-input w-full mt-1 font-mono" @keydown.enter="addDevice" /></label>
        <label class="block flex-1 min-w-[160px]"><span class="ms-label">Name</span>
          <input v-model="newLabel" placeholder="Label (optional)" class="ms-input w-full mt-1" @keydown.enter="addDevice" /></label>
        <label class="block"><span class="ms-label">Kind</span>
          <select v-model="newType" class="ms-input mt-1">
            <option value="rockblock">RockBLOCK</option>
            <option value="iridium_imt">Iridium 9704</option>
            <option value="android">Android phone</option>
            <option value="other">Other</option>
          </select></label>
        <button class="ms-btn-primary" :disabled="atCap || !newIMEI.trim()" @click="addDevice">Add</button>
      </div>
    </section>

    <section v-if="deviceList.length > 0 || loading" class="ms-panel overflow-hidden" aria-label="Devices">
      <div class="px-4 py-3 border-b border-ms-border">
        <label class="relative block max-w-sm">
          <span class="sr-only">Filter</span>
          <Icon name="search" :size="14" class="absolute left-2.5 top-2 text-ms-muted" />
          <input v-model="q" type="search" placeholder="Filter by name, IMEI or kind" class="ms-input w-full pl-8" />
        </label>
      </div>
      <div class="overflow-x-auto">
        <table class="ms-table">
          <thead>
            <tr><th>Name</th><th>IMEI</th><th class="hidden sm:table-cell">Kind</th><th>Last heard</th><th class="hidden md:table-cell">Check-in</th><th></th></tr>
          </thead>
          <tbody>
            <tr v-if="loading && !deviceList.length"><td colspan="6" class="text-ms-muted">Loading devices.</td></tr>
            <tr v-for="d in shown" :key="d.imei" class="hover:bg-ms-well/40">
              <td><router-link :to="`/devices/${d.imei}`" class="text-[13px] font-medium text-ms-text hover:text-ms-primary">{{ d.label || d.imei }}</router-link></td>
              <td class="ms-id text-ms-text2">{{ d.imei }}</td>
              <td class="hidden sm:table-cell text-ms-muted">{{ TYPE_WORDS[d.type] || d.type }}</td>
              <td class="whitespace-nowrap" :title="formatUTC(d.last_seen)">
                <span v-if="missed.has(d.imei)" class="text-ms-error font-medium">Missed check-in</span>
                <span v-else class="text-ms-text2">{{ ago(d.last_seen) }}</span>
              </td>
              <td class="hidden md:table-cell text-ms-muted whitespace-nowrap">{{ watched[d.imei] ? `Every ${Math.round(watched[d.imei].interval_sec / 60)} min` : 'Not watched' }}</td>
              <td class="text-right"><button class="ms-btn-danger" @click="removeDevice(d.imei)">Delete</button></td>
            </tr>
          </tbody>
        </table>
      </div>
    </section>
    <div v-else class="ms-panel px-6 py-10 text-center">
      <Icon name="devices" :size="26" class="mx-auto text-ms-muted" />
      <h2 class="ms-h2 mt-3">No devices yet</h2>
      <p class="text-[13px] text-ms-muted mt-1 max-w-[52ch] mx-auto">Register a satellite modem by its IMEI and the Hub files its messages and positions under it.</p>
    </div>
  </div>
</template>
