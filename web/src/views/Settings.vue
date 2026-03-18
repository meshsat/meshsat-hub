<script setup>
import { ref, onMounted } from 'vue'
import { health, constellations, mptcp as mptcpApi, devices, ratelimit } from '../api/client'

const hubHealth = ref(null)
const readyz = ref(null)
const backends = ref([])
const mptcpStatus = ref(null)
const deviceCount = ref(0)
const loading = ref(true)

onMounted(async () => {
  const [h, r, c, m, d] = await Promise.all([
    health.check(),
    fetch('/readyz').then(r => r.json()).catch(() => null),
    constellations.list().catch(() => ({ backends: [] })),
    mptcpApi.status().catch(() => null),
    devices.list().catch(() => []),
  ])
  hubHealth.value = h
  readyz.value = r
  backends.value = c.backends || []
  mptcpStatus.value = m
  deviceCount.value = Array.isArray(d) ? d.length : 0
  loading.value = false
})

function checkColor(v) {
  return v === 'ok' || v === 'healthy' ? 'text-green-400' : 'text-red-400'
}
</script>

<template>
  <div>
    <h1 class="text-xl font-bold mb-4">Settings</h1>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>

    <template v-else>
      <!-- System Status -->
      <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4 mb-6">
        <div class="bg-gray-800 rounded-lg p-4">
          <div class="text-gray-400 text-sm mb-1">Health</div>
          <div class="text-lg font-bold" :class="checkColor(hubHealth?.status)">
            {{ hubHealth?.status || 'unknown' }}
          </div>
        </div>
        <div class="bg-gray-800 rounded-lg p-4">
          <div class="text-gray-400 text-sm mb-1">Readiness</div>
          <div class="text-lg font-bold" :class="checkColor(readyz?.status)">
            {{ readyz?.status || 'unknown' }}
          </div>
        </div>
        <div class="bg-gray-800 rounded-lg p-4">
          <div class="text-gray-400 text-sm mb-1">Devices</div>
          <div class="text-lg font-bold text-cyan-400">{{ deviceCount }}</div>
        </div>
        <div class="bg-gray-800 rounded-lg p-4">
          <div class="text-gray-400 text-sm mb-1">Constellations</div>
          <div class="text-lg font-bold text-cyan-400">{{ backends.join(', ') || 'none' }}</div>
        </div>
      </div>

      <!-- Dependency Checks -->
      <div v-if="readyz?.checks" class="bg-gray-800 rounded-lg p-5 mb-6">
        <h2 class="text-lg font-semibold mb-3">Dependency Checks</h2>
        <div class="space-y-2">
          <div v-for="(status, name) in readyz.checks" :key="name" class="flex items-center gap-3">
            <div class="w-2 h-2 rounded-full" :class="status === 'ok' ? 'bg-green-400' : 'bg-red-400'"></div>
            <span class="text-sm text-gray-300">{{ name }}</span>
            <span class="text-xs" :class="checkColor(status)">{{ status }}</span>
          </div>
        </div>
      </div>

      <!-- MPTCP Status -->
      <div v-if="mptcpStatus" class="bg-gray-800 rounded-lg p-5 mb-6">
        <h2 class="text-lg font-semibold mb-3">MPTCP</h2>
        <div class="grid grid-cols-3 gap-4 text-sm">
          <div>
            <span class="text-gray-400">Kernel:</span>
            <span :class="mptcpStatus.available ? 'text-green-400' : 'text-gray-500'" class="ml-2">{{ mptcpStatus.available ? 'available' : 'not available' }}</span>
          </div>
          <div>
            <span class="text-gray-400">Enabled:</span>
            <span :class="mptcpStatus.enabled ? 'text-green-400' : 'text-gray-500'" class="ml-2">{{ mptcpStatus.enabled ? 'yes' : 'no' }}</span>
          </div>
          <div>
            <span class="text-gray-400">Strategy:</span>
            <span class="text-gray-300 ml-2">{{ mptcpStatus.strategy }}</span>
          </div>
        </div>
      </div>

      <!-- API Reference -->
      <div class="bg-gray-800 rounded-lg p-5">
        <h2 class="text-lg font-semibold mb-3">API Reference</h2>
        <div class="space-y-4 text-sm text-gray-400">
          <div>
            <h3 class="text-gray-300 font-medium mb-1">Devices & Config</h3>
            <ul class="space-y-1">
              <li><code class="text-gray-300">GET /api/devices</code> — Device registry</li>
              <li><code class="text-gray-300">PUT /api/devices/{imei}/config</code> — Update config</li>
            </ul>
          </div>
          <div>
            <h3 class="text-gray-300 font-medium mb-1">Safety</h3>
            <ul class="space-y-1">
              <li><code class="text-gray-300">GET /api/escalation/chains</code> — Escalation chains</li>
              <li><code class="text-gray-300">GET /api/alerts</code> — Active alerts</li>
              <li><code class="text-gray-300">GET /api/deadman</code> — Dead man's switch</li>
              <li><code class="text-gray-300">GET /api/notifications/prefs</code> — Notification preferences</li>
            </ul>
          </div>
          <div>
            <h3 class="text-gray-300 font-medium mb-1">Infrastructure</h3>
            <ul class="space-y-1">
              <li><code class="text-gray-300">GET /api/constellations</code> — Satellite backends</li>
              <li><code class="text-gray-300">GET /api/mptcp/status</code> — MPTCP concentrator</li>
              <li><code class="text-gray-300">GET /api/webhooks</code> — Outbound webhooks</li>
              <li><code class="text-gray-300">GET /api/ota/targets</code> — OTA targets</li>
            </ul>
          </div>
        </div>
      </div>
    </template>
  </div>
</template>
