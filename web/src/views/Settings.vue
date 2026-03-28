<script setup>
import { ref, onMounted } from 'vue'
import { health, constellations, mptcp as mptcpApi, tor, codecs, ipougrs, backup, reticulum, settings } from '../api/client'

const loading = ref(true)
const hubHealth = ref(null)
const readyz = ref(null)
const backendList = ref([])
const mptcpStatus = ref(null)
const torStatus = ref(null)
const codecList = ref([])
const ipougrsStatus = ref(null)
const retIdentity = ref(null)
const error = ref('')
const exportLoading = ref(false)
const exportResult = ref(null)
const mqttUrl = ref('')
const mqttUrlSaving = ref(false)
const mqttUrlSaved = ref(false)
const securityStatus = ref(null)
const rotateLoading = ref(false)
const rotateResult = ref(null)

onMounted(async () => {
  const results = await Promise.allSettled([
    health.check(),
    health.readyz(),
    constellations.list(),
    mptcpApi.status(),
    tor.onion(),
    codecs.list(),
    ipougrs.status(),
    reticulum.identity(),
    settings.getMqttUrl(),
    settings.getSecurity(),
  ])

  hubHealth.value = results[0].status === 'fulfilled' ? results[0].value : null
  readyz.value = results[1].status === 'fulfilled' ? results[1].value : null
  const cons = results[2].status === 'fulfilled' ? results[2].value : {}
  backendList.value = cons.backends || []
  mptcpStatus.value = results[3].status === 'fulfilled' ? results[3].value : null
  torStatus.value = results[4].status === 'fulfilled' ? results[4].value : null
  codecList.value = results[5].status === 'fulfilled' && Array.isArray(results[5].value) ? results[5].value : []
  ipougrsStatus.value = results[6].status === 'fulfilled' ? results[6].value : null
  retIdentity.value = results[7].status === 'fulfilled' ? results[7].value : null
  mqttUrl.value = results[8].status === 'fulfilled' ? (results[8].value?.mqtt_url || '') : ''
  securityStatus.value = results[9].status === 'fulfilled' ? results[9].value : null
  loading.value = false
})

async function saveMqttUrl() {
  mqttUrlSaving.value = true
  mqttUrlSaved.value = false
  try {
    await settings.setMqttUrl(mqttUrl.value)
    mqttUrlSaved.value = true
    setTimeout(() => { mqttUrlSaved.value = false }, 3000)
  } catch (e) {
    error.value = e.message
  } finally {
    mqttUrlSaving.value = false
  }
}

async function exportBackup() {
  exportLoading.value = true
  error.value = ''
  try {
    const data = await backup.exportData()
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = `meshsat-hub-backup-${new Date().toISOString().substring(0, 10)}.json`
    a.click()
    URL.revokeObjectURL(url)
    exportResult.value = `Exported ${data.devices?.length || 0} devices, ${data.messages?.length || 0} messages`
  } catch (e) {
    error.value = e.message
  } finally {
    exportLoading.value = false
  }
}

async function rotateServicePasswords() {
  if (!confirm('Rotate all service passwords? This requires restarting NATS, Redis, and Hub services.')) return
  rotateLoading.value = true
  rotateResult.value = null
  try {
    const result = await settings.rotatePasswords()
    rotateResult.value = result.message || 'Passwords rotated'
    securityStatus.value = await settings.getSecurity()
  } catch (e) {
    error.value = e.message
  } finally {
    rotateLoading.value = false
  }
}

function statusDot(ok) {
  return ok ? 'bg-emerald-400' : 'bg-red-400'
}

function statusText(ok) {
  return ok ? 'text-emerald-400' : 'text-red-400'
}
</script>

