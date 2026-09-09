<script setup>
// The upgrade call to action, wherever a device limit is reached.
//
// It carries the claim code with it deliberately. The code is what matches a
// Ko-fi payment to this account, it is minted on /api/tenant/usage, and it used
// to live only on the Settings page while every upgrade link went straight to
// Ko-fi without it. Somebody who follows the link from here and pays without
// the code lands as an unmatched payment: their money is taken, no plan is
// granted, and an operator has to fix it by hand. So the click copies the code
// and says so before the payment page opens.
import { computed } from 'vue'
import { useToastStore } from '../stores/toast'

const props = defineProps({
  usage: { type: Object, required: true },
  // 'button' beside a full-width banner, 'link' inline in a header row.
  variant: { type: String, default: 'link' },
})

const toast = useToastStore()
const code = computed(() => props.usage?.claim_code || '')

async function go(e) {
  if (!code.value) return // nothing to copy; let the link open normally
  try {
    await navigator.clipboard.writeText(code.value)
    toast.success(`Claim code ${code.value} copied — paste it into the Ko-fi message so the payment reaches this account.`)
  } catch {
    e.preventDefault()
    toast.info(`Put claim code ${code.value} in the Ko-fi message so the payment reaches this account.`)
    window.open(props.usage.upgrade_url, '_blank', 'noopener')
  }
}
</script>

<template>
  <div v-if="usage?.upgrade_url" class="flex flex-wrap items-center gap-2">
    <a :href="usage.upgrade_url" target="_blank" rel="noopener noreferrer" @click="go"
      :class="variant === 'button'
        ? 'px-3 py-1.5 bg-brand-primary hover:bg-brand-accent text-ms-on-primary text-sm font-medium rounded transition-colors'
        : 'text-xs px-3 py-1.5 rounded border border-ms-border text-ms-text2 hover:text-ms-text hover:border-ms-border-light transition-colors'">
      Support &amp; upgrade
    </a>
    <span v-if="code" class="text-[11px] text-ms-muted">
      code <code class="font-mono text-ms-text2 tracking-wider">{{ code }}</code> goes in the Ko-fi message
    </span>
  </div>
</template>
