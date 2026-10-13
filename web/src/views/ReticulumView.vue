<script setup>
// The tenant's LXMF address on the Hub's Reticulum node (MESHSAT-1555).
// Sideband, CrossTalk, MeshChat and NomadNet users message the address;
// what arrives is the inbox below, what the tenant sends is proven by the
// peer. The owner creates the address and sets its name, stamp cost and
// whether it is enabled; operators send; everyone with access reads.
import { ref, computed, onMounted, onUnmounted } from 'vue'
import { lxmf } from '../api/client'
import { useAuthStore } from '../stores/auth'
import { useToastStore } from '../stores/toast'
import { formatWhen } from '../utils/time'
import Icon from '../components/Icon.vue'

const auth = useAuthStore()
const toast = useToastStore()

const nodeOff = ref(false)
const identity = ref(null)
const qr = ref('')
const messages = ref([])
const peers = ref([])
const error = ref('')
const loading = ref(true)
const busy = ref(false)
const filter = ref('all')

const canSend = computed(() => auth.isOwner || auth.role === 'operator')
const form = ref({ display_name: '', stamp_cost: 0, enabled: true, propagation_enabled: true, propagation_stamp_cost: 16 })
const compose = ref({ destination: '', content: '', title: '', method: 'auto' })
const destOK = computed(() => /^[0-9a-f]{32}$/.test(compose.value.destination.trim().toLowerCase()))

const shown = computed(() => {
  if (filter.value === 'in') return messages.value.filter((m) => m.direction === 'in')
  if (filter.value === 'out') return messages.value.filter((m) => m.direction === 'out')
  return messages.value
})

function stateLabel(m) {
  if (m.direction === 'in') return 'received'
  return m.state
}

function short(h) {
  return h && h.length > 12 ? `${h.slice(0, 6)}…${h.slice(-6)}` : h
}

async function loadIdentity() {
  const id = await lxmf.identity()
  identity.value = id
  if (id?.exists) {
    form.value = { display_name: id.display_name, stamp_cost: id.stamp_cost, enabled: id.enabled, propagation_enabled: id.propagation_enabled, propagation_stamp_cost: id.propagation_stamp_cost }
    try {
      const blob = await lxmf.qr(320)
      if (qr.value) URL.revokeObjectURL(qr.value)
      qr.value = URL.createObjectURL(blob)
    } catch {
      qr.value = ''
    }
  }
}

async function loadMessages() {
  if (!identity.value?.exists) {
    messages.value = []
    peers.value = []
    return
  }
  const [list, p] = await Promise.all([lxmf.messages({ limit: 100 }), lxmf.peers()])
  messages.value = list || []
  peers.value = p || []
}

async function load() {
  error.value = ''
  try {
    await loadIdentity()
    await loadMessages()
    nodeOff.value = false
  } catch (e) {
    // 503 is the node switched off on this Hub, a fact, not a failure.
    if (e?.status === 503) nodeOff.value = true
    else error.value = e?.message || 'Could not read the Reticulum page'
  } finally {
    loading.value = false
  }
}

async function createIdentity() {
  busy.value = true
  error.value = ''
  try {
    await lxmf.createIdentity()
    await load()
    toast.success('Address created; it is announced on the network within a minute')
  } catch (e) {
    error.value = e?.message || 'Could not create the address'
  } finally {
    busy.value = false
  }
}

async function saveIdentity() {
  busy.value = true
  error.value = ''
  try {
    await lxmf.updateIdentity({ display_name: form.value.display_name, stamp_cost: Number(form.value.stamp_cost), enabled: form.value.enabled,
      propagation_enabled: form.value.propagation_enabled, propagation_stamp_cost: Number(form.value.propagation_stamp_cost) })
    await loadIdentity()
    toast.success('Address settings saved')
  } catch (e) {
    error.value = e?.message || 'Could not save the settings'
  } finally {
    busy.value = false
  }
}

async function copyText(text, what) {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(`${what} copied`)
  } catch {
    toast.error('Copy failed; select the text instead')
  }
}

