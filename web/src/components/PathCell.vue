<script setup>
import { STATE_WORDS } from '../utils/paths'

// One bearer family on one kit, drawn as a short link segment so it reads by
// shape as well as tone: solid = working, half = coming up, outline = fitted
// but not working, dot = not fitted. Normal is Off White, never green
// (ISA-101), so a working fleet is calm and a broken link stands out by form.
defineProps({
  state: { type: String, default: 'absent' },
  label: { type: String, default: '' },
})
</script>

<template>
  <span class="inline-flex items-center justify-center w-7 h-5" role="img" :aria-label="`${label}: ${STATE_WORDS[state]}`" :title="`${label}: ${STATE_WORDS[state]}`">
    <span v-if="state === 'up'" class="w-5 h-[5px] rounded-full bg-ms-text" />
    <span v-else-if="state === 'coming'" class="w-5 h-[5px] rounded-full border border-ms-text overflow-hidden flex"><span class="w-1/2 h-full bg-ms-text" /></span>
    <span v-else-if="state === 'down'" class="w-5 h-[5px] rounded-full border border-ms-muted" />
    <span v-else class="w-1 h-1 rounded-full bg-ms-border-light" />
  </span>
</template>
