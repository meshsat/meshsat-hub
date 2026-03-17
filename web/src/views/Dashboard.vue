<script setup>
import { ref, onMounted } from 'vue'
import { devices, messages, health, credits } from '../api/client'

const stats = ref({ devices: 0, messages: 0, health: 'loading', credits: null })

onMounted(async () => {
  try {
    const [devs, msgs, h] = await Promise.all([
      devices.list(),
      messages.list('', 1),
      health.check(),
    ])
    stats.value.devices = devs.length
    stats.value.messages = msgs.length > 0 ? '...' : '0'
    stats.value.health = h.status || 'ok'
  } catch (e) {
    stats.value.health = 'error'
  }

  try {
    const c = await credits.get()
    stats.value.credits = c.balance
  } catch { /* credits endpoint may not be configured */ }
})
</script>

<template>
  <div>
    <h1 style="font-size: 1.5rem; font-weight: bold; margin-bottom: 1rem;">Dashboard</h1>

    <div style="display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 1rem;">
      <div style="background: #1f2937; padding: 1.5rem; border-radius: 8px;">
        <div style="color: #9ca3af; font-size: 0.875rem;">Hub Status</div>
        <div style="font-size: 1.5rem; font-weight: bold;" :style="{ color: stats.health === 'ok' ? '#22d3ee' : '#ef4444' }">
          {{ stats.health }}
        </div>
      </div>

      <div style="background: #1f2937; padding: 1.5rem; border-radius: 8px;">
        <div style="color: #9ca3af; font-size: 0.875rem;">Devices</div>
        <div style="font-size: 1.5rem; font-weight: bold; color: #22d3ee;">{{ stats.devices }}</div>
      </div>

      <div style="background: #1f2937; padding: 1.5rem; border-radius: 8px;">
        <div style="color: #9ca3af; font-size: 0.875rem;">Iridium Credits</div>
        <div style="font-size: 1.5rem; font-weight: bold; color: #22d3ee;">
          {{ stats.credits !== null ? stats.credits : '—' }}
        </div>
      </div>
    </div>
  </div>
</template>
