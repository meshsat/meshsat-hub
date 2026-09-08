import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { authApi } from '../api/client'

export const useAuthStore = defineStore('auth', () => {
  const token = ref(localStorage.getItem('auth_token') || '')
  const user = ref(JSON.parse(localStorage.getItem('auth_user') || 'null'))

  const isAuthenticated = computed(() => !!token.value)
  const isPlatformAdmin = computed(() => !!user.value?.platform_admin)

  // Login methods offered by this Hub: { modes: ['oidc','local',...], oidc_login_url }.
  const authConfig = ref(null)
  async function fetchAuthConfig() {
    if (authConfig.value) return authConfig.value
    try {
      const res = await fetch('/api/auth/config')
      if (res.ok) authConfig.value = await res.json()
    } catch {
      // Older Hubs have no /api/auth/config; the login page falls back to the local form.
    }
    if (!authConfig.value) authConfig.value = { modes: ['local'] }
    return authConfig.value
  }

  // Start the browser OIDC login: the Hub redirects to the identity provider.
  function startOIDCLogin(next) {
    const base = authConfig.value?.oidc_login_url || '/api/auth/oidc/login'
    const q = next && next.startsWith('/') ? `?next=${encodeURIComponent(next)}` : ''
    window.location.assign(base + q)
  }

  // Complete the OIDC login after the callback: the Hub set the HttpOnly
  // meshsat_refresh cookie, and POST /api/auth/refresh turns it into an
  // access token. Nothing sensitive ever appears in the URL.
  async function completeOIDC() {
    let data
    try {
      const res = await fetch('/api/auth/refresh', { method: 'POST', credentials: 'same-origin' })
      if (!res.ok || !(res.headers.get('content-type') || '').includes('application/json')) return false
      data = await res.json()
    } catch {
      return false
    }
    if (!data?.access_token) return false
    token.value = data.access_token
    localStorage.setItem('auth_token', data.access_token)
    if (data.refresh_token) localStorage.setItem('auth_refresh_token', data.refresh_token)
    await fetchUser()
    return true
  }
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
  // One refresh in flight at a time: a page load with a stale token fires a
  // dozen API calls that all get 401 and would each rotate the refresh token
  // (single-use), the second one failing and logging the user out.
  let refreshInFlight = null
  function refreshToken() {
    if (!refreshInFlight) {
      refreshInFlight = doRefresh().finally(() => { refreshInFlight = null })
    }
    return refreshInFlight
  }

  async function doRefresh() {
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

  return { token, user, isAuthenticated, isPlatformAdmin, role, isOwner, authConfig, fetchAuthConfig, startOIDCLogin, completeOIDC, login, logout, fetchUser, refreshToken }
})
