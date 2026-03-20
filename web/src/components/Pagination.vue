<script setup>
import { computed } from 'vue'

const props = defineProps({
  page: { type: Number, required: true },
  pageSize: { type: Number, default: 50 },
  total: { type: Number, default: 0 },
})

const emit = defineEmits(['update:page'])

const totalPages = computed(() => Math.max(1, Math.ceil(props.total / props.pageSize)))
const canPrev = computed(() => props.page > 1)
const canNext = computed(() => props.page < totalPages.value)

function prev() { if (canPrev.value) emit('update:page', props.page - 1) }
function next() { if (canNext.value) emit('update:page', props.page + 1) }
</script>

<template>
  <div v-if="total > pageSize" class="flex items-center justify-between text-sm text-gray-400 mt-3">
    <span>{{ (page - 1) * pageSize + 1 }}–{{ Math.min(page * pageSize, total) }} of {{ total }}</span>
    <div class="flex gap-1">
      <button @click="prev" :disabled="!canPrev"
        class="px-2 py-1 rounded bg-gray-700 hover:bg-gray-600 disabled:opacity-30 disabled:cursor-not-allowed">
        Prev
      </button>
      <button @click="next" :disabled="!canNext"
        class="px-2 py-1 rounded bg-gray-700 hover:bg-gray-600 disabled:opacity-30 disabled:cursor-not-allowed">
        Next
      </button>
    </div>
  </div>
</template>
