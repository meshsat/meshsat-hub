<script setup>
// Beta requests waiting for a decision (MESHSAT-978). Platform admins only.
// Approving here does what the approval script did: activates the account,
// grants the role, and asks the edge to admit the address they signed up
// from. The panel hides itself when the Hub has no identity provider token,
// so it can ship ahead of that being configured.
import { ref, onMounted } from 'vue'
import { signups as signupsApi } from '../api/client'
import { useAuthStore } from '../stores/auth'
import { useToastStore } from '../stores/toast'

const auth = useAuthStore()
const toast = useToastStore()

const available = ref(false)
const loading = ref(true)
const pending = ref([])
const roles = ref({})
const busy = ref('')

async function load() {
  loading.value = true
  try {
    const res = await signupsApi.list()
    pending.value = res?.signups || []
    for (const s of pending.value) if (!roles.value[s.pk]) roles.value[s.pk] = 'owner'
    available.value = true
  } catch {
    available.value = false
  } finally {
    loading.value = false
  }
}

async function approve(s) {
  busy.value = s.pk
  try {
    const res = await signupsApi.approve(s.pk, roles.value[s.pk] || 'owner')
    // The edge step can fail without the approval failing, and the operator
    // needs to know which of those happened.
    if (res.edge_allowlist === 'requested') {
      toast.success(`${res.email} approved as ${res.role}; their address was sent to the edge`)
    } else {
      toast.info(`${res.email} approved as ${res.role} — edge: ${res.edge_allowlist}`)
    }
    await load()
  } catch (e) {
    toast.error(e.message || 'Approval failed')
  } finally {
    busy.value = ''
  }
}

async function reject(s) {
  if (!confirm(`Reject and delete the request from ${s.email}?`)) return
  busy.value = s.pk
  try {
    await signupsApi.reject(s.pk)
    toast.success(`${s.email} rejected`)
    await load()
  } catch (e) {
    toast.error(e.message || 'Rejection failed')
  } finally {
    busy.value = ''
  }
}

onMounted(() => { if (auth.user?.platform_admin) load() })
</script>

<template>
  <div v-if="auth.user?.platform_admin && available" class="bg-tactical-surface rounded-lg border border-tactical-border p-4">
    <div class="flex items-center justify-between mb-1">
      <h3 class="text-sm font-semibold text-ms-text">Beta requests</h3>
      <button class="text-xs text-ms-muted hover:text-brand-primary" @click="load">Refresh</button>
    </div>
    <p class="text-xs text-ms-muted mb-3">
      Approving activates the account, grants the role, and asks the edge to admit the address they signed up from.
    </p>

    <p v-if="loading" class="text-xs text-ms-muted">Loading…</p>
    <p v-else-if="!pending.length" class="text-xs text-ms-muted">Nothing waiting.</p>

    <ul v-else class="flex flex-col gap-3">
      <li v-for="s in pending" :key="s.pk" class="bg-ms-well rounded border border-tactical-border p-3">
        <div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
          <span class="text-sm text-ms-text font-medium">{{ s.name || s.username }}</span>
          <span class="text-xs text-ms-muted2 font-mono">{{ s.email }}</span>
          <span v-if="!s.email_verified"
                class="text-[11px] px-1.5 py-0.5 rounded bg-amber-900/50 text-amber-300 border border-amber-700/50">
            email not verified
          </span>
        </div>
        <dl class="mt-2 grid grid-cols-2 sm:grid-cols-3 gap-x-4 gap-y-1 text-xs">
          <div v-if="s.organisation"><dt class="text-ms-muted2 inline">Organisation: </dt><dd class="text-ms-text2 inline">{{ s.organisation }}</dd></div>
          <div v-if="s.country"><dt class="text-ms-muted2 inline">Country: </dt><dd class="text-ms-text2 inline">{{ s.country }}</dd></div>
          <div v-if="s.callsign"><dt class="text-ms-muted2 inline">Callsign: </dt><dd class="text-ms-text2 inline">{{ s.callsign }}</dd></div>
          <div v-if="s.matrix_id"><dt class="text-ms-muted2 inline">Matrix: </dt><dd class="text-ms-text2 inline font-mono">{{ s.matrix_id }}</dd></div>
          <div v-if="s.signup_ip"><dt class="text-ms-muted2 inline">Signed up from: </dt><dd class="text-ms-text2 inline font-mono">{{ s.signup_ip }}</dd></div>
        </dl>
        <p v-if="s.intended_use" class="mt-2 text-xs text-ms-text2">{{ s.intended_use }}</p>
        <p v-if="!s.signup_ip" class="mt-2 text-xs text-ms-warning">
          No address recorded, so the edge cannot be opened automatically. Ask them for it and run whitelist-ip.sh.
        </p>
        <div class="mt-3 flex flex-wrap items-center gap-2">
          <select v-model="roles[s.pk]"
                  class="text-xs bg-tactical-surface border border-tactical-border rounded px-2 py-1 text-ms-text">
            <option value="owner">owner</option>
            <option value="operator">operator</option>
            <option value="viewer">viewer</option>
          </select>
          <button :disabled="busy === s.pk" @click="approve(s)"
                  class="text-xs px-3 py-1 rounded bg-brand-primary hover:bg-brand-accent text-ms-on-primary disabled:opacity-50">
            {{ busy === s.pk ? 'Working…' : 'Approve' }}
          </button>
          <button :disabled="busy === s.pk" @click="reject(s)"
                  class="text-xs px-3 py-1 rounded border border-tactical-border text-ms-muted hover:text-ms-error disabled:opacity-50">
            Reject
          </button>
        </div>
      </li>
    </ul>
  </div>
</template>
