<script setup>
// Hosted TAK, from the customer's side (MESHSAT-1037): turn the account's own TAK
// server on, and see where it has got to.
//
// The Hub is the only party that talks to a TAK server (epic MESHSAT-1459): kits
// and the MeshSat apps export to the Hub over MQTT and the Hub forwards to the
// tenant's servers, hosted or their own, and reads them back. No TAK app connects
// to the Hub since public port 8089 closed on 11 Nov 2026 (MESHSAT-1466), so the
// people, enrolment links and QR codes that used to be here are gone, and so is
// the port. There is still no host field: the instance's address is an in-cluster
// name only the Hub dials, and the API deliberately does not return it.
import { ref, computed, onMounted } from 'vue'
import { tak } from '../api/client'

const status = ref(null)
const error = ref('')
const notice = ref('')
const loading = ref(false)
const busy = ref(false)

const enabled = computed(() => status.value?.enabled === true)
const ready = computed(() => status.value?.phase === 'Ready')

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    status.value = await tak.status()
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function enable() {
  busy.value = true
  error.value = ''
  try {
    await tak.enable()
    // The operator builds the server on a ticker, so "on" does not mean "ready".
    notice.value = 'Your TAK server is being created. This usually takes a minute.'
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="ms-page">
    <h1 class="ms-h1">TAK</h1>
    <p class="ms-lede mb-5">
      A TAK server of your own, run by the Hub. Your kits and the MeshSat apps reach it through the
      Hub, over the connection they already have.
    </p>

    <div v-if="error" role="alert" class="ms-alert mb-4">
      {{ error }}
    </div>
    <div v-if="notice" class="ms-note mb-4 flex items-start justify-between gap-3">
      <span>{{ notice }}</span>
      <button @click="notice = ''" class="ms-btn-ghost h-7 text-xs">Dismiss</button>
    </div>

    <!-- Not turned on yet -->
    <section v-if="!loading && status && !enabled" class="ms-panel p-5 mb-6">
      <h2 class="ms-h2 mb-2">TAK is not switched on</h2>
      <p class="text-sm text-ms-muted mb-4 max-w-[70ch]">
        Switching it on creates a TAK server for your account alone, on every plan including free.
      </p>
      <button @click="enable" :disabled="busy" class="ms-btn-primary">
        {{ busy ? 'Creating…' : 'Switch TAK on' }}
      </button>
    </section>

    <!-- On -->
    <section v-if="enabled" class="ms-panel mb-6">
      <div class="ms-panel-head">
        <h2 class="ms-h2">Your TAK server</h2>
        <span class="text-sm" :class="ready ? 'text-ms-text' : 'text-ms-warning'">
          {{ status.phase || 'Unknown' }}
        </span>
      </div>
      <p v-if="!ready" class="px-4 py-3 text-sm text-ms-muted">
        It is still being built, or it has stopped. This page shows the server's own word for it.
      </p>
      <p v-else class="px-4 py-3 text-sm text-ms-muted">
        Running. The Hub forwards to it whatever your kits and apps send.
      </p>
    </section>

    <section class="ms-panel">
      <div class="ms-panel-head">
        <h2 class="ms-h2">How TAK reaches your team</h2>
      </div>
      <div class="px-4 py-3 text-sm text-ms-text space-y-3 max-w-[78ch]">
        <p>
          Kits and the MeshSat apps with TAK switched on send their positions, SOS and messages to
          the Hub. The Hub passes them to your TAK server and to your other kits and apps, and brings
          back what the server sends.
        </p>
        <p>
          TAK apps such as ATAK, iTAK and WinTAK do not connect to the Hub. To use one, run a TAK
          server of your own and add it in
          <router-link to="/integrations" class="text-brand-primary">Integrations</router-link>
          as "Your own TAK server". The Hub then forwards to it the same way.
        </p>
      </div>
    </section>
  </div>
</template>
