<script setup>
import { ref, onMounted } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { useAuthStore } from '../stores/auth'
import BrandLockup from '../components/BrandLockup.vue'

const authStore = useAuthStore()
const router = useRouter()
const route = useRoute()

const state = ref('working') // working | pending | error
const message = ref('')

// Error codes are a fixed vocabulary set by the Hub callback; nothing from
// the identity provider is echoed into the page.
const messages = {
  pending_approval: 'Your MeshSat ID is registered but not approved yet. You will receive an email when access is granted.',
  denied: 'Sign-in was cancelled at the identity provider.',
  disabled: 'This account is disabled. Contact the Hub operator.',
  provision: 'Your account could not be set up. Contact the Hub operator.',
  state: 'The sign-in session expired or was tampered with. Please try again.',
  token: 'The identity provider returned an invalid response. Please try again.',
  exchange: 'The identity provider could not be reached. Please try again.',
  provider: 'The identity provider reported an error. Please try again.',
  session: 'Your session could not be created. Please try again.',
}

onMounted(async () => {
  const err = typeof route.query.error === 'string' ? route.query.error : ''
  if (err) {
    state.value = err === 'pending_approval' ? 'pending' : 'error'
    message.value = messages[err] || 'Sign-in failed. Please try again.'
    return
  }
  const ok = await authStore.completeOIDC()
  if (!ok) {
    state.value = 'error'
    message.value = messages.session
    return
  }
  const next = typeof route.query.next === 'string' && route.query.next.startsWith('/') && !route.query.next.startsWith('//')
    ? route.query.next
    : { name: 'dashboard' }
  router.replace(next)
})
</script>

<template>
  <div class="min-h-screen bg-tactical-bg flex items-center justify-center px-4">
    <div class="w-full max-w-sm">
      <h1 class="flex justify-center mb-8"><BrandLockup size="lg" /></h1>
      <div class="bg-tactical-surface rounded-lg p-6 space-y-4 text-center" data-testid="auth-callback">
        <template v-if="state === 'working'">
          <p class="text-gray-300 text-sm">Completing sign-in…</p>
        </template>
        <template v-else-if="state === 'pending'">
          <p class="text-ms-warning font-medium">Awaiting approval</p>
          <p class="text-gray-400 text-sm">{{ message }}</p>
          <p class="text-gray-500 text-xs">
            Questions? Write to
            <a href="mailto:beta-access-hub@meshsat.net" class="text-gray-300 underline">beta-access-hub@meshsat.net</a>
            or ask in the MeshSat Matrix room.
          </p>
          <router-link :to="{ name: 'login' }" class="inline-block text-sm text-gray-300 hover:text-white underline">Back to sign-in</router-link>
        </template>
        <template v-else>
          <p class="text-ms-error font-medium">Sign-in failed</p>
          <p class="text-gray-400 text-sm">{{ message }}</p>
          <router-link :to="{ name: 'login' }" class="inline-block text-sm text-gray-300 hover:text-white underline">Back to sign-in</router-link>
        </template>
      </div>
    </div>
  </div>
</template>
