<script setup>
import { ref, onMounted } from 'vue'
import { integrations } from '../api/client'

const loading = ref(true)
const error = ref('')
const items = ref([])
const copied = ref(null)

onMounted(async () => {
  try {
    const data = await integrations.list()
    items.value = Array.isArray(data) ? data : []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
})

function fullURL(path) {
  if (!path) return ''
  return window.location.origin + path
}

async function copyURL(path) {
  try {
    await navigator.clipboard.writeText(fullURL(path))
    copied.value = path
    setTimeout(() => { copied.value = null }, 2000)
  } catch {
    // Fallback for non-HTTPS contexts
    const el = document.createElement('textarea')
    el.value = fullURL(path)
    document.body.appendChild(el)
    el.select()
    document.execCommand('copy')
    document.body.removeChild(el)
    copied.value = path
    setTimeout(() => { copied.value = null }, 2000)
  }
}

function typeBadge(type) {
  const colors = {
    webhook: 'bg-blue-900/50 text-blue-300 border-blue-700/50',
    mqtt: 'bg-purple-900/50 text-purple-300 border-purple-700/50',
    sms: 'bg-amber-900/50 text-amber-300 border-amber-700/50',
    email: 'bg-emerald-900/50 text-emerald-300 border-emerald-700/50',
  }
  return colors[type] || 'bg-gray-700 text-gray-300 border-gray-600'
}
</script>

<template>
  <div class="p-4 lg:p-6 max-w-6xl mx-auto">
    <div class="mb-6">
      <h1 class="text-2xl font-display font-bold">Integrations</h1>
      <p class="text-gray-400 text-sm mt-1">Inbound message channels and webhook endpoints</p>
    </div>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 px-4 py-3 rounded mb-4">{{ error }}</div>

    <div v-if="loading" class="text-center text-gray-500 py-16">Loading integration status...</div>

    <div v-else class="grid grid-cols-1 lg:grid-cols-2 gap-4">
      <div v-for="item in items" :key="item.name"
        class="bg-tactical-surface rounded-lg border border-tactical-border p-5 flex flex-col">

        <!-- Header -->
        <div class="flex items-center justify-between mb-3">
          <div class="flex items-center gap-2.5">
            <span class="w-2.5 h-2.5 rounded-full shrink-0"
              :class="item.enabled ? 'bg-emerald-400' : 'bg-gray-600'"></span>
            <h2 class="text-sm font-display font-semibold text-gray-200">{{ item.name }}</h2>
          </div>
          <div class="flex items-center gap-2">
            <!-- MQTT connected badge -->
            <span v-if="item.type === 'mqtt' && item.enabled"
              class="text-[10px] font-bold uppercase px-1.5 py-0.5 rounded border"
              :class="item.connected
                ? 'bg-emerald-900/50 text-emerald-300 border-emerald-700/50'
                : 'bg-red-900/50 text-red-300 border-red-700/50'">
              {{ item.connected ? 'Connected' : 'Disconnected' }}
            </span>
            <!-- Type badge -->
            <span class="text-[10px] font-medium uppercase px-1.5 py-0.5 rounded border"
              :class="typeBadge(item.type)">
              {{ item.type }}
            </span>
            <!-- Enabled badge -->
            <span class="text-[10px] font-bold uppercase px-1.5 py-0.5 rounded border"
              :class="item.enabled
                ? 'bg-emerald-900/50 text-emerald-300 border-emerald-700/50'
                : 'bg-gray-800 text-gray-500 border-gray-700'">
              {{ item.enabled ? 'Enabled' : 'Disabled' }}
            </span>
          </div>
        </div>

        <!-- Description -->
        <p v-if="item.description" class="text-gray-400 text-xs mb-3">{{ item.description }}</p>

        <!-- Webhook URL -->
        <div v-if="item.webhook_url" class="mb-3">
          <div class="text-[10px] text-gray-500 uppercase tracking-wider mb-1">Endpoint</div>
          <div class="flex items-center gap-2">
            <code class="text-xs font-mono text-teal-400 bg-gray-800/50 px-2 py-1 rounded flex-1 truncate">
              {{ item.webhook_url }}
            </code>
            <button @click="copyURL(item.webhook_url)"
              class="text-xs px-2 py-1 rounded transition-colors shrink-0"
              :class="copied === item.webhook_url
                ? 'bg-emerald-900/50 text-emerald-300'
                : 'bg-gray-700 text-gray-300 hover:bg-gray-600'">
              {{ copied === item.webhook_url ? 'Copied' : 'Copy URL' }}
            </button>
          </div>
          <div class="text-[10px] text-gray-600 font-mono mt-1 truncate">{{ fullURL(item.webhook_url) }}</div>
        </div>

        <!-- Configuration -->
        <div v-if="item.config && Object.keys(item.config).length > 0" class="mb-3">
          <div class="text-[10px] text-gray-500 uppercase tracking-wider mb-1.5">Configuration</div>
          <div class="space-y-1">
            <div v-for="(val, key) in item.config" :key="key"
              class="flex items-center justify-between text-xs">
              <span class="text-gray-400">{{ key.replace(/_/g, ' ') }}</span>
              <span class="font-mono text-gray-300 truncate max-w-[60%] text-right"
                :class="{
                  'text-emerald-400': val === 'configured',
                  'text-amber-400': val === 'not set',
                  'text-red-400': val.startsWith && val.startsWith('missing'),
                }">
                {{ val }}
              </span>
            </div>
          </div>
        </div>

        <!-- Last message -->
        <div v-if="item.last_message" class="mb-3">
          <div class="text-[10px] text-gray-500 uppercase tracking-wider mb-1">Last Message</div>
          <span class="text-xs text-gray-300">{{ new Date(item.last_message).toLocaleString() }}</span>
        </div>

        <!-- Setup instructions -->
        <div v-if="item.setup" class="mt-auto pt-3 border-t border-tactical-border/50">
          <div class="flex items-start gap-2">
            <svg class="w-3.5 h-3.5 text-gray-500 mt-0.5 shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z"/>
            </svg>
            <p class="text-[11px] text-gray-500 leading-relaxed">{{ item.setup }}</p>
          </div>
        </div>
      </div>
    </div>

    <div v-if="!loading && items.length === 0 && !error" class="text-center text-gray-500 py-16">
      No integrations configured.
    </div>
  </div>
</template>
