<script setup>
import { ref, computed, watch, onMounted, onUnmounted } from 'vue'
import { RouterView, useRouter, useRoute } from 'vue-router'
import { useAuthStore } from './stores/auth'
import { useThemeStore } from './stores/theme'
import { titleFor } from './nav'
import BrandLockup from './components/BrandLockup.vue'
import ToastContainer from './components/ToastContainer.vue'
import Icon from './components/Icon.vue'
import NavRail from './components/shell/NavRail.vue'
import AttentionPill from './components/shell/AttentionPill.vue'
import JumpSearch from './components/shell/JumpSearch.vue'
import OpsLive from './components/shell/OpsLive.vue'
import ViewAsBanner from './components/platform/ViewAsBanner.vue'
import { health } from './api/client'

const auth = useAuthStore()
const theme = useThemeStore()
const router = useRouter()
const route = useRoute()

// The rail's collapsed state is a per-viewer convenience; storage may be
// unavailable (private window), and the rail works without it.
const COLLAPSE_KEY = 'meshsat-rail-collapsed'
function readCollapsed() { try { return localStorage.getItem(COLLAPSE_KEY) === '1' } catch { return false } }
const collapsed = ref(readCollapsed())
function toggleRail() {
  collapsed.value = !collapsed.value
  try { localStorage.setItem(COLLAPSE_KEY, collapsed.value ? '1' : '0') } catch { /* per-viewer only */ }
}

const drawerOpen = ref(false)
const accountOpen = ref(false)
const accountRoot = ref(null)
watch(() => route.fullPath, () => { drawerOpen.value = false; accountOpen.value = false })

// Browser tab title follows the page, so a row of Hub tabs can be told apart.
watch(() => route.name, (n) => {
  const t = titleFor(n)
  document.title = t ? `${t} · MeshSat Hub` : 'MeshSat Hub'
}, { immediate: true })

// Hub health: quiet when ready, coloured only when it is not (ISA-101).
// Links to the public status page, because somebody looking at a red dot
// wants to know whether it is them or us (MESHSAT-1134).
const hubState = ref('unknown')
const utc = ref('')
let healthTimer = null
let clockTimer = null
async function pollHealth() {
  const r = await health.readyz()
  hubState.value = r?.status === 'ok' ? 'ok' : r?.status === 'error' ? 'down' : 'degraded'
}
function tick() { utc.value = new Date().toISOString().slice(11, 16) + 'Z' }
function onDoc(e) { if (accountOpen.value && accountRoot.value && !accountRoot.value.contains(e.target)) accountOpen.value = false }
onMounted(() => {
  tick(); clockTimer = setInterval(tick, 10000)
  pollHealth(); healthTimer = setInterval(pollHealth, 60000)
  document.addEventListener('mousedown', onDoc)
})
onUnmounted(() => {
  clearInterval(clockTimer); clearInterval(healthTimer)
  document.removeEventListener('mousedown', onDoc)
})

const bleed = computed(() => !!route.meta.bleed)

function logout() {
  auth.logout()
  router.push({ name: 'login' })
}

const initial = computed(() => (auth.user?.name || auth.user?.email || 'U')[0].toUpperCase())
const roleLabel = computed(() => ({ owner: 'Owner', operator: 'Operator', viewer: 'Viewer' }[auth.role] || auth.role))
</script>

