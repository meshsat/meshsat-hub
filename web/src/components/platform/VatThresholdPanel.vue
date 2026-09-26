<script setup>
// Progress toward the EU cross-border VAT threshold. The figure is integer
// minor units from the server; the bar is plain until it is a caution (80%)
// and an alarm at 95%, because the person reading it at 80% has to act.
import { ref, computed, onMounted } from 'vue'
import { admin as adminApi } from '../../api/client'
import { formatMinor } from '../../utils/money'

const data = ref(null)
const error = ref('')
const loading = ref(true)

async function load() {
  loading.value = true
  error.value = ''
  try {
    data.value = await adminApi.billing.vatThreshold()
  } catch (e) {
    error.value = e?.message || 'Could not read the VAT threshold'
  } finally {
    loading.value = false
  }
}
defineExpose({ load })

const percent = computed(() => {
  const p = Number(data.value?.percent_used)
  if (!Number.isFinite(p) || p < 0) return 0
  return Math.min(100, p)
})
const fill = computed(() => percent.value > 95 ? 'bg-ms-error' : percent.value > 80 ? 'bg-ms-warning' : 'bg-ms-text2')
const percentText = computed(() => `${Math.trunc(percent.value)} percent`)
const countries = computed(() => Object.entries(data.value?.by_country || {}).sort((a, b) => b[1] - a[1]))

onMounted(load)
</script>

<template>
  <section class="ms-panel" aria-labelledby="vat-h">
    <div class="ms-panel-head">
      <h2 id="vat-h" class="ms-h2">EU VAT threshold{{ data?.year ? ' ' + data.year : '' }}</h2>
      <span v-if="data" class="text-xs text-ms-muted ms-num">{{ percentText }}</span>
    </div>
    <div v-if="loading" class="px-4 py-6 text-sm text-ms-muted">Loading.</div>
    <div v-else-if="error" class="px-4 py-3 text-[13px] text-ms-error">{{ error }}</div>
    <div v-else-if="data" class="px-4 py-4 text-[13px]">
      <div class="h-2 w-full rounded bg-ms-well overflow-hidden" role="progressbar" :aria-valuenow="Math.trunc(percent)" aria-valuemin="0" aria-valuemax="100" aria-label="Cross-border sales against the threshold">
        <div class="h-full transition-all" :class="fill" :style="{ width: percent + '%' }" />
      </div>
      <dl class="mt-3 grid grid-cols-1 sm:grid-cols-[auto_1fr] gap-x-6 gap-y-1">
        <dt class="text-ms-muted">Cross-border sales this year</dt>
        <dd class="ms-num text-ms-text">{{ formatMinor(data.cross_border_cents, 'EUR') }}</dd>
        <dt class="text-ms-muted">Of which refunded</dt>
        <dd class="ms-num text-ms-text2">{{ formatMinor(data.refunded_cents, 'EUR') }}</dd>
        <dt class="text-ms-muted">Threshold</dt>
        <dd class="ms-num text-ms-text2">{{ formatMinor(data.threshold_cents, 'EUR') }}</dd>
      </dl>
      <table v-if="countries.length" class="ms-table mt-3">
        <thead><tr><th>Country</th><th class="text-right">Sales</th></tr></thead>
        <tbody>
          <tr v-for="[c, cents] in countries" :key="c">
            <td>{{ c }}</td>
            <td class="ms-num text-right">{{ formatMinor(cents, 'EUR') }}</td>
          </tr>
        </tbody>
      </table>
      <p v-else class="mt-3 text-ms-muted">No cross-border sales this year.</p>
      <p v-if="data.note" class="ms-note mt-3">{{ data.note }}</p>
    </div>
  </section>
</template>
