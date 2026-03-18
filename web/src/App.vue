<script setup>
import { ref } from 'vue'
import { RouterLink, RouterView, useRouter } from 'vue-router'
import { useAuthStore } from './stores/auth'

const auth = useAuthStore()
const router = useRouter()
const navOpen = ref(false)

function logout() {
  auth.logout()
  router.push({ name: 'login' })
}
</script>

<template>
  <div class="min-h-screen bg-gray-900 text-gray-100">
    <template v-if="auth.isAuthenticated">
      <header class="bg-gray-800 border-b border-gray-700 px-4 py-3 flex items-center justify-between">
        <div class="flex items-center gap-3">
          <span class="text-xl font-bold text-cyan-400">MeshSat Hub</span>
        </div>
        <nav class="hidden md:flex items-center gap-4 text-sm">
          <RouterLink to="/" class="hover:text-cyan-400" active-class="text-cyan-400">Dashboard</RouterLink>
          <RouterLink to="/map" class="hover:text-cyan-400" active-class="text-cyan-400">Map</RouterLink>
          <RouterLink to="/devices" class="hover:text-cyan-400" active-class="text-cyan-400">Devices</RouterLink>
          <RouterLink to="/messages" class="hover:text-cyan-400" active-class="text-cyan-400">Messages</RouterLink>
          <RouterLink to="/settings" class="hover:text-cyan-400" active-class="text-cyan-400">Settings</RouterLink>
          <RouterLink to="/audit" class="hover:text-cyan-400" active-class="text-cyan-400">Audit</RouterLink>
          <button @click="logout" class="text-gray-400 hover:text-red-400 ml-4">Logout</button>
        </nav>
        <button @click="navOpen = !navOpen" class="md:hidden text-gray-400">&#9776;</button>
      </header>

      <nav v-if="navOpen" class="md:hidden bg-gray-800 border-b border-gray-700 px-4 py-2 flex flex-col gap-2 text-sm">
        <RouterLink to="/" @click="navOpen=false" class="hover:text-cyan-400">Dashboard</RouterLink>
        <RouterLink to="/map" @click="navOpen=false" class="hover:text-cyan-400">Map</RouterLink>
        <RouterLink to="/devices" @click="navOpen=false" class="hover:text-cyan-400">Devices</RouterLink>
        <RouterLink to="/messages" @click="navOpen=false" class="hover:text-cyan-400">Messages</RouterLink>
        <RouterLink to="/settings" @click="navOpen=false" class="hover:text-cyan-400">Settings</RouterLink>
        <RouterLink to="/audit" @click="navOpen=false" class="hover:text-cyan-400">Audit</RouterLink>
        <button @click="logout(); navOpen=false" class="text-left text-gray-400 hover:text-red-400">Logout</button>
      </nav>
    </template>

    <main :class="auth.isAuthenticated ? 'p-4' : ''">
      <RouterView />
    </main>
  </div>
</template>
