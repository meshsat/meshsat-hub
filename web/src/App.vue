<script setup>
import { ref, computed, watch } from 'vue'
import { RouterLink, RouterView, useRouter } from 'vue-router'
import { useAuthStore } from './stores/auth'
import { useThemeStore } from './stores/theme'
import ToastContainer from './components/ToastContainer.vue'
import StatusBar from './components/StatusBar.vue'

const auth = useAuthStore()
const theme = useThemeStore()
const router = useRouter()
const navOpen = ref(false)
const userMenuOpen = ref(false)
const searchQuery = ref('')
const searchOpen = ref(false)
const mobileSearchQuery = ref('')
const openDropdown = ref(null)
let dropdownTimer = null

// Close mobile nav on route change
watch(() => router.currentRoute.value.path, () => { navOpen.value = false; openDropdown.value = null })

function isGroupActive(group) {
  const path = router.currentRoute.value.path
  return group.items.some(item => item.to === path || (item.to !== '/' && path.startsWith(item.to)))
}

function showDropdown(label) {
  clearTimeout(dropdownTimer)
  openDropdown.value = label
}

function hideDropdown() {
  dropdownTimer = setTimeout(() => { openDropdown.value = null }, 150)
}

function cancelHide() {
  clearTimeout(dropdownTimer)
}

function handleSearch() {
  if (!searchQuery.value.trim()) return
  router.push({ path: '/devices', query: { q: searchQuery.value.trim() } })
  searchQuery.value = ''
  searchOpen.value = false
}

function handleMobileSearch() {
  if (!mobileSearchQuery.value.trim()) return
  router.push({ path: '/devices', query: { q: mobileSearchQuery.value.trim() } })
  mobileSearchQuery.value = ''
  navOpen.value = false
}

function logout() {
  auth.logout()
  router.push({ name: 'login' })
}

function userInitial() {
  if (auth.user?.name) return auth.user.name[0].toUpperCase()
  if (auth.user?.email) return auth.user.email[0].toUpperCase()
  return 'U'
}

const navGroups = [
  { label: 'Operations', items: [
    { to: '/', label: 'Dashboard' },
    { to: '/map', label: 'Map' },
    { to: '/devices', label: 'Devices' },
    { to: '/messages', label: 'Messages' },
  ]},
  { label: 'Safety', items: [
    { to: '/escalation', label: 'Escalation' },
    { to: '/deadman', label: 'Deadman' },
    { to: '/geofences', label: 'Geofences' },
    { to: '/notifications', label: 'Notifications' },
  ]},
  { label: 'Channels', items: [
    { to: '/email', label: 'Email' },
    { to: '/routing', label: 'Routing' },
    { to: '/webhooks', label: 'Webhooks' },
  ]},
  { label: 'Infrastructure', items: [
    { to: '/cluster', label: 'Cluster' },
    { to: '/network', label: 'Network' },
    { to: '/topology', label: 'Topology' },
    { to: '/ota', label: 'OTA' },
    { to: '/backup', label: 'Backup' },
    { to: '/settings', label: 'Settings' },
  ]},
]
</script>

