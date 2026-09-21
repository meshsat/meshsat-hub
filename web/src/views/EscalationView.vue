<script setup>
import { ref, computed, onMounted } from 'vue'
import { escalation } from '../api/client'
import { formatUTC } from '../utils/time'
import { ago } from '../utils/paths'
import { useOpsStore } from '../stores/ops'
import { useAuthStore } from '../stores/auth'
import { useToastStore } from '../stores/toast'
import Icon from '../components/Icon.vue'

// Alerts follow ISA-18.2: an alert is unacknowledged (the chain is still
// calling people) until someone acknowledges it; after that it is history.
// The API's field is `state` (triggered, escalating, acknowledged, exhausted);
// this page used to read `a.status`, which does not exist, so it never
// offered Acknowledge and never said what state an alert was in.
const ops = useOpsStore()
const auth = useAuthStore()
const toast = useToastStore()

const chains = ref([])
const alerts = ref([])
const showAll = ref(false)
const error = ref('')
const loading = ref(true)
const busy = ref('')

function blankTier(waitSec = 0) {
  // Tier fields mirror store.EscalationTier exactly: name, targets, wait_sec,
  // max_retries (readJSON refuses anything else, MESHSAT-1115).
  return { name: '', targets: '', wait_sec: waitSec, max_retries: 1 }
}
const newChain = ref({ name: '', tiers: [blankTier(0)] })
const showForm = ref(false)
const canAct = computed(() => auth.role !== 'viewer')

onMounted(loadData)

