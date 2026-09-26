<script setup>
// Refunds by status. Requeue a blocked one once its cause is fixed; withdraw
// one that was recorded by mistake before its credit note goes out.
import { ref, watch, onMounted } from 'vue'
import { admin as adminApi } from '../../api/client'
import { useToastStore } from '../../stores/toast'
import { formatMinor } from '../../utils/money'
import { formatWhen } from '../../utils/time'
import ConfirmDialog from './ConfirmDialog.vue'

const toast = useToastStore()
const status = ref('pending')
const rows = ref([])
const loading = ref(true)
const error = ref('')
const busy = ref('')

async function load() {
  loading.value = true
  error.value = ''
  try {
    const res = await adminApi.billing.refunds(status.value)
    rows.value = Array.isArray(res) ? res : []
  } catch (e) {
    error.value = e?.message || 'Could not load the refunds'
  } finally {
    loading.value = false
  }
}
defineExpose({ load })
watch(status, load)

async function requeue(r) {
  busy.value = r.id
  try {
    await adminApi.billing.requeueRefund(r.id)
    toast.success('Refund requeued.')
    await load()
  } catch (e) {
    toast.error(e?.message || 'Could not requeue')
  } finally {
    busy.value = ''
  }
}

const withdrawing = ref(null)
const withdrawBusy = ref(false)
async function confirmWithdraw() {
  if (!withdrawing.value) return
  withdrawBusy.value = true
  try {
    await adminApi.billing.withdrawRefund(withdrawing.value.id)
    toast.success('Refund withdrawn.')
    withdrawing.value = null
    await load()
  } catch (e) {
    toast.error(e?.message || 'Could not withdraw the refund')
  } finally {
    withdrawBusy.value = false
  }
}
onMounted(load)
</script>

<template>
  <section class="ms-panel" aria-labelledby="refunds-h">
    <div class="ms-panel-head">
      <h2 id="refunds-h" class="ms-h2">Refunds</h2>
      <label class="flex items-center gap-2 text-xs text-ms-muted">
        Status
        <select v-model="status" class="ms-input !h-7">
          <option value="pending">pending</option>
          <option value="issued">issued</option>
          <option value="blocked">blocked</option>
        </select>
      </label>
    </div>
    <div v-if="loading" class="px-4 py-6 text-sm text-ms-muted">Loading.</div>
    <div v-else-if="error" class="px-4 py-3 text-[13px] text-ms-error">{{ error }}</div>
    <div v-else-if="!rows.length" class="px-4 py-6 text-sm text-ms-muted">No {{ status }} refunds.</div>
    <div v-else class="overflow-x-auto">
      <table class="ms-table">
        <thead><tr><th>Refunded</th><th>Tenant</th><th>Receipt</th><th class="text-right">Amount</th><th>Reason</th><th>By</th><th>Credit note</th><th></th></tr></thead>
        <tbody>
          <tr v-for="r in rows" :key="r.id">
            <td class="ms-num whitespace-nowrap text-ms-muted">{{ formatWhen(r.refunded_at, '') }}</td>
            <td class="ms-id">{{ r.tenant_id }}</td>
            <td class="ms-id">{{ r.receipt_id }}</td>
            <td class="ms-num text-right whitespace-nowrap">{{ formatMinor(r.amount_cents, r.currency) }}</td>
            <td class="max-w-[28ch] truncate" :title="r.reason">{{ r.reason || '' }}</td>
            <td class="ms-id">{{ r.requested_by || '' }}</td>
            <td class="whitespace-nowrap">
              <template v-if="r.status === 'issued'">{{ r.credit_number || r.credit_ref || 'issued' }}</template>
              <span v-else-if="r.status === 'blocked'" class="text-ms-warning" :title="r.last_error">blocked<span v-if="r.attempts" class="ms-num"> after {{ r.attempts }}</span></span>
              <span v-else class="text-ms-muted">queued</span>
            </td>
            <td class="whitespace-nowrap">
              <div v-if="r.status !== 'issued'" class="flex justify-end gap-2">
                <button v-if="r.status === 'blocked'" type="button" class="ms-btn" :disabled="busy === r.id" @click="requeue(r)">Requeue</button>
                <button type="button" class="ms-btn-danger" :disabled="busy === r.id" @click="withdrawing = r">Withdraw</button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <ConfirmDialog :open="!!withdrawing" title="Withdraw this refund?" label="Withdraw refund" :busy="withdrawBusy" @confirm="confirmWithdraw" @cancel="withdrawing = null">
      <p>The record of {{ formatMinor(withdrawing?.amount_cents, withdrawing?.currency) }} given back on receipt <span class="ms-id text-ms-text">{{ withdrawing?.receipt_id }}</span> is removed and no credit note is produced. Use this for a refund recorded by mistake; it does not move money.</p>
    </ConfirmDialog>
  </section>
</template>
