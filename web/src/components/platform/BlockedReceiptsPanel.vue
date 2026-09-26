<script setup>
// Receipts parked for a person (rule 25: never issued on a guess). Requeue
// once the cause is fixed, or record a refund against the payment.
import { ref, onMounted } from 'vue'
import { admin as adminApi } from '../../api/client'
import { useToastStore } from '../../stores/toast'
import { formatMinor } from '../../utils/money'
import { formatWhen } from '../../utils/time'
import ConfirmDialog from './ConfirmDialog.vue'

const emit = defineEmits(['refunded'])
const toast = useToastStore()
const rows = ref([])
const loading = ref(true)
const error = ref('')
const busy = ref('')

async function load() {
  loading.value = true
  error.value = ''
  try {
    const res = await adminApi.billing.blockedReceipts()
    rows.value = Array.isArray(res) ? res : []
  } catch (e) {
    error.value = e?.message || 'Could not load the blocked receipts'
  } finally {
    loading.value = false
  }
}
defineExpose({ load })

async function requeue(r) {
  busy.value = r.id
  try {
    await adminApi.billing.requeueReceipt(r.id)
    toast.success('Receipt requeued; the drainer tries it now.')
    await load()
  } catch (e) {
    toast.error(e?.status === 404 ? 'No blocked receipt with that id any more.' : (e?.message || 'Could not requeue'))
  } finally {
    busy.value = ''
  }
}

// Record refund: the amount in minor units as an integer, never a decimal.
const refunding = ref(null)
const amount = ref('')
const reason = ref('')
const refundBusy = ref(false)
const refundError = ref('')
function startRefund(r) { refunding.value = r; amount.value = String(r.amount_cents ?? ''); reason.value = ''; refundError.value = '' }
function amountValid() {
  const t = amount.value.trim()
  if (t === '') return true
  const n = Number(t)
  return Number.isInteger(n) && n >= 0 && n <= Number(refunding.value?.amount_cents ?? 0)
}
async function confirmRefund() {
  if (!refunding.value || !amountValid()) return
  refundBusy.value = true
  refundError.value = ''
  try {
    const cents = amount.value.trim() === '' ? 0 : Number(amount.value.trim())
    await adminApi.billing.recordRefund(refunding.value.id, { amount_cents: cents, reason: reason.value.trim() })
    toast.success('Refund recorded; the credit note is queued.')
    refunding.value = null
    emit('refunded')
    await load()
  } catch (e) {
    refundError.value = e?.message || 'Could not record the refund'
  } finally {
    refundBusy.value = false
  }
}
onMounted(load)
</script>

<template>
  <section class="ms-panel" aria-labelledby="blocked-h">
    <div class="ms-panel-head">
      <h2 id="blocked-h" class="ms-h2">Blocked receipts</h2>
      <span class="text-xs text-ms-muted ms-num">{{ rows.length }}</span>
    </div>
    <div v-if="loading" class="px-4 py-6 text-sm text-ms-muted">Loading.</div>
    <div v-else-if="error" class="px-4 py-3 text-[13px] text-ms-error">{{ error }}</div>
    <div v-else-if="!rows.length" class="px-4 py-6 text-sm text-ms-muted">No receipt is waiting for a person.</div>
    <div v-else class="overflow-x-auto">
      <table class="ms-table">
        <thead><tr><th>Paid</th><th>Customer</th><th>Tenant</th><th class="text-right">Amount</th><th>Country</th><th>Why it stopped</th><th></th></tr></thead>
        <tbody>
          <tr v-for="r in rows" :key="r.id">
            <td class="ms-num whitespace-nowrap text-ms-muted">{{ formatWhen(r.paid_at, '') }}</td>
            <td class="max-w-0 min-w-[12rem]">
              <div class="truncate">{{ r.name || '' }}</div>
              <div class="ms-id text-ms-muted truncate">{{ r.email }}</div>
            </td>
            <td class="ms-id">{{ r.tenant_id }}</td>
            <td class="ms-num text-right whitespace-nowrap">{{ formatMinor(r.amount_cents, r.currency) }}</td>
            <td>{{ r.country || 'unknown' }}</td>
            <td class="max-w-[36ch] truncate text-ms-muted" :title="r.last_error">{{ r.last_error || '' }}<span v-if="r.attempts" class="ms-num"> ({{ r.attempts }} attempts)</span></td>
            <td class="whitespace-nowrap">
              <div class="flex justify-end gap-2">
                <button type="button" class="ms-btn" :disabled="busy === r.id" @click="requeue(r)">Requeue</button>
                <button type="button" class="ms-btn" :disabled="busy === r.id" @click="startRefund(r)">Record refund</button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <ConfirmDialog :open="!!refunding" title="Record a refund" label="Record refund" :busy="refundBusy" :disabled="!amountValid()" @confirm="confirmRefund" @cancel="refunding = null">
      <p>Records money already given back for the payment from <span class="ms-id text-ms-text">{{ refunding?.email }}</span> ({{ formatMinor(refunding?.amount_cents, refunding?.currency) }}) and queues the credit note. It does not move money.</p>
      <label class="block">
        <span class="ms-label">Amount in cents</span>
        <input v-model="amount" type="number" inputmode="numeric" min="0" step="1" :max="refunding?.amount_cents" class="ms-input w-full mt-1" />
        <span class="block mt-1 text-xs text-ms-muted">Minor units of {{ (refunding?.currency || '').toUpperCase() }}, a whole number. Empty or 0 means the whole payment; never more than was paid.</span>
      </label>
      <label class="block">
        <span class="ms-label">Reason</span>
        <textarea v-model="reason" class="ms-input w-full mt-1 !h-auto py-2" rows="2" maxlength="500" placeholder="For the audit line; the customer never sees it"></textarea>
      </label>
      <p v-if="refundError" class="text-ms-error" role="alert">{{ refundError }}</p>
    </ConfirmDialog>
  </section>
</template>
