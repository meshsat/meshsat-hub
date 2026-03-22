<script setup>
import { ref, onMounted, onUnmounted } from 'vue'
import { health, devices, credits, messages } from '../api/client'

const clusterStatus = ref('--')
const clusterColor = ref('text-gray-500')
const deviceCount = ref({ online: 0, total: 0 })
const creditBalance = ref(null)
const msgToday = ref({ mo: 0, mt: 0 })
const utcTime = ref('')

let pollTimer = null
let clockTimer = null

function updateClock() {
  const now = new Date()
  utcTime.value = now.toISOString().slice(11, 19) + 'Z'
}

async function poll() {
  try {
    const r = await health.readyz()
    if (r.status === 'ok') {
      clusterStatus.value = 'OK'
      clusterColor.value = 'text-emerald-400'
    } else {
      clusterStatus.value = 'DEGRADED'
      clusterColor.value = 'text-amber-400'
    }
  } catch {
    clusterStatus.value = 'DOWN'
    clusterColor.value = 'text-red-400'
  }

  try {
    const devs = await devices.list()
    if (Array.isArray(devs)) {
      deviceCount.value.total = devs.length
      deviceCount.value.online = devs.filter(d => d.status === 'online').length
    }
  } catch { /* ignore */ }

  try {
    const c = await credits.get()
    if (c && c.balance !== undefined) creditBalance.value = c.balance
  } catch { /* ignore */ }

  try {
    const msgs = await messages.list('', 1000)
    if (Array.isArray(msgs)) {
      const today = new Date().toISOString().slice(0, 10)
      const todayMsgs = msgs.filter(m => m.timestamp && m.timestamp.startsWith(today))
      msgToday.value.mo = todayMsgs.filter(m => m.direction === 'MO' || m.direction === 'mo').length
      msgToday.value.mt = todayMsgs.filter(m => m.direction === 'MT' || m.direction === 'mt').length
    }
  } catch { /* ignore */ }
}

onMounted(() => {
  updateClock()
  poll()
  clockTimer = setInterval(updateClock, 1000)
  pollTimer = setInterval(poll, 30000)
})

onUnmounted(() => {
  clearInterval(clockTimer)
  clearInterval(pollTimer)
})
</script>

<template>
  <div class="flex items-center gap-1.5 text-[9px]">
    <!-- Cluster health -->
    <span class="inline-flex items-center gap-1 font-medium" :class="clusterColor === 'text-emerald-400' ? 'text-emerald-400/70' : clusterColor">
      <span class="w-1.5 h-1.5 rounded-full" :class="clusterColor.replace('text-', 'bg-')"></span>
      HUB
    </span>

    <span class="hidden md:block w-px h-4 bg-gray-700/50" />

    <!-- Device count -->
    <span class="inline-flex items-center gap-1 text-gray-300 font-medium">
      <svg class="w-3 h-3" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 3v2m6-2v2M9 19v2m6-2v2M5 9H3m2 6H3m18-6h-2m2 6h-2M7 19h10a2 2 0 002-2V7a2 2 0 00-2-2H7a2 2 0 00-2 2v10a2 2 0 002 2z"/></svg>
      <span class="font-mono" :class="deviceCount.online > 0 ? 'text-emerald-400' : ''">{{ deviceCount.online }}</span>/<span class="font-mono">{{ deviceCount.total }}</span>
    </span>

    <span class="hidden md:block w-px h-4 bg-gray-700/50" />

    <!-- Messages today -->
    <span class="inline-flex items-center gap-1 text-gray-300 font-medium">
      <svg class="w-3 h-3" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 8l7.89 5.26a2 2 0 002.22 0L21 8M5 19h14a2 2 0 002-2V7a2 2 0 00-2-2H5a2 2 0 00-2 2v10a2 2 0 002 2z"/></svg>
      <span class="font-mono text-emerald-400">{{ msgToday.mo }}</span><span class="text-gray-500">/</span><span class="font-mono text-sky-400">{{ msgToday.mt }}</span>
    </span>

    <!-- Credits -->
    <template v-if="creditBalance !== null">
      <span class="hidden md:block w-px h-4 bg-gray-700/50" />
      <span class="inline-flex items-center gap-1 text-gray-300 font-medium">
        <svg class="w-3 h-3" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 8c-1.657 0-3 .895-3 2s1.343 2 3 2 3 .895 3 2-1.343 2-3 2m0-8c1.11 0 2.08.402 2.599 1M12 8V7m0 1v8m0 0v1m0-1c-1.11 0-2.08-.402-2.599-1M21 12a9 9 0 11-18 0 9 9 0 0118 0z"/></svg>
        <span class="font-mono">{{ creditBalance }}</span>
      </span>
    </template>

    <span class="hidden md:block w-px h-4 bg-gray-700/50" />

    <!-- UTC clock -->
    <span class="font-mono text-[10px] text-gray-500 tabular-nums">{{ utcTime }}</span>
  </div>
</template>
