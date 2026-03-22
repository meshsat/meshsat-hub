<script setup>
import { ref, onMounted } from 'vue'
import { email } from '../api/client'
import EmptyState from '../components/EmptyState.vue'

const contacts = ref([])
const publicKey = ref('')
const loading = ref(true)
const error = ref('')
const success = ref('')

const showForm = ref(false)
const formEmail = ref('')
const formKey = ref('')

const showTest = ref(false)
const testTo = ref('')
const testSubject = ref('MeshSat Hub Test')
const testBody = ref('This is a test email from MeshSat Hub.')
const testSending = ref(false)

onMounted(async () => {
  await loadData()
})

async function loadData() {
  loading.value = true
  const results = await Promise.allSettled([
    email.listContacts(),
    email.publicKey(),
  ])
  contacts.value = results[0].status === 'fulfilled' && Array.isArray(results[0].value) ? results[0].value : []
  publicKey.value = results[1].status === 'fulfilled' ? (results[1].value?.key || results[1].value || '') : ''
  loading.value = false
}

async function addContact() {
  if (!formEmail.value) return
  error.value = ''
  try {
    await email.addContact({ email: formEmail.value, pgp_key: formKey.value || undefined })
    formEmail.value = ''
    formKey.value = ''
    showForm.value = false
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

async function removeContact(addr) {
  if (!confirm(`Remove ${addr}?`)) return
  try {
    await email.deleteContact(addr)
    await loadData()
  } catch (e) {
    error.value = e.message
  }
}

async function sendTest() {
  if (!testTo.value) return
  testSending.value = true
  error.value = ''
  success.value = ''
  try {
    await email.testSend({ to: testTo.value, subject: testSubject.value, body: testBody.value })
    success.value = `Test email sent to ${testTo.value}`
    showTest.value = false
  } catch (e) {
    error.value = e.message
  } finally {
    testSending.value = false
  }
}
</script>

<template>
  <div class="p-4 lg:p-6 max-w-5xl mx-auto">
    <div class="flex items-center justify-between mb-6">
      <h1 class="text-2xl font-display font-bold">Email Gateway</h1>
      <div class="flex gap-2">
        <button @click="showTest = !showTest" class="text-sm text-teal-400 hover:text-teal-300 px-3 py-2">
          Test Send
        </button>
        <button @click="showForm = !showForm"
          class="bg-teal-600 hover:bg-teal-500 text-white text-sm px-4 py-2 rounded">
          + Add Contact
        </button>
      </div>
    </div>

    <div v-if="error" class="bg-red-900/50 border border-red-700 text-red-200 rounded p-3 mb-4">{{ error }}</div>
    <div v-if="success" class="bg-emerald-900/50 border border-emerald-700 text-emerald-200 rounded p-3 mb-4">{{ success }}</div>

    <!-- Test Send Form -->
    <div v-if="showTest" class="bg-tactical-surface rounded-lg border border-tactical-border p-4 mb-4">
      <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">Send Test Email</h2>
      <div class="grid grid-cols-1 sm:grid-cols-2 gap-3 mb-3">
        <input v-model="testTo" placeholder="recipient@example.com" class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
        <input v-model="testSubject" placeholder="Subject" class="bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
      </div>
      <textarea v-model="testBody" rows="3" class="w-full bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm mb-3"></textarea>
      <div class="flex gap-2">
        <button @click="sendTest" :disabled="testSending"
          class="bg-teal-600 hover:bg-teal-500 disabled:bg-gray-600 text-white text-sm px-4 py-2 rounded">
          {{ testSending ? 'Sending...' : 'Send' }}
        </button>
        <button @click="showTest = false" class="text-gray-400 hover:text-gray-300 text-sm px-3 py-2">Cancel</button>
      </div>
    </div>

    <!-- Add Contact Form -->
    <div v-if="showForm" class="bg-tactical-surface rounded-lg border border-tactical-border p-4 mb-4">
      <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-3">Add Email Contact</h2>
      <div class="space-y-3">
        <input v-model="formEmail" placeholder="email@example.com" class="w-full bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm">
        <textarea v-model="formKey" placeholder="PGP public key (armored, optional)" rows="4"
          class="w-full bg-gray-800 border border-gray-700 rounded px-3 py-2 text-sm font-mono text-xs"></textarea>
        <div class="flex gap-2">
          <button @click="addContact" class="bg-teal-600 hover:bg-teal-500 text-white text-sm px-4 py-2 rounded">Add</button>
          <button @click="showForm = false" class="text-gray-400 hover:text-gray-300 text-sm px-3 py-2">Cancel</button>
        </div>
      </div>
    </div>

    <div v-if="loading" class="text-center text-gray-500 py-8">Loading...</div>

    <template v-else>
      <!-- Hub PGP Public Key -->
      <div v-if="publicKey" class="bg-tactical-surface rounded-lg border border-tactical-border p-4 mb-4">
        <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider mb-2">Hub PGP Public Key</h2>
        <pre class="text-[10px] text-gray-500 font-mono max-h-24 overflow-y-auto">{{ publicKey }}</pre>
      </div>

      <!-- Contact List -->
      <div class="bg-tactical-surface rounded-lg border border-tactical-border overflow-hidden">
        <div class="px-4 py-3 border-b border-tactical-border">
          <h2 class="text-sm font-display font-semibold text-gray-200 uppercase tracking-wider">Contacts ({{ contacts.length }})</h2>
        </div>
        <EmptyState v-if="contacts.length === 0" icon="users" title="No email contacts" message="Add PGP-enabled contacts to send encrypted email through the gateway." />
        <div v-else class="divide-y divide-tactical-border/50">
          <div v-for="c in contacts" :key="c.email" class="px-4 py-3 flex items-center justify-between">
            <div>
              <span class="text-gray-300 text-sm">{{ c.email }}</span>
              <span v-if="c.has_pgp_key" class="ml-2 text-emerald-400 text-xs">PGP</span>
              <span v-else class="ml-2 text-gray-500 text-xs">no PGP</span>
            </div>
            <button @click="removeContact(c.email)" class="text-red-400 hover:text-red-300 text-xs">Remove</button>
          </div>
        </div>
      </div>
    </template>
  </div>
</template>
