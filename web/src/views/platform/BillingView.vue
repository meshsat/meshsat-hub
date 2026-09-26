<script setup>
// The platform's money page: the VAT threshold, receipts a person has to look
// at, payments that matched no tenant, and refunds. Every figure is integer
// minor units (rule 24). No primary button: nothing here is the one thing to
// do; each row carries its own quiet actions.
import { ref, onMounted } from 'vue'
import { admin as adminApi } from '../../api/client'
import { formatWhen } from '../../utils/time'
import { formatMinor } from '../../utils/money'
import VatThresholdPanel from '../../components/platform/VatThresholdPanel.vue'
import BlockedReceiptsPanel from '../../components/platform/BlockedReceiptsPanel.vue'
import RefundsPanel from '../../components/platform/RefundsPanel.vue'

const vat = ref(null)
const blocked = ref(null)
const refunds = ref(null)

const unmatched = ref([])
const unmatchedError = ref('')
const unmatchedLoading = ref(true)

// The payment is the provider's own JSON, kept as it arrived. Pull out what a
// person reconciling a bank line looks for; the rest is in the title.
// A row's `payment` is what recordUnattributed wrote: provider, type (the
// provider's event name), event (its id), payer_email, amount_cents,
// currency and the reason nothing matched. An event that carried no money
// (a subscription lifecycle event) shows no amount rather than "0.00".
function paymentFacts(p) {
  const pay = p?.payment || {}
  const amount = pay.amount_cents
  const what = [pay.provider, pay.type].filter(Boolean).join(' ')
  return {
    what,
    email: pay.payer_email || pay.email || '',
    amount: Number.isInteger(amount) && amount !== 0 && pay.currency ? formatMinor(amount, pay.currency) : '',
    ref: pay.event || pay.id || '',
    reason: pay.reason || '',
  }
}

async function loadUnmatched() {
  unmatchedLoading.value = true
  unmatchedError.value = ''
  try {
    const res = await adminApi.billing.unmatched()
    unmatched.value = Array.isArray(res) ? res : []
  } catch (e) {
    unmatchedError.value = e?.message || 'Could not load the unattributed payments'
  } finally {
    unmatchedLoading.value = false
  }
}

function refreshAll() {
  vat.value?.load()
  blocked.value?.load()
  refunds.value?.load()
  loadUnmatched()
}
onMounted(loadUnmatched)
</script>

<template>
  <div class="ms-page max-w-6xl">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Billing</h1>
        <p class="ms-lede">What the platform has taken and given back, and the documents that need a person. Stripe moves the money; this page records it.</p>
      </div>
      <button type="button" class="ms-btn" @click="refreshAll">Refresh</button>
    </div>

    <div class="grid grid-cols-1 gap-5">
      <VatThresholdPanel ref="vat" />
      <BlockedReceiptsPanel ref="blocked" @refunded="refunds?.load(); vat?.load()" />

      <section class="ms-panel" aria-labelledby="unmatched-h">
        <div class="ms-panel-head">
          <h2 id="unmatched-h" class="ms-h2">Unattributed payments</h2>
          <span class="text-xs text-ms-muted ms-num">{{ unmatched.length }}</span>
        </div>
        <div v-if="unmatchedLoading" class="px-4 py-6 text-sm text-ms-muted">Loading.</div>
        <div v-else-if="unmatchedError" class="px-4 py-3 text-[13px] text-ms-error">{{ unmatchedError }}</div>
        <div v-else-if="!unmatched.length" class="px-4 py-6 text-sm text-ms-muted">Every payment matched a tenant.</div>
        <div v-else class="overflow-x-auto">
          <table class="ms-table">
            <thead><tr><th>Recorded</th><th>Event</th><th>Payer</th><th class="text-right">Amount</th><th>Why it did not match</th></tr></thead>
            <tbody>
              <tr v-for="(p, i) in unmatched" :key="i">
                <td class="ms-num whitespace-nowrap text-ms-muted">{{ formatWhen(p.recorded_at, '') }}</td>
                <td>
                  <div>{{ paymentFacts(p).what }}</div>
                  <div class="ms-id text-xs text-ms-muted max-w-[32ch] truncate" :title="paymentFacts(p).ref">{{ paymentFacts(p).ref }}</div>
                </td>
                <td class="ms-id">{{ paymentFacts(p).email }}</td>
                <td class="ms-num text-right whitespace-nowrap">{{ paymentFacts(p).amount }}</td>
                <td class="text-ms-text2">{{ paymentFacts(p).reason }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <RefundsPanel ref="refunds" />
    </div>
  </div>
</template>
