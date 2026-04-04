<script setup>
import { ref, onMounted } from 'vue'
import { alertRules, escalation } from '../api/client'
import EmptyState from '../components/EmptyState.vue'

const rules = ref([])
const chains = ref([])
const loading = ref(true)
const error = ref('')
const showForm = ref(false)

const form = ref({
  name: '',
  condition_type: 'device_not_seen',
  threshold_hours: 6,
  chain_id: '',
  device_filter: '*',
  enabled: true,
})

const editingId = ref(null)

onMounted(async () => {
  await loadData()
})

async function loadData() {
  loading.value = true
  try {
    const [r, c] = await Promise.all([
      alertRules.list().catch(() => []),
      escalation.listChains().catch(() => []),
    ])
    rules.value = Array.isArray(r) ? r : []
    chains.value = Array.isArray(c) ? c : []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function resetForm() {
  form.value = { name: '', condition_type: 'device_not_seen', threshold_hours: 6, chain_id: '', device_filter: '*', enabled: true }
  editingId.value = null
}

function startEdit(rule) {
  let hours = 6
  try { hours = JSON.parse(rule.condition_params || '{}').threshold_hours || 6 } catch { /* ignore */ }
  form.value = {
    name: rule.name,
    condition_type: rule.condition_type,
    threshold_hours: hours,
    chain_id: rule.chain_id,
    device_filter: rule.device_filter || '*',
    enabled: rule.enabled,
  }
  editingId.value = rule.id
  showForm.value = true
}

async function saveRule() {
  if (!form.value.name.trim() || !form.value.chain_id) return
  error.value = ''
  const payload = {
    name: form.value.name.trim(),
    condition_type: form.value.condition_type,
    condition_params: JSON.stringify({ threshold_hours: Number(form.value.threshold_hours) || 6 }),
    chain_id: form.value.chain_id,
    device_filter: form.value.device_filter || '*',
    enabled: form.value.enabled,
  }
  try {
    if (editingId.value) {
      await alertRules.update(editingId.value, payload)
    } else {
      await alertRules.create(payload)
    }
    resetForm()
    showForm.value = false
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

async function toggleEnabled(rule) {
  error.value = ''
  try {
    await alertRules.update(rule.id, { ...rule, enabled: !rule.enabled })
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

async function deleteRule(id) {
  if (!confirm('Delete this alert rule?')) return
  error.value = ''
  try {
    await alertRules.delete(id)
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

function chainName(id) {
  const c = chains.value.find(c => c.id === id)
  return c?.name || id?.slice(0, 12) || '—'
}

function parseHours(params) {
  try { return JSON.parse(params || '{}').threshold_hours || '—' } catch { return '—' }
}
</script>

<template>
  <div class="p-4 lg:p-6 max-w-5xl mx-auto">
    <div class="flex items-center justify-between mb-6">
      <h1 class="text-2xl font-display font-bold">Alert Rules</h1>
      <button @click="showForm ? (showForm = false, resetForm()) : (showForm = true)"
        class="text-sm px-4 py-2 rounded font-medium"
        :class="showForm ? 'text-gray-400 hover:text-gray-300' : 'bg-teal-600 hover:bg-teal-500 text-white'">
        {{ showForm ? 'Cancel' : '+ New Rule' }}
      </button>
    </div>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4 text-sm">{{ error }}</div>

    <!-- Create / Edit form -->
    <div v-if="showForm" class="bg-tactical-surface rounded-lg border border-tactical-border p-4 mb-6">
      <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">
        {{ editingId ? 'Edit Rule' : 'New Alert Rule' }}
      </h2>
      <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 mb-3">
        <input v-model="form.name" placeholder="Rule name" class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
        <select v-model="form.condition_type" class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
          <option value="device_not_seen">Device Not Seen</option>
          <option value="battery_low">Battery Low</option>
          <option value="geofence_breach">Geofence Breach</option>
          <option value="message_rate_drop">Message Rate Drop</option>
        </select>
        <div>
          <label class="block text-xs text-gray-500 mb-1">Threshold (hours)</label>
          <input v-model.number="form.threshold_hours" type="number" min="1" class="w-full bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
        </div>
        <div>
          <label class="block text-xs text-gray-500 mb-1">Escalation Chain</label>
          <select v-model="form.chain_id" class="w-full bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
            <option value="" disabled>Select chain...</option>
            <option v-for="c in chains" :key="c.id" :value="c.id">{{ c.name }}</option>
          </select>
        </div>
        <input v-model="form.device_filter" placeholder="Device filter (* = all)" class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
        <label class="flex items-center gap-2 text-sm text-gray-300">
          <input type="checkbox" v-model="form.enabled" class="rounded">
          Enabled
        </label>
      </div>
      <div class="flex gap-2">
        <button @click="saveRule" :disabled="!form.name.trim() || !form.chain_id"
          class="bg-teal-600 hover:bg-teal-500 disabled:bg-gray-600 text-white text-sm px-4 py-2 rounded">
          {{ editingId ? 'Update' : 'Create' }}
        </button>
        <button @click="showForm = false; resetForm()" class="text-gray-400 hover:text-gray-300 text-sm px-3 py-2">Cancel</button>
      </div>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>

    <template v-else>
      <div class="bg-tactical-surface rounded-lg border border-tactical-border overflow-hidden">
        <div class="px-4 py-3 border-b border-tactical-border">
          <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider">Rules ({{ rules.length }})</h2>
        </div>
        <EmptyState v-if="rules.length === 0" icon="bell" title="No alert rules" message="Create alert rules to get notified when conditions are met." />
        <table v-else class="w-full text-sm">
          <thead>
            <tr class="text-left text-xs text-gray-500 border-b border-tactical-border">
              <th class="px-4 py-2">Name</th>
              <th class="px-4 py-2">Condition</th>
              <th class="px-4 py-2">Chain</th>
              <th class="px-4 py-2">Filter</th>
              <th class="px-4 py-2">Status</th>
              <th class="px-4 py-2 text-right">Actions</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in rules" :key="r.id" class="border-b border-tactical-border/50 hover:bg-white/[0.02]">
              <td class="px-4 py-2 text-gray-200">{{ r.name }}</td>
              <td class="px-4 py-2">
                <span class="text-xs px-1.5 py-0.5 rounded bg-gray-700 text-gray-300">{{ r.condition_type }}</span>
                <span class="text-xs text-gray-500 ml-1">{{ parseHours(r.condition_params) }}h</span>
              </td>
              <td class="px-4 py-2 text-gray-400 text-xs">{{ chainName(r.chain_id) }}</td>
              <td class="px-4 py-2 font-mono text-xs text-gray-500">{{ r.device_filter }}</td>
              <td class="px-4 py-2">
                <button @click="toggleEnabled(r)"
                  class="text-xs px-2 py-0.5 rounded"
                  :class="r.enabled ? 'bg-emerald-900/50 text-emerald-300' : 'bg-gray-700 text-gray-500'">
                  {{ r.enabled ? 'Enabled' : 'Disabled' }}
                </button>
              </td>
              <td class="px-4 py-2 text-right space-x-2">
                <button @click="startEdit(r)" class="text-xs text-teal-400 hover:text-teal-300">Edit</button>
                <button @click="deleteRule(r.id)" class="text-xs text-red-400 hover:text-red-300">Delete</button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
  </div>
</template>
