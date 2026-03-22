<script setup>
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { useAuthStore } from '../stores/auth'

const authStore = useAuthStore()
const router = useRouter()

const mode = ref('email') // 'email' or 'token'
const email = ref('')
const password = ref('')
const apiToken = ref('')
const error = ref('')
const loading = ref(false)

async function handleLogin() {
  error.value = ''
  loading.value = true

  try {
    if (mode.value === 'email') {
      await loginWithEmail()
    } else {
      await loginWithToken()
    }
  } catch (e) {
    error.value = e.message || 'Unable to connect to API'
  } finally {
    loading.value = false
  }
}

async function loginWithEmail() {
  if (!email.value.trim() || !password.value) {
    error.value = 'Email and password are required'
    return
  }

  const res = await fetch('/api/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email: email.value.trim(), password: password.value }),
  })

  if (res.status === 429) {
    error.value = 'Too many login attempts. Please wait and try again.'
    return
  }
  if (res.status === 403) {
    error.value = 'Account is locked or disabled'
    return
  }
  if (res.status === 401) {
    error.value = 'Invalid email or password'
    return
  }
  if (!res.ok) {
    const body = await res.json().catch(() => ({}))
    error.value = body.error || 'Login failed'
    return
  }

  const data = await res.json()
  authStore.login(data.access_token)
  // Store refresh token for silent renewal
  if (data.refresh_token) {
    localStorage.setItem('auth_refresh_token', data.refresh_token)
  }
  router.push({ name: 'dashboard' })
}

async function loginWithToken() {
  if (!apiToken.value.trim()) {
    error.value = 'API token is required'
    return
  }

  const res = await fetch('/api/devices', {
    headers: { Authorization: `Bearer ${apiToken.value}` },
  })
  if (res.status === 401) {
    error.value = 'Invalid API token'
    return
  }
  if (!res.ok && res.status !== 403) {
    error.value = 'Unable to verify token'
    return
  }
  authStore.login(apiToken.value)
  router.push({ name: 'dashboard' })
}
</script>

<template>
  <div class="min-h-screen bg-tactical-bg flex items-center justify-center px-4">
    <div class="w-full max-w-sm">
      <h1 class="text-2xl font-display font-bold text-gray-200 text-center mb-8 tracking-wide">MeshSat Hub</h1>

      <form @submit.prevent="handleLogin" class="bg-tactical-surface rounded-lg p-6 space-y-4">
        <!-- Mode toggle -->
        <div class="flex rounded-lg overflow-hidden border border-gray-700">
          <button type="button" @click="mode = 'email'; error = ''"
            class="flex-1 py-2 text-sm font-medium transition-colors"
            :class="mode === 'email' ? 'bg-brand-primary text-white' : 'bg-gray-800 text-gray-400 hover:text-gray-200'">
            Email
          </button>
          <button type="button" @click="mode = 'token'; error = ''"
            class="flex-1 py-2 text-sm font-medium transition-colors"
            :class="mode === 'token' ? 'bg-brand-primary text-white' : 'bg-gray-800 text-gray-400 hover:text-gray-200'">
            API Token
          </button>
        </div>

        <!-- Email/Password fields -->
        <template v-if="mode === 'email'">
          <div>
            <label for="email" class="block text-sm text-gray-400 mb-1">Email</label>
            <input
              id="email"
              v-model="email"
              type="email"
              placeholder="you@example.com"
              autocomplete="email"
              class="w-full px-3 py-2 bg-gray-800 border border-gray-700 rounded text-gray-100 placeholder-gray-500 focus:outline-none focus:border-teal-500"
            />
          </div>
          <div>
            <label for="password" class="block text-sm text-gray-400 mb-1">Password</label>
            <input
              id="password"
              v-model="password"
              type="password"
              placeholder="Enter your password"
              autocomplete="current-password"
              class="w-full px-3 py-2 bg-gray-800 border border-gray-700 rounded text-gray-100 placeholder-gray-500 focus:outline-none focus:border-teal-500"
            />
          </div>
        </template>

        <!-- API Token field -->
        <div v-else>
          <label for="token" class="block text-sm text-gray-400 mb-1">API Token</label>
          <input
            id="token"
            v-model="apiToken"
            type="password"
            placeholder="Enter your API token"
            autocomplete="off"
            class="w-full px-3 py-2 bg-gray-800 border border-gray-700 rounded text-gray-100 placeholder-gray-500 focus:outline-none focus:border-teal-500"
          />
        </div>

        <p v-if="error" class="text-red-400 text-sm">{{ error }}</p>

        <button
          type="submit"
          :disabled="loading"
          class="w-full py-2 bg-brand-primary hover:bg-brand-accent disabled:opacity-50 text-white rounded-lg font-medium transition-colors"
        >
          {{ loading ? 'Signing in...' : 'Sign In' }}
        </button>
      </form>
    </div>
  </div>
</template>
