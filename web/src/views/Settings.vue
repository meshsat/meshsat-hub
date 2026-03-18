<script setup>
import { ref, onMounted } from 'vue'
import { health } from '../api/client'

const hubHealth = ref(null)

onMounted(async () => {
  hubHealth.value = await health.check()
})
</script>

<template>
  <div>
    <h1 class="text-xl font-bold mb-4">Settings</h1>

    <div class="bg-gray-800 rounded-lg p-5 mb-4">
      <h2 class="text-lg font-semibold mb-3">Hub Status</h2>
      <pre class="font-mono text-sm text-gray-400">{{ JSON.stringify(hubHealth, null, 2) }}</pre>
    </div>

    <div class="bg-gray-800 rounded-lg p-5">
      <h2 class="text-lg font-semibold mb-3">API Endpoints</h2>
      <div class="space-y-4 text-sm text-gray-400">
        <div>
          <h3 class="text-gray-300 font-medium mb-1">Devices & Config</h3>
          <ul class="space-y-1">
            <li><code class="text-gray-300">GET /api/devices</code> — Device registry</li>
            <li><code class="text-gray-300">GET /api/devices/{imei}/config</code> — Device config (latest)</li>
            <li><code class="text-gray-300">PUT /api/devices/{imei}/config</code> — Create config version</li>
            <li><code class="text-gray-300">GET /api/devices/{imei}/config/history</code> — Config version history</li>
          </ul>
        </div>
        <div>
          <h3 class="text-gray-300 font-medium mb-1">Messages & Positions</h3>
          <ul class="space-y-1">
            <li><code class="text-gray-300">GET /api/messages</code> — Message history</li>
            <li><code class="text-gray-300">GET /api/positions/latest</code> — All device positions</li>
          </ul>
        </div>
        <div>
          <h3 class="text-gray-300 font-medium mb-1">Safety</h3>
          <ul class="space-y-1">
            <li><code class="text-gray-300">GET /api/escalation/chains</code> — Escalation chains</li>
            <li><code class="text-gray-300">GET /api/alerts</code> — Active alerts</li>
            <li><code class="text-gray-300">GET /api/deadman</code> — Dead man's switch configs</li>
            <li><code class="text-gray-300">GET /api/notifications/prefs</code> — Notification preferences</li>
          </ul>
        </div>
        <div>
          <h3 class="text-gray-300 font-medium mb-1">Infrastructure</h3>
          <ul class="space-y-1">
            <li><code class="text-gray-300">GET /api/constellations</code> — Satellite backends</li>
            <li><code class="text-gray-300">GET /api/credits</code> — Iridium credit balance</li>
            <li><code class="text-gray-300">GET /api/mptcp/status</code> — MPTCP concentrator</li>
            <li><code class="text-gray-300">GET /api/webhooks</code> — Outbound webhooks</li>
            <li><code class="text-gray-300">GET /api/ota/targets</code> — OTA targets</li>
            <li><code class="text-gray-300">GET /api/ratelimit</code> — Rate limit / budget status</li>
          </ul>
        </div>
        <div>
          <h3 class="text-gray-300 font-medium mb-1">Admin</h3>
          <ul class="space-y-1">
            <li><code class="text-gray-300">GET /api/auth/keys</code> — API keys (owner)</li>
            <li><code class="text-gray-300">GET /api/audit</code> — Audit log (owner)</li>
            <li><code class="text-gray-300">GET /api/backup/export</code> — Download backup</li>
            <li><code class="text-gray-300">POST /api/webhook/rockblock</code> — RockBLOCK MO webhook</li>
          </ul>
        </div>
      </div>
    </div>
  </div>
</template>
