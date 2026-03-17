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
    <h1 style="font-size: 1.5rem; font-weight: bold; margin-bottom: 1rem;">Settings</h1>

    <div style="background: #1f2937; padding: 1.5rem; border-radius: 8px; margin-bottom: 1rem;">
      <h2 style="font-size: 1.125rem; font-weight: 600; margin-bottom: 0.75rem;">Hub Status</h2>
      <pre style="font-family: monospace; font-size: 0.875rem; color: #9ca3af;">{{ JSON.stringify(hubHealth, null, 2) }}</pre>
    </div>

    <div style="background: #1f2937; padding: 1.5rem; border-radius: 8px;">
      <h2 style="font-size: 1.125rem; font-weight: 600; margin-bottom: 0.75rem;">API Endpoints</h2>
      <ul style="list-style: none; padding: 0; font-size: 0.875rem; color: #9ca3af;">
        <li style="padding: 0.25rem 0;">GET /api/devices — Device registry</li>
        <li style="padding: 0.25rem 0;">GET /api/messages — Message history</li>
        <li style="padding: 0.25rem 0;">GET /api/positions/latest — All device positions (map)</li>
        <li style="padding: 0.25rem 0;">GET /api/credits — Iridium credit balance</li>
        <li style="padding: 0.25rem 0;">GET /api/ratelimit — Rate limit status</li>
        <li style="padding: 0.25rem 0;">GET /api/webhooks — Outbound webhooks</li>
        <li style="padding: 0.25rem 0;">GET /api/backup/export — Download backup</li>
        <li style="padding: 0.25rem 0;">POST /api/webhook/rockblock — RockBLOCK MO webhook</li>
      </ul>
    </div>
  </div>
</template>
