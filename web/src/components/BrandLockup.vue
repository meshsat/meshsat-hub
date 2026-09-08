<script setup>
// MeshSat lockup: production mark (theme variant) next to the wordmark, the
// same composition the website header uses. The mark files come from
// meshsat-website/site/static/images and are never redrawn here.
import { useThemeStore } from '../stores/theme'

defineProps({
  // 'sm' for the app header, 'lg' for the login pages
  size: { type: String, default: 'sm' },
  // Optional product label after the wordmark
  label: { type: String, default: 'Hub' },
})

const theme = useThemeStore()
</script>

<template>
  <span class="inline-flex items-center shrink-0" :class="size === 'lg' ? 'gap-3' : 'gap-2'">
    <img
      :src="theme.dark ? '/meshsat-mark-dark.png' : '/meshsat-mark-light.png'"
      alt=""
      :class="size === 'lg' ? 'h-10' : 'h-7'"
      class="w-auto"
      width="47"
      height="28"
    />
    <span class="font-display font-bold tracking-wider leading-none" :class="size === 'lg' ? 'text-2xl' : 'text-sm'">
      <span class="text-ms-text">Mesh</span><span class="text-ms-primary">Sat</span>
      <span v-if="label" class="font-medium text-ms-muted ml-1">{{ label }}</span>
    </span>
  </span>
</template>
