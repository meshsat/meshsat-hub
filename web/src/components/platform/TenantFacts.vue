<script setup>
// The facts and the usage of one tenant, read-only. Plain text throughout:
// nothing here is abnormal except being over the ceiling, which is a caution.
import { computed } from 'vue'
import { formatWhen } from '../../utils/time'

const props = defineProps({
  tenant: { type: Object, required: true },
  usage: { type: Object, default: null },
})

function cap(v, fallback) {
  return v ? String(v) : fallback
}

const facts = computed(() => {
  const t = props.tenant
  return [
    ['Plan', t.plan || ''],
    ['Plan expires', formatWhen(t.plan_expires_at, 'no expiry')],
    ['Status', t.deleted_at ? 'closed' : (t.status || '')],
    ['Send limit per device', `${cap(t.ratelimit_daily_cap, `platform default (${t.ratelimit_daily_default ?? '?'})`)} per day, ${cap(t.ratelimit_monthly_cap, t.ratelimit_monthly_default ? `platform default (${t.ratelimit_monthly_default})` : 'no monthly limit')} per month`],
    ['Billing country', t.billing_country ? `${t.billing_country}${t.billing_country_evidence ? ` (${t.billing_country_evidence})` : ''}` : 'not known'],
    ['Stripe customer', t.stripe_customer_id || 'none'],
    ['Stripe subscription', t.stripe_subscription_id || 'none'],
    ['Lapse warned', formatWhen(t.lapse_warned_at, 'never')],
    ['Created', formatWhen(t.created_at, '')],
    ['Updated', formatWhen(t.updated_at, '')],
  ]
})

const monoKeys = new Set(['Stripe customer', 'Stripe subscription'])
</script>

<template>
  <section class="ms-panel" aria-labelledby="facts-h">
    <div class="ms-panel-head"><h2 id="facts-h" class="ms-h2">Facts</h2></div>
    <dl class="px-4 py-3 grid grid-cols-1 sm:grid-cols-[auto_1fr] gap-x-6 gap-y-1.5 text-[13px]">
      <template v-for="[k, v] in facts" :key="k">
        <dt class="text-ms-muted">{{ k }}</dt>
        <dd class="text-ms-text2 break-words" :class="monoKeys.has(k) ? 'ms-id' : ''">{{ v }}</dd>
      </template>
    </dl>
  </section>

  <section class="ms-panel" aria-labelledby="usage-h">
    <div class="ms-panel-head"><h2 id="usage-h" class="ms-h2">Usage</h2></div>
    <div v-if="!usage" class="px-4 py-6 text-sm text-ms-muted">Usage is not available for this tenant.</div>
    <div v-else class="px-4 py-3 text-[13px]">
      <p class="text-ms-text2">
        <span class="ms-num" :class="usage.over_limit ? 'text-ms-warning' : 'text-ms-text'">
          {{ usage.used }}<template v-if="usage.limit !== -1"> of {{ usage.limit }}</template>
        </span>
        registered<template v-if="usage.limit === -1">, no ceiling</template>:
        {{ usage.devices }} device<span v-if="usage.devices !== 1">s</span> and
        {{ usage.bridges }} bridge<span v-if="usage.bridges !== 1">s</span>.
        <span v-if="usage.over_limit" class="text-ms-warning">Over the ceiling: it keeps everything it has and cannot add more.</span>
      </p>
      <table v-if="usage.tiers?.length" class="ms-table mt-3">
        <thead><tr><th>Tier</th><th class="text-right">Devices and bridges</th><th></th></tr></thead>
        <tbody>
          <tr v-for="tier in usage.tiers" :key="tier.plan">
            <td>{{ tier.plan }}</td>
            <td class="ms-num text-right">{{ tier.devices === -1 ? 'no ceiling' : tier.devices }}</td>
            <td class="text-ms-muted">{{ tier.current ? 'current' : '' }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>
