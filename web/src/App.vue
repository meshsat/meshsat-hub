<script setup>
import { ref, watch } from 'vue'
import { RouterLink, RouterView, useRouter } from 'vue-router'
import { useAuthStore } from './stores/auth'
import { useThemeStore } from './stores/theme'
import ToastContainer from './components/ToastContainer.vue'

const auth = useAuthStore()
const theme = useThemeStore()
const router = useRouter()
const navOpen = ref(false)
const userMenuOpen = ref(false)
const searchQuery = ref('')
const searchOpen = ref(false)
const mobileSearchQuery = ref('')

// Close mobile nav on route change
watch(() => router.currentRoute.value.path, () => { navOpen.value = false })

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
    { to: '/deadman', label: 'Dead Man' },
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
  <div class="min-h-screen bg-gray-900 text-gray-100 dark:bg-gray-900 dark:text-gray-100" :class="{ 'bg-gray-50 text-gray-900': !theme.dark }">
    <template v-if="auth.isAuthenticated">
      <header class="bg-gray-800 border-b border-gray-700 px-4 py-3 flex items-center justify-between">
        <div class="flex items-center gap-3">
          <span class="text-xl font-bold text-teal-400">MeshSat Hub</span>
        </div>
        <nav class="hidden md:flex items-center gap-1 text-sm">
          <template v-for="group in navGroups" :key="group.label">
            <span class="text-gray-600 text-xs px-1">|</span>
            <RouterLink v-for="item in group.items" :key="item.to" :to="item.to"
              class="px-2 py-1 rounded hover:text-teal-400 hover:bg-gray-700/50 transition-colors" active-class="text-teal-400 bg-gray-700/50">
              {{ item.label }}
            </RouterLink>
          </template>
          <span class="text-gray-600 text-xs px-1">|</span>
          <RouterLink v-if="auth.isOwner" to="/users" class="px-2 py-1 rounded hover:text-teal-400 hover:bg-gray-700/50 transition-colors" active-class="text-teal-400 bg-gray-700/50">Users</RouterLink>
          <RouterLink v-if="auth.isOwner" to="/api-keys" class="px-2 py-1 rounded hover:text-teal-400 hover:bg-gray-700/50 transition-colors" active-class="text-teal-400 bg-gray-700/50">API Keys</RouterLink>
          <RouterLink v-if="auth.isOwner" to="/audit" class="px-2 py-1 rounded hover:text-teal-400 hover:bg-gray-700/50 transition-colors" active-class="text-teal-400 bg-gray-700/50">Audit</RouterLink>
          <RouterLink to="/settings" class="px-2 py-1 rounded hover:text-teal-400 hover:bg-gray-700/50 transition-colors" active-class="text-teal-400 bg-gray-700/50">Settings</RouterLink>

          <!-- Search -->
          <div class="relative ml-2">
            <input v-if="searchOpen" v-model="searchQuery" @keydown.enter="handleSearch" @keydown.escape="searchOpen = false"
              placeholder="Search devices..." autofocus
              class="bg-gray-700 border border-gray-600 rounded px-2 py-1 text-xs w-40 focus:outline-none focus:border-teal-500">
            <button v-else @click="searchOpen = true" class="text-gray-400 hover:text-gray-200 px-1" title="Search (/)">
              <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z"/></svg>
            </button>
          </div>

          <!-- Theme toggle -->
          <button @click="theme.toggle()" class="text-gray-400 hover:text-gray-200 px-1 ml-1" :title="theme.dark ? 'Light mode' : 'Dark mode'">
            <svg v-if="theme.dark" class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 3v1m0 16v1m9-9h-1M4 12H3m15.364 6.364l-.707-.707M6.343 6.343l-.707-.707m12.728 0l-.707.707M6.343 17.657l-.707.707M16 12a4 4 0 11-8 0 4 4 0 018 0z"/></svg>
            <svg v-else class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M20.354 15.354A9 9 0 018.646 3.646 9.003 9.003 0 0012 21a9.003 9.003 0 008.354-5.646z"/></svg>
          </button>

          <!-- User menu -->
          <div class="relative ml-2">
            <button @click="userMenuOpen = !userMenuOpen"
              class="w-8 h-8 rounded-full bg-teal-700 text-white text-sm font-bold flex items-center justify-center hover:bg-teal-600 transition-colors">
              {{ userInitial() }}
            </button>
            <div v-if="userMenuOpen" @click="userMenuOpen = false"
              class="absolute right-0 mt-2 w-56 bg-gray-800 border border-gray-700 rounded-lg shadow-xl z-50 py-2">
              <div class="px-4 py-2 border-b border-gray-700">
                <div class="text-sm font-medium">{{ auth.user?.name || auth.user?.id || 'User' }}</div>
                <div v-if="auth.user?.email" class="text-xs text-gray-400">{{ auth.user.email }}</div>
                <div class="flex items-center gap-2 mt-1">
                  <span class="text-xs px-1.5 py-0.5 rounded font-medium"
                    :class="auth.role === 'owner' ? 'bg-purple-900/50 text-purple-300' : auth.role === 'operator' ? 'bg-teal-900/50 text-teal-300' : 'bg-gray-700 text-gray-300'">
                    {{ auth.role }}
                  </span>
                  <span v-if="auth.user?.tenant_id" class="text-xs text-gray-500">{{ auth.user.tenant_id }}</span>
                </div>
              </div>
              <button @click="logout" class="w-full text-left px-4 py-2 text-sm text-gray-400 hover:text-red-400 hover:bg-gray-700/50">
                Logout
              </button>
            </div>
          </div>
        </nav>
        <button @click="navOpen = !navOpen" class="md:hidden text-gray-400">&#9776;</button>
      </header>

      <!-- Mobile nav overlay -->
      <Transition name="mobile-nav">
        <div v-if="navOpen" class="md:hidden fixed inset-0 z-40" @click.self="navOpen = false">
          <div class="absolute inset-0 bg-black/50" />
          <nav class="absolute left-0 top-0 bottom-0 w-72 bg-gray-800 border-r border-gray-700 overflow-y-auto flex flex-col">
            <!-- Mobile header -->
            <div class="px-4 py-3 border-b border-gray-700 flex items-center justify-between">
              <span class="text-lg font-bold text-teal-400">MeshSat Hub</span>
              <button @click="navOpen = false" class="text-gray-400 hover:text-gray-200 text-xl">&times;</button>
            </div>

            <!-- Mobile search -->
            <div class="px-4 py-3 border-b border-gray-700">
              <div class="relative">
                <input v-model="mobileSearchQuery" @keydown.enter="handleMobileSearch"
                  placeholder="Search devices..."
                  class="w-full bg-gray-700 border border-gray-600 rounded px-3 py-2 text-sm focus:outline-none focus:border-teal-500">
                <svg class="w-4 h-4 text-gray-500 absolute right-3 top-2.5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M21 21l-6-6m2-5a7 7 0 11-14 0 7 7 0 0114 0z"/>
                </svg>
              </div>
            </div>

            <!-- Nav groups -->
            <div class="flex-1 px-2 py-2 space-y-1">
              <template v-for="group in navGroups" :key="group.label">
                <div class="text-xs text-gray-500 uppercase tracking-wider px-3 pt-3 pb-1">{{ group.label }}</div>
                <RouterLink v-for="item in group.items" :key="item.to" :to="item.to"
                  class="block px-3 py-2 rounded hover:text-teal-400 hover:bg-gray-700/50 transition-colors"
                  active-class="text-teal-400 bg-gray-700/50">
                  {{ item.label }}
                </RouterLink>
              </template>
              <template v-if="auth.isOwner">
                <div class="text-xs text-gray-500 uppercase tracking-wider px-3 pt-3 pb-1">Admin</div>
                <RouterLink to="/users" class="block px-3 py-2 rounded hover:text-teal-400 hover:bg-gray-700/50 transition-colors" active-class="text-teal-400 bg-gray-700/50">Users</RouterLink>
                <RouterLink to="/api-keys" class="block px-3 py-2 rounded hover:text-teal-400 hover:bg-gray-700/50 transition-colors" active-class="text-teal-400 bg-gray-700/50">API Keys</RouterLink>
                <RouterLink to="/audit" class="block px-3 py-2 rounded hover:text-teal-400 hover:bg-gray-700/50 transition-colors" active-class="text-teal-400 bg-gray-700/50">Audit</RouterLink>
              </template>
            </div>

            <!-- Mobile footer: theme + user -->
            <div class="border-t border-gray-700 px-4 py-3">
              <div class="flex items-center justify-between mb-3">
                <span class="text-sm text-gray-400">Theme</span>
                <button @click="theme.toggle()" class="flex items-center gap-2 text-sm text-gray-300 hover:text-teal-400">
                  <svg v-if="theme.dark" class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 3v1m0 16v1m9-9h-1M4 12H3m15.364 6.364l-.707-.707M6.343 6.343l-.707-.707m12.728 0l-.707.707M6.343 17.657l-.707.707M16 12a4 4 0 11-8 0 4 4 0 018 0z"/></svg>
                  <svg v-else class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M20.354 15.354A9 9 0 018.646 3.646 9.003 9.003 0 0012 21a9.003 9.003 0 008.354-5.646z"/></svg>
                  {{ theme.dark ? 'Light' : 'Dark' }}
                </button>
              </div>
              <div v-if="auth.user" class="flex items-center gap-2 mb-2">
                <div class="w-8 h-8 rounded-full bg-teal-700 text-white text-sm font-bold flex items-center justify-center shrink-0">
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

    <main :class="auth.isAuthenticated ? 'p-4' : ''">
      <RouterView />
    </main>

    <ToastContainer />
  </div>
</template>

<style>
@import 'leaflet/dist/leaflet.css';
body { margin: 0; font-family: system-ui, -apple-system, sans-serif; }

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
