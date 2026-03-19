<script setup>
import { ref, onMounted } from 'vue'
import { messages } from '../api/client'
import { formatUTC } from '../utils/time'

const messageList = ref([])
const filter = ref('')
const loading = ref(false)
const error = ref('')

onMounted(async () => {
  await loadMessages()
})

async function loadMessages() {
  loading.value = true
  error.value = ''
  try {
    messageList.value = await messages.list(filter.value, 200) || []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function formatTime(ts) {
  return formatUTC(ts)
}

function dirClass(dir) {
  return dir === 'mo' ? 'text-teal-400' : 'text-purple-400'
}

function statusClass(status) {
  if (status === 'received' || status === 'delivered') return 'text-green-400'
  if (status === 'failed') return 'text-red-400'
  return 'text-yellow-400'
}
</script>

<template>
  <div>
    <h1 class="text-xl font-bold mb-4">Messages</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">
      {{ error }}
    </div>

    <div class="flex gap-2 mb-4">
      <input v-model="filter" placeholder="Filter by device IMEI" @keyup.enter="loadMessages"
        class="bg-gray-700 border border-gray-600 px-3 py-2 rounded text-gray-100 placeholder-gray-500 focus:outline-none focus:border-teal-400 flex-1" />
      <button @click="loadMessages"
        class="bg-teal-600 hover:bg-teal-500 text-white px-4 py-2 rounded font-medium transition-colors">
        Refresh
      </button>
    </div>

    <div class="overflow-x-auto">
      <table class="w-full border-collapse text-sm">
        <thead>
          <tr class="border-b border-gray-700 text-left text-gray-400">
            <th class="px-3 py-2">Time</th>
            <th class="px-3 py-2">Dir</th>
            <th class="px-3 py-2">Device</th>
            <th class="px-3 py-2">Channel</th>
            <th class="px-3 py-2">Text</th>
            <th class="px-3 py-2">Status</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="m in messageList" :key="m.id" class="border-b border-gray-800 hover:bg-gray-800/50">
            <td class="px-3 py-2 text-gray-400 whitespace-nowrap">{{ formatTime(m.created_at) }}</td>
            <td class="px-3 py-2">
              <span :class="dirClass(m.direction)" class="font-semibold text-xs px-1.5 py-0.5 rounded"
                :style="{ background: m.direction === 'mo' ? 'rgba(34,211,238,0.1)' : 'rgba(167,139,250,0.1)' }">
                {{ m.direction?.toUpperCase() }}
              </span>
            </td>
            <td class="px-3 py-2 font-mono text-xs">{{ m.device_imei }}</td>
            <td class="px-3 py-2 text-gray-400">{{ m.channel }}</td>
            <td class="px-3 py-2 max-w-xs truncate">{{ m.text || m.raw_hex || '—' }}</td>
            <td class="px-3 py-2">
              <span :class="statusClass(m.status)" class="text-xs font-medium">{{ m.status }}</span>
            </td>
          </tr>
          <tr v-if="messageList.length === 0 && !loading">
            <td colspan="6" class="px-3 py-8 text-center text-gray-500">No messages</td>
          </tr>
          <tr v-if="loading">
            <td colspan="6" class="px-3 py-8 text-center text-gray-500">Loading...</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
