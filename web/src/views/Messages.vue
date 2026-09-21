<script setup>
import { ref, computed, onMounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { messages, devices } from '../api/client'
import { useAuthStore } from '../stores/auth'
import { useToastStore } from '../stores/toast'
import { formatUTC } from '../utils/time'
import { exportCSV } from '../utils/csv'
import { bearerOf, bodyOf, ago } from '../utils/paths'
import Icon from '../components/Icon.vue'

const auth = useAuthStore()
const toast = useToastStore()
const route = useRoute()
const router = useRouter()

const messageList = ref([])
const deviceList = ref([])
const loading = ref(false)
const error = ref('')
const q = ref(String(route.query.q || ''))
const dir = ref('all')
const bearer = ref('all')

const canSend = computed(() => auth.isOwner || auth.role === 'operator')
const tab = ref('sat')

// Satellite compose
const sendImei = ref('')
const sendText = ref('')
const sendCompress = ref(true)
const sendEncrypt = ref(true)
const sending = ref(false)

// SMS compose
const smsTo = ref('')
const smsText = ref('')
const smsCompress = ref(false)
const smsEncrypt = ref(false)
const smsSending = ref(false)
const showOptions = ref(false)

onMounted(async () => {
  await Promise.all([loadMessages(), loadDevices()])
})

async function loadMessages() {
  loading.value = true
  error.value = ''
  try {
    messageList.value = await messages.list('', 300) || []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function loadDevices() {
  try {
    deviceList.value = await devices.list() || []
    if (deviceList.value.length > 0 && !sendImei.value) sendImei.value = deviceList.value[0].imei
  } catch { /* the picker says there are none */ }
}

const names = computed(() => Object.fromEntries(deviceList.value.map((d) => [d.imei, d.label || ''])))

const rows = computed(() => {
  const t = q.value.trim().toLowerCase()
  return messageList.value
    .map((m) => ({ ...m, b: bearerOf(m), body: bodyOf(m), who: names.value[m.device_imei] || '' }))
    .filter((m) => dir.value === 'all' || m.direction === dir.value)
    .filter((m) => bearer.value === 'all' || m.b.key === bearer.value)
    .filter((m) => !t || `${m.device_imei} ${m.who} ${m.text || ''}`.toLowerCase().includes(t))
})

watch(q, (v) => router.replace({ query: { ...route.query, q: v || undefined } }))

const STATUS = {
  received: { t: 'Received', c: 'text-ms-muted' },
  delivered: { t: 'Delivered', c: 'text-ms-muted' },
  sent: { t: 'Sent', c: 'text-ms-muted' },
  queued: { t: 'Queued', c: 'text-ms-text2' },
  scheduled: { t: 'Scheduled', c: 'text-ms-text2' },
  failed: { t: 'Failed', c: 'text-ms-error font-medium' },
}

// GSM 03.38: what fits a 160-character SMS. One character outside it (a curly
// apostrophe, an em dash) turns the whole message into UCS-2 at 70, and the
// Hub's number cannot deliver a multi-part SMS at all: it is silently dropped.
const GSM7 = /^[@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞÆæßÉ !"#¤%&'()*+,\-./0-9:;<=>?¡A-ZÄÖÑÜ§¿a-zäöñüà^{}\\[~\]|€]*$/
const smsInfo = computed(() => {
  const t = smsText.value
  const gsm = GSM7.test(t)
  const ext = gsm ? (t.match(/[\^{}\\[~\]|€]/g) || []).length : 0
  const len = gsm ? t.length + ext : t.length
  const limit = gsm ? 160 : 70
  const bad = gsm ? '' : [...new Set([...t].filter((ch) => !GSM7.test(ch)))].join(' ')
  return { len, limit, over: len > limit, gsm, bad }
})

async function sendMessage() {
  if (!sendImei.value || !sendText.value) return
  sending.value = true
  try {
    const result = await messages.send(sendImei.value, sendText.value, sendCompress.value, sendEncrypt.value)
    const how = [result.compressed && 'compressed', result.encrypted && 'encrypted'].filter(Boolean).join(' and ')
    toast.success(`Queued for ${names.value[sendImei.value] || sendImei.value}: ${result.wire_bytes} bytes${how ? `, ${how}` : ''}.`)
    sendText.value = ''
    await loadMessages()
  } catch (e) {
    toast.error(`Not sent: ${e.message}`)
  } finally {
    sending.value = false
  }
}

async function sendSMS() {
  if (!smsTo.value || !smsText.value || smsInfo.value.over) return
  smsSending.value = true
  try {
    const result = await messages.sendSMS(smsTo.value, smsText.value, smsCompress.value, smsEncrypt.value)
    toast.success(`SMS ${result.status || 'sent'} to ${result.to}.`)
    smsText.value = ''
    await loadMessages()
  } catch (e) {
    toast.error(`SMS not sent: ${e.message}`)
  } finally {
    smsSending.value = false
  }
}
</script>

<template>
  <div class="ms-page">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Messages</h1>
        <p class="ms-lede">Everything the Hub has received or sent, over every bearer.</p>
      </div>
    </div>

    <div v-if="error" role="alert" class="mb-4 rounded-lg border border-ms-error/50 bg-ms-error/10 px-4 py-3 text-[13px]">{{ error }}</div>

    <div class="grid gap-5 xl:grid-cols-12 items-start">
      <!-- Log -->
      <section class="ms-panel overflow-hidden xl:col-span-8 min-w-0" aria-label="Message log">
        <div class="flex flex-wrap items-center gap-2 px-4 py-3 border-b border-ms-border">
          <label class="relative flex-1 min-w-[12rem]">
            <span class="sr-only">Search</span>
            <Icon name="search" :size="14" class="absolute left-2.5 top-2 text-ms-muted" />
            <input v-model="q" type="search" placeholder="Search text, device name or id" class="ms-input w-full pl-8" />
          </label>
          <div class="flex rounded-md border border-ms-border overflow-hidden" role="group" aria-label="Direction">
            <button v-for="d in [['all', 'All'], ['mo', 'In'], ['mt', 'Out']]" :key="d[0]" class="h-8 px-3 text-xs transition-colors"
              :class="dir === d[0] ? 'bg-ms-well text-ms-text' : 'text-ms-muted hover:text-ms-text'" :aria-pressed="dir === d[0]" @click="dir = d[0]">{{ d[1] }}</button>
          </div>
          <select v-model="bearer" class="ms-input" aria-label="Bearer">
            <option value="all">Every bearer</option>
            <option value="mesh">Mesh</option>
            <option value="sat">Satellite</option>
            <option value="cell">SMS</option>
            <option value="ip">Internet</option>
          </select>
          <button class="ms-btn-ghost" title="Refresh" aria-label="Refresh" @click="loadMessages"><Icon name="refresh" :size="15" /></button>
          <button class="ms-btn-ghost text-xs" :disabled="!rows.length" title="Download the messages shown as CSV"
            @click="exportCSV(rows, 'meshsat-messages', ['created_at', 'direction', 'device_imei', 'channel', 'text', 'status', 'error'])">CSV</button>
        </div>
        <!-- Phones: the same log as two-line rows -->
        <ul class="md:hidden divide-y divide-ms-border/70">
          <li v-if="!rows.length" class="px-4 py-8 text-[13px] text-ms-muted">{{ loading ? 'Loading messages.' : messageList.length ? 'No message matches these filters.' : 'No messages yet.' }}</li>
          <li v-for="m in rows" :key="'l' + m.id" class="px-4 py-3">
            <div class="flex items-baseline gap-2 min-w-0">
              <span class="font-mono text-xs text-ms-muted ms-num">{{ m.created_at?.slice(11, 16) }}</span>
              <span class="text-xs" :class="m.direction === 'mt' ? 'text-ms-primary' : 'text-ms-muted'">{{ m.direction === 'mt' ? 'Out' : 'In' }}</span>
              <span v-if="m.who" class="text-[13px] font-medium text-ms-text truncate">{{ m.who }}</span>
              <span v-else class="ms-id text-ms-text2 truncate">{{ m.device_imei }}</span>
              <span class="ml-auto text-xs text-ms-muted shrink-0">{{ m.b.label }}</span>
            </div>
            <p class="text-[13px] mt-1 break-words" :class="m.body.kind === 'text' ? 'text-ms-text' : 'text-ms-muted italic'">
              <Icon v-if="m.body.kind === 'sealed'" name="lock" :size="12" class="inline -mt-0.5 mr-1" />{{ m.body.text }}
            </p>
            <p v-if="m.status === 'failed' || m.error" class="text-xs text-ms-error mt-0.5">{{ m.error || 'Failed' }}</p>
          </li>
        </ul>
        <div class="hidden md:block overflow-x-auto">
          <table class="ms-table">
            <thead>
              <tr><th class="w-24">Time</th><th>From or to</th><th class="w-full">Message</th><th class="hidden md:table-cell">Bearer</th><th class="hidden sm:table-cell text-right">State</th></tr>
            </thead>
            <tbody>
              <tr v-if="loading && !rows.length"><td colspan="5" class="text-ms-muted !h-24">Loading messages.</td></tr>
              <tr v-else-if="!rows.length"><td colspan="5" class="text-ms-muted !h-24">{{ messageList.length ? 'No message matches these filters.' : 'No messages yet. They appear here the moment a kit or device sends one.' }}</td></tr>
              <tr v-for="m in rows" :key="m.id" class="align-top">
                <td class="!h-auto py-2.5 whitespace-nowrap" :title="formatUTC(m.created_at)">
                  <div class="font-mono text-xs text-ms-text2 ms-num">{{ m.created_at?.slice(11, 16) }}</div>
                  <div class="text-xs text-ms-muted">{{ ago(m.created_at) }}</div>
                </td>
                <td class="!h-auto py-2.5 whitespace-nowrap">
                  <div class="flex items-center gap-1.5">
                    <span class="text-xs w-7" :class="m.direction === 'mt' ? 'text-ms-primary' : 'text-ms-muted'">{{ m.direction === 'mt' ? 'Out' : 'In' }}</span>
                    <span v-if="m.who" class="text-[13px] text-ms-text">{{ m.who }}</span>
                    <span v-else class="ms-id text-ms-text2">{{ m.device_imei }}</span>
                  </div>
                  <div v-if="m.who" class="ms-id text-ms-muted pl-[2.125rem]">{{ m.device_imei }}</div>
                </td>
                <td class="!h-auto py-2.5">
                  <p class="text-[13px] break-words max-w-[60ch]" :class="m.body.kind === 'text' ? 'text-ms-text' : 'text-ms-muted italic'">
                    <Icon v-if="m.body.kind === 'sealed'" name="lock" :size="12" class="inline -mt-0.5 mr-1" />{{ m.body.text }}
                  </p>
                  <p v-if="m.error" class="text-xs text-ms-error mt-0.5">{{ m.error }}</p>
                </td>
                <td class="!h-auto py-2.5 text-ms-muted whitespace-nowrap hidden md:table-cell">{{ m.b.label }}</td>
                <td class="!h-auto py-2.5 text-right whitespace-nowrap hidden sm:table-cell text-xs" :class="(STATUS[m.status] || {}).c || 'text-ms-muted'">{{ (STATUS[m.status] || {}).t || m.status }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <!-- Compose -->
      <section v-if="canSend" class="ms-panel xl:col-span-4 overflow-hidden xl:sticky xl:top-16" aria-labelledby="compose-h">
        <div class="ms-panel-head">
          <h2 id="compose-h" class="ms-h2">Send</h2>
          <div class="flex rounded-md border border-ms-border overflow-hidden" role="tablist" aria-label="Send to">
            <button role="tab" :aria-selected="tab === 'sat'" class="h-7 px-2.5 text-xs" :class="tab === 'sat' ? 'bg-ms-well text-ms-text' : 'text-ms-muted hover:text-ms-text'" @click="tab = 'sat'">Satellite device</button>
            <button role="tab" :aria-selected="tab === 'sms'" class="h-7 px-2.5 text-xs" :class="tab === 'sms' ? 'bg-ms-well text-ms-text' : 'text-ms-muted hover:text-ms-text'" @click="tab = 'sms'">Phone</button>
          </div>
        </div>

        <div v-if="tab === 'sat'" class="p-4 space-y-3" role="tabpanel">
          <label class="block">
            <span class="ms-label">To</span>
            <select v-model="sendImei" class="ms-input w-full mt-1">
              <option v-for="d in deviceList" :key="d.imei" :value="d.imei">{{ d.label ? `${d.label} (${d.imei})` : d.imei }}</option>
              <option v-if="deviceList.length === 0" value="">No devices registered</option>
            </select>
          </label>
          <label class="block">
            <span class="ms-label">Message</span>
            <textarea v-model="sendText" rows="4" :disabled="sending" placeholder="Short and plain: every byte is airtime."
              class="ms-input w-full mt-1 h-auto py-2 resize-y" @keydown.enter.exact.prevent="sendMessage" />
          </label>
          <p class="text-xs text-ms-muted">Queued at Iridium until the device next opens a satellite session, then delivered. It goes on your own Iridium account.</p>
          <details class="text-xs" :open="showOptions" @toggle="showOptions = $event.target.open">
            <summary class="cursor-pointer text-ms-muted hover:text-ms-text select-none">Options</summary>
            <div class="mt-2 space-y-1.5">
              <label class="flex items-center gap-2 text-ms-text2"><input v-model="sendCompress" type="checkbox" class="accent-brand-primary" /> Compress (SMAZ2)</label>
              <label class="flex items-center gap-2 text-ms-text2"><input v-model="sendEncrypt" type="checkbox" class="accent-brand-primary" /> Encrypt end to end (AES-256-GCM)</label>
            </div>
          </details>
          <button class="ms-btn-primary w-full" :disabled="sending || !sendText || !sendImei" @click="sendMessage">{{ sending ? 'Sending' : 'Send over satellite' }}</button>
        </div>

        <div v-else class="p-4 space-y-3" role="tabpanel">
          <label class="block">
            <span class="ms-label">Phone number</span>
            <input v-model="smsTo" placeholder="+31612345678" inputmode="tel" class="ms-input w-full mt-1 font-mono" />
          </label>
          <label class="block">
            <span class="flex items-baseline justify-between">
              <span class="ms-label">Message</span>
              <span class="text-xs ms-num" :class="smsInfo.over ? 'text-ms-error font-medium' : 'text-ms-muted'">{{ smsInfo.len }} / {{ smsInfo.limit }}</span>
            </span>
            <textarea v-model="smsText" rows="4" :disabled="smsSending" class="ms-input w-full mt-1 h-auto py-2 resize-y" />
          </label>
          <p v-if="smsInfo.over" class="text-xs text-ms-error">Too long for one SMS. A longer message from this number is not delivered, so shorten it{{ smsInfo.gsm ? '' : ' or replace the special characters' }}.</p>
          <p v-else-if="!smsInfo.gsm" class="text-xs text-ms-warning">These characters shrink the limit from 160 to 70: <span class="font-mono">{{ smsInfo.bad }}</span></p>
          <details class="text-xs">
            <summary class="cursor-pointer text-ms-muted hover:text-ms-text select-none">Options</summary>
            <div class="mt-2 space-y-1.5">
              <label class="flex items-center gap-2 text-ms-text2"><input v-model="smsCompress" type="checkbox" class="accent-brand-primary" /> Compress (SMAZ2)</label>
              <label class="flex items-center gap-2 text-ms-text2"><input v-model="smsEncrypt" type="checkbox" class="accent-brand-primary" /> Encrypt end to end (AES-256-GCM)</label>
              <p class="text-ms-muted">Compressed or encrypted SMS go as binary for a MeshSat kit to read, not for a person.</p>
            </div>
          </details>
          <button class="ms-btn-primary w-full" :disabled="smsSending || !smsText || !smsTo || smsInfo.over" @click="sendSMS">{{ smsSending ? 'Sending' : 'Send SMS' }}</button>
        </div>
      </section>
    </div>
  </div>
</template>
