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
// Tier fields mirror store.EscalationTier exactly: name, targets, wait_sec,
// max_retries. They used to be delay_sec/recipients/actions, which matched
// nothing in the API -- readJSON rejects unknown fields, so every attempt to
// create a chain from this page answered 400 and no tenant could configure an
// escalation at all (MESHSAT-1115).
function blankTier(waitSec = 0) {
  return { name: '', targets: '', wait_sec: waitSec, max_retries: 1 }
}
const newChain = ref({ name: '', tiers: [blankTier(0)] })
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
  newChain.value.tiers.push(blankTier(300))
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
      tiers: newChain.value.tiers.map((t, i) => ({
        name: t.name.trim() || `tier-${i + 1}`,
        targets: t.targets.split(',').map(r => r.trim()).filter(Boolean),
        wait_sec: parseInt(t.wait_sec) || 0,
        max_retries: parseInt(t.max_retries) || 0,
      }))
    }
    await escalation.createChain(payload)
    newChain.value = { name: '', tiers: [blankTier(0)] }
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
  if (status === 'active' || status === 'firing') return 'text-ms-error'
  if (status === 'acknowledged') return 'text-ms-warning'
  if (status === 'resolved') return 'text-ms-success'
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
          class="bg-brand-accent hover:bg-brand-primary text-ms-on-primary px-3 py-1 rounded text-sm transition-colors">
          {{ showForm ? 'Cancel' : '+ New Chain' }}
        </button>
      </div>

      <!-- New chain form -->
      <div v-if="showForm" class="bg-tactical-surface rounded-lg p-4 mb-4">
        <input v-model="newChain.name" placeholder="Chain name"
          class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 placeholder-gray-500 focus:outline-none focus:border-brand-primary w-full mb-3" />

        <div v-for="(tier, i) in newChain.tiers" :key="i" class="flex flex-wrap gap-2 mb-2 items-end">
          <div class="flex-1 min-w-[110px]">
            <label class="text-xs text-gray-400">Name</label>
            <input v-model="tier.name" :placeholder="`tier-${i + 1}`"
              class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full placeholder-gray-500 focus:outline-none focus:border-brand-primary" />
          </div>
          <div class="flex-1 min-w-[200px]">
            <label class="text-xs text-gray-400">Targets (comma-sep)</label>
            <input v-model="tier.targets" placeholder="+31612345678"
              class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full placeholder-gray-500 focus:outline-none focus:border-brand-primary" />
          </div>
          <div class="flex-1 min-w-[110px]">
            <label class="text-xs text-gray-400">Wait (sec)</label>
            <input v-model="tier.wait_sec" type="number" min="0" :disabled="i === 0"
              :title="i === 0 ? 'The first tier fires immediately; its wait is not used.' : 'How long to wait before this tier fires.'"
              class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full focus:outline-none focus:border-brand-primary" />
          </div>
          <div class="flex-1 min-w-[90px]">
            <label class="text-xs text-gray-400">Retries</label>
            <input v-model="tier.max_retries" type="number" min="0"
              title="0 means notify once and move straight to the next tier."
              class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 w-full focus:outline-none focus:border-brand-primary" />
          </div>
          <button v-if="newChain.tiers.length > 1" @click="removeTier(i)"
            class="bg-red-900 hover:bg-red-800 text-red-200 px-2 py-2 rounded text-xs">Remove</button>
        </div>

        <p class="text-xs text-gray-500 mt-1">
          Tiers run in order. The first fires as soon as the alert is raised; each later tier waits
          its own Wait before firing. Retries is how many times a tier is tried before moving on, so
          0 means notify once and escalate immediately. Delivery currently goes by SMS, so targets
          should be phone numbers in international form.
        </p>

        <div class="flex gap-2 mt-3">
          <button @click="addTier" class="text-brand-primary hover:text-brand-primary text-sm">+ Add Tier</button>
          <button @click="createChain"
            class="bg-brand-accent hover:bg-brand-primary text-ms-on-primary px-4 py-2 rounded text-sm ml-auto transition-colors">
            Create Chain
          </button>
        </div>
      </div>

      <!-- Chain list -->
      <div v-for="c in chains" :key="c.id" class="bg-tactical-surface rounded-lg p-4 mb-3">
        <div class="flex items-center justify-between mb-2">
          <span class="font-medium">{{ c.name }}</span>
          <button @click="deleteChain(c.id)"
            class="bg-red-900 hover:bg-red-800 text-red-200 px-2 py-1 rounded-lg text-xs transition-colors">Delete</button>
        </div>
        <div v-if="c.tiers && c.tiers.length" class="space-y-1">
          <div v-for="(tier, i) in c.tiers" :key="i" class="text-sm text-gray-400 flex items-center gap-2">
            <span class="bg-gray-700 px-2 py-0.5 rounded text-xs font-mono">T{{ i + 1 }}</span>
            <span v-if="i === 0">immediately</span>
            <span v-else>after {{ tier.wait_sec }}s</span>
            <span class="text-gray-500">&rarr;</span>
            <span>{{ (tier.targets || []).join(', ') || 'no targets' }}</span>
            <span class="text-gray-500">&middot;</span>
            <span>{{ tier.max_retries > 0 ? `${tier.max_retries} tries` : 'once' }}</span>
          </div>
        </div>
      </div>

      <EmptyState v-if="chains.length === 0 && !loading" icon="chart" title="No escalation chains" message="Create an escalation chain to define notification steps for alerts." />
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>
  </div>
</template>
