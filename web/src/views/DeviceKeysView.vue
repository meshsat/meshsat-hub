<script setup>
import { ref, onMounted, watch } from 'vue'
import { devices, deviceKeys } from '../api/client'

const deviceList = ref([])
const selectedImei = ref('')
const keys = ref([])
const loading = ref(false)
const error = ref('')
const mode = ref('decrypt')
const createdKey = ref(null)
const copied = ref(false)

async function loadDevices() {
  try {
    deviceList.value = await devices.list()
    if (deviceList.value.length && !selectedImei.value) {
      selectedImei.value = deviceList.value[0].imei
    }
  } catch (e) {
    error.value = e.message
  }
}

async function loadKeys() {
  if (!selectedImei.value) return
  loading.value = true
  error.value = ''
  try {
    keys.value = await deviceKeys.list(selectedImei.value)
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function generateKey() {
  if (!selectedImei.value) return
  error.value = ''
  createdKey.value = null
  copied.value = false
  try {
    const result = await deviceKeys.create(selectedImei.value, { mode: mode.value })
    createdKey.value = result
    await loadKeys()
  } catch (e) {
    error.value = e.message
  }
}

async function deleteKey(id) {
  if (!confirm('Revoke this encryption key? Devices using it will no longer be able to decrypt.')) return
  error.value = ''
  try {
    await deviceKeys.delete(selectedImei.value, id)
    await loadKeys()
  } catch (e) {
    error.value = e.message
  }
}

function copyKey() {
  if (!createdKey.value?.key_hex) return
  navigator.clipboard.writeText(createdKey.value.key_hex)
  copied.value = true
  setTimeout(() => { copied.value = false }, 2000)
}

function dismissCreatedKey() {
  createdKey.value = null
  copied.value = false
}

watch(selectedImei, loadKeys)

onMounted(() => {
  loadDevices()
})
</script>

<template>
  <div class="max-w-4xl mx-auto">
    <h1 class="text-2xl font-bold mb-4">Device Encryption Keys</h1>

    <!-- Device selector -->
    <div class="flex items-center gap-4 mb-6">
      <label class="text-sm text-gray-400">Device</label>
      <select v-model="selectedImei" class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm flex-1 max-w-xs">
        <option v-for="d in deviceList" :key="d.imei" :value="d.imei">
          {{ d.label || d.imei }} ({{ d.imei }})
        </option>
      </select>
    </div>

    <!-- Error banner -->
    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-2 rounded mb-4 text-sm">
      {{ error }}
    </div>

    <!-- Created key banner (shown once) -->
    <div v-if="createdKey" class="bg-emerald-900/50 border border-emerald-700 rounded p-4 mb-6">
      <div class="flex items-start justify-between">
        <div>
          <div class="text-emerald-300 font-medium mb-1">Key created successfully</div>
          <div class="text-xs text-gray-400 mb-2">Copy this key now. It will not be shown again.</div>
        </div>
        <button @click="dismissCreatedKey" class="text-gray-500 hover:text-gray-300">&times;</button>
      </div>
      <div class="flex items-center gap-2">
        <code class="bg-gray-900 px-3 py-2 rounded text-sm font-mono text-emerald-200 flex-1 break-all select-all">{{ createdKey.key_hex }}</code>
        <button @click="copyKey"
          class="px-3 py-2 rounded text-sm font-medium shrink-0"
          :class="copied ? 'bg-emerald-700 text-emerald-200' : 'bg-gray-700 text-gray-300 hover:bg-gray-600'">
          {{ copied ? 'Copied' : 'Copy' }}
        </button>
      </div>
      <div class="text-xs text-gray-500 mt-2">
        Hash: <code class="text-gray-400">{{ createdKey.key_hash?.slice(0, 16) }}...</code>
        &middot; Mode: <span class="text-gray-400">{{ createdKey.mode }}</span>
      </div>
    </div>

    <!-- Generate key form -->
    <div class="bg-gray-800 border border-gray-700 rounded p-4 mb-6">
      <h2 class="text-sm font-medium text-gray-300 mb-3">Generate New Key</h2>
      <div class="flex items-end gap-4">
        <div>
          <label class="block text-xs text-gray-500 mb-1">Mode</label>
          <select v-model="mode" class="bg-gray-900 border border-gray-700 rounded px-3 py-2 text-sm">
            <option value="decrypt">Decrypt (server can read messages)</option>
            <option value="passthrough">Passthrough (true E2E, server cannot read)</option>
          </select>
        </div>
        <button @click="generateKey" :disabled="!selectedImei"
          class="px-4 py-2 rounded text-sm font-medium bg-cyan-700 text-white hover:bg-cyan-600 disabled:opacity-50 disabled:cursor-not-allowed">
          Generate Key
        </button>
      </div>
    </div>

    <!-- Key list -->
    <div class="bg-gray-800 border border-gray-700 rounded">
      <div class="px-4 py-3 border-b border-gray-700">
        <h2 class="text-sm font-medium text-gray-300">Keys for {{ selectedImei || '...' }}</h2>
      </div>
      <div v-if="loading" class="px-4 py-8 text-center text-gray-500 text-sm">Loading...</div>
      <div v-else-if="!keys.length" class="px-4 py-8 text-center text-gray-500 text-sm">No encryption keys for this device.</div>
      <table v-else class="w-full text-sm">
        <thead>
          <tr class="text-left text-xs text-gray-500 border-b border-gray-700">
            <th class="px-4 py-2">Hash</th>
            <th class="px-4 py-2">Mode</th>
            <th class="px-4 py-2">Created</th>
            <th class="px-4 py-2 text-right">Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(k, i) in keys" :key="k.id" class="border-b border-gray-700/50 hover:bg-gray-700/30">
            <td class="px-4 py-2 font-mono text-xs">
              {{ k.key_hash?.slice(0, 16) }}...
              <span v-if="i === 0" class="ml-1 text-[10px] px-1.5 py-0.5 rounded bg-cyan-900/50 text-cyan-300">active</span>
            </td>
            <td class="px-4 py-2">
              <span class="text-xs px-1.5 py-0.5 rounded"
                :class="k.mode === 'decrypt' ? 'bg-emerald-900/50 text-emerald-300' : 'bg-gray-700 text-gray-400'">
                {{ k.mode }}
              </span>
            </td>
            <td class="px-4 py-2 text-gray-400">{{ new Date(k.created_at).toLocaleString() }}</td>
            <td class="px-4 py-2 text-right">
              <button @click="deleteKey(k.id)" class="text-xs text-red-400 hover:text-red-300">Revoke</button>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
