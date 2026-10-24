<script setup>
// The tenant's contacts (the directory, MESHSAT-538) as a page. Until
// MESHSAT-1685 they could only be made through the REST API or a kit, while
// the "@" lane's default lets devices write only to people listed here.
import { ref, computed, onMounted } from 'vue'
import { directory } from '../api/client'
import { useAuthStore } from '../stores/auth'

const auth = useAuthStore()
const contacts = ref([])
const loading = ref(true)
const error = ref('')
const name = ref('')
const phone = ref('')
const mail = ref('')
const saving = ref(false)

const canEdit = computed(() => auth.isOwner || auth.role === 'operator' || auth.isPlatformAdmin)

function addr(c, kinds) {
  const a = (c.addresses || []).filter((x) => kinds.includes(x.kind))
  a.sort((x, y) => (x.primary_rank || 0) - (y.primary_rank || 0))
  return a.map((x) => x.value)
}

// What a device types to write to this contact: the name with its spaces
// taken out (the lane matches it that way, case folded).
function handle(c) {
  return '@' + (c.display_name || '').replace(/\s+/g, '')
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    contacts.value = (await directory.listContacts()) || []
    contacts.value.sort((a, b) => (a.display_name || '').localeCompare(b.display_name || ''))
  } catch (e) {
    error.value = e.message || 'Could not load the contacts'
  } finally {
    loading.value = false
  }
}

async function add() {
  const n = name.value.trim()
  const p = phone.value.replace(/[\s().-]/g, '')
  const m = mail.value.trim()
  error.value = ''
  if (!n) { error.value = 'A contact needs a name'; return }
  if (!p && !m) { error.value = 'Give a phone number, an email address or both'; return }
  if (p && !/^\+[1-9][0-9]{6,14}$/.test(p)) { error.value = 'Write the number in international form, +31612345678'; return }
  if (m && !/^[^\s@<>"]+@[^\s@<>"]+\.[^\s@<>"]+$/.test(m)) { error.value = 'That email address does not look right'; return }
  const addresses = []
  if (p) addresses.push({ kind: 'SMS', value: p, primary_rank: 0 })
  if (m) addresses.push({ kind: 'EMAIL', value: m, primary_rank: p ? 1 : 0 })
  saving.value = true
  try {
    await directory.createContact({ display_name: n, addresses })
    name.value = ''; phone.value = ''; mail.value = ''
    await load()
  } catch (e) {
    error.value = e.status === 409 ? 'Another contact already has that number or address' : (e.message || 'Save failed')
  } finally {
    saving.value = false
  }
}

async function remove(c) {
  if (!confirm(`Remove ${c.display_name} from your contacts?`)) return
  try {
    await directory.deleteContact(c.id)
    await load()
  } catch (e) {
    error.value = e.message || 'Remove failed'
  }
}

onMounted(load)
</script>

<template>
  <div class="ms-page">
    <div class="ms-page-head">
      <div>
        <h1 class="ms-h1">Contacts</h1>
        <p class="ms-lede">
          The people your devices can write to. A device starts its message with a contact's name,
          number or address, and the reply comes back to the device. Who devices may write to is
          set in Settings.
        </p>
      </div>
    </div>

    <div v-if="error" role="alert" class="ms-alert mb-4">{{ error }}</div>

    <div v-if="canEdit" class="ms-panel mb-5">
      <div class="ms-panel-head"><h2 class="ms-h2">Add a contact</h2></div>
      <div class="p-4 grid grid-cols-1 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_minmax(0,1.3fr)_auto] gap-3 items-end">
        <div class="min-w-0">
          <label for="contact-name" class="ms-label block mb-1">Name</label>
          <input id="contact-name" v-model="name" class="ms-input w-full" autocomplete="off" />
        </div>
        <div class="min-w-0">
          <label for="contact-phone" class="ms-label block mb-1">Phone (SMS)</label>
          <input id="contact-phone" v-model="phone" class="ms-input w-full" inputmode="tel" placeholder="+31612345678" autocomplete="off" />
        </div>
        <div class="min-w-0">
          <label for="contact-mail" class="ms-label block mb-1">Email</label>
          <input id="contact-mail" v-model="mail" class="ms-input w-full" type="email" placeholder="name@example.org" autocomplete="off" />
        </div>
        <button class="ms-btn-primary" :disabled="saving" @click="add">Add</button>
      </div>
    </div>

    <div class="ms-panel overflow-x-auto">
      <table class="ms-table">
        <thead>
          <tr>
            <th>Name</th>
            <th>A device writes</th>
            <th>Phone</th>
            <th>Email</th>
            <th v-if="canEdit"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="c in contacts" :key="c.id">
            <td class="text-ms-text whitespace-nowrap">{{ c.display_name }}</td>
            <td class="ms-id">{{ handle(c) }}</td>
            <td class="ms-id">{{ addr(c, ['SMS', 'CELLULAR']).join(', ') || '' }}</td>
            <td class="ms-id">{{ addr(c, ['EMAIL']).join(', ') || '' }}</td>
            <td v-if="canEdit" class="text-right">
              <button class="ms-btn-danger" @click="remove(c)">Remove</button>
            </td>
          </tr>
          <tr v-if="!loading && contacts.length === 0">
            <td :colspan="canEdit ? 5 : 4" class="text-center text-ms-muted py-8">
              No contacts yet. Add the people your devices should be able to reach.
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
