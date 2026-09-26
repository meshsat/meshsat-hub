<script setup>
// Plan, expiry and send caps for one tenant, set by an operator. One Save
// for the four, because they are one decision about one account. A cap
// typed here is honoured as typed (owner ruling, MESHSAT-1289): it is the
// tenant's own carrier bill.
import { ref, computed, watch } from 'vue'
import { admin as adminApi } from '../../api/client'
import { useToastStore } from '../../stores/toast'

const props = defineProps({
  tenant: { type: Object, required: true },
  tiers: { type: Array, default: () => [] },
})
const emit = defineEmits(['saved'])
const toast = useToastStore()

const plan = ref('')
const expires = ref('')
const capDaily = ref('')
const capMonthly = ref('')
const saving = ref(false)
const error = ref('')

// The <input type="date"> speaks YYYY-MM-DD; the API speaks RFC 3339.
function toDateInput(ts) {
  if (!ts || String(ts).startsWith('0001-')) return ''
  const d = new Date(ts)
  return isNaN(d.getTime()) ? '' : d.toISOString().slice(0, 10)
}
function fromDateInput(s) {
  if (!s) return ''
  // End of that day, UTC: an expiry "on the 30th" is still good on the 30th.
  return `${s}T23:59:59Z`
}

function reset() {
  plan.value = props.tenant.plan || ''
  expires.value = toDateInput(props.tenant.plan_expires_at)
  capDaily.value = props.tenant.ratelimit_daily_cap ? String(props.tenant.ratelimit_daily_cap) : ''
  capMonthly.value = props.tenant.ratelimit_monthly_cap ? String(props.tenant.ratelimit_monthly_cap) : ''
}
watch(() => props.tenant, reset, { immediate: true })

const planOptions = computed(() => {
  const names = props.tiers.map((t) => t.plan)
  if (plan.value && !names.includes(plan.value)) names.push(plan.value)
  return names
})

const dirty = computed(() =>
  plan.value !== (props.tenant.plan || '') ||
  expires.value !== toDateInput(props.tenant.plan_expires_at) ||
  capDaily.value.trim() !== (props.tenant.ratelimit_daily_cap ? String(props.tenant.ratelimit_daily_cap) : '') ||
  capMonthly.value.trim() !== (props.tenant.ratelimit_monthly_cap ? String(props.tenant.ratelimit_monthly_cap) : ''))

async function save() {
  const daily = capDaily.value.trim() === '' ? 0 : Number(capDaily.value.trim())
  const monthly = capMonthly.value.trim() === '' ? 0 : Number(capMonthly.value.trim())
  if (![daily, monthly].every((n) => Number.isInteger(n) && n >= 0)) {
    error.value = 'A send limit must be a whole number of messages'
    return
  }
  saving.value = true
  error.value = ''
  try {
    const body = { plan: plan.value, plan_expires_at: fromDateInput(expires.value), ratelimit_daily_cap: daily, ratelimit_monthly_cap: monthly }
    const res = await adminApi.tenants.update(props.tenant.id, body)
    toast.success('Plan and caps saved')
    emit('saved', res)
  } catch (e) {
    error.value = e?.message || 'Save failed'
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <section class="ms-panel" aria-labelledby="plan-h">
    <div class="ms-panel-head"><h2 id="plan-h" class="ms-h2">Plan and caps</h2></div>
    <form class="px-4 py-4 grid grid-cols-1 sm:grid-cols-2 gap-4" @submit.prevent="save">
      <label class="block">
        <span class="ms-label">Plan</span>
        <select v-model="plan" class="ms-input w-full mt-1">
          <option v-for="p in planOptions" :key="p" :value="p">{{ p }}</option>
        </select>
      </label>
      <label class="block">
        <span class="ms-label">Plan expires</span>
        <input v-model="expires" type="date" class="ms-input w-full mt-1" />
        <span class="block mt-1 text-xs text-ms-muted">Leave empty for no expiry. A lapse never touches delivery or SOS.</span>
      </label>
      <label class="block">
        <span class="ms-label">Send limit per device, per day</span>
        <input v-model="capDaily" type="number" inputmode="numeric" min="0" class="ms-input w-full mt-1"
          :placeholder="`platform default ${tenant.ratelimit_daily_default ?? ''}`" />
      </label>
      <label class="block">
        <span class="ms-label">Send limit per device, per month</span>
        <input v-model="capMonthly" type="number" inputmode="numeric" min="0" class="ms-input w-full mt-1"
          :placeholder="tenant.ratelimit_monthly_default ? `platform default ${tenant.ratelimit_monthly_default}` : 'no monthly limit'" />
      </label>
      <p class="sm:col-span-2 text-xs text-ms-muted">
        Empty means the platform default. A number set here is honoured as it is, above or below the default: the airtime is this tenant's own account. An SOS is never counted.
      </p>
      <p v-if="error" class="sm:col-span-2 text-[13px] text-ms-error">{{ error }}</p>
      <div class="sm:col-span-2 flex items-center gap-2">
        <button type="submit" class="ms-btn-primary" :disabled="saving || !dirty">{{ saving ? 'Saving' : 'Save changes' }}</button>
        <button type="button" class="ms-btn-ghost" :disabled="saving || !dirty" @click="reset">Discard</button>
      </div>
    </form>
  </section>
</template>
