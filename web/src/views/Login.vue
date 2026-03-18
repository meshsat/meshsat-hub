<script setup>
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { useAuthStore } from '../stores/auth'

const authStore = useAuthStore()
const router = useRouter()

const apiToken = ref('')
const error = ref('')
const loading = ref(false)

async function handleLogin() {
  error.value = ''
  if (!apiToken.value.trim()) {
    error.value = 'API token is required'
    return
  }

  loading.value = true
  try {
    const res = await fetch('/api/healthz', {
      headers: { Authorization: `Bearer ${apiToken.value}` },
    })
    if (!res.ok) {
      error.value = 'Invalid API token'
      return
    }
    authStore.login(apiToken.value)
    router.push({ name: 'dashboard' })
  } catch {
    error.value = 'Unable to connect to API'
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="min-h-screen bg-gray-900 flex items-center justify-center px-4">
    <div class="w-full max-w-sm">
      <h1 class="text-2xl font-bold text-cyan-400 text-center mb-8">MeshSat Hub</h1>
      <form @submit.prevent="handleLogin" class="bg-gray-800 rounded-lg p-6 space-y-4">
        <div>
          <label for="token" class="block text-sm text-gray-400 mb-1">API Token</label>
          <input
            id="token"
            v-model="apiToken"
            type="password"
            placeholder="Enter your API token"
            class="w-full px-3 py-2 bg-gray-700 border border-gray-600 rounded text-gray-100 placeholder-gray-500 focus:outline-none focus:border-cyan-400"
          />
        </div>
        <p v-if="error" class="text-red-400 text-sm">{{ error }}</p>
        <button
          type="submit"
          :disabled="loading"
          class="w-full py-2 bg-cyan-600 hover:bg-cyan-500 disabled:opacity-50 text-white rounded font-medium transition-colors"
        >
          {{ loading ? 'Verifying...' : 'Sign In' }}
        </button>
      </form>
    </div>
  </div>
</template>
