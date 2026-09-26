<script setup>
// Support access (owner's Settings): lets MeshSat support open this
// workspace for a time the owner chooses, behind a PIN the owner shares out
// of band. Hidden for the platform tenant and whenever the API says there is
// no such thing here (400/404, read off the status).
import { ref, computed, onMounted } from 'vue'
import { tenant as tenantApi } from '../api/client'
import { useAuthStore } from '../stores/auth'
import { useToastStore } from '../stores/toast'
import { formatWhen } from '../utils/time'

const auth = useAuthStore()
const toast = useToastStore()

const available = ref(false)
const grant = ref(null)
const pin = ref('')
const duration = ref(0)
const busy = ref(false)
const error = ref('')

const isDefaultTenant = computed(() => auth.user?.tenant_id === 'default')

const STEPS = [30, 120, 480, 1440, 4320]
function stepLabel(m) {
  if (m < 60) return `${m} min`
  const h = m / 60
  return h < 24 ? `${h} h` : `${h / 24} days`
}
const durations = computed(() => {
  const min = Number(grant.value?.min_minutes) || 30
  const max = Number(grant.value?.max_minutes) || 4320
  const opts = STEPS.filter((m) => m >= min && m <= max)
  if (!opts.includes(min)) opts.unshift(min)
  if (!opts.includes(max)) opts.push(max)
  return [...new Set(opts)].sort((a, b) => a - b)
})

async function load() {
  if (isDefaultTenant.value) { available.value = false; return }
  try {
    grant.value = await tenantApi.supportAccess.get()
    available.value = true
    if (!duration.value || !durations.value.includes(duration.value)) {
      duration.value = durations.value.includes(120) ? 120 : durations.value[0]
    }
  } catch (e) {
    available.value = !(e?.status === 400 || e?.status === 404)
    if (available.value) error.value = e?.message || 'Could not read support access'
  }
}

// 12 alphanumerics from the CSPRNG, rejection-sampled so no character is
// likelier than another. Shown in the box, never stored here.
const ALPHABET = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789'
function generate() {
  const out = []
  const limit = 256 - (256 % ALPHABET.length)
  const buf = new Uint8Array(32)
  while (out.length < 12) {
    crypto.getRandomValues(buf)
    for (const b of buf) {
      if (b < limit) out.push(ALPHABET[b % ALPHABET.length])
      if (out.length === 12) break
    }
  }
  pin.value = out.join('')
}

async function grantAccess() {
  if (pin.value.length < 10) { error.value = 'The PIN needs at least 10 characters'; return }
  busy.value = true
  error.value = ''
  try {
    grant.value = await tenantApi.supportAccess.create(pin.value, duration.value)
    pin.value = ''
    toast.success('Support access granted. Share the PIN with support out of band.')
  } catch (e) {
    error.value = e?.message || 'Could not grant support access'
  } finally {
    busy.value = false
  }
}

async function revoke() {
  busy.value = true
  error.value = ''
  try {
    await tenantApi.supportAccess.revoke()
    await load()
    toast.success('Support access revoked')
  } catch (e) {
    error.value = e?.message || 'Could not revoke support access'
  } finally {
    busy.value = false
  }
}

onMounted(load)
</script>

<template>
  <div v-if="available && !isDefaultTenant" class="pt-4 border-t border-ms-border" data-testid="support-access">
    <h4 class="text-sm font-medium text-ms-text">Support access</h4>
    <p class="text-[11px] text-ms-muted mt-1 mb-3">
      Lets MeshSat support open your workspace for a time you choose. You share the PIN with support out of band; it is stored hashed and never shown again. Every action they take is written to your audit log.
    </p>

    <div v-if="grant?.active" class="text-[13px] space-y-1">
      <p class="text-ms-text2">Active until <span class="ms-num">{{ formatWhen(grant.expires_at, '') }}</span><template v-if="grant.created_by_email">, granted by <span class="ms-id">{{ grant.created_by_email }}</span></template>.</p>
      <p class="text-ms-muted">
        <template v-if="grant.used_at">Opened by <span class="ms-id">{{ grant.used_by_email || 'support' }}</span> at <span class="ms-num">{{ formatWhen(grant.used_at, '') }}</span>.</template>
        <template v-else>Not opened yet.</template>
      </p>
      <div class="pt-1">
        <button type="button" class="ms-btn-danger" :disabled="busy" @click="revoke">Revoke</button>
      </div>
    </div>

    <form v-else class="flex flex-wrap items-end gap-2" @submit.prevent="grantAccess">
      <label class="block flex-1 min-w-[12rem]">
        <span class="ms-label">PIN</span>
        <div class="flex gap-2 mt-1">
          <input v-model="pin" type="text" class="ms-input w-full ms-id" minlength="10" maxlength="128" autocomplete="off" spellcheck="false" />
          <button type="button" class="ms-btn" @click="generate">Generate</button>
        </div>
      </label>
      <label class="block">
        <span class="ms-label">For</span>
        <select v-model.number="duration" class="ms-input mt-1 block">
          <option v-for="m in durations" :key="m" :value="m">{{ stepLabel(m) }}</option>
        </select>
      </label>
      <button type="submit" class="ms-btn" :disabled="busy || pin.length < 10">Grant</button>
    </form>
    <p v-if="error" class="text-ms-error text-[13px] mt-2">{{ error }}</p>
  </div>
</template>
