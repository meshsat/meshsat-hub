import { defineStore } from 'pinia'
import { ref } from 'vue'

let nextId = 0

export const useToastStore = defineStore('toast', () => {
  const toasts = ref([])

  function add(message, type = 'info', duration = 4000) {
    const id = ++nextId
    toasts.value.push({ id, message, type })
    if (duration > 0) {
      setTimeout(() => remove(id), duration)
    }
    return id
  }

  function remove(id) {
    toasts.value = toasts.value.filter(t => t.id !== id)
  }

  function success(message) { return add(message, 'success') }
  function error(message) { return add(message, 'error', 6000) }
  function info(message) { return add(message, 'info') }
  function warning(message) { return add(message, 'warning', 5000) }

  return { toasts, add, remove, success, error, info, warning }
})
