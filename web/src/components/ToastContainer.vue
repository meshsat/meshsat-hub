<script setup>
import { useToastStore } from '../stores/toast'

const toast = useToastStore()

function typeClasses(type) {
  switch (type) {
    case 'success': return 'bg-emerald-900/90 border-emerald-700 text-emerald-200'
    case 'error': return 'bg-red-900/90 border-red-700 text-red-200'
    case 'warning': return 'bg-amber-900/90 border-amber-700 text-amber-200'
    default: return 'bg-gray-800/90 border-gray-600 text-gray-200'
  }
}
</script>

<template>
  <Teleport to="body">
    <div class="fixed top-4 right-4 z-[9999] flex flex-col gap-2 max-w-sm">
      <TransitionGroup name="toast">
        <div v-for="t in toast.toasts" :key="t.id"
          :class="typeClasses(t.type)"
          class="border rounded-lg px-4 py-3 shadow-lg text-sm flex items-center justify-between gap-3 backdrop-blur-sm">
          <span>{{ t.message }}</span>
          <button @click="toast.remove(t.id)" class="opacity-60 hover:opacity-100 text-xs shrink-0">
            &#10005;
          </button>
        </div>
      </TransitionGroup>
    </div>
  </Teleport>
</template>

<style scoped>
.toast-enter-active { transition: all 0.3s ease-out; }
.toast-leave-active { transition: all 0.2s ease-in; }
.toast-enter-from { opacity: 0; transform: translateX(100%); }
.toast-leave-to { opacity: 0; transform: translateX(100%); }
</style>
