<script setup>
// One confirm dialog for the platform console: a title, whatever the caller
// puts in the body, Cancel, and one confirming button. The confirming button
// is ms-btn-destroy by default (this is the one place that class belongs);
// `variant="plain"` gives an ms-btn for a step that is checked rather than
// destructive, such as a PIN. Escape and the scrim cancel.
import { ref, watch, nextTick, onBeforeUnmount } from 'vue'

const props = defineProps({
  open: { type: Boolean, default: false },
  title: { type: String, required: true },
  label: { type: String, default: 'Confirm' },
  busy: { type: Boolean, default: false },
  // The caller's own gate (a slug typed back, a PIN long enough).
  disabled: { type: Boolean, default: false },
  variant: { type: String, default: 'destroy' },
})
const emit = defineEmits(['confirm', 'cancel'])

const panel = ref(null)
const id = `dlg-${Math.random().toString(36).slice(2, 8)}`

function onKey(e) {
  if (e.key === 'Escape' && props.open && !props.busy) emit('cancel')
}

watch(() => props.open, async (v) => {
  if (v) {
    document.addEventListener('keydown', onKey)
    await nextTick()
    // Focus the first field if there is one, else the panel itself.
    const first = panel.value?.querySelector('input, textarea, select')
    ;(first || panel.value)?.focus()
  } else {
    document.removeEventListener('keydown', onKey)
  }
}, { immediate: true })
onBeforeUnmount(() => document.removeEventListener('keydown', onKey))
</script>

<template>
  <div v-if="open" class="fixed inset-0 z-50 flex items-center justify-center p-4" role="dialog" aria-modal="true" :aria-labelledby="id">
    <div class="absolute inset-0 bg-black/60" @click="!busy && emit('cancel')" />
    <div ref="panel" tabindex="-1" class="relative ms-panel p-5 sm:p-6 w-full max-w-md focus:outline-none">
      <h3 :id="id" class="ms-h2 text-base">{{ title }}</h3>
      <div class="mt-3 text-[13px] text-ms-text2 space-y-3">
        <slot />
      </div>
      <div class="flex flex-wrap justify-end gap-2 mt-6">
        <button type="button" class="ms-btn-ghost" :disabled="busy" @click="emit('cancel')">Cancel</button>
        <button type="button" :class="variant === 'plain' ? 'ms-btn' : 'ms-btn-destroy'" :disabled="busy || disabled" @click="emit('confirm')">
          {{ busy ? 'Working' : label }}
        </button>
      </div>
    </div>
  </div>
</template>
