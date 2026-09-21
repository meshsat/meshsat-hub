<script setup>
import { ref, onMounted } from 'vue'
import { auditLog } from '../api/client'

const entries = ref([])
const chainStatus = ref(null)
const verifying = ref(false)
const loading = ref(false)

onMounted(async () => {
  await loadEntries()
})

async function loadEntries() {
  loading.value = true
  try {
    entries.value = await auditLog.list(500)
  } catch (e) {
    console.error('Failed to load audit entries:', e)
  } finally {
    loading.value = false
  }
}

async function verifyChain() {
  verifying.value = true
  try {
    chainStatus.value = await auditLog.verify()
  } catch (e) {
    console.error('Failed to verify chain:', e)
    chainStatus.value = { valid: false, error: e.message }
  } finally {
    verifying.value = false
  }
}

function actionColor(action) {
  // Actions are not states: none of them is abnormal, so none takes a hue.
  return action ? 'text-ms-text2' : 'text-ms-muted'
}
</script>

<template>
  <div class="ms-page max-w-7xl">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Audit log</h1>
        <p class="ms-lede">Every change and every inbound delivery, kept in a tamper-evident chain.</p>
      </div>
      <div class="flex gap-2">
        <button @click="verifyChain" :disabled="verifying"
          class="ms-btn-primary">
          {{ verifying ? 'Verifying...' : 'Verify the chain' }}
        </button>
        <button @click="loadEntries" :disabled="loading"
          class="text-sm text-brand-primary hover:text-brand-primary px-3 py-2">
          Refresh
        </button>
      </div>
    </div>

    <!-- Chain verification result -->
    <div v-if="chainStatus" class="rounded-lg p-4 mb-6"
      :class="chainStatus.valid ? 'bg-emerald-900/40 border border-emerald-700' : 'bg-red-900/40 border border-red-700'">
      <span v-if="chainStatus.valid" class="text-emerald-300">
        Chain verified. {{ chainStatus.verified }} entries, no tampering detected.
      </span>
      <span v-else class="text-red-300">
        Chain integrity failure{{ chainStatus.verified ? ` at entry ${chainStatus.verified}` : '' }}.
        {{ chainStatus.error || 'Possible tampering detected.' }}
      </span>
    </div>

    <!-- Audit table -->
    <div class="bg-tactical-surface rounded-lg border border-tactical-border overflow-hidden">
      <table class="w-full text-sm">
        <thead class="text-gray-400 text-left border-b border-tactical-border">
          <tr>
            <th class="px-4 py-2.5">Time</th>
            <th class="px-4 py-2.5">Action</th>
            <th class="px-4 py-2.5">Actor</th>
            <th class="px-4 py-2.5">Detail</th>
            <th class="px-4 py-2.5">IP</th>
            <th class="px-4 py-2.5">Hash</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-tactical-border/50">
          <tr v-for="e in entries" :key="e.id" class="hover:bg-white/[0.02]">
            <td class="px-4 py-2.5 text-gray-400 whitespace-nowrap">{{ e.created_at?.substring(0, 19) }}</td>
            <td class="px-4 py-2.5">
              <span :class="actionColor(e.action)">{{ e.action }}</span>
            </td>
            <td class="px-4 py-2.5">{{ e.actor }}</td>
            <td class="px-4 py-2.5 max-w-[300px] truncate">{{ e.detail }}</td>
            <td class="px-4 py-2.5 font-mono text-xs text-gray-400">{{ e.ip || '-' }}</td>
            <td class="px-4 py-2.5 font-mono text-[10px] text-gray-500 max-w-[120px] truncate" :title="e.hash">
              {{ e.hash?.substring(0, 12) }}...
            </td>
          </tr>
          <tr v-if="entries.length === 0">
            <td colspan="6" class="px-4 py-8 text-center text-gray-500">No audit entries</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
