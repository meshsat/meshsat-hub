<script setup>
import { ref, onMounted } from 'vue'
import { escalation } from '../api/client'
import { formatUTC } from '../utils/time'
import EmptyState from '../components/EmptyState.vue'

const chains = ref([])
const alerts = ref([])
const showActive = ref(true)
const error = ref('')
const loading = ref(true)

// New chain form
const newChain = ref({ name: '', tiers: [{ delay_sec: 0, recipients: '', actions: 'notify' }] })
const showForm = ref(false)

onMounted(async () => {
  await loadData()
})

async function loadData() {
  loading.value = true
  try {
    const [c, a] = await Promise.all([
      escalation.listChains().catch(() => []),
      escalation.listAlerts(showActive.value).catch(() => []),
    ])
    chains.value = Array.isArray(c) ? c : []
    alerts.value = Array.isArray(a) ? a : []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function addTier() {
  newChain.value.tiers.push({ delay_sec: 300, recipients: '', actions: 'notify' })
}

function removeTier(i) {
  newChain.value.tiers.splice(i, 1)
}

async function createChain() {
  if (!newChain.value.name.trim()) return
  error.value = ''
  try {
    const payload = {
      name: newChain.value.name.trim(),
      tiers: newChain.value.tiers.map(t => ({
        delay_sec: parseInt(t.delay_sec) || 0,
        recipients: t.recipients.split(',').map(r => r.trim()).filter(Boolean),
        actions: t.actions.split(',').map(a => a.trim()).filter(Boolean),
      }))
    }
    await escalation.createChain(payload)
    newChain.value = { name: '', tiers: [{ delay_sec: 0, recipients: '', actions: 'notify' }] }
    showForm.value = false
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

async function deleteChain(id) {
  if (!confirm('Delete this escalation chain?')) return
  try {
    await escalation.deleteChain(id)
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

async function ackAlert(id) {
  try {
    await escalation.ackAlert(id)
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

function statusColor(status) {
  if (status === 'active' || status === 'firing') return 'text-red-400'
  if (status === 'acknowledged') return 'text-yellow-400'
  if (status === 'resolved') return 'text-green-400'
  return 'text-gray-400'
}

function statusBg(status) {
  if (status === 'active' || status === 'firing') return 'bg-red-900/50 border-red-700'
  if (status === 'acknowledged') return 'bg-yellow-900/50 border-yellow-700'
  return 'bg-gray-800 border-gray-700'
}
</script>

<template>
  <div>
    <h1 class="text-2xl font-display font-bold mb-4">Escalation & Alerts</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">{{ error }}</div>

    <!-- Active Alerts -->
    <div class="mb-8">
      <div class="flex items-center justify-between mb-3">
        <h2 class="text-lg font-semibold uppercase tracking-wider">Alerts</h2>
        <label class="flex items-center gap-2 text-sm text-gray-400">
          <input type="checkbox" v-model="showActive" @change="loadData()" class="rounded" />
          Active only
        </label>
      </div>

      <EmptyState v-if="alerts.length === 0 && !loading" icon="shield" title="No alerts" message="Alerts will appear here when devices trigger SOS or escalation conditions." />

      <div v-for="a in alerts" :key="a.id" class="border rounded-lg p-4 mb-3" :class="statusBg(a.status)">
        <div class="flex items-center justify-between mb-2">
          <div class="flex items-center gap-3">
            <span :class="statusColor(a.status)" class="font-medium text-xs uppercase">{{ a.status }}</span>
            <span class="text-sm">{{ a.type || 'alert' }}</span>
            <span v-if="a.device_imei" class="font-mono text-xs text-gray-400">{{ a.device_imei }}</span>
          </div>
          <button v-if="a.status === 'active' || a.status === 'firing'" @click="ackAlert(a.id)"
            class="bg-yellow-700 hover:bg-yellow-600 text-white px-3 py-1 rounded text-xs transition-colors">
            Acknowledge
          </button>
        </div>
        <div v-if="a.detail" class="text-sm text-gray-300 mb-1">{{ a.detail }}</div>
        <div class="text-xs text-gray-500">
          {{ formatUTC(a.created_at) }}
          <span v-if="a.acked_by"> &middot; Acked by {{ a.acked_by }}</span>
        </div>
      </div>
    </div>

    <!-- Escalation Chains -->
    <div>
      <div class="flex items-center justify-between mb-3">
        <h2 class="text-lg font-semibold uppercase tracking-wider">Escalation Chains</h2>
        <button @click="showForm = !showForm"
          class="bg-teal-600 hover:bg-teal-500 text-white px-3 py-1 rounded text-sm transition-colors">
          {{ showForm ? 'Cancel' : '+ New Chain' }}
        </button>
      </div>

      <!-- New chain form -->
      <div v-if="showForm" class="bg-gray-900 rounded-xl p-4 mb-4">
        <input v-model="newChain.name" placeholder="Chain name"
          class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 placeholder-gray-500 focus:outline-none focus:border-teal-500 w-full mb-3" />

        <div v-for="(tier, i) in newChain.tiers" :key="i" class="flex flex-wrap gap-2 mb-2 items-end">
          <div class="flex-1 min-w-[120px]">
            <label class="text-xs text-gray-400">Delay (sec)</label>
            <input v-model="tier.delay_sec" type="number" min="0"
              class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full focus:outline-none focus:border-teal-500" />
          </div>
          <div class="flex-1 min-w-[200px]">
            <label class="text-xs text-gray-400">Recipients (comma-sep)</label>
            <input v-model="tier.recipients" placeholder="email@example.com"
              class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full placeholder-gray-500 focus:outline-none focus:border-teal-500" />
          </div>
          <div class="flex-1 min-w-[120px]">
            <label class="text-xs text-gray-400">Actions</label>
            <input v-model="tier.actions" placeholder="notify"
              class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full placeholder-gray-500 focus:outline-none focus:border-teal-500" />
          </div>
          <button v-if="newChain.tiers.length > 1" @click="removeTier(i)"
            class="bg-red-900 hover:bg-red-800 text-red-200 px-2 py-2 rounded text-xs">Remove</button>
        </div>

        <div class="flex gap-2 mt-3">
          <button @click="addTier" class="text-teal-400 hover:text-teal-300 text-sm">+ Add Tier</button>
          <button @click="createChain"
            class="bg-teal-600 hover:bg-teal-500 text-white px-4 py-2 rounded text-sm ml-auto transition-colors">
            Create Chain
          </button>
        </div>
      </div>

      <!-- Chain list -->
      <div v-for="c in chains" :key="c.id" class="bg-gray-900 rounded-xl p-4 mb-3">
        <div class="flex items-center justify-between mb-2">
          <span class="font-medium">{{ c.name }}</span>
          <button @click="deleteChain(c.id)"
            class="bg-red-900 hover:bg-red-800 text-red-200 px-2 py-1 rounded-lg text-xs transition-colors">Delete</button>
        </div>
        <div v-if="c.tiers && c.tiers.length" class="space-y-1">
          <div v-for="(tier, i) in c.tiers" :key="i" class="text-sm text-gray-400 flex items-center gap-2">
            <span class="bg-gray-700 px-2 py-0.5 rounded text-xs font-mono">T{{ i + 1 }}</span>
            <span>{{ tier.delay_sec }}s delay</span>
            <span class="text-gray-500">&rarr;</span>
            <span>{{ (tier.recipients || []).join(', ') || 'no recipients' }}</span>
          </div>
        </div>
      </div>

      <EmptyState v-if="chains.length === 0 && !loading" icon="chart" title="No escalation chains" message="Create an escalation chain to define notification steps for alerts." />
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>
  </div>
</template>