<template>
  <div class="min-h-screen bg-ms-bg text-ms-text">
    <template v-if="auth.isAuthenticated">
      <OpsLive />
      <a href="#main" class="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:z-[70] ms-btn-primary">Skip to content</a>

      <!-- Desktop rail -->
      <aside class="hidden md:block fixed inset-y-0 left-0 z-40">
        <NavRail :collapsed="collapsed" @toggle="toggleRail" />
      </aside>

      <!-- Phone drawer -->
      <Transition name="drawer">
        <div v-if="drawerOpen" class="md:hidden fixed inset-0 z-50">
          <div class="scrim absolute inset-0 bg-black/60" @click="drawerOpen = false" />
          <aside class="panel absolute inset-y-0 left-0 shadow-2xl">
            <NavRail drawer @close="drawerOpen = false" />
          </aside>
        </div>
      </Transition>

      <div :class="collapsed ? 'md:pl-14' : 'md:pl-[232px]'">
        <header class="sticky top-0 z-30 h-12 flex items-center gap-2 sm:gap-3 px-3 sm:px-5 bg-ms-bg/90 backdrop-blur border-b border-ms-border">
          <button class="md:hidden ms-btn-ghost -ml-1" aria-label="Open menu" @click="drawerOpen = true">
            <Icon name="menu" />
          </button>
          <router-link :to="{ name: 'dashboard' }" class="md:hidden" aria-label="MeshSat Hub overview"><BrandLockup /></router-link>

          <div class="hidden sm:flex flex-1 min-w-0"><JumpSearch /></div>
          <div class="flex-1 sm:hidden" />

          <AttentionPill />

          <a href="https://status.meshsat.net" target="_blank" rel="noopener"
            class="hidden lg:inline-flex items-center gap-1.5 h-8 px-2 rounded-md text-[13px] hover:bg-ms-well"
            :class="hubState === 'ok' || hubState === 'unknown' ? 'text-ms-muted' : hubState === 'down' ? 'text-ms-error' : 'text-ms-warning'"
            :title="hubState === 'ok' ? 'Hub is ready. Opens the public status page.' : 'Hub is not fully ready. Opens the public status page.'">
            <span class="w-1.5 h-1.5 rounded-full" :class="hubState === 'ok' ? 'bg-ms-success' : hubState === 'down' ? 'bg-ms-error' : hubState === 'degraded' ? 'bg-ms-warning' : 'bg-ms-muted'" />
            {{ hubState === 'ok' || hubState === 'unknown' ? 'Hub' : hubState === 'down' ? 'Hub down' : 'Hub degraded' }}
          </a>

          <span class="hidden lg:inline font-mono text-xs text-ms-muted ms-num" title="Coordinated Universal Time">{{ utc }}</span>

          <button class="ms-btn-ghost" :aria-label="theme.dark ? 'Switch to light theme' : 'Switch to dark theme'" :title="theme.dark ? 'Light theme' : 'Dark theme'" @click="theme.toggle()">
            <Icon :name="theme.dark ? 'sun' : 'moon'" :size="17" />
          </button>

          <div ref="accountRoot" class="relative">
            <button class="w-8 h-8 rounded-full bg-ms-well border border-ms-border text-[13px] font-semibold text-ms-text flex items-center justify-center hover:border-ms-border-light"
              :aria-expanded="accountOpen" aria-label="Account" @click="accountOpen = !accountOpen">{{ initial }}</button>
            <div v-if="accountOpen" class="absolute right-0 mt-2 w-64 ms-panel shadow-2xl shadow-black/40 z-50 py-1.5">
              <div class="px-4 py-2.5 border-b border-ms-border">
                <div class="text-sm font-medium truncate">{{ auth.user?.name || auth.user?.id || 'Signed in' }}</div>
                <div v-if="auth.user?.email" class="text-xs text-ms-muted truncate">{{ auth.user.email }}</div>
                <div class="mt-1.5 text-xs text-ms-muted">
                  {{ roleLabel }}<template v-if="auth.user?.tenant_id">, account <span class="ms-id text-ms-text2">{{ auth.user.tenant_id }}</span></template>
                </div>
              </div>
              <router-link to="/settings" class="flex items-center gap-2.5 px-4 h-9 text-[13px] text-ms-text2 hover:bg-ms-well"><Icon name="settings" :size="16" class="text-ms-muted" />Settings</router-link>
              <router-link to="/help" class="flex items-center gap-2.5 px-4 h-9 text-[13px] text-ms-text2 hover:bg-ms-well"><Icon name="help" :size="16" class="text-ms-muted" />Help</router-link>
              <button class="w-full flex items-center gap-2.5 px-4 h-9 text-[13px] text-ms-text2 hover:bg-ms-well hover:text-ms-error" @click="logout">
                <Icon name="logout" :size="16" class="text-ms-muted" />Sign out
              </button>
            </div>
          </div>
        </header>

        <ViewAsBanner />
        <main id="main" tabindex="-1" class="focus:outline-none" :class="bleed ? (auth.viewAs ? 'h-[calc(100dvh-3rem-2.75rem)]' : 'h-[calc(100dvh-3rem)]') : ''">
          <RouterView />
        </main>
      </div>
    </template>

    <main v-else>
      <RouterView />
    </main>

    <ToastContainer />
  </div>
</template>

<style>
body { margin: 0; }

.drawer-enter-active .panel { transition: transform 0.22s ease-out; }
.drawer-leave-active .panel { transition: transform 0.18s ease-in; }
.drawer-enter-from .panel, .drawer-leave-to .panel { transform: translateX(-100%); }
.drawer-enter-active .scrim, .drawer-leave-active .scrim { transition: opacity 0.2s; }
.drawer-enter-from .scrim, .drawer-leave-to .scrim { opacity: 0; }
</style>
