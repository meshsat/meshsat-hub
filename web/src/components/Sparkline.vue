<script setup>
import { computed } from 'vue'

const props = defineProps({
  data: { type: Array, default: () => [] },
  width: { type: Number, default: 120 },
  height: { type: Number, default: 32 },
  color: { type: String, default: '#2dd4bf' },
  fillOpacity: { type: Number, default: 0.15 },
})

const path = computed(() => {
  if (props.data.length < 2) return ''
  const vals = props.data.map(Number)
  const min = Math.min(...vals)
  const max = Math.max(...vals)
  const range = max - min || 1
  const stepX = props.width / (vals.length - 1)
  const points = vals.map((v, i) => {
    const x = i * stepX
    const y = props.height - ((v - min) / range) * (props.height - 4) - 2
    return `${x.toFixed(1)},${y.toFixed(1)}`
  })
  return 'M' + points.join('L')
})

const fillPath = computed(() => {
  if (!path.value) return ''
  return `${path.value}L${props.width},${props.height}L0,${props.height}Z`
})
</script>

<template>
  <svg :width="width" :height="height" :viewBox="`0 0 ${width} ${height}`" preserveAspectRatio="none" class="inline-block">
    <path v-if="fillPath" :d="fillPath" :fill="color" :fill-opacity="fillOpacity" />
    <path v-if="path" :d="path" :stroke="color" stroke-width="1.5" fill="none" />
  </svg>
</template>
