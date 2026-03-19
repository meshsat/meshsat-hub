<script setup>
import { ref, onMounted } from 'vue'
import { devices, health, credits, ratelimit } from '../api/client'

const stats = ref({ devices: 0, health: 'loading', credits: null })
const budgetList = ref([])
const loading = ref(true)

onMounted(async () => {
  try {
    const [devs, h] = await Promise.all([
      devices.list().catch(() => []),
      health.check(),
    ])
    stats.value.devices = Array.isArray(devs) ? devs.length : 0
    stats.value.health = h.status || 'ok'
  } catch {
    stats.value.health = 'error'
  }

  try {
    const c = await credits.get()
    stats.value.credits = c.balance ?? c
  } catch { /* credits endpoint may not be configured */ }

  try {
    budgetList.value = await ratelimit.all() || []
  } catch { /* rate limit endpoint may return empty */ }

  loading.value = false
})

function budgetPercent(sent, cap) {
  if (!cap || cap <= 0) return 0
  return Math.min(100, Math.round((sent / cap) * 100))
}

function budgetBarColor(pct) {
  if (pct >= 90) return 'bg-red-500'
  if (pct >= 70) return 'bg-yellow-500'
  return 'bg-teal-500'
}
</script>

<template>
  <div>
    <h1 class="text-xl font-bold mb-4">Dashboard</h1>

    <!-- KPI cards -->
    <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 mb-8">
      <div class="bg-gray-800 rounded-lg p-5">
        <div class="text-gray-400 text-sm mb-1">Hub Status</div>
        <div class="text-2xl font-bold" :class="stats.health === 'ok' ? 'text-green-400' : 'text-red-400'">
          {{ stats.health }}
        </div>
      </div>

      <div class="bg-gray-800 rounded-lg p-5">
        <div class="text-gray-400 text-sm mb-1">Devices</div>
        <div class="text-2xl font-bold text-teal-400">{{ stats.devices }}</div>
      </div>

      <div class="bg-gray-800 rounded-lg p-5">
        <div class="text-gray-400 text-sm mb-1">Iridium Credits</div>
        <div class="text-2xl font-bold text-teal-400">
          {{ stats.credits !== null ? stats.credits : '—' }}
        </div>
      </div>
    </div>

    <!-- Per-device budget usage -->
    <div v-if="budgetList.length > 0">
      <h2 class="text-lg font-semibold mb-3">Device Budget Usage</h2>
      <div class="grid grid-cols-1 md:grid-cols-2 gap-4">
        <div v-for="b in budgetList" :key="b.device_id" class="bg-gray-800 rounded-lg p-4">
          <div class="flex items-center justify-between mb-2">
            <span class="font-mono text-xs text-gray-300">{{ b.device_id }}</span>
            <span v-if="b.throttled" class="text-red-400 text-xs font-semibold">THROTTLED</span>
          </div>

          <!-- Daily usage bar -->
          <div v-if="b.daily_cap > 0" class="mb-2">
            <div class="flex justify-between text-xs text-gray-400 mb-1">
              <span>Daily</span>
              <span>{{ b.daily_sent }} / {{ b.daily_cap }}</span>
            </div>
            <div class="w-full bg-gray-700 rounded-full h-2">
              <div :class="budgetBarColor(budgetPercent(b.daily_sent, b.daily_cap))"
                class="h-2 rounded-full transition-all"
                :style="{ width: budgetPercent(b.daily_sent, b.daily_cap) + '%' }">
              </div>
            </div>
          </div>

          <!-- Monthly usage bar -->
          <div v-if="b.monthly_cap > 0">
            <div class="flex justify-between text-xs text-gray-400 mb-1">
              <span>Monthly</span>
              <span>{{ b.monthly_sent }} / {{ b.monthly_cap }}</span>
            </div>
            <div class="w-full bg-gray-700 rounded-full h-2">
              <div :class="budgetBarColor(budgetPercent(b.monthly_sent, b.monthly_cap))"
                class="h-2 rounded-full transition-all"
                :style="{ width: budgetPercent(b.monthly_sent, b.monthly_cap) + '%' }">
              </div>
            </div>
          </div>

          <div v-if="b.daily_cap <= 0 && b.monthly_cap <= 0" class="text-xs text-gray-500">
            No budget limits configured
          </div>
        </div>
      </div>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>
  </div>
</template>