<template>
  <div class="p-4 lg:p-6 max-w-6xl mx-auto">
    <h1 class="text-2xl font-display font-bold mb-6">Settings & System Info</h1>

    <div v-if="loading" class="text-center text-gray-500 py-16">Loading system status...</div>

    <template v-else>
      <!-- Health & Readiness -->
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-5 mb-6">
        <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-4">System Health</h2>
        <div class="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-4">
          <div>
            <span class="text-gray-400 text-xs">Liveness</span>
            <p class="text-lg font-bold" :class="statusText(hubHealth?.status === 'ok')">{{ hubHealth?.status || '?' }}</p>
          </div>
          <div>
            <span class="text-gray-400 text-xs">Readiness</span>
            <p class="text-lg font-bold" :class="statusText(readyz?.status === 'ok')">{{ readyz?.status || '?' }}</p>
          </div>
          <div>
            <span class="text-gray-400 text-xs">Constellations</span>
            <p class="text-sm font-medium text-gray-300">{{ backendList.join(', ') || 'none' }}</p>
          </div>
          <div>
            <span class="text-gray-400 text-xs">MPTCP</span>
            <p class="text-sm font-medium" :class="statusText(mptcpStatus?.enabled)">
              {{ mptcpStatus?.enabled ? mptcpStatus.strategy : 'disabled' }}
            </p>
          </div>
        </div>
        <div v-if="readyz?.checks" class="border-t border-gray-700 pt-3">
          <div class="flex flex-wrap gap-4">
            <div v-for="(status, name) in readyz.checks" :key="name" class="flex items-center gap-2">
              <span class="w-2 h-2 rounded-full" :class="statusDot(status === 'ok')"></span>
              <span class="text-sm text-gray-300">{{ name }}</span>
            </div>
          </div>
        </div>
      </div>

      <!-- Platform Settings -->
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-5 mb-6">
        <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-4">Platform</h2>
        <div class="space-y-3">
          <div>
            <label class="text-gray-400 text-xs block mb-1">MQTT Public URL</label>
            <p class="text-gray-500 text-xs mb-2">Shown to bridges during onboarding (Fleet page). Use <code class="text-gray-400">wss://</code> for WebSocket over TLS.</p>
            <div class="flex items-center gap-2">
              <input v-model="mqttUrl" type="text" placeholder="wss://hub.meshsat.net/mqtt"
                class="flex-1 bg-gray-800 border border-gray-600 rounded px-3 py-1.5 text-sm text-gray-200 placeholder-gray-500 focus:border-teal-500 focus:outline-none" />
              <button @click="saveMqttUrl" :disabled="mqttUrlSaving || !mqttUrl"
                class="bg-teal-600 hover:bg-teal-500 disabled:bg-gray-600 text-white text-sm px-4 py-1.5 rounded whitespace-nowrap">
                {{ mqttUrlSaving ? 'Saving...' : 'Save' }}
              </button>
              <span v-if="mqttUrlSaved" class="text-emerald-400 text-xs">Saved</span>
            </div>
          </div>
        </div>
      </div>

      <!-- Service Security -->
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-5 mb-6">
        <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-4">Service Security</h2>
        <div v-if="securityStatus" class="space-y-3">
          <div class="grid grid-cols-2 sm:grid-cols-4 gap-4">
            <div>
              <span class="text-gray-400 text-xs">NATS MQTT Auth</span>
              <div class="flex items-center gap-2 mt-1">
                <span class="w-2 h-2 rounded-full" :class="statusDot(securityStatus.nats_mqtt_auth)"></span>
                <span class="text-sm" :class="statusText(securityStatus.nats_mqtt_auth)">
                  {{ securityStatus.nats_mqtt_auth ? 'Password' : 'None' }}
                </span>
              </div>
            </div>
            <div>
              <span class="text-gray-400 text-xs">NATS Leafnode Auth</span>
              <div class="flex items-center gap-2 mt-1">
                <span class="w-2 h-2 rounded-full" :class="statusDot(securityStatus.nats_leaf_auth)"></span>
                <span class="text-sm" :class="statusText(securityStatus.nats_leaf_auth)">
                  {{ securityStatus.nats_leaf_auth ? 'Token' : 'None' }}
                </span>
              </div>
            </div>
            <div>
              <span class="text-gray-400 text-xs">Redis Auth</span>
              <div class="flex items-center gap-2 mt-1">
                <span class="w-2 h-2 rounded-full" :class="statusDot(securityStatus.redis_auth)"></span>
                <span class="text-sm" :class="statusText(securityStatus.redis_auth)">
                  {{ securityStatus.redis_auth ? 'Password' : 'None' }}
                </span>
              </div>
            </div>
            <div>
              <span class="text-gray-400 text-xs">stunnel mTLS</span>
              <div class="flex items-center gap-2 mt-1">
                <span class="w-2 h-2 rounded-full" :class="statusDot(securityStatus.stunnel_mtls)"></span>
                <span class="text-sm" :class="statusText(securityStatus.stunnel_mtls)">
                  {{ securityStatus.stunnel_mtls ? 'verify=2' : 'No verify' }}
                </span>
              </div>
            </div>
          </div>
          <div v-if="securityStatus.auto_generated" class="text-xs text-gray-500 mt-2">
            Passwords auto-generated on first boot
          </div>
          <div class="flex items-center gap-3 pt-2 border-t border-gray-700">
            <button @click="rotateServicePasswords" :disabled="rotateLoading"
              class="text-xs px-3 py-1.5 rounded bg-amber-700 hover:bg-amber-600 text-white transition-colors disabled:opacity-50">
              {{ rotateLoading ? 'Rotating...' : 'Rotate Service Passwords' }}
            </button>
            <span v-if="rotateResult" class="text-xs text-amber-300">{{ rotateResult }}</span>
          </div>
        </div>
        <p v-else class="text-gray-500 text-sm">Security status unavailable</p>
      </div>

      <!-- Network Identity & Services -->
      <div class="grid grid-cols-1 lg:grid-cols-2 gap-4 mb-6">
        <!-- Reticulum Identity -->
        <div class="bg-tactical-surface rounded-lg border border-tactical-border p-5">
          <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">Reticulum Identity</h2>
          <div v-if="retIdentity" class="space-y-2 text-sm">
            <div>
              <span class="text-gray-400">Dest Hash</span>
              <p class="font-mono text-teal-400 text-xs break-all">{{ retIdentity.dest_hash }}</p>
            </div>
            <div>
              <span class="text-gray-400">App Name</span>
              <p class="text-gray-300">{{ retIdentity.app_name }}</p>
            </div>
            <div>
              <span class="text-gray-400">Public Key</span>
              <p class="font-mono text-[10px] text-gray-500 break-all">{{ retIdentity.public_key_hex }}</p>
            </div>
          </div>
          <p v-else class="text-gray-500 text-sm">Not loaded</p>
        </div>

        <!-- Tor & WireGuard -->
        <div class="bg-tactical-surface rounded-lg border border-tactical-border p-5">
          <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">Network Services</h2>
          <div class="space-y-3 text-sm">
            <div class="flex items-center justify-between">
              <div class="flex items-center gap-2">
                <span class="w-2 h-2 rounded-full" :class="statusDot(torStatus?.available)"></span>
                <span class="text-gray-300">Tor Hidden Service</span>
              </div>
              <span v-if="torStatus?.available" class="font-mono text-xs text-purple-400 truncate max-w-[200px]">
                {{ torStatus.http_address }}
              </span>
              <span v-else class="text-gray-500 text-xs">not configured</span>
            </div>
            <div class="flex items-center justify-between">
              <div class="flex items-center gap-2">
                <span class="w-2 h-2 rounded-full" :class="statusDot(ipougrsStatus?.enabled)"></span>
                <span class="text-gray-300">IPoUGRS Tunnel</span>
              </div>
              <span class="text-gray-500 text-xs">{{ ipougrsStatus?.enabled ? 'active' : 'disabled' }}</span>
            </div>
          </div>
        </div>
      </div>

      <!-- Sensor Codecs -->
      <div v-if="codecList.length > 0" class="bg-tactical-surface rounded-lg border border-tactical-border p-5 mb-6">
        <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">Sensor Payload Codecs</h2>
        <div class="flex flex-wrap gap-2">
          <span v-for="c in codecList" :key="c.name || c"
                class="bg-gray-700 text-gray-300 text-xs px-2.5 py-1 rounded">
            {{ c.name || c }}
          </span>
        </div>
      </div>

      <!-- Backup & Data -->
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-5 mb-6">
        <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">Backup & Data</h2>
        <div class="flex items-center gap-3">
          <button @click="exportBackup" :disabled="exportLoading"
            class="bg-teal-600 hover:bg-teal-500 disabled:bg-gray-600 text-white text-sm px-4 py-2 rounded">
            {{ exportLoading ? 'Exporting...' : 'Export Backup' }}
          </button>
          <span v-if="exportResult" class="text-emerald-400 text-sm">{{ exportResult }}</span>
        </div>
        <div v-if="error" class="mt-2 text-red-400 text-sm">{{ error }}</div>
      </div>

      <!-- API Documentation -->
      <div class="bg-tactical-surface rounded-lg border border-tactical-border p-5">
        <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">API Documentation</h2>
        <div class="space-y-2 text-sm">
          <a href="/api/docs" target="_blank"
             class="inline-flex items-center gap-2 text-teal-400 hover:text-teal-300">
            Swagger UI
            <span class="text-xs text-gray-500">/api/docs</span>
          </a>
          <div class="flex gap-4">
            <a href="/api/docs/swagger.json" target="_blank" class="text-gray-400 hover:text-gray-300 text-xs">
              OpenAPI JSON
            </a>
            <a href="/api/docs/swagger.yaml" target="_blank" class="text-gray-400 hover:text-gray-300 text-xs">
              OpenAPI YAML
            </a>
          </div>
        </div>
      </div>
    </template>
  </div>
</template>
