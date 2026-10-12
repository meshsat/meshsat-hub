<script setup>
// Hub federation (MESHSAT-1549), owner's Settings: relay satellite messages
// to kits on another MeshSat Hub, self-hosted or hosted, and receive theirs.
// Each owner adds the other's certificate; nothing flows until both have,
// and a peer's modem reaches a device here only through a grant.
import { ref, computed, onMounted } from 'vue'
import { federation } from '../api/client'
import { useToastStore } from '../stores/toast'
import { formatWhen } from '../utils/time'

const toast = useToastStore()

const available = ref(false)
const identity = ref(null)
const peers = ref([])
const grants = ref([])
const error = ref('')
const busy = ref(false)

const peerForm = ref({ name: '', relay_url: '', certificate_pem: '' })
const grantForm = ref({ peer_id: '', from_imei: '', to_imei: '' })
const lastAdded = ref(null)

const peerName = computed(() => Object.fromEntries(peers.value.map((p) => [p.id, p.name])))
const imeiOK = (v) => /^[0-9]{15}$/.test(v || '')

async function load() {
  try {
    const [id, p, g] = await Promise.all([federation.identity(), federation.peers.list(), federation.grants.list()])
    identity.value = id
    peers.value = p || []
    grants.value = g || []
    available.value = true
    if (!grantForm.value.peer_id && peers.value.length) grantForm.value.peer_id = peers.value[0].id
  } catch (e) {
    // 403: not the owner. Anything else is worth saying.
    available.value = e?.status !== 403
    if (available.value) error.value = e?.message || 'Could not read federation'
  }
}

async function copy(text, what) {
  try {
    await navigator.clipboard.writeText(text)
    toast.success(`${what} copied`)
  } catch {
    toast.error('Copy failed; select the text instead')
  }
}

async function addPeer() {
  busy.value = true
  error.value = ''
  try {
    lastAdded.value = await federation.peers.create(peerForm.value)
    peerForm.value = { name: '', relay_url: '', certificate_pem: '' }
    await load()
  } catch (e) {
    error.value = e?.message || 'Could not add the peer'
  } finally {
    busy.value = false
  }
}

async function removePeer(p) {
  if (!confirm(`Remove ${p.name}? Its grants go with it, and it can no longer relay to your devices.`)) return
  busy.value = true
  try {
    await federation.peers.remove(p.id)
    await load()
  } catch (e) {
    error.value = e?.message || 'Could not remove the peer'
  } finally {
    busy.value = false
  }
}

async function addGrant() {
  busy.value = true
  error.value = ''
  try {
    await federation.grants.create(grantForm.value)
    grantForm.value = { ...grantForm.value, from_imei: '', to_imei: '' }
    await load()
  } catch (e) {
    error.value = e?.message || 'Could not add the grant'
  } finally {
    busy.value = false
  }
}

async function removeGrant(g) {
  busy.value = true
  try {
    await federation.grants.remove(g.id)
    await load()
  } catch (e) {
    error.value = e?.message || 'Could not remove the grant'
  } finally {
    busy.value = false
  }
}

onMounted(load)
</script>

