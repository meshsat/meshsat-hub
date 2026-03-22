<script setup>
import { ref, onMounted } from 'vue'
import { messages, devices } from '../api/client'
import { useAuthStore } from '../stores/auth'
import { formatUTC } from '../utils/time'

const auth = useAuthStore()
const messageList = ref([])
const deviceList = ref([])
const filter = ref('')
const loading = ref(false)
const error = ref('')
const success = ref('')

// MT Send form
const sendImei = ref('')
const sendText = ref('')
const sendCompress = ref(true)
const sendEncrypt = ref(true)
const sending = ref(false)

// SMS Send form
const smsTo = ref('')
const smsText = ref('')
const smsCompress = ref(false)
const smsEncrypt = ref(false)
const smsSending = ref(false)

onMounted(async () => {
  await Promise.all([loadMessages(), loadDevices()])
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

async function loadDevices() {
  try {
    deviceList.value = await devices.list() || []
    if (deviceList.value.length > 0 && !sendImei.value) {
      sendImei.value = deviceList.value[0].imei
    }
  } catch (_) { /* ignore */ }
}

async function sendMessage() {
  if (!sendImei.value || !sendText.value) return
  sending.value = true
  error.value = ''
  success.value = ''
  try {
    const result = await messages.send(sendImei.value, sendText.value, sendCompress.value, sendEncrypt.value)
    const flags = [result.compressed && 'SMAZ2', result.encrypted && 'AES-256-GCM'].filter(Boolean).join(' + ') || 'plaintext'
    success.value = `MT queued — ID: ${result.mt_id || 'pending'} | ${result.original_bytes}B → ${result.wire_bytes}B (${flags})`
    sendText.value = ''
    await loadMessages()
  } catch (e) {
    error.value = `Send failed: ${e.message}`
  } finally {
    sending.value = false
  }
}

async function sendSMS() {
  if (!smsTo.value || !smsText.value) return
  smsSending.value = true
  error.value = ''
  success.value = ''
  try {
    const result = await messages.sendSMS(smsTo.value, smsText.value, smsCompress.value, smsEncrypt.value)
    const flags = [result.compressed && 'SMAZ2', result.encrypted && 'AES-256-GCM'].filter(Boolean).join(' + ') || 'plaintext'
    success.value = `SMS ${result.status} to ${result.to} | SID: ${result.sid} | ${flags}`
    smsText.value = ''
    await loadMessages()
  } catch (e) {
    error.value = `SMS failed: ${e.message}`
  } finally {
    smsSending.value = false
  }
}

function formatTime(ts) {
  return formatUTC(ts)
}

function dirClass(dir) {
  return dir === 'mo' ? 'text-emerald-400' : 'text-sky-400'
}

function statusClass(status) {
  if (status === 'received' || status === 'delivered') return 'text-green-400'
  if (status === 'failed') return 'text-red-400'
  return 'text-yellow-400'
}
</script>

<template>
  <div>
    <h1 class="text-2xl font-display font-bold mb-4">Messages</h1>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">
      {{ error }}
    </div>
    <div v-if="success" class="bg-green-900/50 border border-green-700 text-green-200 px-4 py-3 rounded mb-4">
      {{ success }}
    </div>

    <!-- Send MT Message -->
    <div v-if="auth.isOwner || auth.role === 'operator'" class="bg-tactical-surface border border-tactical-border rounded-lg p-4 mb-4">
      <h2 class="text-sm font-semibold text-gray-300 mb-3">Send Message to Device (MT via Iridium)</h2>
      <div class="flex gap-2">
        <select v-model="sendImei"
          class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 focus:outline-none focus:border-teal-500">
          <option v-for="d in deviceList" :key="d.imei" :value="d.imei">{{ d.imei }} {{ d.label ? `(${d.label})` : '' }}</option>
          <option v-if="deviceList.length === 0" value="">No devices registered</option>
        </select>
        <input v-model="sendText" placeholder="Type message to send via satellite..."
          @keyup.enter="sendMessage" :disabled="sending"
          class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 placeholder-gray-500 focus:outline-none focus:border-teal-500 flex-1" />
        <button @click="sendMessage" :disabled="sending || !sendText || !sendImei"
          class="bg-teal-600 hover:bg-teal-500 disabled:bg-gray-600 text-white px-4 py-2 rounded-lg font-medium transition-colors whitespace-nowrap">
          {{ sending ? 'Sending...' : 'Send MT' }}
        </button>
      </div>
      <div class="flex gap-4 mt-2">
        <label class="flex items-center gap-1.5 text-xs text-gray-400 cursor-pointer">
          <input type="checkbox" v-model="sendCompress" class="accent-teal-500" />
          SMAZ2 Compress
        </label>
        <label class="flex items-center gap-1.5 text-xs text-gray-400 cursor-pointer">
          <input type="checkbox" v-model="sendEncrypt" class="accent-teal-500" />
          AES-256-GCM Encrypt
        </label>
        <span class="text-xs text-gray-500">Message queued for next satellite pass (30-90s typical).</span>
      </div>
    </div>

    <!-- Send SMS -->
    <div v-if="auth.isOwner || auth.role === 'operator'" class="bg-tactical-surface border border-tactical-border rounded-lg p-4 mb-4">
      <h2 class="text-sm font-semibold text-gray-300 mb-3">Send SMS (via Twilio)</h2>
      <div class="flex gap-2">
        <input v-model="smsTo" placeholder="+31612345678"
          class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 placeholder-gray-500 focus:outline-none focus:border-green-400 w-48" />
        <input v-model="smsText" placeholder="Type SMS message..."
          @keyup.enter="sendSMS" :disabled="smsSending"
          class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 placeholder-gray-500 focus:outline-none focus:border-green-400 flex-1" />
        <button @click="sendSMS" :disabled="smsSending || !smsText || !smsTo"
          class="bg-green-600 hover:bg-green-500 disabled:bg-gray-600 text-white px-4 py-2 rounded-lg font-medium transition-colors whitespace-nowrap">
          {{ smsSending ? 'Sending...' : 'Send SMS' }}
        </button>
      </div>
      <div class="flex gap-4 mt-2">
        <label class="flex items-center gap-1.5 text-xs text-gray-400 cursor-pointer">
          <input type="checkbox" v-model="smsCompress" class="accent-green-500" />
          SMAZ2 Compress
        </label>
        <label class="flex items-center gap-1.5 text-xs text-gray-400 cursor-pointer">
          <input type="checkbox" v-model="smsEncrypt" class="accent-green-500" />
          AES-256-GCM Encrypt
        </label>
        <span class="text-xs text-gray-500">Compressed/encrypted SMS sent as hex-encoded binary with MSMS: prefix.</span>
      </div>
    </div>

    <div class="flex gap-2 mb-4">
      <input v-model="filter" placeholder="Filter by device IMEI" @keyup.enter="loadMessages"
        class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 placeholder-gray-500 focus:outline-none focus:border-teal-500 flex-1" />
      <button @click="loadMessages"
        class="bg-teal-600 hover:bg-teal-500 text-white px-4 py-2 rounded-lg font-medium transition-colors">
        Refresh
      </button>
    </div>

    <div class="overflow-x-auto">
      <table class="w-full border-collapse text-sm">
        <thead>
          <tr class="border-b border-tactical-border text-left text-gray-500">
            <th class="px-3 py-2">Time</th>
            <th class="px-3 py-2">Dir</th>
            <th class="px-3 py-2">Device</th>
            <th class="px-3 py-2">Channel</th>
            <th class="px-3 py-2">Text</th>
            <th class="px-3 py-2">Status</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="m in messageList" :key="m.id" class="border-b border-tactical-border/30 hover:bg-white/[0.02]">
            <td class="px-3 py-2 text-gray-400 whitespace-nowrap">{{ formatTime(m.created_at) }}</td>
            <td class="px-3 py-2">
              <span :class="[dirClass(m.direction), m.direction === 'mo' ? 'bg-emerald-900/30' : 'bg-sky-900/30']"
                class="font-semibold text-xs px-1.5 py-0.5 rounded">
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