async function copyAddress() {
  try {
    await navigator.clipboard.writeText(identity.value.address)
    toast.success('Address copied')
  } catch {
    toast.error('Copy failed; select the text instead')
  }
}

async function send() {
  busy.value = true
  error.value = ''
  try {
    await lxmf.send({ destination: compose.value.destination.trim().toLowerCase(), content: compose.value.content, title: compose.value.title, method: compose.value.method })
    compose.value = { ...compose.value, content: '', title: '' }
    await loadMessages()
    toast.success('Queued; the state moves when the peer proves delivery')
  } catch (e) {
    error.value = e?.message || 'Could not queue the message'
  } finally {
    busy.value = false
  }
}

function replyTo(m) {
  compose.value.destination = m.source
  document.getElementById('lxmf-compose')?.scrollIntoView({ behavior: 'smooth', block: 'start' })
}

let timer = null
onMounted(() => {
  load()
  timer = setInterval(() => { if (identity.value?.exists) loadMessages().catch(() => {}) }, 10000)
})
onUnmounted(() => {
  if (timer) clearInterval(timer)
  if (qr.value) URL.revokeObjectURL(qr.value)
})
</script>

<template>
  <div class="ms-page" data-testid="reticulum">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Reticulum</h1>
        <p class="ms-lede">
          Your LXMF address on the Hub's Reticulum node. Sideband, CrossTalk, MeshChat and NomadNet users message it; what arrives is here, and what you send is marked delivered when the peer proves it.
        </p>
      </div>
      <button type="button" class="ms-btn-ghost" :disabled="loading" @click="load"><Icon name="refresh" :size="16" /> Refresh</button>
    </div>

    <div v-if="nodeOff" class="ms-alert">The Reticulum node is not enabled on this Hub.</div>

    <template v-else>
      <section class="ms-panel">
        <div class="ms-panel-head"><h2 class="ms-h2">Address</h2></div>
        <div v-if="loading" class="text-ms-muted text-[13px]">Loading</div>
        <div v-else-if="!identity?.exists" class="space-y-3">
          <p class="text-ms-text2 text-[13px]">This workspace has no LXMF address yet. The address is a Reticulum identity the Hub keeps for you; other clients reach it by its 32-character hash.</p>
          <p v-if="identity?.public_endpoint" class="text-ms-muted text-[13px]">Clients connect to this Hub at <span class="ms-id">{{ identity.public_endpoint }}</span>.</p>
          <button v-if="auth.isOwner" type="button" class="ms-btn-primary" :disabled="busy" @click="createIdentity">Create address</button>
          <p v-else class="text-ms-muted text-[13px]">An owner of this workspace creates it.</p>
        </div>
        <div v-else class="grid gap-6 md:grid-cols-[auto_1fr]">
          <div class="flex flex-col items-start gap-2">
            <img v-if="qr" :src="qr" alt="QR code of the LXMF address" width="160" height="160" class="rounded bg-white p-2" />
            <div v-else class="w-40 h-40 rounded border border-ms-border flex items-center justify-center text-ms-muted text-[11px]">QR unavailable</div>
          </div>
          <div class="space-y-3 text-[13px] min-w-0">
            <div>
              <span class="ms-label">LXMF address</span>
              <div class="flex flex-wrap items-center gap-2 mt-1">
                <span class="ms-id break-all" data-testid="lxmf-address">{{ identity.address }}</span>
                <button type="button" class="ms-btn" @click="copyAddress">Copy</button>
              </div>
              <p class="text-ms-muted text-[11px] mt-1">Paste it into a client's "new conversation" field, or scan the code. Clients show the display name once they have heard the address announce.</p>
            </div>
            <div v-if="identity.public_endpoint">
              <span class="ms-label">Where a client connects</span>
              <div class="flex flex-wrap items-center gap-2 mt-1">
                <span class="ms-id" data-testid="lxmf-endpoint">{{ identity.public_endpoint }}</span>
                <button type="button" class="ms-btn" @click="copyText(identity.public_endpoint, 'Endpoint')">Copy</button>
              </div>
              <p class="text-ms-muted text-[11px] mt-1">Add a TCP client interface to this host and port in Sideband (Connectivity), CrossTalk, MeshChat or NomadNet; no interface access code. The Hub's addresses appear in the client within a minute of connecting.</p>
            </div>
            <p class="text-ms-text2">
              <template v-if="!identity.enabled">Disabled: not announced and receiving nothing.</template>
              <template v-else-if="identity.last_announce">Enabled, announced {{ formatWhen(identity.last_announce, '') }}.</template>
              <template v-else>Enabled; announced every 30 minutes by the leader replica.</template>
            </p>
            <div>
              <span class="ms-label">Propagation node</span>
              <div class="flex flex-wrap items-center gap-2 mt-1">
                <span class="ms-id break-all" data-testid="lxmf-pn-address">{{ identity.propagation_address }}</span>
                <button type="button" class="ms-btn" @click="copyText(identity.propagation_address, 'Propagation node address')">Copy</button>
              </div>
              <p class="text-ms-muted text-[11px] mt-1">
                <template v-if="identity.propagation_enabled">Holds messages for clients that are offline and hands them over when they sync; set it as the propagation node in a client. Messages for this address arrive in the inbox directly.</template>
                <template v-else>Off: this address holds nothing for other clients.</template>
              </p>
            </div>
            <form v-if="auth.isOwner" class="grid gap-3 sm:grid-cols-[1fr_8rem_auto] items-end" @submit.prevent="saveIdentity">
              <label class="block"><span class="ms-label">Display name</span>
                <input v-model="form.display_name" class="ms-input w-full mt-1" maxlength="64" autocomplete="off" /></label>
              <label class="block"><span class="ms-label">Stamp cost</span>
                <input v-model.number="form.stamp_cost" type="number" min="0" max="32" class="ms-input w-full mt-1 ms-num" /></label>
              <label class="flex items-center gap-2 pb-2"><input v-model="form.enabled" type="checkbox" /> <span>Enabled</span></label>
              <label class="flex items-center gap-2 sm:col-span-2"><input v-model="form.propagation_enabled" type="checkbox" /> <span>Propagation node: hold messages for offline clients</span></label>
              <label class="block"><span class="ms-label">Propagation stamp cost</span>
                <input v-model.number="form.propagation_stamp_cost" type="number" min="0" max="32" class="ms-input w-full mt-1 ms-num" /></label>
              <div class="sm:col-span-3 flex items-center gap-3">
                <button type="submit" class="ms-btn" :disabled="busy || !form.display_name">Save</button>
                <span class="text-ms-muted text-[11px]">A stamp cost makes a sender do proof of work per message; 0 accepts everything, Sideband's default. The propagation stamp is a separate, cheaper proof a client attaches to a message it hands the node; LXMF's default is 16. Costs above 16 keep most clients out.</span>
              </div>
            </form>
          </div>
        </div>
      </section>

      <section v-if="identity?.exists && canSend" id="lxmf-compose" class="ms-panel">
        <div class="ms-panel-head"><h2 class="ms-h2">Send</h2></div>
        <form class="grid gap-3 md:grid-cols-2" @submit.prevent="send">
          <label class="block"><span class="ms-label">To (LXMF address)</span>
            <input v-model="compose.destination" class="ms-input w-full mt-1 ms-id" maxlength="32" autocomplete="off" spellcheck="false" placeholder="32 hex characters" /></label>
          <label class="block"><span class="ms-label">Title (optional)</span>
            <input v-model="compose.title" class="ms-input w-full mt-1" maxlength="256" autocomplete="off" /></label>
          <label class="block md:col-span-2"><span class="ms-label">Message</span>
            <textarea v-model="compose.content" rows="3" class="ms-input w-full mt-1" maxlength="65536"></textarea></label>
          <div class="md:col-span-2 flex flex-wrap items-center gap-3">
            <label class="flex items-center gap-2 text-[13px]"><span class="ms-label">Delivery</span>
              <select v-model="compose.method" class="ms-input">
                <option value="auto">Auto</option>
                <option value="direct">Direct link</option>
                <option value="opportunistic">Opportunistic</option>
              </select></label>
            <button type="submit" class="ms-btn" :disabled="busy || !identity.enabled || !destOK || !compose.content.trim()">Send</button>
            <span class="text-ms-muted text-[11px]">Auto sends a short message as one packet and a long one over a link. Across a satellite, choose Direct link.</span>
          </div>
        </form>
      </section>

      <section v-if="identity?.exists" class="ms-panel">
        <div class="ms-panel-head">
          <h2 class="ms-h2">Messages</h2>
          <div class="flex gap-1" role="tablist">
            <button v-for="f in [['all', 'All'], ['in', 'Received'], ['out', 'Sent']]" :key="f[0]" type="button" role="tab"
              :aria-selected="filter === f[0]" :class="filter === f[0] ? 'ms-btn-primary' : 'ms-btn-ghost'" @click="filter = f[0]">{{ f[1] }}</button>
          </div>
        </div>
        <table v-if="shown.length" class="ms-table w-full text-[13px]">
          <thead><tr><th>When</th><th>Direction</th><th>Peer</th><th>Message</th><th>State</th><th></th></tr></thead>
          <tbody>
            <tr v-for="m in shown" :key="m.id">
              <td class="ms-num whitespace-nowrap">{{ formatWhen(m.created_at, '') }}</td>
              <td>{{ m.direction === 'in' ? 'In' : 'Out' }}</td>
              <td class="ms-id" :title="m.direction === 'in' ? m.source : m.destination">{{ short(m.direction === 'in' ? m.source : m.destination) }}</td>
              <td class="max-w-md">
                <div v-if="m.title" class="font-medium">{{ m.title }}</div>
                <div class="whitespace-pre-wrap break-words">{{ m.content }}</div>
                <div v-if="!m.signature_valid && m.direction === 'in'" class="text-ms-warning text-[11px]">Signature not verified: the sender has not been heard announce yet.</div>
              </td>
              <td>
                <span class="ms-chip" :class="{ 'text-ms-error': m.state === 'failed' }">{{ stateLabel(m) }}</span>
                <div v-if="m.state === 'failed' && m.detail" class="text-ms-muted text-[11px] mt-1">{{ m.detail }}</div>
                <div v-else-if="m.method" class="text-ms-muted text-[11px] mt-1">{{ m.method }}</div>
              </td>
              <td><button v-if="m.direction === 'in' && canSend" type="button" class="ms-btn-ghost" @click="replyTo(m)">Reply</button></td>
            </tr>
          </tbody>
        </table>
        <p v-else class="text-ms-muted text-[13px]">No messages yet. Share the address and the first one appears here.</p>
      </section>

      <section v-if="identity?.exists" class="ms-panel">
        <div class="ms-panel-head"><h2 class="ms-h2">Peers heard</h2></div>
        <table v-if="peers.length" class="ms-table w-full text-[13px]">
          <thead><tr><th>Display name</th><th>Address</th><th>Hops</th><th>Stamp cost</th><th>Last heard</th></tr></thead>
          <tbody>
            <tr v-for="p in peers" :key="p.dest_hash">
              <td>{{ p.display_name || '(no name)' }}</td>
              <td class="ms-id break-all">{{ p.dest_hash }}</td>
              <td class="ms-num">{{ p.hops }}</td>
              <td class="ms-num">{{ p.stamp_cost }}</td>
              <td class="ms-num">{{ p.last_seen }}</td>
            </tr>
          </tbody>
        </table>
        <p v-else class="text-ms-muted text-[13px]">No LXMF peers heard on this replica yet. Peers are recorded by the replica that runs the node; the other replica shows none.</p>
      </section>
    </template>

    <p v-if="error" class="text-ms-error text-[13px]">{{ error }}</p>
  </div>
</template>
