<script setup>
import { ref, onMounted } from 'vue'
import { devices } from '../api/client'

const deviceList = ref([])
const newIMEI = ref('')
const newLabel = ref('')
const error = ref('')

onMounted(async () => {
  await loadDevices()
})

async function loadDevices() {
  try {
    deviceList.value = await devices.list()
  } catch (e) {
    error.value = e.message
  }
}

async function addDevice() {
  if (!newIMEI.value) return
  try {
    await devices.create({ imei: newIMEI.value, label: newLabel.value || newIMEI.value })
    newIMEI.value = ''
    newLabel.value = ''
    await loadDevices()
  } catch (e) {
    error.value = e.message
  }
}

async function removeDevice(imei) {
  if (!confirm(`Delete device ${imei}?`)) return
  try {
    await devices.delete(imei)
    await loadDevices()
  } catch (e) {
    error.value = e.message
  }
}
</script>

<template>
  <div>
    <h1 style="font-size: 1.5rem; font-weight: bold; margin-bottom: 1rem;">Devices</h1>

    <div v-if="error" style="background: #7f1d1d; padding: 0.75rem; border-radius: 4px; margin-bottom: 1rem;">
      {{ error }}
    </div>

    <!-- Add device form -->
    <div style="display: flex; gap: 0.5rem; margin-bottom: 1rem;">
      <input v-model="newIMEI" placeholder="IMEI" style="background: #374151; border: 1px solid #4b5563; padding: 0.5rem; border-radius: 4px; color: #f3f4f6; flex: 1;" />
      <input v-model="newLabel" placeholder="Label (optional)" style="background: #374151; border: 1px solid #4b5563; padding: 0.5rem; border-radius: 4px; color: #f3f4f6; flex: 1;" />
      <button @click="addDevice" style="background: #0891b2; color: white; padding: 0.5rem 1rem; border-radius: 4px; border: none; cursor: pointer;">Add</button>
    </div>

    <!-- Device table -->
    <table style="width: 100%; border-collapse: collapse;">
      <thead>
        <tr style="border-bottom: 1px solid #374151; text-align: left;">
          <th style="padding: 0.5rem;">IMEI</th>
          <th style="padding: 0.5rem;">Label</th>
          <th style="padding: 0.5rem;">Type</th>
          <th style="padding: 0.5rem;">Last Seen</th>
          <th style="padding: 0.5rem;"></th>
        </tr>
      </thead>
      <tbody>
        <tr v-for="d in deviceList" :key="d.imei" style="border-bottom: 1px solid #1f2937;">
          <td style="padding: 0.5rem; font-family: monospace;">{{ d.imei }}</td>
          <td style="padding: 0.5rem;">{{ d.label }}</td>
          <td style="padding: 0.5rem;">{{ d.type }}</td>
          <td style="padding: 0.5rem; color: #9ca3af;">{{ d.last_seen || '—' }}</td>
          <td style="padding: 0.5rem;">
            <button @click="removeDevice(d.imei)" style="background: #991b1b; color: white; padding: 0.25rem 0.5rem; border-radius: 4px; border: none; cursor: pointer; font-size: 0.75rem;">Delete</button>
          </td>
        </tr>
        <tr v-if="deviceList.length === 0">
          <td colspan="5" style="padding: 1rem; text-align: center; color: #6b7280;">No devices registered</td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
