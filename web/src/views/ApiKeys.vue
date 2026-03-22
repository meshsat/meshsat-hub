<script setup>
import { ref, onMounted } from 'vue'
import { authApi } from '../api/client'
import { formatUTC } from '../utils/time'

const keys = ref([])
const newLabel = ref('')
const newRole = ref('viewer')
const newDevice = ref('')
const newExpires = ref('')
const createdKey = ref(null)
const error = ref('')
const loading = ref(false)

onMounted(async () => {
  await loadKeys()
})

async function loadKeys() {
  loading.value = true
  try {
    keys.value = await authApi.listKeys() || []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function createKey() {
  if (!newLabel.value.trim()) {
    error.value = 'Label is required'
    return
  }
  error.value = ''
  createdKey.value = null
  try {
    const data = { label: newLabel.value.trim(), role: newRole.value }
    if (newDevice.value.trim()) data.device_imei = newDevice.value.trim()
    if (newExpires.value) data.expires_in = newExpires.value
    const result = await authApi.createKey(data)
    createdKey.value = result
    newLabel.value = ''
    newDevice.value = ''
    newExpires.value = ''
    await loadKeys()
  } catch (e) {
    error.value = e.message
  }
}

async function revokeKey(id, prefix) {
  if (!confirm(`Revoke API key ${prefix}...?`)) return
  try {
    await authApi.deleteKey(id)
    await loadKeys()
  } catch (e) {
    error.value = e.message
  }
}

function formatDate(d) {
  return formatUTC(d)
}

function roleBadgeClass(role) {
  if (role === 'owner') return 'bg-purple-900/50 text-purple-300'
  if (role === 'operator') return 'bg-teal-900/50 text-teal-300'
  return 'bg-gray-700 text-gray-300'
}
</script>

<template>
  <div>
    <h1 class="text-2xl font-display font-bold mb-4">API Keys</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">
      {{ error }}
    </div>

    <!-- Created key banner (shown once) -->
    <div v-if="createdKey" class="bg-green-900/30 border border-green-700 rounded-lg p-4 mb-4">
      <div class="text-green-300 font-semibold mb-2">API Key Created</div>
      <div class="text-sm text-gray-300 mb-2">Copy this key now — it won't be shown again:</div>
      <code class="block bg-gray-900 text-green-400 px-3 py-2 rounded font-mono text-sm break-all select-all">
        {{ createdKey.key }}
      </code>
      <button @click="createdKey = null" class="mt-3 text-sm text-gray-400 hover:text-gray-200">Dismiss</button>
    </div>

    <!-- Create key form -->
    <div class="bg-tactical-surface rounded-lg p-4 mb-6">
      <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">Create New Key</h2>
      <div class="flex flex-wrap gap-2">
        <input v-model="newLabel" placeholder="Label (e.g. CI pipeline)"
          class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 placeholder-gray-500 focus:outline-none focus:border-teal-500 flex-1 min-w-[160px]" />
        <select v-model="newRole"
          class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 focus:outline-none focus:border-teal-500">
          <option value="viewer">Viewer</option>
          <option value="operator">Operator</option>
          <option value="owner">Owner</option>
        </select>
        <input v-model="newDevice" placeholder="Device IMEI (optional)"
          class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 placeholder-gray-500 focus:outline-none focus:border-teal-500 min-w-[160px]" />
        <select v-model="newExpires"
          class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 focus:outline-none focus:border-teal-500">
          <option value="">No expiry</option>
          <option value="720h">30 days</option>
          <option value="2160h">90 days</option>
          <option value="8760h">1 year</option>
        </select>
        <button @click="createKey"
          class="bg-teal-600 hover:bg-teal-500 text-white px-4 py-2 rounded-lg font-medium transition-colors">
          Create
        </button>
      </div>
    </div>

    <!-- Keys table -->
    <div class="overflow-x-auto">
      <table class="w-full border-collapse text-sm">
        <thead>
          <tr class="border-b border-tactical-border text-left text-gray-500">
            <th class="px-3 py-2">Key Prefix</th>
            <th class="px-3 py-2">Label</th>
            <th class="px-3 py-2">Role</th>
            <th class="px-3 py-2">Device</th>
            <th class="px-3 py-2">Last Used</th>
            <th class="px-3 py-2">Expires</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="k in keys" :key="k.id" class="border-b border-tactical-border/30 hover:bg-white/[0.02]">
            <td class="px-3 py-2 font-mono text-xs">{{ k.key_prefix }}...</td>
            <td class="px-3 py-2">{{ k.label }}</td>
            <td class="px-3 py-2">
              <span :class="roleBadgeClass(k.role)" class="text-xs px-2 py-0.5 rounded font-medium">
                {{ k.role }}
              </span>
            </td>
            <td class="px-3 py-2 font-mono text-xs text-gray-400">{{ k.device_imei || '—' }}</td>
            <td class="px-3 py-2 text-gray-400">{{ formatDate(k.last_used) }}</td>
            <td class="px-3 py-2 text-gray-400">{{ formatDate(k.expires_at) }}</td>
            <td class="px-3 py-2 text-right">
              <button @click="revokeKey(k.id, k.key_prefix)"
                class="bg-red-900 hover:bg-red-800 text-red-200 px-2 py-1 rounded-lg text-xs transition-colors">
                Revoke
              </button>
            </td>
          </tr>
          <tr v-if="keys.length === 0 && !loading">
            <td colspan="7" class="px-3 py-8 text-center text-gray-500">No API keys</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