<template>
  <div v-if="available" class="pt-4 border-t border-ms-border" data-testid="federation">
    <h4 class="text-sm font-medium text-ms-text">Hub federation</h4>
    <p class="text-[11px] text-ms-muted mt-1 mb-3">
      Relay satellite messages to kits on another MeshSat Hub, hosted or self-hosted, and receive theirs. You add their certificate and they add yours; nothing flows until both have. A message from one of their modems reaches one of your devices only through a grant below, and it is sent on your own satellite account.
    </p>

    <div v-if="identity" class="space-y-2 text-[13px]">
      <h5 class="ms-label">Your identity</h5>
      <p class="text-ms-text2">Fingerprint <span class="ms-id break-all">{{ identity.fingerprint }}</span></p>
      <p class="text-ms-muted text-[11px]">Read it to the other owner over a channel you trust, and check theirs the same way.</p>
      <div class="flex flex-wrap gap-2">
        <button type="button" class="ms-btn" @click="copy(identity.certificate_pem, 'Certificate')">Copy certificate</button>
        <button type="button" class="ms-btn" :disabled="!identity.relay_url" @click="copy(identity.relay_url, 'Relay URL')">Copy relay URL</button>
      </div>
      <p v-if="!identity.relay_url" class="text-ms-warning text-[11px]">This Hub has no public URL configured, so peers cannot reach it.</p>
      <p v-else class="text-ms-muted text-[11px]">The relay URL carries a secret token: give it only to the other owner.</p>
    </div>

    <div class="mt-4 space-y-2">
      <h5 class="ms-label">Peers</h5>
      <table v-if="peers.length" class="ms-table w-full text-[13px]">
        <thead><tr><th>Name</th><th>Fingerprint</th><th>Peer id</th><th>Added</th><th></th></tr></thead>
        <tbody>
          <tr v-for="p in peers" :key="p.id">
            <td>{{ p.name }}</td>
            <td class="ms-id break-all">{{ p.fingerprint }}</td>
            <td class="ms-id">{{ p.id }}</td>
            <td class="ms-num">{{ formatWhen(p.created_at, '') }}</td>
            <td><button type="button" class="ms-btn-danger" :disabled="busy" @click="removePeer(p)">Remove</button></td>
          </tr>
        </tbody>
      </table>
      <p v-else class="text-ms-muted text-[13px]">No peers yet.</p>

      <form class="grid gap-2 md:grid-cols-2" @submit.prevent="addPeer">
        <label class="block"><span class="ms-label">Name</span>
          <input v-model="peerForm.name" class="ms-input w-full mt-1" maxlength="128" autocomplete="off" /></label>
        <label class="block"><span class="ms-label">Their relay URL</span>
          <input v-model="peerForm.relay_url" class="ms-input w-full mt-1 ms-id" placeholder="https://hub.example/api/webhook/federation/..." autocomplete="off" spellcheck="false" /></label>
        <label class="block md:col-span-2"><span class="ms-label">Their certificate</span>
          <textarea v-model="peerForm.certificate_pem" rows="4" class="ms-input w-full mt-1 ms-id text-[11px]" placeholder="-----BEGIN CERTIFICATE-----" spellcheck="false"></textarea></label>
        <div class="md:col-span-2">
          <button type="submit" class="ms-btn" :disabled="busy || !peerForm.name || !peerForm.relay_url || !peerForm.certificate_pem">Add peer</button>
        </div>
      </form>
      <p v-if="lastAdded" class="text-ms-text2 text-[13px]">
        Added {{ lastAdded.name }}. Its fingerprint is <span class="ms-id break-all">{{ lastAdded.fingerprint }}</span>: check it against what their owner reads on their own Hub.
      </p>
    </div>

    <div class="mt-4 space-y-2">
      <h5 class="ms-label">Grants</h5>
      <table v-if="grants.length" class="ms-table w-full text-[13px]">
        <thead><tr><th>Peer</th><th>Their modem</th><th>Your device</th><th></th></tr></thead>
        <tbody>
          <tr v-for="g in grants" :key="g.id">
            <td>{{ peerName[g.peer_id] || g.peer_id }}</td>
            <td class="ms-id">{{ g.from_imei }}</td>
            <td class="ms-id">{{ g.to_imei }}</td>
            <td><button type="button" class="ms-btn-danger" :disabled="busy" @click="removeGrant(g)">Remove</button></td>
          </tr>
        </tbody>
      </table>
      <p v-else class="text-ms-muted text-[13px]">No grants yet.</p>

      <form v-if="peers.length" class="flex flex-wrap items-end gap-2" @submit.prevent="addGrant">
        <label class="block"><span class="ms-label">Peer</span>
          <select v-model="grantForm.peer_id" class="ms-input mt-1 block">
            <option v-for="p in peers" :key="p.id" :value="p.id">{{ p.name }}</option>
          </select></label>
        <label class="block"><span class="ms-label">Their modem IMEI</span>
          <input v-model="grantForm.from_imei" class="ms-input mt-1 ms-id w-44" maxlength="15" inputmode="numeric" autocomplete="off" /></label>
        <label class="block"><span class="ms-label">Your device IMEI</span>
          <input v-model="grantForm.to_imei" class="ms-input mt-1 ms-id w-44" maxlength="15" inputmode="numeric" autocomplete="off" /></label>
        <button type="submit" class="ms-btn" :disabled="busy || !grantForm.peer_id || !imeiOK(grantForm.from_imei) || !imeiOK(grantForm.to_imei)">Add grant</button>
      </form>
      <p class="text-ms-muted text-[11px]">
        To send the other way, add a route with the destination "Relay to a kit on a peer Hub" and the recipient written as peer id, a slash, and their device's IMEI.
      </p>
    </div>

    <p v-if="error" class="text-ms-error text-[13px] mt-2">{{ error }}</p>
  </div>
</template>
