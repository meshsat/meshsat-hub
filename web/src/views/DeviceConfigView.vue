<script setup>
import { ref, onMounted, watch } from 'vue'
import { deviceConfig, devices } from '../api/client'
import { formatUTC, formatDateUTC } from '../utils/time'

const deviceList = ref([])
const selectedIMEI = ref('')
const currentConfig = ref(null)
const history = ref([])
const error = ref('')
const loading = ref(true)

const editMode = ref(false)
const editJSON = ref('')
const editComment = ref('')

onMounted(async () => {
  try {
    deviceList.value = await devices.list() || []
    if (deviceList.value.length > 0) {
      selectedIMEI.value = deviceList.value[0].imei
    }
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
})

watch(selectedIMEI, async (imei) => {
  if (!imei) return
  await loadConfig(imei)
})

async function loadConfig(imei) {
  error.value = ''
  try {
    const [latest, hist] = await Promise.all([
      deviceConfig.getLatest(imei).catch(() => null),
      deviceConfig.listVersions(imei).catch(() => []),
    ])
    currentConfig.value = latest
    history.value = Array.isArray(hist) ? hist : []
    if (latest) {
      editJSON.value = typeof latest.config === 'string' ? latest.config : JSON.stringify(latest.config, null, 2)
    } else {
      editJSON.value = '{}'
    }
  } catch (e) {
    error.value = e.message
  }
}

function startEdit() {
  editMode.value = true
  editComment.value = ''
}

async function saveConfig() {
  if (!selectedIMEI.value) return
  error.value = ''
  try {
    JSON.parse(editJSON.value) // validate
  } catch {
    error.value = 'Invalid JSON'
    return
  }
  try {
    await deviceConfig.createVersion(selectedIMEI.value, {
      config: editJSON.value,
      comment: editComment.value,
    })
    editMode.value = false
    await loadConfig(selectedIMEI.value)
  } catch (e) {
    error.value = e.message
  }
}

async function viewVersion(v) {
  try {
    const ver = await deviceConfig.getVersion(selectedIMEI.value, v.version)
    currentConfig.value = ver
    editJSON.value = typeof ver.config === 'string' ? ver.config : JSON.stringify(ver.config, null, 2)
    editMode.value = false
  } catch (e) {
    error.value = e.message
  }
}
</script>

<template>
  <div>
    <h1 class="text-2xl font-display font-bold mb-4">Device Configuration</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">{{ error }}</div>

    <!-- Device selector -->
    <div class="flex flex-wrap gap-3 mb-4">
      <select v-model="selectedIMEI"
        class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 focus:outline-none focus:border-teal-500 flex-1 min-w-[200px]">
        <option value="">Select device...</option>
        <option v-for="d in deviceList" :key="d.imei" :value="d.imei">{{ d.label || d.imei }} ({{ d.imei }})</option>
      </select>
      <button v-if="!editMode && selectedIMEI" @click="startEdit"
        class="bg-teal-600 hover:bg-teal-500 text-white px-4 py-2 rounded text-sm transition-colors">Edit Config</button>
    </div>

    <div v-if="selectedIMEI" class="grid grid-cols-1 lg:grid-cols-3 gap-4">
      <!-- Config editor / viewer -->
      <div class="lg:col-span-2">
        <div class="bg-gray-900 rounded-xl p-4">
          <div class="flex items-center justify-between mb-3">
            <h2 class="text-lg font-semibold">
              {{ editMode ? 'Edit Configuration' : 'Current Configuration' }}
            </h2>
            <span v-if="currentConfig" class="text-xs text-gray-400 font-mono">v{{ currentConfig.version }}</span>
          </div>

          <div v-if="editMode" class="mb-3">
            <textarea v-model="editJSON" rows="16"
              class="bg-gray-900 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full font-mono text-sm focus:outline-none focus:border-teal-500"></textarea>
            <input v-model="editComment" placeholder="Change comment (optional)"
              class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full mt-2 placeholder-gray-500 focus:outline-none focus:border-teal-500" />
            <div class="flex gap-2 mt-3 justify-end">
              <button @click="editMode = false"
                class="bg-gray-700 hover:bg-gray-600 text-gray-200 px-4 py-2 rounded text-sm transition-colors">Cancel</button>
              <button @click="saveConfig"
                class="bg-teal-600 hover:bg-teal-500 text-white px-4 py-2 rounded text-sm transition-colors">Save New Version</button>
            </div>
          </div>

          <div v-else>
            <pre v-if="currentConfig" class="bg-gray-900 rounded p-3 font-mono text-sm text-gray-300 overflow-auto max-h-96">{{ typeof currentConfig.config === 'string' ? currentConfig.config : JSON.stringify(currentConfig.config, null, 2) }}</pre>
            <div v-else class="text-gray-500 py-4">No configuration found</div>
            <div v-if="currentConfig" class="mt-2 text-xs text-gray-500">
              <span v-if="currentConfig.author">By {{ currentConfig.author }}</span>
              <span v-if="currentConfig.comment"> &middot; {{ currentConfig.comment }}</span>
              <span v-if="currentConfig.created_at"> &middot; {{ formatUTC(currentConfig.created_at) }}</span>
            </div>
          </div>
        </div>
      </div>

      <!-- Version history -->
      <div>
        <div class="bg-gray-900 rounded-xl p-4">
          <h2 class="text-lg font-semibold mb-3">Version History</h2>
          <div v-if="history.length === 0" class="text-gray-500 text-sm">No versions</div>
          <div v-for="v in history" :key="v.version"
            @click="viewVersion(v)"
            class="flex items-center justify-between py-2 px-2 rounded cursor-pointer hover:bg-white/5 transition-colors"
            :class="currentConfig && currentConfig.version === v.version ? 'bg-gray-800/50' : ''">
            <div>
              <div class="font-mono text-xs text-teal-400">v{{ v.version }}</div>
              <div v-if="v.comment" class="text-xs text-gray-400">{{ v.comment }}</div>
            </div>
            <div class="text-xs text-gray-500">{{ formatDateUTC(v.created_at) }}</div>
          </div>
        </div>
      </div>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>
  </div>
</template>
