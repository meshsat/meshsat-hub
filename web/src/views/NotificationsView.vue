<script setup>
import { ref, onMounted } from 'vue'
import { notifications, devices } from '../api/client'

const prefs = ref([])
const deviceList = ref([])
const error = ref('')
const loading = ref(true)

const form = ref({ imei: '', urls: '', events: 'sos,deadman,geofence', enabled: true })
const editing = ref(false)

onMounted(async () => {
  await loadData()
})

async function loadData() {
  loading.value = true
  try {
    const [p, d] = await Promise.all([
      notifications.listPrefs().catch(() => []),
      devices.list().catch(() => []),
    ])
    prefs.value = Array.isArray(p) ? p : []
    deviceList.value = Array.isArray(d) ? d : []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function editPref(p) {
  form.value = {
    imei: p.device_imei,
    urls: (p.urls || []).join('\n'),
    events: (p.events || []).join(','),
    enabled: p.enabled !== false,
  }
  editing.value = true
}

function newPref() {
  form.value = { imei: '', urls: '', events: 'sos,deadman,geofence', enabled: true }
  editing.value = true
}

async function savePref() {
  if (!form.value.imei) return
  error.value = ''
  try {
    await notifications.savePref(form.value.imei, {
      device_imei: form.value.imei,
      urls: form.value.urls.split('\n').map(u => u.trim()).filter(Boolean),
      events: form.value.events.split(',').map(e => e.trim()).filter(Boolean),
      enabled: form.value.enabled,
    })
    editing.value = false
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

async function deletePref(imei) {
  if (!confirm(`Remove notification preferences for ${imei}?`)) return
  try {
    await notifications.deletePref(imei)
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}
</script>

<template>
  <div>
    <h1 class="text-xl font-bold mb-4">Notifications</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">{{ error }}</div>

    <div class="flex justify-end mb-4">
      <button @click="editing ? (editing = false) : newPref()"
        class="bg-teal-600 hover:bg-teal-500 text-white px-3 py-1 rounded text-sm transition-colors">
        {{ editing ? 'Cancel' : '+ Add Preference' }}
      </button>
    </div>

    <!-- Form -->
    <div v-if="editing" class="bg-gray-800 rounded-lg p-4 mb-4">
      <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 mb-3">
        <div>
          <label class="text-xs text-gray-400">Device</label>
          <select v-model="form.imei"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full focus:outline-none focus:border-teal-400">
            <option value="">Select device...</option>
            <option v-for="d in deviceList" :key="d.imei" :value="d.imei">{{ d.label || d.imei }}</option>
          </select>
        </div>
        <div>
          <label class="text-xs text-gray-400">Events (comma-sep)</label>
          <input v-model="form.events" placeholder="sos,deadman,geofence"
            class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full placeholder-gray-500 focus:outline-none focus:border-teal-400" />
        </div>
      </div>
      <div class="mb-3">
        <label class="text-xs text-gray-400">Apprise URLs (one per line)</label>
        <textarea v-model="form.urls" rows="3" placeholder="mailto://user:pass@gmail.com&#10;slack://token/channel"
          class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 w-full placeholder-gray-500 focus:outline-none focus:border-teal-400 font-mono text-sm"></textarea>
      </div>
      <div class="flex items-center justify-between">
        <label class="flex items-center gap-2 text-sm text-gray-400">
          <input type="checkbox" v-model="form.enabled" class="rounded" /> Enabled
        </label>
        <button @click="savePref"
          class="bg-teal-600 hover:bg-teal-500 text-white px-4 py-2 rounded text-sm transition-colors">Save</button>
      </div>
    </div>

    <!-- Prefs table -->
    <div class="overflow-x-auto">
      <table class="w-full border-collapse text-sm">
        <thead>
          <tr class="border-b border-gray-700 text-left text-gray-400">
            <th class="px-3 py-2">Device</th>
            <th class="px-3 py-2">URLs</th>
            <th class="px-3 py-2">Events</th>
            <th class="px-3 py-2">Status</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="p in prefs" :key="p.device_imei" class="border-b border-gray-800 hover:bg-gray-800/50">
            <td class="px-3 py-2 font-mono text-xs">{{ p.device_imei }}</td>
            <td class="px-3 py-2 text-gray-400 text-xs">{{ (p.urls || []).length }} configured</td>
            <td class="px-3 py-2">
              <span v-for="e in (p.events || [])" :key="e"
                class="inline-block bg-gray-700 text-gray-300 text-xs px-1.5 py-0.5 rounded mr-1">{{ e }}</span>
            </td>
            <td class="px-3 py-2">
              <span v-if="p.enabled" class="text-green-400 text-xs">Active</span>
              <span v-else class="text-gray-500 text-xs">Disabled</span>
            </td>
            <td class="px-3 py-2 text-right flex gap-1 justify-end">
              <button @click="editPref(p)"
                class="bg-gray-700 hover:bg-gray-600 text-gray-200 px-2 py-1 rounded text-xs transition-colors">Edit</button>
              <button @click="deletePref(p.device_imei)"
                class="bg-red-900 hover:bg-red-800 text-red-200 px-2 py-1 rounded text-xs transition-colors">Delete</button>
            </td>
          </tr>
          <tr v-if="prefs.length === 0 && !loading">
            <td colspan="5" class="px-3 py-8 text-center text-gray-500">No notification preferences configured</td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>
  </div>
</template>
