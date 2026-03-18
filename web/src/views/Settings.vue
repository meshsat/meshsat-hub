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
      <ul class="text-sm text-gray-400 space-y-1">
        <li><code class="text-gray-300">GET /api/devices</code> — Device registry</li>
        <li><code class="text-gray-300">GET /api/devices/{imei}/config</code> — Device config (latest)</li>
        <li><code class="text-gray-300">GET /api/messages</code> — Message history</li>
        <li><code class="text-gray-300">GET /api/positions/latest</code> — All device positions</li>
        <li><code class="text-gray-300">GET /api/credits</code> — Iridium credit balance</li>
        <li><code class="text-gray-300">GET /api/ratelimit</code> — Rate limit / budget status</li>
        <li><code class="text-gray-300">GET /api/audit</code> — Audit log</li>
        <li><code class="text-gray-300">GET /api/audit/verify</code> — Verify audit chain</li>
        <li><code class="text-gray-300">GET /api/auth/keys</code> — API keys</li>
        <li><code class="text-gray-300">GET /api/webhooks</code> — Outbound webhooks</li>
        <li><code class="text-gray-300">GET /api/backup/export</code> — Download backup</li>
        <li><code class="text-gray-300">POST /api/webhook/rockblock</code> — RockBLOCK MO webhook</li>
      </ul>
    </div>
  </div>
</template>
