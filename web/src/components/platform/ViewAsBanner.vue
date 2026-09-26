<script setup>
// Shown above every page while a platform admin is inside a customer's
// workspace. Caution, not alarm: the yellow says "you are not in your own
// account", and the sentence says what that means for the customer.
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { useAuthStore } from '../../stores/auth'

const auth = useAuthStore()
const now = ref(Date.now())
let timer = null
onMounted(() => { timer = setInterval(() => { now.value = Date.now() }, 15000) })
onUnmounted(() => clearInterval(timer))

const remaining = computed(() => {
  const v = auth.viewAs
  if (!v?.expires_at) return ''
  const ms = new Date(v.expires_at).getTime() - now.value
  if (ms <= 0) return 'expired'
  const m = Math.ceil(ms / 60000)
  if (m < 60) return `${m} min`
  const h = Math.trunc(m / 60)
  const r = m % 60
  return r ? `${h} h ${r} min` : `${h} h`
})

const leaving = ref(false)
async function exit() {
  if (leaving.value) return
  leaving.value = true
  try { await auth.endViewAs() } finally { leaving.value = false }
}
</script>

<template>
  <div v-if="auth.viewAs" class="w-full border-b border-ms-warning/60 bg-ms-warning/10 px-3 sm:px-5 py-2" role="status" data-testid="view-as-banner">
    <div class="mx-auto max-w-[1440px] flex flex-wrap items-center gap-x-4 gap-y-1.5 text-[13px] text-ms-text">
      <div class="flex flex-wrap items-baseline gap-x-2 min-w-0">
        <span class="text-ms-muted">Viewing as</span>
        <span class="font-medium truncate">{{ auth.viewAs.name || auth.viewAs.slug || auth.viewAs.id }}</span>
        <span v-if="auth.viewAs.slug" class="ms-id text-ms-muted">{{ auth.viewAs.slug }}</span>
        <span class="text-ms-muted ms-num">expires in {{ remaining }}</span>
      </div>
      <span class="text-ms-text2 basis-full sm:basis-auto sm:flex-1 min-w-0">Everything you do here is recorded in their audit log.</span>
      <button type="button" class="ms-btn ml-auto" :disabled="leaving" @click="exit">Exit</button>
    </div>
  </div>
</template>
