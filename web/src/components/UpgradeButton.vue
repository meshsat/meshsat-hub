<script setup>
// The upgrade call to action, wherever a device limit is reached.
//
// It used to carry a claim code: the old payment provider had no way to say
// which account a payment was for, so the customer had to copy an eight-
// character code into a message box, and anybody who forgot had their money
// taken with no plan granted until an operator fixed it by hand.
//
// Checkout starts here now. The session is created by the Hub and carries the
// tenant with it, so there is nothing to copy and nothing to forget. The old
// external link is kept for as long as that is still how the money arrives.
import { computed, ref } from 'vue'
import { useToastStore } from '../stores/toast'
import { tenant as tenantApi } from '../api/client'

const props = defineProps({
  usage: { type: Object, required: true },
  // 'button' beside a full-width banner, 'link' inline in a header row.
  variant: { type: String, default: 'link' },
  // Which plan this button buys. The banner knows; a header row does not, and
  // sends the customer to Settings to choose.
  plan: { type: String, default: '' },
})

const toast = useToastStore()
const busy = ref(false)
const inHub = computed(() => props.usage?.billing?.provider === 'stripe')
const externalURL = computed(() => props.usage?.upgrade_url || '')

const classes = computed(() => props.variant === 'button'
  ? 'px-3 py-1.5 bg-brand-primary hover:bg-brand-accent text-ms-on-primary text-sm font-medium rounded transition-colors disabled:opacity-60'
  : 'text-xs px-3 py-1.5 rounded border border-ms-border text-ms-text2 hover:text-ms-text hover:border-ms-border-light transition-colors disabled:opacity-60')

async function subscribe() {
  if (busy.value) return
  if (!props.plan) {
    // No plan in context: let them pick one on the page that lists them.
    window.location.hash = '#/settings'
    return
  }
  busy.value = true
  try {
    const { url } = await tenantApi.checkout(props.plan)
    // Same tab: this is a payment page, and a popup blocker eating it looks
    // like the button is broken.
    window.location.href = url
  } catch (e) {
    toast.error(e?.message || 'Could not start checkout. Please try again.')
    busy.value = false
  }
}
</script>

<template>
  <div class="flex flex-wrap items-center gap-2">
    <button v-if="inHub" type="button" :class="classes" :disabled="busy" @click="subscribe">
      {{ busy ? 'Opening…' : 'Subscribe' }}
    </button>
    <a v-else-if="externalURL" :href="externalURL" target="_blank" rel="noopener noreferrer"
      :class="classes">
      Support &amp; upgrade
    </a>
  </div>
</template>
