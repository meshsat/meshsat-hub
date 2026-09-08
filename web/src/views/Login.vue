<script setup>
import { ref, computed, onMounted } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useAuthStore } from '../stores/auth'
import BrandLockup from '../components/BrandLockup.vue'

const authStore = useAuthStore()
const router = useRouter()
const route = useRoute()

const mode = ref('email') // 'email' or 'token'
const modes = ref(null) // from /api/auth/config; null until loaded
const showOther = ref(false) // local/token forms behind a disclosure when OIDC is offered
const hasOIDC = computed(() => modes.value?.includes('oidc'))
const hasLocal = computed(() => !modes.value || modes.value.includes('local'))
const signupUrl = computed(() => authStore.authConfig?.signup_url || '')
const redirectTarget = computed(() => {
  const r = route.query.redirect
  return typeof r === 'string' && r.startsWith('/') && !r.startsWith('//') ? r : ''
})

onMounted(async () => {
  const cfg = await authStore.fetchAuthConfig()
  modes.value = cfg.modes || ['local']
  if (!hasLocal.value) mode.value = 'token'
  if (route.query.error === 'pending_approval') {
    error.value = 'Your MeshSat ID is registered but not approved yet. You will get an email when access is granted.'
  }
})

function signInWithMeshSatID() {
  authStore.startOIDCLogin(redirectTarget.value)
}

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
  router.push(redirectTarget.value || { name: 'dashboard' })
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
  router.push(redirectTarget.value || { name: 'dashboard' })
}
</script>

<template>
  <div class="min-h-screen bg-tactical-bg flex items-center justify-center px-4">
    <div class="w-full max-w-sm">
      <h1 class="flex justify-center mb-8"><BrandLockup size="lg" /></h1>

      <!-- Single sign-on (MeshSat ID) when the Hub offers it -->
      <div v-if="hasOIDC" class="bg-tactical-surface rounded-lg p-6 space-y-4 mb-4" data-testid="sso-panel">
        <button
          type="button"
          @click="signInWithMeshSatID"
          class="w-full py-2.5 bg-brand-primary hover:bg-brand-accent text-ms-on-primary rounded-lg font-medium transition-colors"
        >
          Sign in with MeshSat ID
        </button>
        <p v-if="signupUrl" class="text-xs text-ms-muted text-center">
          No account yet?
          <a :href="signupUrl" class="text-ms-text hover:text-ms-primary underline" data-testid="signup-link">Request beta access</a>
        </p>
        <p v-if="error && !showOther" class="text-ms-error text-sm">{{ error }}</p>
        <button
          type="button"
          @click="showOther = !showOther; error = ''"
          class="w-full text-xs text-gray-500 hover:text-gray-300"
          :aria-expanded="showOther"
        >
          {{ showOther ? 'Hide other sign-in options' : 'Other sign-in options' }}
        </button>
      </div>

      <form v-if="!hasOIDC || showOther" @submit.prevent="handleLogin" class="bg-tactical-surface rounded-lg p-6 space-y-4" data-testid="local-panel">
        <!-- Mode toggle -->
        <div v-if="hasLocal" class="flex rounded-lg overflow-hidden border border-gray-700">
          <button type="button" @click="mode = 'email'; error = ''"
            class="flex-1 py-2 text-sm font-medium transition-colors"
            :class="mode === 'email' ? 'bg-brand-primary text-ms-on-primary' : 'bg-gray-800 text-gray-400 hover:text-gray-200'">
            Email
          </button>
          <button type="button" @click="mode = 'token'; error = ''"
            class="flex-1 py-2 text-sm font-medium transition-colors"
            :class="mode === 'token' ? 'bg-brand-primary text-ms-on-primary' : 'bg-gray-800 text-gray-400 hover:text-gray-200'">
            API Token
          </button>
        </div>

        <!-- Email/Password fields -->
        <template v-if="mode === 'email' && hasLocal">
          <div>
            <label for="email" class="block text-sm text-gray-400 mb-1">Email</label>
            <input
              id="email"
              v-model="email"
              type="email"
              placeholder="you@example.com"
              autocomplete="email"
              class="w-full px-3 py-2 bg-gray-800 border border-gray-700 rounded text-gray-100 placeholder-gray-500 focus:outline-none focus:border-brand-primary"
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
              class="w-full px-3 py-2 bg-gray-800 border border-gray-700 rounded text-gray-100 placeholder-gray-500 focus:outline-none focus:border-brand-primary"
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
            class="w-full px-3 py-2 bg-gray-800 border border-gray-700 rounded text-gray-100 placeholder-gray-500 focus:outline-none focus:border-brand-primary"
          />
        </div>

        <p v-if="error" class="text-ms-error text-sm">{{ error }}</p>

        <button
          type="submit"
          :disabled="loading"
          class="w-full py-2 bg-brand-primary hover:bg-brand-accent disabled:opacity-50 text-ms-on-primary rounded-lg font-medium transition-colors"
        >
          {{ loading ? 'Signing in...' : 'Sign In' }}
        </button>
      </form>
    </div>
  </div>
</template>
