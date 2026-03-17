<script setup>
import { ref, onMounted } from 'vue'
import { messages } from '../api/client'

const messageList = ref([])
const filter = ref('')

onMounted(async () => {
  await loadMessages()
})

async function loadMessages() {
  try {
    messageList.value = await messages.list(filter.value, 200)
  } catch (e) {
    console.error('Failed to load messages:', e)
  }
}
</script>

<template>
  <div>
    <h1 style="font-size: 1.5rem; font-weight: bold; margin-bottom: 1rem;">Messages</h1>

    <div style="display: flex; gap: 0.5rem; margin-bottom: 1rem;">
      <input v-model="filter" placeholder="Filter by device IMEI" @change="loadMessages"
        style="background: #374151; border: 1px solid #4b5563; padding: 0.5rem; border-radius: 4px; color: #f3f4f6; flex: 1;" />
      <button @click="loadMessages" style="background: #0891b2; color: white; padding: 0.5rem 1rem; border-radius: 4px; border: none; cursor: pointer;">Refresh</button>
    </div>

    <table style="width: 100%; border-collapse: collapse; font-size: 0.875rem;">
      <thead>
        <tr style="border-bottom: 1px solid #374151; text-align: left;">
          <th style="padding: 0.5rem;">Time</th>
          <th style="padding: 0.5rem;">Dir</th>
          <th style="padding: 0.5rem;">Device</th>
          <th style="padding: 0.5rem;">Channel</th>
          <th style="padding: 0.5rem;">Text</th>
          <th style="padding: 0.5rem;">Status</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="m in messageList" :key="m.id" style="border-bottom: 1px solid #1f2937;">
          <td style="padding: 0.5rem; color: #9ca3af; white-space: nowrap;">{{ m.created_at?.substring(0,19) }}</td>
          <td style="padding: 0.5rem;">
            <span :style="{ color: m.direction === 'mo' ? '#22d3ee' : '#a78bfa' }">{{ m.direction?.toUpperCase() }}</span>
          </td>
          <td style="padding: 0.5rem; font-family: monospace; font-size: 0.75rem;">{{ m.device_imei }}</td>
          <td style="padding: 0.5rem;">{{ m.channel }}</td>
          <td style="padding: 0.5rem; max-width: 300px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">{{ m.text || m.raw_hex }}</td>
          <td style="padding: 0.5rem;">
            <span :style="{ color: m.status === 'received' ? '#22c55e' : m.status === 'failed' ? '#ef4444' : '#eab308' }">{{ m.status }}</span>
          </td>
        </tr>
        <tr v-if="messageList.length === 0">
          <td colspan="6" style="padding: 1rem; text-align: center; color: #6b7280;">No messages</td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
