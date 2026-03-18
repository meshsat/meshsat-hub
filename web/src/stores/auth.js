import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { authApi } from '../api/client'

export const useAuthStore = defineStore('auth', () => {
  const token = ref(localStorage.getItem('auth_token') || '')
  const user = ref(JSON.parse(localStorage.getItem('auth_user') || 'null'))

  const isAuthenticated = computed(() => !!token.value)
  const role = computed(() => {
    if (!user.value?.roles?.length) return 'viewer'
    const roles = user.value.roles
    if (roles.includes('owner') || roles.includes('admin')) return 'owner'
    if (roles.includes('operator')) return 'operator'
    return 'viewer'
  })
  const isOwner = computed(() => role.value === 'owner')

  function login(authToken) {
    token.value = authToken
    localStorage.setItem('auth_token', authToken)
    fetchUser()
  }

  function logout() {
    token.value = ''
    user.value = null
    localStorage.removeItem('auth_token')
    localStorage.removeItem('auth_user')
  }

  async function fetchUser() {
    try {
      const u = await authApi.me()
      user.value = u
      localStorage.setItem('auth_user', JSON.stringify(u))
    } catch {
      // If /auth/me fails, keep basic auth working
    }
  }

  // Fetch user info on startup if authenticated
  if (token.value && !user.value) {
    fetchUser()
  }

  return { token, user, isAuthenticated, role, isOwner, login, logout, fetchUser }
})
