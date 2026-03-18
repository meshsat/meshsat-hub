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
    // Try to invalidate server-side sessions
    const refreshToken = localStorage.getItem('auth_refresh_token')
    if (token.value) {
      fetch('/api/auth/logout', {
        method: 'POST',
        headers: { 'Authorization': `Bearer ${token.value}` },
      }).catch(() => {})
    }
    token.value = ''
    user.value = null
    localStorage.removeItem('auth_token')
    localStorage.removeItem('auth_user')
    localStorage.removeItem('auth_refresh_token')
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

  // Silent token refresh — call when a 401 is received
  async function refreshToken() {
    const rt = localStorage.getItem('auth_refresh_token')
    if (!rt) return false

    try {
      const res = await fetch('/api/auth/refresh', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refresh_token: rt }),
      })
      if (!res.ok) return false

      const data = await res.json()
      token.value = data.access_token
      localStorage.setItem('auth_token', data.access_token)
      if (data.refresh_token) {
        localStorage.setItem('auth_refresh_token', data.refresh_token)
      }
      return true
    } catch {
      return false
    }
  }

  // Fetch user info on startup if authenticated
  if (token.value && !user.value) {
    fetchUser()
  }

  return { token, user, isAuthenticated, role, isOwner, login, logout, fetchUser, refreshToken }
})
