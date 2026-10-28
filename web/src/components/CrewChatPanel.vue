<script setup>
import { ref, computed, watch, onUnmounted } from 'vue'
import { takChat } from '../api/client'
import { ago } from '../utils/paths'
import Icon from './Icon.vue'

// Write to one crew from the map (MESHSAT-1706). The message goes to the kit or
// phone that owns the crew alone: a kit sends it to that node as a direct
// message, a phone shows it. The crew's answer appears here.
const props = defineProps({ crew: { type: Object, required: true } })
const emit = defineEmits(['close'])

const MAX_BYTES = 200
const messages = ref([])
const text = ref('')
const sending = ref(false)
const problem = ref('')
const encoder = new TextEncoder()
const bytes = computed(() => encoder.encode(text.value).length)
const canSend = computed(() => text.value.trim() !== '' && bytes.value <= MAX_BYTES && !sending.value)

// What a refusal means to the person who pressed Send, by status code.
const REFUSED = {
  404: 'This crew is not on the TAK map any more.',
  409: 'This crew has not been heard for a day, or its kit or phone is gone.',
  413: 'At most 200 bytes.',
  429: 'One message per crew every 10 seconds. Wait a moment and send again.',
  502: 'The message did not go out. Send it again.',
}

// What an outgoing message's status word says to the person who sent it
// (MESHSAT-1746): the Hub's own word ("sent" to the kit, "failed" at the bus),
// then the kit's ("queued" in its ledger, "delivered", "failed" when the
// ledger gave up, "undelivered" when the kit refused it). Not delivered is
// the one state that needs acting on, so it alone is red.
const STATUS = {
  queued: { text: 'queued on the kit' },
  delivered: { text: 'delivered' },
  failed: { text: 'not delivered', alarm: true },
  undelivered: { text: 'not delivered', alarm: true },
}
function statusOf(m) {
  return m.direction === 'out' ? STATUS[m.status] || null : null
}

// How a message reached the kit, or an answer came back (MESHSAT-1744): the
// row says mqtt, sbd, imt or sms; the person reads the bearer, not the modem.
// A row from before, or an answer that came over the TAK stream, says nothing.
const BEARER = { mqtt: 'over MQTT', sbd: 'over satellite', imt: 'over satellite', sms: 'by SMS' }
function bearerOf(m) {
  return BEARER[m.bearer] || ''
}

async function load() {
  try {
    const got = await takChat.messages(props.crew.uid)
    messages.value = Array.isArray(got) ? got : []
  } catch {
    // The thread stays as it was; the next poll tries again.
  }
}

function newRef() {
  return typeof crypto !== 'undefined' && crypto.randomUUID ? crypto.randomUUID() : `${Date.now()}-${Math.random()}`
}

async function send() {
  if (!canSend.value) return
  sending.value = true
  problem.value = ''
  try {
    await takChat.send(props.crew.uid, text.value, newRef())
    text.value = ''
    await load()
  } catch (e) {
    problem.value = REFUSED[e.status] || 'The message did not go out.'
  } finally {
    sending.value = false
  }
}

watch(() => props.crew.uid, () => { messages.value = []; problem.value = ''; load() }, { immediate: true })
const poll = setInterval(load, 5000)
onUnmounted(() => clearInterval(poll))
</script>

<template>
  <aside class="absolute z-10 top-3 right-3 max-h-[calc(100%-24px)] w-[min(320px,calc(100%-24px))] ms-panel shadow-2xl shadow-black/40 flex flex-col overflow-hidden"
    aria-label="Message this crew" data-testid="crew-chat">
    <div class="px-3 pt-3 pb-2 border-b border-ms-border flex items-start justify-between gap-2">
      <div class="min-w-0">
        <span class="ms-h2 block truncate">{{ crew.callsign }}</span>
        <span class="block text-xs text-ms-muted truncate">Through {{ crew.bridge_id }}, heard {{ ago(crew.last_seen) }}</span>
      </div>
      <button class="ms-btn-ghost h-7 px-1.5" aria-label="Close" @click="emit('close')"><Icon name="close" :size="16" /></button>
    </div>
    <ul class="min-h-0 flex-1 overflow-y-auto tactical-scroll px-3 py-2 space-y-2" aria-live="polite">
      <li v-if="!messages.length" class="py-4 text-[13px] text-ms-muted">No messages with this crew yet.</li>
      <li v-for="m in messages" :key="m.id" class="text-[13px]">
        <span class="block text-xs text-ms-muted">
          {{ m.direction === 'out' ? 'Sent' : crew.callsign }}, {{ ago(m.created_at) }}
          <template v-if="statusOf(m)">, <span :class="statusOf(m).alarm ? 'text-ms-error' : ''">{{ statusOf(m).text }}</span></template>
          <template v-if="bearerOf(m)">, <span class="text-ms-muted" data-testid="crew-chat-bearer">{{ bearerOf(m) }}</span></template>
        </span>
        <span class="block text-ms-text break-words">{{ m.text }}</span>
        <span v-if="statusOf(m) && m.status_detail" class="block text-xs break-words" :class="statusOf(m).alarm ? 'text-ms-error' : 'text-ms-muted'" data-testid="crew-chat-status-detail">{{ m.status_detail }}</span>
      </li>
    </ul>
    <form class="px-3 py-2 border-t border-ms-border" @submit.prevent="send">
      <label class="sr-only" for="crew-chat-text">Message</label>
      <textarea id="crew-chat-text" v-model="text" rows="2" class="ms-input w-full resize-none"
        placeholder="A short message; it goes to the crew's radio" @keydown.enter.exact.prevent="send" />
      <div class="flex items-center gap-2 mt-1.5">
        <span class="text-xs ms-num" :class="bytes > MAX_BYTES ? 'text-ms-error' : 'text-ms-muted'">{{ bytes }}/{{ MAX_BYTES }}</span>
        <span v-if="problem" class="text-xs text-ms-error flex-1 min-w-0" role="alert">{{ problem }}</span>
        <button type="submit" class="ms-btn-primary h-8 ml-auto" :disabled="!canSend">Send</button>
      </div>
    </form>
  </aside>
</template>
