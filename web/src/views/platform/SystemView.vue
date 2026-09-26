<script setup>
// The Hub's own settings and facts, platform admins only. The MQTT public
// URL used to sit on Settings (MESHSAT-1116) where a customer could see a
// Save button that only ever 403'd; it lives here now. system_config has no
// tenant_id: this one value is handed to EVERY tenant's bridges at
// onboarding.
import { ref, onMounted } from 'vue'
import { admin as adminApi, settings, health } from '../../api/client'
import { useToastStore } from '../../stores/toast'

const toast = useToastStore()

const mqttUrl = ref('')
const mqttLoaded = ref('')
const mqttSaving = ref(false)
const mqttError = ref('')

const tiers = ref([])
const tiersError = ref('')

const live = ref(null)
const ready = ref(null)

async function load() {
  const r = await Promise.allSettled([
    settings.getMqttUrl(),
    adminApi.tenants.usage('default'),
    health.check(),
    health.readyzVerbose(),
  ])
  if (r[0].status === 'fulfilled') { mqttUrl.value = r[0].value?.mqtt_url || ''; mqttLoaded.value = mqttUrl.value }
  else mqttError.value = r[0].reason?.message || 'Could not read the MQTT URL'
  if (r[1].status === 'fulfilled') tiers.value = r[1].value?.tiers || []
  else tiersError.value = r[1].reason?.message || 'Could not read the plan tiers'
  live.value = r[2].status === 'fulfilled' ? r[2].value : null
  ready.value = r[3].status === 'fulfilled' ? r[3].value : null
}

async function saveMqttUrl() {
  mqttSaving.value = true
  mqttError.value = ''
  try {
    await settings.setMqttUrl(mqttUrl.value.trim())
    mqttLoaded.value = mqttUrl.value.trim()
    toast.success('MQTT public URL saved')
  } catch (e) {
    mqttError.value = e?.message || 'Save failed'
  } finally {
    mqttSaving.value = false
  }
}

// /readyz?verbose=1 reports each probe as {status, latency_ms, detail}; older
// builds used a bare string.
function probeOK(p) {
  const st = typeof p === 'string' ? p : p?.status
  return st === 'ok' || st === 'healthy'
}
function probeDetail(p) {
  if (!p || typeof p === 'string') return ''
  return p.detail?.error || (p.latency_ms != null ? `${p.latency_ms} ms` : '')
}

onMounted(load)
</script>

<template>
  <div class="ms-page max-w-5xl">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Platform</h1>
        <p class="ms-lede">Settings that belong to the Hub itself rather than to any one tenant.</p>
      </div>
    </div>

    <div class="grid grid-cols-1 gap-5">
      <section class="ms-panel" aria-labelledby="mqtt-h">
        <div class="ms-panel-head"><h2 id="mqtt-h" class="ms-h2">MQTT public URL</h2></div>
        <form class="px-4 py-4" @submit.prevent="saveMqttUrl">
          <p class="text-xs text-ms-muted mb-2">Shown to every tenant's kits during onboarding. Use <code class="ms-id">wss://</code> for WebSocket over TLS.</p>
          <div class="flex flex-wrap items-center gap-2">
            <label class="sr-only" for="mqtt-url">MQTT public URL</label>
            <input id="mqtt-url" v-model="mqttUrl" type="url" class="ms-input flex-1 min-w-[16rem] ms-id" placeholder="wss://hub.meshsat.net/mqtt" autocomplete="off" />
            <button type="submit" class="ms-btn-primary" :disabled="mqttSaving || !mqttUrl.trim() || mqttUrl.trim() === mqttLoaded">{{ mqttSaving ? 'Saving' : 'Save' }}</button>
          </div>
          <p v-if="mqttError" class="mt-2 text-[13px] text-ms-error">{{ mqttError }}</p>
        </form>
      </section>

      <section class="ms-panel" aria-labelledby="tiers-h">
        <div class="ms-panel-head"><h2 id="tiers-h" class="ms-h2">Plan tiers</h2></div>
        <div v-if="tiersError" class="px-4 py-3 text-[13px] text-ms-error">{{ tiersError }}</div>
        <div v-else-if="!tiers.length" class="px-4 py-6 text-sm text-ms-muted">No tiers reported.</div>
        <div v-else class="overflow-x-auto">
          <table class="ms-table">
            <thead><tr><th>Plan</th><th class="text-right">Devices and bridges</th></tr></thead>
            <tbody>
              <tr v-for="t in tiers" :key="t.plan">
                <td>{{ t.plan }}</td>
                <td class="ms-num text-right">{{ t.devices === -1 ? 'no ceiling' : t.devices }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <p class="px-4 py-3 text-xs text-ms-muted border-t border-ms-border">The numbers come from the ConfigMap (HUB_PLAN_&lt;TIER&gt;_DEVICES); changing them is an edit and an Argo sync. The ceiling gates registering, never ingest, delivery or SOS.</p>
      </section>

      <section class="ms-panel" aria-labelledby="hub-h">
        <div class="ms-panel-head">
          <h2 id="hub-h" class="ms-h2">Hub facts</h2>
          <RouterLink :to="{ name: 'settings' }" class="ms-btn-ghost text-xs">Full health is on Settings</RouterLink>
        </div>
        <dl class="px-4 py-3 grid grid-cols-1 sm:grid-cols-[auto_1fr] gap-x-6 gap-y-1.5 text-[13px]">
          <dt class="text-ms-muted">Liveness</dt>
          <dd :class="live?.status === 'ok' ? 'text-ms-text2' : 'text-ms-error'">{{ live?.status || 'unknown' }}</dd>
          <dt class="text-ms-muted">Readiness</dt>
          <dd :class="ready?.status === 'ok' ? 'text-ms-text2' : 'text-ms-error'">{{ ready?.status || 'unknown' }}</dd>
          <template v-if="live?.version"><dt class="text-ms-muted">Version</dt><dd class="ms-id">{{ live.version }}</dd></template>
          <template v-if="live?.commit"><dt class="text-ms-muted">Commit</dt><dd class="ms-id">{{ live.commit }}</dd></template>
          <template v-for="(p, name) in (ready?.checks || {})" :key="name">
            <dt class="text-ms-muted">{{ name }}</dt>
            <dd :class="probeOK(p) ? 'text-ms-text2' : 'text-ms-error'">{{ probeOK(p) ? 'ok' : 'failing' }}<span v-if="probeDetail(p)" class="text-ms-muted ms-num"> ({{ probeDetail(p) }})</span></dd>
          </template>
          <template v-for="(p, name) in (ready?.info || {})" :key="'i-' + name">
            <dt class="text-ms-muted">{{ name }}</dt>
            <dd :class="probeOK(p) ? 'text-ms-text2' : 'text-ms-warning'">{{ probeOK(p) ? 'ok' : 'not ready' }}<span v-if="probeDetail(p)" class="text-ms-muted ms-num"> ({{ probeDetail(p) }})</span></dd>
          </template>
        </dl>
      </section>
    </div>
  </div>
</template>
