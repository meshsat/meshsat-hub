<script setup>
import { ref } from 'vue'
import { backup } from '../api/client'

const error = ref('')
const success = ref('')
const exporting = ref(false)
const importing = ref(false)
const diffing = ref(false)
const diffResult = ref(null)
const importFile = ref(null)

async function exportBackup() {
  exporting.value = true
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
    success.value = `Exported: ${data.devices?.length || 0} devices, ${data.messages?.length || 0} messages, ${data.routes?.length || 0} routes`
  } catch (e) {
    error.value = e.message
  } finally {
    exporting.value = false
  }
}

function onFileSelect(e) {
  importFile.value = e.target.files[0]
}

async function diffBackup() {
  if (!importFile.value) return
  diffing.value = true
  error.value = ''
  try {
    const text = await importFile.value.text()
    const data = JSON.parse(text)
    diffResult.value = await backup.diff(data)
  } catch (e) {
    error.value = e.message
  } finally {
    diffing.value = false
  }
}

async function importBackup() {
  if (!importFile.value) return
  if (!confirm('This will merge imported data with current state. Continue?')) return
  importing.value = true
  error.value = ''
  success.value = ''
  try {
    const text = await importFile.value.text()
    const data = JSON.parse(text)
    const result = await backup.importData(data)
    success.value = `Import complete: ${result.imported || 'done'}`
    importFile.value = null
    diffResult.value = null
  } catch (e) {
    error.value = e.message
  } finally {
    importing.value = false
  }
}
</script>

<template>
  <div class="ms-page max-w-4xl">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Backup</h1>
        <p class="ms-lede">Export this account's configuration and history, or restore from a file.</p>
      </div>
    </div>

    <div v-if="error" role="alert" class="ms-alert mb-4">{{ error }}</div>
    <div v-if="success" class="bg-emerald-900/50 border border-emerald-700 text-emerald-200 rounded p-3 mb-4">{{ success }}</div>

    <!-- Export -->
    <div class="bg-tactical-surface rounded-lg border border-tactical-border p-5 mb-6">
      <h2 class="text-sm font-sans font-semibold text-gray-200 mb-3">Export</h2>
      <p class="text-sm text-gray-400 mb-3">Download a full backup of devices, messages, routes, escalation chains, and configuration.</p>
      <button @click="exportBackup" :disabled="exporting"
        class="ms-btn-primary">
        {{ exporting ? 'Exporting...' : 'Export a JSON backup' }}
      </button>
    </div>

    <!-- Import -->
    <div class="bg-tactical-surface rounded-lg border border-tactical-border p-5">
      <h2 class="text-sm font-sans font-semibold text-gray-200 mb-3">Import</h2>
      <p class="text-sm text-gray-400 mb-3">Upload a backup file to preview changes or merge with current data.</p>

      <input type="file" accept=".json" @change="onFileSelect"
        class="block w-full text-sm text-gray-400 file:mr-4 file:py-2 file:px-4 file:rounded file:border-0 file:text-sm file:bg-gray-700 file:text-gray-300 hover:file:bg-gray-600 mb-4">

      <div v-if="importFile" class="flex gap-2">
        <button @click="diffBackup" :disabled="diffing"
          class="bg-gray-600 hover:bg-gray-500 text-white text-sm px-4 py-2 rounded">
          {{ diffing ? 'Comparing...' : 'Preview changes' }}
        </button>
        <button @click="importBackup" :disabled="importing"
          class="bg-amber-600 hover:bg-amber-500 text-white text-sm px-4 py-2 rounded">
          {{ importing ? 'Importing...' : 'Import' }}
        </button>
      </div>

      <!-- Diff Result -->
      <div v-if="diffResult" class="mt-4 bg-tactical-surface rounded p-4">
        <h3 class="text-sm font-medium text-gray-300 mb-2">Preview of changes</h3>
        <pre class="text-xs text-gray-400 max-h-64 overflow-y-auto">{{ JSON.stringify(diffResult, null, 2) }}</pre>
      </div>
    </div>
  </div>
</template>
