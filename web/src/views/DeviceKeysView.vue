<script setup>
import { ref, onMounted, watch } from 'vue'
import { devices, deviceKeys, channelKeys, bridges as bridgesApi } from '../api/client'

const deviceList = ref([])
const selectedImei = ref('')
const keys = ref([])
const loading = ref(false)
const error = ref('')
const mode = ref('decrypt')
const createdKey = ref(null)
const copied = ref(false)

// Import key state
const showImport = ref(false)
const importHex = ref('')
const importMode = ref('decrypt')

// Rotate & distribute state
const showRotate = ref(false)
const rotateChannelType = ref('iridium')
const rotateAddress = ref('')
const rotateBridgeIds = ref('')
const rotateResult = ref(null)

// Distribute state
const distributeKeyId = ref(null)
const distributeBridgeIds = ref('')
const distributeResult = ref(null)

// Channel key rotation state
const showChannelRotate = ref(false)
const chChannelType = ref('iridium')
const chAddress = ref('')
const chBridgeIds = ref('')
const chResult = ref(null)

// Bridge list for selectors
const bridgeList = ref([])

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

async function importKey() {
  if (!selectedImei.value || !importHex.value.trim()) return
  error.value = ''
  createdKey.value = null
  try {
    const result = await deviceKeys.import(selectedImei.value, { key_hex: importHex.value.trim(), mode: importMode.value })
    createdKey.value = result
    importHex.value = ''
    showImport.value = false
    await loadKeys()
  } catch (e) {
    error.value = e.message
  }
}

async function rotateAndDistribute() {
  if (!selectedImei.value) return
  error.value = ''
  rotateResult.value = null
  const ids = rotateBridgeIds.value.split(',').map(s => s.trim()).filter(Boolean)
  try {
    const result = await deviceKeys.rotateAndDistribute(selectedImei.value, {
      channel_type: rotateChannelType.value,
      address: rotateAddress.value,
      bridge_ids: ids.length ? ids : undefined,
    })
    rotateResult.value = result
    await loadKeys()
  } catch (e) {
    error.value = e.message
  }
}

async function distributeKey(keyId) {
  error.value = ''
  distributeResult.value = null
  distributeKeyId.value = keyId
  const ids = distributeBridgeIds.value.split(',').map(s => s.trim()).filter(Boolean)
  try {
    const result = await deviceKeys.distribute(selectedImei.value, { bridge_ids: ids.length ? ids : undefined })
    distributeResult.value = result
  } catch (e) {
    error.value = e.message
  }
}

async function rotateChannelKey() {
  error.value = ''
  chResult.value = null
  const ids = chBridgeIds.value.split(',').map(s => s.trim()).filter(Boolean)
  try {
    const result = await channelKeys.rotate({
      channel_type: chChannelType.value,
      address: chAddress.value,
      bridge_ids: ids.length ? ids : undefined,
    })
    chResult.value = result
  } catch (e) {
    error.value = e.message
  }
}

watch(selectedImei, loadKeys)

onMounted(async () => {
  loadDevices()
  bridgeList.value = await bridgesApi.list().catch(() => [])
})
</script>

