<script setup>
import { ref, onMounted } from 'vue'
import { deadman, devices, escalation } from '../api/client'

const configs = ref([])
const deviceList = ref([])
const chainList = ref([])
const error = ref('')
const loading = ref(true)

// New/edit form
const form = ref({ imei: '', chain_id: '', interval_min: 60, grace_min: 15, enabled: true })
const editing = ref(false)

onMounted(async () => {
  await loadData()
})

async function loadData() {
  loading.value = true
  try {
    const [c, d, ch] = await Promise.all([
      deadman.list().catch(() => []),
      devices.list().catch(() => []),
      escalation.listChains().catch(() => []),
    ])
    configs.value = Array.isArray(c) ? c : []
    deviceList.value = Array.isArray(d) ? d : []
    chainList.value = Array.isArray(ch) ? ch : []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function editConfig(c) {
  form.value = {
    imei: c.device_imei,
    chain_id: c.chain_id || '',
    interval_min: Math.round((c.interval || 3600000000000) / 60000000000),
    grace_min: Math.round((c.grace || 900000000000) / 60000000000),
    enabled: c.enabled !== false,
  }
  editing.value = true
}

function newConfig() {
  form.value = { imei: '', chain_id: '', interval_min: 60, grace_min: 15, enabled: true }
  editing.value = true
}

async function saveConfig() {
  if (!form.value.imei) return
  error.value = ''
  try {
    await deadman.configure(form.value.imei, {
      chain_id: form.value.chain_id,
      interval_min: parseInt(form.value.interval_min) || 60,
      grace_min: parseInt(form.value.grace_min) || 15,
      enabled: form.value.enabled,
    })
    editing.value = false
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

async function deleteConfig(imei) {
  if (!confirm(`Remove dead man's switch for ${imei}?`)) return
  try {
    await deadman.delete(imei)
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

async function snoozeDevice(imei) {
  try {
    await deadman.snooze(imei, { duration_min: 60 })
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

function chainName(id) {
  const c = chainList.value.find(c => c.id === id)
  return c ? c.name : id || '—'
}
</script>

<template>
  <div>
    <h1 class="text-xl font-bold mb-4">Dead Man's Switch</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">{{ error }}</div>

    <div class="flex justify-end mb-4">
      <button @click="newConfig()"
        class="bg-cyan-600 hover:bg-cyan-500 text-white px-3 py-1 rounded text-sm transition-colors">
        {{ editing ? 'Cancel' : '+ Configure Device' }}
      </button>
    </div>

    <!-- Config form -->
    <div v-if="editing" class="bg-gray-800 rounded-lg p-4 mb-4">
      <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 mb-3">
        <div>
          <label class="text-xs text-gray-400">Device</label>
          <select v-model="form.imei"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full focus:outline-none focus:border-cyan-400">
            <option value="">Select device...</option>
            <option v-for="d in deviceList" :key="d.imei" :value="d.imei">{{ d.label || d.imei }}</option>
          </select>
        </div>
        <div>
          <label class="text-xs text-gray-400">Escalation Chain</label>
          <select v-model="form.chain_id"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full focus:outline-none focus:border-cyan-400">
            <option value="">None</option>
            <option v-for="c in chainList" :key="c.id" :value="c.id">{{ c.name }}</option>
          </select>
        </div>
        <div>
          <label class="text-xs text-gray-400">Check-in interval (min)</label>
          <input v-model="form.interval_min" type="number" min="1"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full focus:outline-none focus:border-cyan-400" />
        </div>
        <div>
          <label class="text-xs text-gray-400">Grace period (min)</label>
          <input v-model="form.grace_min" type="number" min="0"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full focus:outline-none focus:border-cyan-400" />
        </div>
      </div>
      <div class="flex items-center justify-between">
        <label class="flex items-center gap-2 text-sm text-gray-400">
          <input type="checkbox" v-model="form.enabled" class="rounded" /> Enabled
        </label>
        <button @click="saveConfig"
          class="bg-cyan-600 hover:bg-cyan-500 text-white px-4 py-2 rounded text-sm transition-colors">Save</button>
      </div>
    </div>

    <!-- Configs table -->
    <div class="overflow-x-auto">
      <table class="w-full border-collapse text-sm">
        <thead>
          <tr class="border-b border-gray-700 text-left text-gray-400">
            <th class="px-3 py-2">Device</th>
            <th class="px-3 py-2">Interval</th>
            <th class="px-3 py-2">Grace</th>
            <th class="px-3 py-2">Chain</th>
            <th class="px-3 py-2">Status</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="c in configs" :key="c.device_imei" class="border-b border-gray-800 hover:bg-gray-800/50">
            <td class="px-3 py-2 font-mono text-xs">{{ c.device_imei }}</td>
            <td class="px-3 py-2 text-gray-400">{{ Math.round((c.interval || 0) / 60000000000) }}m</td>
            <td class="px-3 py-2 text-gray-400">{{ Math.round((c.grace || 0) / 60000000000) }}m</td>
            <td class="px-3 py-2 text-gray-400">{{ chainName(c.chain_id) }}</td>
            <td class="px-3 py-2">
              <span v-if="c.snoozed_until" class="text-yellow-400 text-xs">Snoozed</span>
              <span v-else-if="c.enabled" class="text-green-400 text-xs">Active</span>
              <span v-else class="text-gray-500 text-xs">Disabled</span>
            </td>
            <td class="px-3 py-2 text-right flex gap-1 justify-end">
              <button @click="snoozeDevice(c.device_imei)"
                class="bg-yellow-900 hover:bg-yellow-800 text-yellow-200 px-2 py-1 rounded text-xs transition-colors">Snooze 1h</button>
              <button @click="editConfig(c)"
                class="bg-gray-700 hover:bg-gray-600 text-gray-200 px-2 py-1 rounded text-xs transition-colors">Edit</button>
              <button @click="deleteConfig(c.device_imei)"
                class="bg-red-900 hover:bg-red-800 text-red-200 px-2 py-1 rounded text-xs transition-colors">Delete</button>
            </td>
          </tr>
          <tr v-if="configs.length === 0 && !loading">
            <td colspan="6" class="px-3 py-8 text-center text-gray-500">No dead man's switch configured</td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>
  </div>
</template>
