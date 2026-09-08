<script setup>
// Per-tenant provider accounts (MESHSAT-977): Cloudloop, Twilio, Rock7,
// RockBLOCK, Globalstar. Secrets are write-only; leaving a secret empty keeps
// the stored value. Generated tokens are shown once after saving.
import { ref, reactive, onMounted } from 'vue'
import { tenant } from '../api/client'
import { useAuthStore } from '../stores/auth'
import { useToastStore } from '../stores/toast'

const auth = useAuthStore()
const toast = useToastStore()
const loading = ref(true)
const error = ref('')
const accounts = ref([])
const open = ref(null)
const drafts = reactive({})
const busy = ref('')
const reveal = ref({})
const testResult = ref({})

async function load() {
  try {
    accounts.value = await tenant.integrations()
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}
onMounted(load)

function edit(a) {
  open.value = open.value === a.provider ? null : a.provider
  reveal.value = {}
  testResult.value = {}
  const d = {}
  for (const f of a.fields) d[f.key] = f.secret ? '' : (a.values[f.key] || f.default || '')
  drafts[a.provider] = d
}

async function save(a) {
  busy.value = a.provider
  try {
    const res = await tenant.setIntegration(a.provider, drafts[a.provider])
    reveal.value = res.reveal || {}
    toast.success(`${a.label} saved`)
    await load()
    const fresh = accounts.value.find(x => x.provider === a.provider)
    if (fresh) { const d = {}; for (const f of fresh.fields) d[f.key] = f.secret ? '' : (fresh.values[f.key] || ''); drafts[a.provider] = d }
  } catch (e) {
    toast.error(e.message)
  } finally {
    busy.value = ''
  }
}

async function remove(a) {
  if (!confirm(`Remove the ${a.label} account for this tenant?`)) return
  busy.value = a.provider
  try {
    await tenant.deleteIntegration(a.provider)
    toast.success(`${a.label} removed`)
    open.value = null
    await load()
  } catch (e) {
    toast.error(e.message)
  } finally {
    busy.value = ''
  }
}

async function test(a) {
  busy.value = a.provider
  try {
    testResult.value = { ...testResult.value, [a.provider]: await tenant.testIntegration(a.provider) }
  } catch (e) {
    testResult.value = { ...testResult.value, [a.provider]: { ok: false, detail: e.message } }
  } finally {
    busy.value = ''
  }
}

function webhookURL(a) {
  return a.webhook_path ? window.location.origin + a.webhook_path : ''
}

async function copy(text) {
  try { await navigator.clipboard.writeText(text); toast.success('Copied') } catch { toast.error('Copy failed') }
}
</script>

<template>
  <section class="mb-8">
    <div class="mb-3">
      <h2 class="text-lg font-display font-semibold">Provider accounts</h2>
      <p class="text-gray-400 text-sm mt-1">
        Your tenant's own Cloudloop, Twilio, Rock7, RockBLOCK and Globalstar credentials. Devices in this tenant send and
        receive through these accounts; the inbound webhooks identify your tenant by the token or secret below.
      </p>
    </div>
    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">{{ error }}</div>
    <div v-if="loading" class="text-gray-500 text-sm py-6">Loading provider accounts...</div>
    <div v-else class="grid grid-cols-1 lg:grid-cols-2 gap-4">
      <div v-for="a in accounts" :key="a.provider" class="bg-tactical-surface rounded-lg border border-tactical-border p-4">
        <div class="flex items-start justify-between gap-3 mb-2">
          <div class="min-w-0">
            <div class="flex items-center gap-2 flex-wrap">
              <span class="font-semibold">{{ a.label }}</span>
              <span v-if="a.configured && a.platform" class="text-[10px] px-1.5 py-0.5 rounded border bg-sky-900/50 text-sky-300 border-sky-700/50">platform account</span>
              <span v-else-if="a.configured" class="text-[10px] px-1.5 py-0.5 rounded border bg-green-900/50 text-green-300 border-green-700/50">configured</span>
              <span v-else class="text-[10px] px-1.5 py-0.5 rounded border bg-gray-800 text-gray-400 border-gray-700">not configured</span>
            </div>
            <p class="text-xs text-gray-400 mt-1">{{ a.description }}</p>
          </div>
          <button v-if="auth.isOwner" @click="edit(a)" class="shrink-0 text-xs px-3 py-1.5 rounded bg-brand-primary text-ms-on-primary hover:bg-brand-accent">
            {{ open === a.provider ? 'Close' : (a.configured && !a.platform ? 'Edit' : 'Configure') }}
          </button>
        </div>

        <dl v-if="a.configured && open !== a.provider" class="text-xs grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 mt-2">
          <template v-for="f in a.fields" :key="f.key">
            <dt class="text-gray-500">{{ f.label }}</dt>
            <dd class="font-mono text-gray-300 truncate">{{ a.values[f.key] || '—' }}</dd>
          </template>
        </dl>

        <div v-if="webhookURL(a)" class="mt-3 text-xs">
          <span class="text-gray-500">Webhook URL for the provider console</span>
          <div class="flex items-center gap-2 mt-1">
            <code class="flex-1 min-w-0 truncate bg-gray-800 px-2 py-1 rounded text-gray-300">{{ webhookURL(a) }}</code>
            <button @click="copy(webhookURL(a))" class="text-gray-400 hover:text-brand-primary">Copy</button>
          </div>
        </div>

        <form v-if="open === a.provider && drafts[a.provider]" @submit.prevent="save(a)" class="mt-3 space-y-3">
          <div v-for="f in a.fields" :key="f.key">
            <label :for="`${a.provider}-${f.key}`" class="block text-xs text-gray-400 mb-1">
              {{ f.label }}<span v-if="f.required" class="text-ms-error"> *</span>
            </label>
            <input :id="`${a.provider}-${f.key}`" v-model="drafts[a.provider][f.key]" :type="f.secret ? 'password' : 'text'"
              :placeholder="f.secret && a.values[f.key] ? `stored (${a.values[f.key]}), leave empty to keep` : (f.generate ? 'generated when empty' : f.default || '')"
              autocomplete="off"
              class="w-full px-3 py-1.5 bg-ms-well border border-ms-border rounded text-sm text-ms-text placeholder-ms-muted focus:outline-none focus:border-brand-primary" />
            <p v-if="f.hint" class="text-[11px] text-gray-500 mt-1">{{ f.hint }}</p>
            <p v-if="reveal[f.key]" class="text-[11px] mt-1 text-ms-warning">
              New value, shown once: <code class="font-mono select-all">{{ reveal[f.key] }}</code>
            </p>
          </div>
          <div class="flex flex-wrap gap-2 items-center">
            <button type="submit" :disabled="busy === a.provider" class="px-3 py-1.5 rounded bg-brand-primary text-ms-on-primary text-sm hover:bg-brand-accent disabled:opacity-50">Save</button>
            <button type="button" v-if="a.configured && !a.platform" @click="test(a)" :disabled="busy === a.provider" class="px-3 py-1.5 rounded border border-tactical-border text-sm text-gray-300 hover:border-brand-primary disabled:opacity-50">Test</button>
            <button type="button" v-if="a.configured && !a.platform" @click="remove(a)" :disabled="busy === a.provider" class="px-3 py-1.5 rounded border border-tactical-border text-sm text-ms-error hover:border-ms-error disabled:opacity-50">Remove</button>
            <span v-if="testResult[a.provider]" :class="testResult[a.provider].ok ? 'text-ms-success' : 'text-ms-error'" class="text-xs">
              {{ testResult[a.provider].ok ? 'OK' : 'Failed' }}: {{ testResult[a.provider].detail }}
            </span>
          </div>
        </form>
      </div>
    </div>
  </section>
</template>