<template>
  <div class="max-w-4xl mx-auto">
    <h1 class="text-2xl font-display font-bold mb-4">Device Encryption Keys</h1>

    <!-- Device selector -->
    <div class="flex items-center gap-4 mb-6">
      <label class="text-sm text-gray-400">Device</label>
      <select v-model="selectedImei" class="bg-tactical-surface border border-tactical-border rounded-lg px-3 py-2 text-sm flex-1 max-w-xs">
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

    <!-- Key actions row -->
    <div class="bg-tactical-surface border border-tactical-border rounded-lg p-4 mb-6">
      <h2 class="text-sm font-medium text-gray-300 mb-3">Key Operations</h2>
      <div class="flex flex-wrap items-end gap-4">
        <div>
          <label class="block text-xs text-gray-500 mb-1">Mode</label>
          <select v-model="mode" class="bg-tactical-surface border border-gray-700 rounded px-3 py-2 text-sm">
            <option value="decrypt">Decrypt (server can read messages)</option>
            <option value="passthrough">Passthrough (true E2E, server cannot read)</option>
          </select>
        </div>
        <button @click="generateKey" :disabled="!selectedImei"
          class="px-4 py-2 rounded text-sm font-medium bg-teal-700 text-white hover:bg-teal-600 disabled:opacity-50 disabled:cursor-not-allowed">
          Generate Key
        </button>
        <button @click="showImport = !showImport"
          class="px-4 py-2 rounded text-sm font-medium bg-gray-700 text-gray-300 hover:bg-gray-600">
          {{ showImport ? 'Cancel Import' : 'Import Key' }}
        </button>
        <button @click="showRotate = !showRotate"
          class="px-4 py-2 rounded text-sm font-medium bg-gray-700 text-gray-300 hover:bg-gray-600">
          {{ showRotate ? 'Cancel' : 'Rotate & Distribute' }}
        </button>
      </div>
    </div>

    <!-- Import key form -->
    <div v-if="showImport" class="bg-tactical-surface border border-tactical-border rounded-lg p-4 mb-6">
      <h2 class="text-sm font-medium text-gray-300 mb-3">Import Existing Key</h2>
      <div class="space-y-3">
        <input v-model="importHex" placeholder="Hex-encoded AES-256 key (64 hex characters)"
          class="w-full bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm font-mono">
        <div class="flex items-end gap-3">
          <div>
            <label class="block text-xs text-gray-500 mb-1">Mode</label>
            <select v-model="importMode" class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
              <option value="decrypt">Decrypt</option>
              <option value="passthrough">Passthrough</option>
            </select>
          </div>
          <button @click="importKey" :disabled="!importHex.trim() || !selectedImei"
            class="px-4 py-2 rounded text-sm font-medium bg-teal-600 text-white hover:bg-teal-500 disabled:opacity-50">
            Import
          </button>
        </div>
      </div>
    </div>

    <!-- Rotate & distribute form -->
    <div v-if="showRotate" class="bg-tactical-surface border border-tactical-border rounded-lg p-4 mb-6">
      <h2 class="text-sm font-medium text-gray-300 mb-3">Rotate Key & Distribute to Bridges</h2>
      <div class="grid grid-cols-1 sm:grid-cols-3 gap-3 mb-3">
        <div>
          <label class="block text-xs text-gray-500 mb-1">Channel Type</label>
          <select v-model="rotateChannelType" class="w-full bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
            <option value="iridium">Iridium</option>
            <option value="sms">SMS</option>
            <option value="mesh">Mesh</option>
          </select>
        </div>
        <div>
          <label class="block text-xs text-gray-500 mb-1">Address</label>
          <input v-model="rotateAddress" placeholder="e.g. +31612345678" class="w-full bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
        </div>
        <div>
          <label class="block text-xs text-gray-500 mb-1">Bridge IDs (comma-separated, blank = all)</label>
          <input v-model="rotateBridgeIds" placeholder="bridge-1, bridge-2" class="w-full bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
        </div>
      </div>
      <button @click="rotateAndDistribute" :disabled="!selectedImei"
        class="px-4 py-2 rounded text-sm font-medium bg-teal-600 text-white hover:bg-teal-500 disabled:opacity-50">
        Rotate & Distribute
      </button>
      <div v-if="rotateResult" class="mt-3 bg-emerald-900/50 border border-emerald-700 rounded p-3 text-sm">
        <div class="text-emerald-300 font-medium mb-1">Key rotated</div>
        <div class="text-xs text-gray-400">Hash: <code class="text-gray-200">{{ rotateResult.key_hash?.slice(0, 16) }}...</code> &middot; Version: {{ rotateResult.version }}</div>
        <div v-if="rotateResult.distributed?.length" class="mt-2 space-y-1">
          <div v-for="d in rotateResult.distributed" :key="d.bridge_id" class="text-xs">
            <span class="font-mono text-gray-300">{{ d.bridge_id }}</span>
            <span :class="d.status === 'ok' ? 'text-emerald-400' : 'text-red-400'" class="ml-2">{{ d.status }}</span>
            <span v-if="d.error" class="text-red-400 ml-1">{{ d.error }}</span>
          </div>
        </div>
      </div>
    </div>

    <!-- Key list -->
    <div class="bg-tactical-surface border border-tactical-border rounded-lg">
      <div class="px-4 py-3 border-b border-tactical-border">
        <h2 class="text-sm font-medium text-gray-300">Keys for {{ selectedImei || '...' }}</h2>
      </div>
      <div v-if="loading" class="px-4 py-8 text-center text-gray-500 text-sm">Loading...</div>
      <div v-else-if="!keys.length" class="px-4 py-8 text-center text-gray-500 text-sm">No encryption keys for this device.</div>
      <table v-else class="w-full text-sm">
        <thead>
          <tr class="text-left text-xs text-gray-500 border-b border-tactical-border">
            <th class="px-4 py-2">Hash</th>
            <th class="px-4 py-2">Mode</th>
            <th class="px-4 py-2">Created</th>
            <th class="px-4 py-2 text-right">Actions</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(k, i) in keys" :key="k.id" class="border-b border-tactical-border/50 hover:bg-white/[0.02]">
            <td class="px-4 py-2 font-mono text-xs">
              {{ k.key_hash?.slice(0, 16) }}...
              <span v-if="i === 0" class="ml-1 text-[10px] px-1.5 py-0.5 rounded bg-teal-900/50 text-teal-300">active</span>
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
    <!-- Channel Key Rotation -->
    <div class="bg-tactical-surface border border-tactical-border rounded-lg p-4 mt-6">
      <div class="flex items-center justify-between mb-3">
        <h2 class="text-sm font-medium text-gray-300">Channel Key Rotation</h2>
        <button @click="showChannelRotate = !showChannelRotate"
          class="text-xs px-3 py-1.5 rounded bg-gray-700 text-gray-300 hover:bg-gray-600">
          {{ showChannelRotate ? 'Cancel' : 'Rotate Channel Key' }}
        </button>
      </div>
      <p class="text-xs text-gray-500 mb-3">Rotate a shared channel key and push it to all online bridges.</p>
      <div v-if="showChannelRotate" class="space-y-3">
        <div class="grid grid-cols-1 sm:grid-cols-3 gap-3">
          <div>
            <label class="block text-xs text-gray-500 mb-1">Channel Type</label>
            <select v-model="chChannelType" class="w-full bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
              <option value="iridium">Iridium</option>
              <option value="sms">SMS</option>
              <option value="mesh">Mesh</option>
            </select>
          </div>
          <div>
            <label class="block text-xs text-gray-500 mb-1">Address</label>
            <input v-model="chAddress" placeholder="e.g. +31612345678 or !abcd1234" class="w-full bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
          </div>
          <div>
            <label class="block text-xs text-gray-500 mb-1">Bridge IDs (blank = all)</label>
            <input v-model="chBridgeIds" placeholder="bridge-1, bridge-2" class="w-full bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
          </div>
        </div>
        <button @click="rotateChannelKey"
          class="px-4 py-2 rounded text-sm font-medium bg-teal-600 text-white hover:bg-teal-500">
          Rotate Channel Key
        </button>
        <div v-if="chResult" class="bg-emerald-900/50 border border-emerald-700 rounded p-3 text-sm">
          <div class="text-emerald-300 font-medium mb-1">Channel key rotated — copy now, shown once</div>
          <code class="block bg-gray-900 px-3 py-2 rounded font-mono text-emerald-200 break-all select-all text-xs mb-2">{{ chResult.key_hex }}</code>
          <div class="text-xs text-gray-400">
            Type: {{ chResult.channel_type }} &middot; Address: {{ chResult.address }} &middot;
            Version: {{ chResult.version }} &middot; Distributed: {{ chResult.distributed || 0 }}
            <span v-if="chResult.failed_bridges?.length" class="text-red-400"> &middot; Failed: {{ chResult.failed_bridges.join(', ') }}</span>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>
