<script setup>
import { ref } from 'vue'
import { RouterLink, RouterView } from 'vue-router'

const navOpen = ref(false)
</script>

<template>
  <div class="min-h-screen bg-gray-900 text-gray-100">
    <header class="bg-gray-800 border-b border-gray-700 px-4 py-3 flex items-center justify-between">
      <div class="flex items-center gap-3">
        <span class="text-xl font-bold text-cyan-400">MeshSat Hub</span>
      </div>
      <nav class="hidden md:flex gap-4 text-sm">
        <RouterLink to="/" class="hover:text-cyan-400" active-class="text-cyan-400">Dashboard</RouterLink>
        <RouterLink to="/map" class="hover:text-cyan-400" active-class="text-cyan-400">Map</RouterLink>
        <RouterLink to="/devices" class="hover:text-cyan-400" active-class="text-cyan-400">Devices</RouterLink>
        <RouterLink to="/messages" class="hover:text-cyan-400" active-class="text-cyan-400">Messages</RouterLink>
        <RouterLink to="/settings" class="hover:text-cyan-400" active-class="text-cyan-400">Settings</RouterLink>
      </nav>
      <button @click="navOpen = !navOpen" class="md:hidden text-gray-400">&#9776;</button>
    </header>

    <nav v-if="navOpen" class="md:hidden bg-gray-800 border-b border-gray-700 px-4 py-2 flex flex-col gap-2 text-sm">
      <RouterLink to="/" @click="navOpen=false" class="hover:text-cyan-400">Dashboard</RouterLink>
      <RouterLink to="/map" @click="navOpen=false" class="hover:text-cyan-400">Map</RouterLink>
      <RouterLink to="/devices" @click="navOpen=false" class="hover:text-cyan-400">Devices</RouterLink>
      <RouterLink to="/messages" @click="navOpen=false" class="hover:text-cyan-400">Messages</RouterLink>
      <RouterLink to="/settings" @click="navOpen=false" class="hover:text-cyan-400">Settings</RouterLink>
    </nav>

    <main class="p-4">
      <RouterView />
    </main>
  </div>
</template>

<style>
@import 'leaflet/dist/leaflet.css';
body { margin: 0; font-family: system-ui, -apple-system, sans-serif; }
.bg-gray-900 { background-color: #111827; }
.bg-gray-800 { background-color: #1f2937; }
.text-gray-100 { color: #f3f4f6; }
.text-gray-400 { color: #9ca3af; }
.text-cyan-400 { color: #22d3ee; }
.border-gray-700 { border-color: #374151; }
</style>
