import { defineStore } from 'pinia'
import { ref, watch } from 'vue'

export const useThemeStore = defineStore('theme', () => {
  const dark = ref(localStorage.getItem('theme') !== 'light')

  watch(dark, (val) => {
    localStorage.setItem('theme', val ? 'dark' : 'light')
    applyTheme(val)
  })

  function applyTheme(isDark) {
    if (isDark) {
      document.documentElement.classList.add('dark')
    } else {
      document.documentElement.classList.remove('dark')
    }
  }

  function toggle() {
    dark.value = !dark.value
  }

  // Apply on load
  applyTheme(dark.value)

  return { dark, toggle }
})
