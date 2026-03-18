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
</script>

<template>
  <div>
    <div style="display: flex; align-items: center; justify-content: space-between; margin-bottom: 1rem;">
      <h1 style="font-size: 1.5rem; font-weight: bold;">Audit Log</h1>
      <div style="display: flex; gap: 0.5rem;">
        <button @click="verifyChain" :disabled="verifying"
          style="background: #7c3aed; color: white; padding: 0.5rem 1rem; border-radius: 4px; border: none; cursor: pointer;">
          {{ verifying ? 'Verifying...' : 'Verify Chain' }}
        </button>
        <button @click="loadEntries" :disabled="loading"
          style="background: #0891b2; color: white; padding: 0.5rem 1rem; border-radius: 4px; border: none; cursor: pointer;">
          Refresh
        </button>
      </div>
    </div>

    <div v-if="chainStatus" style="padding: 0.75rem 1rem; border-radius: 4px; margin-bottom: 1rem;"
      :style="{ background: chainStatus.valid ? '#064e3b' : '#7f1d1d', border: chainStatus.valid ? '1px solid #065f46' : '1px solid #991b1b' }">
      <span v-if="chainStatus.valid" style="color: #6ee7b7;">
        Chain verified — {{ chainStatus.verified }} entries, no tampering detected.
      </span>
      <span v-else style="color: #fca5a5;">
        Chain integrity failure{{ chainStatus.verified ? ` at entry ${chainStatus.verified}` : '' }}.
        {{ chainStatus.error || 'Possible tampering detected.' }}
      </span>
    </div>

    <table style="width: 100%; border-collapse: collapse; font-size: 0.875rem;">
      <thead>
        <tr style="border-bottom: 1px solid #374151; text-align: left;">
          <th style="padding: 0.5rem;">Time</th>
          <th style="padding: 0.5rem;">Action</th>
          <th style="padding: 0.5rem;">Actor</th>
          <th style="padding: 0.5rem;">Detail</th>
          <th style="padding: 0.5rem;">IP</th>
          <th style="padding: 0.5rem;">Hash</th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="e in entries" :key="e.id" style="border-bottom: 1px solid #1f2937;">
          <td style="padding: 0.5rem; color: #9ca3af; white-space: nowrap;">{{ e.created_at?.substring(0, 19) }}</td>
          <td style="padding: 0.5rem;">
            <span :style="{
              color: e.action === 'message_received' ? '#22d3ee' : e.action === 'message_sent' ? '#a78bfa' : '#d1d5db'
            }">{{ e.action }}</span>
          </td>
          <td style="padding: 0.5rem;">{{ e.actor }}</td>
          <td style="padding: 0.5rem; max-width: 300px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">{{ e.detail }}</td>
          <td style="padding: 0.5rem; font-family: monospace; font-size: 0.75rem; color: #9ca3af;">{{ e.ip || '-' }}</td>
          <td style="padding: 0.5rem; font-family: monospace; font-size: 0.625rem; color: #6b7280; max-width: 120px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap;"
            :title="e.hash">{{ e.hash?.substring(0, 12) }}...</td>
        </tr>
        <tr v-if="entries.length === 0">
          <td colspan="6" style="padding: 1rem; text-align: center; color: #6b7280;">No audit entries</td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
