<script setup>
// Hosted TAK, from the customer's side (MESHSAT-1037, MESHSAT-1040): turn it on,
// decide who sees the fleet on a map, and hand each of those people a one-time
// enrolment their ATAK, WinTAK or iTAK imports.
//
// Two things here are deliberate and should not be "tidied":
//
//   - The enrolment link is shown ONCE and never stored in this component's list.
//     It is a live credential for fifteen minutes: anyone holding it gets a
//     certificate that can read this tenant's whole map. So it lives in a banner
//     the operator dismisses, exactly like the API key page does, and minting a
//     new one invalidates the previous.
//   - There is no host field. The instance's address is an in-cluster service
//     name no phone can resolve, and the API deliberately does not return it --
//     a phone is configured from its package, not by typing an address.
import { ref, computed, onMounted } from 'vue'
import { tak } from '../api/client'
import { formatUTC } from '../utils/time'

const status = ref(null)
const users = ref([])
const newUsername = ref('')
const newCallsign = ref('')
const minted = ref(null)
const qrURL = ref('')
const qrFor = ref('')
const error = ref('')
const notice = ref('')
const loading = ref(false)
const busy = ref('')

const enabled = computed(() => status.value?.enabled === true)
const atCeiling = computed(() => status.value?.remaining === 0)

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    status.value = await tak.status()
    if (status.value?.enabled) {
      users.value = (await tak.users()) || []
    }
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function enable() {
  busy.value = 'enable'
  error.value = ''
  try {
    await tak.enable()
    // The operator builds the server on a ticker, so "on" does not mean "ready".
    notice.value = 'Your TAK server is being created. This usually takes a minute.'
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

// A username OpenTAKServer will accept: lowercase letters and digits only. It
// rejects hyphens, dots and @, so the field shows what the account will actually
// be called rather than letting the operator deny it afterwards.
const sanitised = computed(() => newUsername.value.toLowerCase().replace(/[^a-z0-9]/g, '').slice(0, 32))
const usernameOK = computed(() => sanitised.value.length >= 3)

async function addUser() {
  if (!usernameOK.value) {
    error.value = 'A TAK username needs at least 3 letters or digits'
    return
  }
  busy.value = 'add'
  error.value = ''
  try {
    await tak.addUser({ username: sanitised.value, callsign: newCallsign.value.trim() })
    newUsername.value = ''
    newCallsign.value = ''
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

async function removeUser(username) {
  if (!confirm(`Remove ${username}? Their certificate stops working and they disappear from the map.`)) return
  busy.value = username
  error.value = ''
  try {
    await tak.removeUser(username)
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

async function enrol(username) {
  busy.value = username
  error.value = ''
  clearQR()
  try {
    minted.value = await tak.enrol(username)
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

async function showQR(username) {
  busy.value = username
  error.value = ''
  minted.value = null
  clearQR()
  try {
    const blob = await tak.enrolQR(username)
    qrURL.value = URL.createObjectURL(blob)
    qrFor.value = username
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = ''
  }
}

function clearQR() {
  if (qrURL.value) {
    // Release the blob rather than leaving it attached to the document: it is an
    // image of a credential.
    URL.revokeObjectURL(qrURL.value)
    qrURL.value = ''
    qrFor.value = ''
  }
}

async function copyLink() {
  try {
    await navigator.clipboard.writeText(minted.value.url)
    notice.value = 'Enrolment link copied'
  } catch {
    notice.value = 'Select the link and copy it'
  }
}
</script>

<template>
  <div>
    <h1 class="text-2xl font-display font-bold mb-1">TAK</h1>
    <p class="text-sm text-gray-400 mb-4">
      Your own TAK server. People you add here see your fleet on their ATAK, WinTAK or iTAK.
    </p>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">
      {{ error }}
    </div>
    <div v-if="notice" class="bg-sky-900/30 border border-sky-700 text-sky-200 px-4 py-3 rounded mb-4 flex justify-between">
      <span>{{ notice }}</span>
      <button @click="notice = ''" class="text-sky-400 hover:text-sky-200">Dismiss</button>
    </div>

    <!-- Not turned on yet -->
    <div v-if="!loading && !enabled" class="bg-tactical-surface rounded-lg border border-tactical-border p-6 mb-6">
      <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-2">
        TAK is not switched on
      </h2>
      <p class="text-sm text-gray-400 mb-4">
        Switching it on creates a TAK server for your account alone, on every plan including free.
        You can add {{ status?.limit === -1 ? 'as many people as you like' : `up to ${status?.limit} people` }}
        on the {{ status?.plan }} plan.
      </p>
      <button @click="enable" :disabled="busy === 'enable'"
        class="bg-brand-primary hover:bg-brand-accent text-ms-on-primary px-4 py-2 rounded-lg font-medium transition-colors disabled:opacity-50">
        {{ busy === 'enable' ? 'Creating…' : 'Switch TAK on' }}
      </button>
    </div>

    <!-- Status -->
    <div v-if="enabled" class="grid grid-cols-1 sm:grid-cols-3 gap-3 mb-6">
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4">
        <div class="text-xs text-gray-500 uppercase tracking-wider mb-1">Server</div>
        <div class="text-lg font-display">
          <span :class="status.phase === 'Ready' ? 'text-ms-success' : 'text-ms-warning'">
            {{ status.phase || 'unknown' }}
          </span>
        </div>
      </div>
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4">
        <div class="text-xs text-gray-500 uppercase tracking-wider mb-1">Port</div>
        <div class="text-lg font-mono">{{ status.port }}</div>
      </div>
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-4">
        <div class="text-xs text-gray-500 uppercase tracking-wider mb-1">People</div>
        <div class="text-lg font-display">
          {{ status.users }}<span class="text-gray-500 text-sm"> / {{ status.limit === -1 ? '∞' : status.limit }}</span>
        </div>
      </div>
    </div>

    <!-- The minted enrolment, shown once -->
    <div v-if="minted" class="bg-green-900/30 border border-green-700 rounded-lg p-4 mb-4">
      <div class="text-green-300 font-semibold mb-2">Enrolment for {{ minted.username }}</div>
      <div class="text-sm text-gray-300 mb-2">
        Open this on the phone, once. It works for one download and expires
        {{ formatUTC(minted.expires_at) }}.
      </div>
      <code class="block bg-gray-900 text-ms-success px-3 py-2 rounded font-mono text-xs break-all select-all">
        {{ minted.url }}
      </code>
      <div class="mt-3 flex gap-3">
        <button @click="copyLink" class="text-sm text-brand-primary hover:text-brand-accent">Copy link</button>
        <button @click="minted = null" class="text-sm text-gray-400 hover:text-gray-200">Dismiss</button>
      </div>
    </div>

    <!-- The same thing as a QR -->
    <div v-if="qrURL" class="bg-tactical-surface border border-tactical-border rounded-lg p-4 mb-4">
      <div class="text-gray-200 font-semibold mb-2">Scan this on {{ qrFor }}'s phone</div>
      <img :src="qrURL" :alt="`Enrolment QR code for ${qrFor}`" class="bg-white p-2 rounded max-w-[260px]" />
      <div class="mt-3">
        <button @click="clearQR" class="text-sm text-gray-400 hover:text-gray-200">Dismiss</button>
      </div>
    </div>

    <!-- Add somebody -->
    <div v-if="enabled" class="bg-tactical-surface rounded-lg border border-tactical-border p-4 mb-6">
      <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">Add a person</h2>
      <div class="flex flex-wrap gap-2">
        <input v-model="newUsername" placeholder="Username"
          class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 placeholder-gray-500 focus:outline-none focus:border-brand-primary flex-1 min-w-[160px]" />
        <input v-model="newCallsign" placeholder="Callsign (optional)"
          class="bg-gray-800 border border-gray-700 px-3 py-2 rounded-lg text-gray-200 placeholder-gray-500 focus:outline-none focus:border-brand-primary min-w-[160px]" />
        <button @click="addUser" :disabled="busy === 'add' || atCeiling"
          class="bg-brand-primary hover:bg-brand-accent text-ms-on-primary px-4 py-2 rounded-lg font-medium transition-colors disabled:opacity-50">
          {{ busy === 'add' ? 'Adding…' : 'Add' }}
        </button>
      </div>
      <!-- Say what the account will be called BEFORE the server refuses it. -->
      <p v-if="newUsername && sanitised !== newUsername.toLowerCase()" class="text-xs text-ms-warning mt-2">
        TAK usernames are letters and digits only — this will be created as
        <span class="font-mono">{{ sanitised || '(nothing usable)' }}</span>
      </p>
      <p v-if="atCeiling" class="text-xs text-ms-warning mt-2">
        You are at the {{ status.plan }} plan's limit of {{ status.limit }}. Remove somebody, or change plan in Settings.
      </p>
    </div>

    <!-- Who can connect -->
    <div v-if="enabled" class="overflow-x-auto">
      <table class="w-full border-collapse text-sm">
        <thead>
          <tr class="border-b border-tactical-border text-left text-gray-500">
            <th class="px-3 py-2">Username</th>
            <th class="px-3 py-2">Callsign</th>
            <th class="px-3 py-2">State</th>
            <th class="px-3 py-2">Certificate</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="u in users" :key="u.username" class="border-b border-tactical-border/30 hover:bg-white/[0.02]">
            <td class="px-3 py-2 font-mono text-xs">{{ u.username }}</td>
            <td class="px-3 py-2">{{ u.callsign || '—' }}</td>
            <td class="px-3 py-2">
              <span v-if="u.active" class="text-xs px-2 py-0.5 rounded font-medium bg-green-900/50 text-green-300 border border-green-700/50">active</span>
              <span v-else class="text-xs px-2 py-0.5 rounded font-medium bg-gray-700 text-gray-300">suspended</span>
            </td>
            <td class="px-3 py-2 font-mono text-xs text-gray-400">
              {{ u.cert_serial || 'not enrolled yet' }}
            </td>
            <td class="px-3 py-2 text-right whitespace-nowrap">
              <button @click="enrol(u.username)" :disabled="busy === u.username || !u.active"
                class="bg-brand-primary/15 hover:bg-brand-primary/25 text-brand-primary px-2 py-1 rounded-lg text-xs mr-2 transition-colors disabled:opacity-40">
                Enrol
              </button>
              <button @click="showQR(u.username)" :disabled="busy === u.username || !u.active"
                class="bg-gray-700 hover:bg-gray-600 text-gray-200 px-2 py-1 rounded-lg text-xs mr-2 transition-colors disabled:opacity-40">
                QR
              </button>
              <button @click="removeUser(u.username)" :disabled="busy === u.username"
                class="bg-red-900 hover:bg-red-800 text-red-200 px-2 py-1 rounded-lg text-xs transition-colors disabled:opacity-40">
                Remove
              </button>
            </td>
          </tr>
          <tr v-if="users.length === 0 && !loading">
            <td colspan="5" class="px-3 py-8 text-center text-gray-500">
              Nobody yet. Add a person, then enrol their phone.
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