async function loadData() {
  loading.value = true
  try {
    const [c, a] = await Promise.all([
      escalation.listChains().catch(() => []),
      escalation.listAlerts(!showAll.value, 100).catch(() => []),
    ])
    chains.value = Array.isArray(c) ? c : []
    alerts.value = Array.isArray(a) ? a : []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

const OPEN = new Set(['triggered', 'escalating'])
const names = computed(() => Object.fromEntries(ops.devices.map((d) => [d.imei, d.label || ''])))
const chainName = computed(() => Object.fromEntries(chains.value.map((c) => [c.id, c])))
const sorted = computed(() => [...alerts.value].sort((a, b) =>
  Number(OPEN.has(b.state)) - Number(OPEN.has(a.state)) ||
  Number(b.type === 'sos') - Number(a.type === 'sos') ||
  new Date(b.created_at) - new Date(a.created_at)))
const openCount = computed(() => alerts.value.filter((a) => OPEN.has(a.state)).length)

const TYPE_WORDS = { sos: 'SOS', deadman: 'Missed check-in', geofence: 'Geofence', custom: 'Alert' }
const STATE_WORDS = { triggered: 'Calling the first step', escalating: 'Escalating', acknowledged: 'Acknowledged', exhausted: 'Chain finished, nobody acknowledged' }

function progress(a) {
  const c = chainName.value[a.chain_id]
  const tiers = c?.tiers?.length || 0
  const parts = []
  if (tiers) parts.push(`Step ${Math.min(a.current_tier + 1, tiers)} of ${tiers}${c.name ? ` in ${c.name}` : ''}`)
  if (OPEN.has(a.state) && a.next_esc_at && !String(a.next_esc_at).startsWith('0001-')) {
    const s = Math.round((new Date(a.next_esc_at).getTime() - Date.now()) / 1000)
    if (s > 0) parts.push(`next step in ${s < 90 ? `${s} s` : `${Math.round(s / 60)} min`}`)
  }
  return parts.join(', ')
}

async function ack(a) {
  busy.value = a.id
  try {
    await escalation.ackAlert(a.id, {})
    toast.success('Acknowledged. The chain stops calling.')
    await Promise.all([loadData(), ops.load()])
  } catch (e) {
    toast.error(`Acknowledge failed: ${e.message}`)
  } finally {
    busy.value = ''
  }
}

function addTier() { newChain.value.tiers.push(blankTier(300)) }
function removeTier(i) { newChain.value.tiers.splice(i, 1) }

async function createChain() {
  if (!newChain.value.name.trim()) { error.value = 'Give the chain a name.'; return }
  error.value = ''
  try {
    await escalation.createChain({
      name: newChain.value.name.trim(),
      tiers: newChain.value.tiers.map((t, i) => ({
        name: t.name.trim() || `tier-${i + 1}`,
        targets: t.targets.split(',').map((r) => r.trim()).filter(Boolean),
        wait_sec: parseInt(t.wait_sec) || 0,
        max_retries: parseInt(t.max_retries) || 0,
      })),
    })
    newChain.value = { name: '', tiers: [blankTier(0)] }
    showForm.value = false
    toast.success('Chain created.')
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

async function deleteChain(c) {
  if (!confirm(`Delete the chain "${c.name}"? Alerts that use it stop escalating.`)) return
  try {
    await escalation.deleteChain(c.id)
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

function waitWords(sec) {
  const s = parseInt(sec) || 0
  if (s < 60) return `${s} s`
  if (s % 60 === 0) return `${s / 60} min`
  return `${Math.floor(s / 60)} min ${s % 60} s`
}
</script>

<template>
  <div class="ms-page max-w-6xl">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Alerts</h1>
        <p class="ms-lede">An alert works down its escalation chain, texting each step in turn, until someone acknowledges it.</p>
      </div>
    </div>

    <div v-if="error" role="alert" class="mb-4 flex items-start justify-between gap-4 rounded-lg border border-ms-error/50 bg-ms-error/10 px-4 py-3 text-[13px]">
      <span>{{ error }}</span><button class="text-xs text-ms-muted hover:text-ms-text" @click="error = ''">Dismiss</button>
    </div>

    <!-- Alerts -->
    <section class="mb-8" aria-labelledby="alerts-h">
      <div class="flex flex-wrap items-center justify-between gap-3 mb-3">
        <h2 id="alerts-h" class="ms-h2">{{ showAll ? 'All alerts' : openCount ? `Unacknowledged (${openCount})` : 'Unacknowledged' }}</h2>
        <label class="flex items-center gap-2 text-[13px] text-ms-muted cursor-pointer">
          <input v-model="showAll" type="checkbox" class="accent-brand-primary" @change="loadData" />
          Include acknowledged and finished
        </label>
      </div>

      <div v-if="loading && !alerts.length" class="ms-panel px-4 py-8 text-[13px] text-ms-muted">Loading alerts.</div>
      <div v-else-if="!sorted.length" class="ms-panel px-5 py-8 flex items-start gap-3">
        <span class="mt-1 w-2 h-2 rounded-full border border-ms-muted shrink-0" aria-hidden="true" />
        <div>
          <p class="text-[13px] text-ms-text">{{ showAll ? 'No alerts on record.' : 'Nothing is waiting for an acknowledgement.' }}</p>
          <p class="text-xs text-ms-muted mt-1">An SOS, a missed check-in or a geofence crossing raises an alert here and starts its chain.</p>
        </div>
      </div>
      <ul v-else class="ms-panel divide-y divide-ms-border overflow-hidden">
        <li v-for="a in sorted" :key="a.id" class="px-5 py-4 flex flex-wrap gap-x-5 gap-y-3"
          :class="OPEN.has(a.state) && a.type === 'sos' ? 'bg-ms-error/5' : ''">
          <span class="mt-0.5" :class="OPEN.has(a.state) ? 'text-ms-error' : 'text-ms-muted'">
            <Icon :name="OPEN.has(a.state) ? 'alerts' : 'check'" :size="18" />
          </span>
          <div class="min-w-0 flex-1 basis-64">
            <div class="flex flex-wrap items-baseline gap-x-2.5">
              <span class="text-sm font-semibold" :class="OPEN.has(a.state) ? 'text-ms-error' : 'text-ms-text'">{{ TYPE_WORDS[a.type] || a.type || 'Alert' }}</span>
              <span v-if="names[a.device_imei]" class="text-[13px] text-ms-text">{{ names[a.device_imei] }}</span>
              <span v-if="a.device_imei" class="ms-id text-ms-muted">{{ a.device_imei }}</span>
            </div>
            <p v-if="a.detail" class="text-[13px] text-ms-text2 mt-1">{{ a.detail }}</p>
            <p class="text-xs text-ms-muted mt-1.5">
              Raised {{ ago(a.created_at) }} <span class="hidden sm:inline">({{ formatUTC(a.created_at) }})</span>.
              {{ STATE_WORDS[a.state] || a.state }}<template v-if="progress(a)">, {{ progress(a).charAt(0).toLowerCase() + progress(a).slice(1) }}</template>.
              <template v-if="a.acked_by"> Acknowledged by {{ a.acked_by }} {{ ago(a.acked_at) }}.</template>
            </p>
          </div>
          <div class="flex items-start gap-2">
            <button v-if="OPEN.has(a.state) && canAct" class="ms-btn-primary" :disabled="busy === a.id" @click="ack(a)">{{ busy === a.id ? 'Acknowledging' : 'Acknowledge' }}</button>
            <router-link v-if="a.device_imei" :to="{ name: 'map', query: { focus: a.device_imei } }" class="ms-btn"><Icon name="pin" :size="14" />Map</router-link>
          </div>
        </li>
      </ul>
    </section>

    <!-- Chains -->
    <section aria-labelledby="chains-h">
      <div class="flex flex-wrap items-center justify-between gap-3 mb-3">
        <div>
          <h2 id="chains-h" class="ms-h2">Escalation chains</h2>
          <p class="text-xs text-ms-muted mt-0.5">Who is texted, in what order, and how long each step waits.</p>
        </div>
        <button v-if="canAct && !showForm" class="ms-btn-primary" @click="showForm = true"><Icon name="plus" :size="15" />New chain</button>
      </div>

      <div v-if="showForm" class="ms-panel p-5 mb-4">
        <label class="block max-w-md">
          <span class="ms-label">Chain name</span>
          <input v-model="newChain.name" placeholder="Chain name" class="ms-input w-full mt-1" />
        </label>
        <ol class="mt-4 space-y-3">
          <li v-for="(tier, i) in newChain.tiers" :key="i" class="rounded-lg border border-ms-border p-3">
            <div class="flex items-center justify-between mb-2">
              <span class="text-[13px] font-medium">Step {{ i + 1 }}<span class="text-ms-muted font-normal">{{ i === 0 ? ', as soon as the alert is raised' : ', after the wait below' }}</span></span>
              <button v-if="newChain.tiers.length > 1" class="text-xs text-ms-muted hover:text-ms-error" @click="removeTier(i)">Remove step</button>
            </div>
            <div class="grid gap-2 sm:grid-cols-[1fr_2fr_7rem_6rem]">
              <label class="block"><span class="ms-label">Name</span><input v-model="tier.name" :placeholder="`tier-${i + 1}`" class="ms-input w-full mt-1" /></label>
              <label class="block"><span class="ms-label">Phone numbers, comma separated</span><input v-model="tier.targets" placeholder="+31612345678" class="ms-input w-full mt-1 font-mono" /></label>
              <label class="block"><span class="ms-label">Wait (s)</span><input v-model="tier.wait_sec" type="number" min="0" :disabled="i === 0" class="ms-input w-full mt-1"
                :title="i === 0 ? 'The first step fires at once; its wait is not used.' : 'How long after the previous step this one fires.'" /></label>
              <label class="block"><span class="ms-label">Tries</span><input v-model="tier.max_retries" type="number" min="0" class="ms-input w-full mt-1"
                title="How many times this step is tried before moving on. 0 means text once and move on." /></label>
            </div>
          </li>
        </ol>
        <p class="text-xs text-ms-muted mt-3 max-w-[70ch]">Steps are texted by SMS, so targets are phone numbers in international form. Each later step waits its own time before it fires; acknowledging the alert stops the chain wherever it is.</p>
        <div class="flex flex-wrap gap-2 mt-4">
          <button class="ms-btn" @click="addTier"><Icon name="plus" :size="14" />Add a step</button>
          <span class="flex-1" />
          <button class="ms-btn" @click="showForm = false">Cancel</button>
          <button class="ms-btn-primary" @click="createChain">Create chain</button>
        </div>
      </div>

      <div v-if="!loading && !chains.length && !showForm" class="ms-panel px-5 py-8 text-[13px] text-ms-muted">
        No chains yet. Without one, an SOS is recorded but nobody is texted. <button v-if="canAct" class="text-ms-primary hover:underline" @click="showForm = true">Create the first chain</button>.
      </div>

      <div class="grid gap-4 lg:grid-cols-2">
        <article v-for="c in chains" :key="c.id" class="ms-panel p-5">
          <div class="flex items-start justify-between gap-3">
            <h3 class="text-sm font-semibold">{{ c.name }}</h3>
            <button v-if="canAct" class="text-xs text-ms-muted hover:text-ms-error" @click="deleteChain(c)">Delete</button>
          </div>
          <ol class="mt-4 relative">
            <li v-for="(tier, i) in c.tiers || []" :key="i" class="relative pl-8 pb-4 last:pb-0">
              <span v-if="i < (c.tiers || []).length - 1" class="absolute left-[11px] top-6 bottom-0 w-px bg-ms-border" aria-hidden="true" />
              <span class="absolute left-0 top-0 w-6 h-6 rounded-full border border-ms-border-light text-xs flex items-center justify-center ms-num text-ms-text2">{{ i + 1 }}</span>
              <div class="text-[13px] text-ms-text">{{ i === 0 ? 'At once' : `After ${waitWords(tier.wait_sec)}` }}<span class="text-ms-muted">, {{ tier.max_retries > 0 ? `up to ${tier.max_retries} ${tier.max_retries === 1 ? 'try' : 'tries'}` : 'once' }}</span></div>
              <div class="ms-id text-ms-muted mt-0.5 break-all">{{ (tier.targets || []).join(', ') || 'No one yet' }}</div>
            </li>
          </ol>
        </article>
      </div>
    </section>
  </div>
</template>
