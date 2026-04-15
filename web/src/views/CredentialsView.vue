<script setup>
import { ref, onMounted } from 'vue'
import { credentials as credApi } from '../api/client'

const creds = ref([])
const expiring = ref([])
const loading = ref(false)
const error = ref('')

// Upload state
const uploadFile = ref(null)
const uploadFileName = ref('')
const uploadProvider = ref('')
const uploadName = ref('')
const uploadScope = ref('hub')
const uploading = ref(false)
const uploadResult = ref('')

onMounted(async () => {
  await loadAll()
})

async function loadAll() {
  loading.value = true
  try {
    const data = await credApi.list()
    creds.value = data?.credentials || []
    const exp = await credApi.expiring(30)
    expiring.value = exp?.credentials || []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function onFileSelected(e) {
  const f = e.target.files[0]
  if (f) {
    uploadFile.value = f
    uploadFileName.value = f.name
    uploadResult.value = ''
  }
}

async function doUpload() {
  if (!uploadFile.value || !uploadProvider.value) return
  uploading.value = true
  error.value = ''
  uploadResult.value = ''
  try {
    const result = await credApi.upload(
      uploadFile.value, uploadProvider.value,
      uploadName.value || uploadProvider.value, uploadScope.value
    )
    uploadResult.value = `Uploaded: ${result.cred_type} (${result.subject || result.fingerprint?.substring(0, 16) || 'ok'})`
    uploadFile.value = null
    uploadFileName.value = ''
    uploadProvider.value = ''
    uploadName.value = ''
    await loadAll()
  } catch (e) {
    error.value = e.message
  } finally {
    uploading.value = false
  }
}

async function distribute(id) {
  try {
    const result = await credApi.distribute(id)
    alert(`Distributed to ${Object.keys(result.bridges || {}).length} bridge(s)`)
    await loadAll()
  } catch (e) {
    error.value = e.message
  }
}

async function deleteCred(id) {
  if (!confirm('Delete this credential?')) return
  try {
    await credApi.del(id)
    await loadAll()
  } catch (e) {
    error.value = e.message
  }
}

function expiryClass(c) {
  if (!c.cert_not_after) return 'bg-gray-600 text-gray-300'
  const days = Math.floor((new Date(c.cert_not_after) - Date.now()) / 86400000)
  if (days <= 0) return 'bg-red-600 text-white'
  if (days <= 30) return 'bg-amber-600 text-white'
  return 'bg-emerald-600 text-white'
}

function expiryLabel(c) {
  if (!c.cert_not_after) return 'No expiry'
  const days = Math.floor((new Date(c.cert_not_after) - Date.now()) / 86400000)
  if (days <= 0) return 'EXPIRED'
  return `${days}d left`
}
</script>

<template>
  <div class="max-w-5xl mx-auto space-y-6">
    <h1 class="text-2xl font-bold">Credentials</h1>
    <p class="text-sm text-gray-400">Upload and manage TLS certificates and provider credentials. Distribute to field bridges via MQTT.</p>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-300 px-4 py-2 rounded text-sm">{{ error }}</div>

    <!-- Expiry warnings -->
    <div v-if="expiring.length > 0" class="bg-amber-900/30 border border-amber-700 rounded-lg p-4">
      <h3 class="text-sm font-semibold text-amber-300 mb-2">Expiring Certificates (30 days)</h3>
      <div v-for="c in expiring" :key="c.id" class="text-xs text-amber-200 flex gap-2">
        <span class="font-mono">{{ c.name }}</span>
        <span class="px-1.5 rounded" :class="expiryClass(c)">{{ expiryLabel(c) }}</span>
        <span class="text-amber-400">{{ c.cert_subject }}</span>
      </div>
    </div>

    <!-- Upload section -->
    <div class="bg-gray-800 rounded-lg p-6 border border-gray-700">
      <h2 class="text-lg font-semibold mb-4">Upload Certificate</h2>
      <div class="border-2 border-dashed border-gray-600 rounded-lg p-6 text-center">
        <input type="file" ref="fileInput" accept=".zip,.pem,.crt,.key,.cer" @change="onFileSelected" class="hidden">
        <button @click="$refs.fileInput.click()" class="px-4 py-2 rounded bg-blue-600 text-white hover:bg-blue-500">
          Select ZIP or PEM File
        </button>
        <p v-if="uploadFileName" class="text-sm text-gray-400 mt-2">{{ uploadFileName }}</p>

        <div v-if="uploadFile" class="mt-4 flex items-center gap-3 justify-center flex-wrap">
          <select v-model="uploadProvider" class="px-3 py-2 rounded bg-gray-900 border border-gray-600 text-sm">
            <option value="">Select provider...</option>
            <option value="cloudloop_mqtt">Cloudloop MQTT</option>
            <option value="cloudloop_api">Cloudloop API</option>
            <option value="rockblock">RockBLOCK</option>
            <option value="globalstar">Globalstar</option>
            <option value="hub_mqtt">Hub MQTT</option>
            <option value="tak">TAK</option>
            <option value="custom">Custom</option>
          </select>
          <input v-model="uploadName" placeholder="Label" class="px-3 py-2 rounded bg-gray-900 border border-gray-600 text-sm w-40">
          <select v-model="uploadScope" class="px-3 py-2 rounded bg-gray-900 border border-gray-600 text-sm">
            <option value="hub">Hub only</option>
            <option value="all">All bridges</option>
            <option value="bridge">Specific bridge</option>
          </select>
          <button @click="doUpload" :disabled="uploading || !uploadProvider"
            class="px-4 py-2 rounded bg-emerald-600 text-white hover:bg-emerald-500 disabled:opacity-40">
            {{ uploading ? 'Uploading...' : 'Upload' }}
          </button>
        </div>
        <p v-if="uploadResult" class="text-sm text-emerald-400 mt-3">{{ uploadResult }}</p>
      </div>
    </div>

    <!-- Credential list -->
    <div class="bg-gray-800 rounded-lg border border-gray-700 overflow-hidden">
      <table class="w-full text-sm">
        <thead class="bg-gray-700/50">
          <tr>
            <th class="px-4 py-3 text-left text-xs text-gray-400 font-medium">Name</th>
            <th class="px-4 py-3 text-left text-xs text-gray-400 font-medium">Provider</th>
            <th class="px-4 py-3 text-left text-xs text-gray-400 font-medium">Type</th>
            <th class="px-4 py-3 text-left text-xs text-gray-400 font-medium">Subject</th>
            <th class="px-4 py-3 text-left text-xs text-gray-400 font-medium">Expiry</th>
            <th class="px-4 py-3 text-left text-xs text-gray-400 font-medium">Scope</th>
            <th class="px-4 py-3 text-right text-xs text-gray-400 font-medium">Actions</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-700">
          <tr v-for="c in creds" :key="c.id" class="hover:bg-gray-700/30">
            <td class="px-4 py-3 font-medium">{{ c.name }}</td>
            <td class="px-4 py-3 text-gray-400">{{ c.provider }}</td>
            <td class="px-4 py-3"><span class="px-2 py-0.5 rounded bg-gray-700 text-xs">{{ c.cred_type }}</span></td>
            <td class="px-4 py-3 text-xs text-gray-400 font-mono">{{ c.cert_subject || '-' }}</td>
            <td class="px-4 py-3">
              <span class="px-2 py-0.5 rounded text-xs" :class="expiryClass(c)">{{ expiryLabel(c) }}</span>
            </td>
            <td class="px-4 py-3 text-xs text-gray-400">{{ c.target_scope }}</td>
            <td class="px-4 py-3 text-right space-x-1">
              <button v-if="c.target_scope !== 'hub'" @click="distribute(c.id)"
                class="px-2 py-1 rounded bg-blue-700 text-xs text-white hover:bg-blue-600">Distribute</button>
              <button @click="deleteCred(c.id)"
                class="px-2 py-1 rounded bg-red-900 text-xs text-red-300 hover:bg-red-800">Delete</button>
            </td>
          </tr>
          <tr v-if="creds.length === 0">
            <td colspan="7" class="px-4 py-8 text-center text-gray-500">No credentials stored. Upload a certificate above.</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
