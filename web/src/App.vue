<script setup>
import { ref } from 'vue'
import { RouterLink, RouterView, useRouter } from 'vue-router'
import { useAuthStore } from './stores/auth'

const auth = useAuthStore()
const router = useRouter()
const navOpen = ref(false)
const userMenuOpen = ref(false)

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
    { to: '/device-config', label: 'Config' },
    { to: '/device-keys', label: 'Keys' },
    { to: '/messages', label: 'Messages' },
  ]},
  { label: 'Safety', items: [
    { to: '/escalation', label: 'Escalation' },
    { to: '/deadman', label: 'Dead Man' },
    { to: '/notifications', label: 'Notifications' },
  ]},
  { label: 'Infrastructure', items: [
    { to: '/cluster', label: 'Cluster' },
    { to: '/network', label: 'Network' },
    { to: '/routing', label: 'Routing' },
    { to: '/webhooks', label: 'Webhooks' },
    { to: '/ota', label: 'OTA' },
  ]},
]
</script>

<template>
  <div class="min-h-screen bg-gray-900 text-gray-100">
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

      <!-- Mobile nav -->
      <nav v-if="navOpen" class="md:hidden bg-gray-800 border-b border-gray-700 px-4 py-2 flex flex-col gap-1 text-sm">
        <template v-for="group in navGroups" :key="group.label">
          <div class="text-xs text-gray-500 uppercase mt-2 mb-1">{{ group.label }}</div>
          <RouterLink v-for="item in group.items" :key="item.to" :to="item.to" @click="navOpen=false"
            class="hover:text-teal-400 py-1 pl-2">{{ item.label }}</RouterLink>
        </template>
        <div class="text-xs text-gray-500 uppercase mt-2 mb-1">Admin</div>
        <RouterLink v-if="auth.isOwner" to="/users" @click="navOpen=false" class="hover:text-teal-400 py-1 pl-2">Users</RouterLink>
        <RouterLink v-if="auth.isOwner" to="/api-keys" @click="navOpen=false" class="hover:text-teal-400 py-1 pl-2">API Keys</RouterLink>
        <RouterLink v-if="auth.isOwner" to="/audit" @click="navOpen=false" class="hover:text-teal-400 py-1 pl-2">Audit</RouterLink>
        <RouterLink to="/settings" @click="navOpen=false" class="hover:text-teal-400 py-1 pl-2">Settings</RouterLink>
        <div v-if="auth.user" class="border-t border-gray-700 pt-2 mt-2">
          <div class="text-xs text-gray-400 mb-1">{{ auth.user?.name || auth.user?.id }} ({{ auth.role }})</div>
        </div>
        <button @click="logout(); navOpen=false" class="text-left text-gray-400 hover:text-red-400 py-1">Logout</button>
      </nav>
    </template>

    <main :class="auth.isAuthenticated ? 'p-4' : ''">
      <RouterView />
    </main>
  </div>
</template>

<style>
@import 'leaflet/dist/leaflet.css';
body { margin: 0; font-family: system-ui, -apple-system, sans-serif; }
</style>