<template>
  <div class="min-h-screen bg-tactical-bg text-gray-100 relative">
    <!-- Fullscreen background logo (matches Bridge) -->
    <div class="fixed inset-0 z-0 flex items-center justify-center pointer-events-none">
      <img src="/logo-bg.png" alt="" class="w-[150vmin] h-[150vmin] object-contain opacity-[0.04]" />
    </div>

    <template v-if="auth.isAuthenticated">
      <header class="sticky top-0 z-50 bg-tactical-surface/95 backdrop-blur border-b border-tactical-border">
        <div class="flex items-center h-12 px-3 lg:px-5 gap-3">
          <!-- Brand -->
          <span class="font-display font-semibold text-sm text-gray-200 tracking-wide shrink-0">MeshSat Hub</span>
          <!-- Nav dropdowns (center, flex-1) -->
          <nav class="hidden md:flex flex-1 items-center mx-2 lg:mx-6 gap-1">
            <template v-for="group in navGroups" :key="group.label">
              <div class="relative" @mouseenter="showDropdown(group.label)" @mouseleave="hideDropdown">
                <button class="px-3 py-1.5 rounded text-xs font-medium whitespace-nowrap transition-colors flex items-center gap-1"
                  :class="isGroupActive(group)
                    ? 'bg-tactical-iridium/10 text-tactical-iridium'
                    : openDropdown === group.label
                      ? 'text-gray-300 bg-white/5'
                      : 'text-gray-500 hover:text-gray-300 hover:bg-white/5'">
                  {{ group.label }}
                  <svg class="w-3 h-3 opacity-50" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7"/></svg>
                </button>
                <div v-if="openDropdown === group.label"
                  class="absolute top-full left-0 mt-1 py-1 bg-tactical-surface border border-tactical-border rounded-lg shadow-xl z-50 min-w-[160px]"
                  @mouseenter="cancelHide" @mouseleave="hideDropdown">
                  <RouterLink v-for="item in group.items" :key="item.to" :to="item.to"
                    class="block px-4 py-2 text-xs font-medium transition-colors text-gray-400 hover:text-gray-200 hover:bg-white/5"
                    active-class="!text-tactical-iridium !bg-tactical-iridium/10"
                    @click="openDropdown = null">
                    {{ item.label }}
                  </RouterLink>
                </div>
              </div>
            </template>
            <!-- Admin group (owner-only) -->
            <div v-if="auth.isOwner" class="relative" @mouseenter="showDropdown('Admin')" @mouseleave="hideDropdown">
              <button class="px-3 py-1.5 rounded text-xs font-medium whitespace-nowrap transition-colors flex items-center gap-1"
                :class="['/users', '/api-keys', '/audit'].includes(router.currentRoute.value.path)
                  ? 'bg-tactical-iridium/10 text-tactical-iridium'
                  : openDropdown === 'Admin'
                    ? 'text-gray-300 bg-white/5'
                    : 'text-gray-500 hover:text-gray-300 hover:bg-white/5'">
                Admin
                <svg class="w-3 h-3 opacity-50" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7"/></svg>
              </button>
              <div v-if="openDropdown === 'Admin'"
                class="absolute top-full left-0 mt-1 py-1 bg-tactical-surface border border-tactical-border rounded-lg shadow-xl z-50 min-w-[160px]"
                @mouseenter="cancelHide" @mouseleave="hideDropdown">
                <RouterLink to="/users" class="block px-4 py-2 text-xs font-medium transition-colors text-gray-400 hover:text-gray-200 hover:bg-white/5"
                  active-class="!text-tactical-iridium !bg-tactical-iridium/10" @click="openDropdown = null">Users</RouterLink>
                <RouterLink to="/api-keys" class="block px-4 py-2 text-xs font-medium transition-colors text-gray-400 hover:text-gray-200 hover:bg-white/5"
                  active-class="!text-tactical-iridium !bg-tactical-iridium/10" @click="openDropdown = null">API Keys</RouterLink>
                <RouterLink to="/audit" class="block px-4 py-2 text-xs font-medium transition-colors text-gray-400 hover:text-gray-200 hover:bg-white/5"
                  active-class="!text-tactical-iridium !bg-tactical-iridium/10" @click="openDropdown = null">Audit</RouterLink>
              </div>
            </div>
          </nav>
          <!-- Right: status bar + controls -->
          <div class="hidden md:flex items-center gap-3 shrink-0">
            <StatusBar />
            <span class="hidden md:block w-px h-4 bg-gray-700/50" />
            <!-- Search -->
            <div class="relative">
              <input v-if="searchOpen" v-model="searchQuery" @keydown.enter="handleSearch" @keydown.escape="searchOpen = false"
                placeholder="Search devices..." autofocus
                class="bg-gray-800 border border-gray-700 rounded px-2 py-1 text-xs w-40 focus:outline-none focus:border-teal-500 text-gray-200">
              <button v-else @click="searchOpen = true" class="text-gray-400 hover:text-gray-200 px-1" title="Search (/)">
                <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z"/></svg>
              </button>
            </div>
            <span class="hidden md:block w-px h-4 bg-gray-700/50" />
            <!-- Theme toggle -->
            <button @click="theme.toggle()" class="text-gray-400 hover:text-gray-200 px-1" :title="theme.dark ? 'Light mode' : 'Dark mode'">
              <svg v-if="theme.dark" class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 3v1m0 16v1m9-9h-1M4 12H3m15.364 6.364l-.707-.707M6.343 6.343l-.707-.707m12.728 0l-.707.707M6.343 17.657l-.707.707M16 12a4 4 0 11-8 0 4 4 0 018 0z"/></svg>
              <svg v-else class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M20.354 15.354A9 9 0 018.646 3.646 9.003 9.003 0 0012 21a9.003 9.003 0 008.354-5.646z"/></svg>
            </button>
            <!-- User menu -->
            <div class="relative">
              <button @click="userMenuOpen = !userMenuOpen"
                class="w-8 h-8 rounded-full bg-tactical-iridium/30 text-tactical-iridium text-sm font-bold flex items-center justify-center hover:bg-tactical-iridium/40 transition-colors">
                {{ userInitial() }}
              </button>
              <div v-if="userMenuOpen" @click="userMenuOpen = false"
                class="absolute right-0 mt-2 w-56 bg-tactical-surface border border-tactical-border rounded-lg shadow-xl z-50 py-2">
                <div class="px-4 py-2 border-b border-tactical-border">
                  <div class="text-sm font-medium">{{ auth.user?.name || auth.user?.id || 'User' }}</div>
                  <div v-if="auth.user?.email" class="text-xs text-gray-400 font-mono">{{ auth.user.email }}</div>
                  <div class="flex items-center gap-2 mt-1">
                    <span class="text-xs px-1.5 py-0.5 rounded font-medium"
                      :class="auth.role === 'owner' ? 'bg-purple-900/50 text-purple-300' : auth.role === 'operator' ? 'bg-teal-900/50 text-teal-300' : 'bg-gray-700 text-gray-300'">
                      {{ auth.role }}
                    </span>
                    <span v-if="auth.user?.tenant_id" class="text-xs text-gray-500 font-mono">{{ auth.user.tenant_id }}</span>
                  </div>
                </div>
                <button @click="logout" class="w-full text-left px-4 py-2 text-sm text-gray-400 hover:text-red-400 hover:bg-gray-800/50">
                  Logout
                </button>
              </div>
            </div>
          </div>
          <!-- Mobile hamburger -->
          <button @click="navOpen = !navOpen" class="md:hidden ml-auto text-gray-400">&#9776;</button>
        </div>
      </header>

      <!-- Mobile nav overlay -->
      <Transition name="mobile-nav">
        <div v-if="navOpen" class="md:hidden fixed inset-0 z-40" @click.self="navOpen = false">
          <div class="absolute inset-0 bg-black/50" />
          <nav class="absolute left-0 top-0 bottom-0 w-72 bg-tactical-surface border-r border-tactical-border overflow-y-auto tactical-scroll flex flex-col">
            <!-- Mobile header -->
            <div class="px-4 py-3 border-b border-tactical-border flex items-center justify-between">
              <span class="text-lg font-display font-bold text-gray-200 tracking-wide">MeshSat Hub</span>
              <button @click="navOpen = false" class="text-gray-400 hover:text-gray-200 text-xl">&times;</button>
            </div>

            <!-- Mobile search -->
            <div class="px-4 py-3 border-b border-tactical-border">
              <div class="relative">
                <input v-model="mobileSearchQuery" @keydown.enter="handleMobileSearch"
                  placeholder="Search devices..."
                  class="w-full bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm text-gray-200 focus:outline-none focus:border-teal-500">
                <svg class="w-4 h-4 text-gray-500 absolute right-3 top-2.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z"/>
                </svg>
              </div>
            </div>

            <!-- Nav groups -->
            <div class="flex-1 px-2 py-2 space-y-1">
              <template v-for="group in navGroups" :key="group.label">
                <div class="text-xs text-gray-500 uppercase tracking-wider px-3 pt-3 pb-1 font-display">{{ group.label }}</div>
                <RouterLink v-for="item in group.items" :key="item.to" :to="item.to"
                  class="block px-3 py-2 rounded text-sm transition-colors text-gray-400 hover:text-gray-200 hover:bg-white/5"
                  active-class="!bg-tactical-iridium/10 !text-tactical-iridium">
                  {{ item.label }}
                </RouterLink>
              </template>
              <template v-if="auth.isOwner">
                <div class="text-xs text-gray-500 uppercase tracking-wider px-3 pt-3 pb-1 font-display">Admin</div>
                <RouterLink to="/users" class="block px-3 py-2 rounded text-sm transition-colors text-gray-400 hover:text-gray-200 hover:bg-white/5" active-class="!bg-tactical-iridium/10 !text-tactical-iridium">Users</RouterLink>
                <RouterLink to="/api-keys" class="block px-3 py-2 rounded text-sm transition-colors text-gray-400 hover:text-gray-200 hover:bg-white/5" active-class="!bg-tactical-iridium/10 !text-tactical-iridium">API Keys</RouterLink>
                <RouterLink to="/audit" class="block px-3 py-2 rounded text-sm transition-colors text-gray-400 hover:text-gray-200 hover:bg-white/5" active-class="!bg-tactical-iridium/10 !text-tactical-iridium">Audit</RouterLink>
              </template>
            </div>

            <!-- Mobile footer: theme + user -->
            <div class="border-t border-tactical-border px-4 py-3">
              <div class="flex items-center justify-between mb-3">
                <span class="text-sm text-gray-400">Theme</span>
                <button @click="theme.toggle()" class="flex items-center gap-2 text-sm text-gray-300 hover:text-tactical-iridium">
                  <svg v-if="theme.dark" class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 3v1m0 16v1m9-9h-1M4 12H3m15.364 6.364l-.707-.707M6.343 6.343l-.707-.707m12.728 0l-.707.707M6.343 17.657l-.707.707M16 12a4 4 0 11-8 0 4 4 0 018 0z"/></svg>
                  <svg v-else class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M20.354 15.354A9 9 0 018.646 3.646 9.003 9.003 0 0012 21a9.003 9.003 0 008.354-5.646z"/></svg>
                  {{ theme.dark ? 'Light' : 'Dark' }}
                </button>
              </div>
              <div v-if="auth.user" class="flex items-center gap-2 mb-2">
                <div class="w-8 h-8 rounded-full bg-tactical-iridium/30 text-tactical-iridium text-sm font-bold flex items-center justify-center shrink-0">
                  {{ userInitial() }}
                </div>
                <div class="min-w-0">
                  <div class="text-sm truncate">{{ auth.user?.name || auth.user?.id || 'User' }}</div>
                  <span class="text-xs px-1.5 py-0.5 rounded font-medium"
                    :class="auth.role === 'owner' ? 'bg-purple-900/50 text-purple-300' : auth.role === 'operator' ? 'bg-teal-900/50 text-teal-300' : 'bg-gray-700 text-gray-300'">
                    {{ auth.role }}
                  </span>
                </div>
              </div>
              <button @click="logout()" class="w-full text-left text-sm text-gray-400 hover:text-red-400 py-1">Logout</button>
            </div>
          </nav>
        </div>
      </Transition>
    </template>

    <main :class="auth.isAuthenticated ? 'p-3 sm:p-4 lg:p-5' : ''">
      <RouterView />
    </main>

    <ToastContainer />
  </div>
</template>

<style>
@import 'leaflet/dist/leaflet.css';
body { margin: 0; }

/* Mobile nav slide */
.mobile-nav-enter-active nav { transition: transform 0.25s ease-out; }
.mobile-nav-leave-active nav { transition: transform 0.2s ease-in; }
.mobile-nav-enter-from nav { transform: translateX(-100%); }
.mobile-nav-leave-to nav { transform: translateX(-100%); }
.mobile-nav-enter-active .bg-black\/50 { transition: opacity 0.25s; }
.mobile-nav-leave-active .bg-black\/50 { transition: opacity 0.2s; }
.mobile-nav-enter-from .bg-black\/50 { opacity: 0; }
.mobile-nav-leave-to .bg-black\/50 { opacity: 0; }
</style>
